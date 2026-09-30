//go:build integration

package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
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

	d := &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0)}

	// Refuse to run against anything but the package's ephemeral container.
	// d.doltServerPort() falls back to doltserver.DefaultPort (3307) — the
	// live production town — whenever GT_DOLT_PORT hasn't propagated to this
	// process. Silently proceeding in that case would let every test below
	// create and drop databases on production instead of the disposable
	// container.
	if port := d.doltServerPort(); strconv.Itoa(port) != containerPort {
		t.Fatalf("refusing to run: Dolt port resolved to %d (production default is %d), want ephemeral container port %s (GT_DOLT_PORT not propagated?)",
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

// setupTestStore opens a real beads database for integration tests. It skips
// only when container tests are not opted in (GT_TEST_DOCKER unset) or Docker
// is absent; once opted in, any error fails the test — a skipped store test is
// coverage lost without a red signal. The store is also closed when the test
// ends; calling cleanup earlier is fine.
//
// BEADS_TEST_MODE is set once in TestMain, not here: t.Setenv would forbid
// t.Parallel in every caller (gt-fx3c).
func setupTestStore(t *testing.T) (beadsdk.Storage, func()) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenTestStore(t, ctx)
	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	return store, func() { _ = store.Close() }
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
// makes the database and every write, journaled (SuppressBDSideEffects sets
// BD_EVENTS_JOURNAL=1), and the store only reads.
func setupJournaledTown(t *testing.T) (string, *beads.Beads, beadsdk.Storage) {
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
func addJournaledRig(t *testing.T, townRoot, rig, prefix string) (*beads.Beads, beadsdk.Storage) {
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
// package's container and opens the store its metadata.json names.
func initJournaledWorkspace(t *testing.T, dir, prefix string) (*beads.Beads, beadsdk.Storage) {
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
	store, err := beads.OpenStoreFromConfig(context.Background(), filepath.Join(dir, ".beads"))
	if err != nil {
		t.Fatalf("open store on the bd workspace in %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
