package daemon

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

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
// carries the test pool's prefix (testutil.doltSQLPoolPrefix), which the
// orphan cleanups treat as test cruft.
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
