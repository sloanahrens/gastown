//go:build integration

package daemon

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/testutil"
)

// testDoltServerDaemon returns a *Daemon that resolves the running Dolt
// server through the package's shared ephemeral container (started by
// TestMain via testutil.WithDolt) rather than the live town on :3307. Without
// the GT_TEST_DOCKER=1 opt-in there is no container and the test skips; once
// opted in, a missing container fails it.
func testDoltServerDaemon(t *testing.T) *Daemon {
	t.Helper()
	containerPort := testutil.DoltContainerPort()
	if containerPort == "" {
		if testutil.DockerTestsEnabled() {
			t.Fatal("no shared Dolt container, though " + testutil.DockerTestsEnv + "=1 opted in")
		}
		t.Skip("no shared Dolt container: container-backed tests are opt-in (" + testutil.DockerTestsEnv + "=1)")
	}

	// gt reads the endpoint from the town's config only (gt-y3pgh.3), so
	// the daemon gets a town whose endpoint is the container.
	townRoot := t.TempDir()
	dataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("listener:\n  port: "+containerPort+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(io.Discard, "", 0)}

	// Refuse to run against anything but the package's ephemeral container.
	// d.doltServerPort() is 0 when the town names no endpoint; a port that
	// is not the container's would let every test below create and drop
	// databases on some other server instead of the disposable container.
	if port := d.doltServerPort(); strconv.Itoa(port) != containerPort {
		t.Fatalf("refusing to run: Dolt port resolved to %d (production default is %d), want ephemeral container port %s (town endpoint not read?)",
			port, doltserver.DefaultPort, containerPort)
	}

	return d
}

// testDoltSQLTimeout bounds the SQL these tests issue themselves against the
// package's container (each test's setup and verification statements). Dolt
// DDL and commits are fsync-bound and serialized on the server, and the
// container shares the Docker VM's disk with every other package's container
// during a -p=8 gate: a 15s bound failed at host load ~59 (gt-81fp6). It guards
// only a disposable local container, so it costs time only when that container
// is truly wedged.
const testDoltSQLTimeout = 2 * time.Minute

// createTestDB hands the test an empty database on the shared container and
// returns its name. The database was created with the container's pool before
// any test ran, and is never dropped: a CREATE or DROP DATABASE while other
// tests run breaks their store opens and migrations, and the pool's teardown
// guard fails the package on one (internal/testutil/doltpool.go). Its name
// carries the SQL test pool's name prefix (see internal/testutil/doltpool.go),
// which the orphan cleanups treat as test cruft.
func createTestDB(t *testing.T) string {
	t.Helper()
	return testutil.TakePooledSQLDatabase(t)
}

// openTestDoltDB opens a connection to dbName on the Dolt server d resolves
// to (the package's container, per testDoltServerDaemon), with read and write
// timeouts generous enough for the container under gate load.
func openTestDoltDB(d *Daemon, dbName string) (*sql.DB, error) {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=%s&writeTimeout=%s",
		d.doltServerHost(), d.doltServerPort(), dbName, testDoltSQLTimeout, testDoltSQLTimeout)
	return sql.Open("mysql", dsn)
}

// setupJournaledTown makes a town whose hq .beads is a bd-initialized
// workspace on the package's container, as a real town's is, and returns its
// root with a bd handle on hq and the in-process store opened from its config.
//
// A ConvoyManager test needs both halves on one database: the manager reads
// closes through bd (bd events tail, gt-7iwy0.2, from the store's canonical
// .beads) and convoy tracking through the store. OpenTestStore's pooled
// database is migrated by the store library and has no bd_events_journal, and
// the library cannot write to bd's schema (gt-idv8s). So, as in production, bd
// makes the database and every write, journaled (each workspace's config
// turns events-journal on), and the store only reads.
func setupJournaledTown(t *testing.T) (string, *beads.Beads, convoy.Store) {
	t.Helper()
	townRoot := t.TempDir()
	bd, store := initJournaledWorkspace(t, townRoot, "hq")
	return townRoot, bd, store
}

// addJournaledRig adds rig to townRoot as setupJournaledTown made its hq: a
// bd workspace reached at <town>/<rig>/.beads, where doltserver.FindRigBeadsDir
// looks, and a route sending prefix- ids to it. The workspace is made outside
// the town and linked in: bd init walks up from a directory inside the town,
// finds hq's workspace and refuses to init over it (a real rig is a git clone,
// which bounds that walk).
func addJournaledRig(t *testing.T, townRoot, rig, prefix string) (*beads.Beads, convoy.Store) {
	t.Helper()
	rigDir := t.TempDir()
	bd, store := initJournaledWorkspace(t, rigDir, prefix)
	if err := os.Symlink(rigDir, filepath.Join(townRoot, rig)); err != nil {
		t.Fatalf("link rig %s: %v", rig, err)
	}
	route := fmt.Sprintf(`{"prefix":"%s-","path":"%s/.beads"}`+"\n", prefix, rig)
	f, err := os.OpenFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open routes: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(route); err != nil {
		t.Fatalf("write route for %s: %v", rig, err)
	}
	return bd, store
}

// initJournaledWorkspace runs bd init --prefix prefix in dir against the
// package's container, turns its events journal on and returns the bd-backed
// convoy store pinned to that workspace — the same shape the daemon opens
// (beads.NewPinned).
func initJournaledWorkspace(t *testing.T, dir, prefix string) (*beads.Beads, convoy.Store) {
	t.Helper()
	testutil.RequireDoltContainer(t)
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil {
		t.Fatalf("container port %q: %v", testutil.DoltContainerPort(), err)
	}
	args := []string{"init", "--prefix", prefix, "--quiet", "--server", "--server-port", strconv.Itoa(port)}
	if out, err := beads.RunTestContainerInit(t.Context(), dir, args, nil); err != nil {
		t.Fatalf("bd init %s in %s: %v\n%s", prefix, dir, err, out)
	}
	bd := beads.NewIsolatedWithPort(dir, port)
	if _, err := beads.EnsureEventsJournal(bd); err != nil {
		t.Fatalf("events journal on in %s: %v", dir, err)
	}
	// GT_DOLT_PORT keeps the pinned store on the package's container: without
	// it a workspace whose metadata bd cannot read would reach the live town's
	// server on 3307.
	store := beads.NewPinned(filepath.Join(dir, ".beads"),
		beads.WithEnv(append(os.Environ(), "GT_DOLT_PORT="+testutil.DoltContainerPort())))
	return bd, store
}

// storeTestSlots bounds how many store-backed tests this package runs at once.
// Each store opens its own database and runs the full beads schema migration
// against the single Dolt container TestMain starts; unbounded, that load
// outruns the container and tests fail on "invalid connection" (gt-ihei).
var storeTestSlots = make(chan struct{}, 4)

// takeStoreSlot claims one of storeTestSlots for the calling test and returns
// it when the test ends. Call it once per test, directly after t.Parallel()
// and before any store work.
func takeStoreSlot(t *testing.T) {
	t.Helper()
	storeTestSlots <- struct{}{}
	t.Cleanup(func() { <-storeTestSlots })
}
