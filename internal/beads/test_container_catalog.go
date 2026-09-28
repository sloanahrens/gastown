package beads

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver for pre-creating test databases
)

// Test Dolt container catalog gate.
//
// A Dolt sql-server (dolthub/dolt-sql-server:2.0.7, the image testutil runs)
// answers an information_schema query with
//
//	Error 1105 (HY000): could not resolve initial root for database <other>/
//	Error 1105 (HY000): no root value found in session
//
// when another session is creating or dropping a database at the same moment:
// the query walks every database in the catalog, and <other> is the one whose
// root is still being made or already gone. Plain reads and writes are not
// affected; only statements that enumerate the catalog are. bd issues exactly
// those statements on every store open and all through a schema migration.
//
// Every container-backed test process shares one server, so one test's bd init
// (CREATE DATABASE) or cleanup (DROP DATABASE) fails the store open or the
// migration of whichever test is mid-flight beside it. A migration killed
// between two steps leaves its working set dirty; bd retries its own open, and
// that retry meets the dirty tables and refuses with "pending schema migrations
// alter pre-existing dirty tables" (gastownhall/beads#4566) — the top flake
// family of the gate.
//
// The gate removes the overlap instead of retrying through it. Catalog changes
// take it exclusively (ChangeTestCatalog); every bd subprocess this process runs
// while a shared test container is up takes it shared. Isolated Init pre-creates
// its database under the exclusive side, so bd init itself only ever meets an
// existing database and changes no catalog.
var (
	testCatalog         sync.RWMutex
	testContainerActive atomic.Bool
	testContainerPort   atomic.Int64
)

// MarkTestContainerActive records that this process runs a shared test Dolt
// container on port, which turns on the shared side of the catalog gate for
// every bd subprocess and the pre-create step of isolated inits that target
// that port. testutil calls it once the container answers. It is a no-op
// outside a test binary.
func MarkTestContainerActive(port int) {
	if testing.Testing() {
		testContainerPort.Store(int64(port))
		testContainerActive.Store(true)
	}
}

// isSharedTestContainerPort reports whether port is the shared test Dolt
// container MarkTestContainerActive recorded. Wrappers aimed anywhere else — a
// stub bd on a made-up port, a per-test isolated container — have no catalog
// on this process's gate to pre-create in.
func isSharedTestContainerPort(port int) bool {
	return testContainerActive.Load() && port > 0 && int64(port) == testContainerPort.Load()
}

// ChangeTestCatalog runs fn — a CREATE DATABASE, DROP DATABASE or
// dolt_purge_dropped_databases against the shared test Dolt container — while
// no bd subprocess of this process is running. fn must not run bd itself: the
// gate is not reentrant.
func ChangeTestCatalog(fn func() error) error {
	testCatalog.Lock()
	defer testCatalog.Unlock()
	return fn()
}

// shareTestCatalog takes the shared side of the gate for one bd subprocess and
// returns its release. Outside a test binary with an active container it takes
// nothing.
func shareTestCatalog() (release func()) {
	if !testing.Testing() || !testContainerActive.Load() {
		return func() {}
	}
	testCatalog.RLock()
	return testCatalog.RUnlock
}

// testDatabaseDDLTimeout bounds one CREATE or DROP DATABASE against the test
// container, connection included.
const testDatabaseDDLTimeout = 2 * time.Minute

// TestDatabaseDSN is the server-level DSN of the test Dolt container on port.
func TestDatabaseDSN(port int) string {
	return fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s&readTimeout=2m&writeTimeout=2m", port)
}

// CreateTestDatabase creates the empty database name on the test Dolt
// container on port, under the exclusive side of the catalog gate.
func CreateTestDatabase(port int, name string) error {
	return execTestCatalogDDL(port, "CREATE DATABASE IF NOT EXISTS `"+name+"`")
}

// DropTestDatabase drops name from the test Dolt container on port and purges
// it from disk, under the exclusive side of the catalog gate.
func DropTestDatabase(port int, name string) error {
	return execTestCatalogDDL(port, "DROP DATABASE IF EXISTS `"+name+"`", "CALL dolt_purge_dropped_databases()")
}

// ExecTestCatalogDDL runs statements, in order, on one server-level session of
// the test Dolt container on port, under the exclusive side of the catalog gate.
// It is for test helpers whose own catalog changes (CREATE/DROP DATABASE) would
// otherwise race every bd call beside them.
func ExecTestCatalogDDL(port int, statements ...string) error {
	return execTestCatalogDDL(port, statements...)
}

func execTestCatalogDDL(port int, statements ...string) error {
	for _, s := range statements {
		if strings.ContainsAny(s, ";") {
			return fmt.Errorf("test catalog DDL: refusing multi-statement %q", s)
		}
	}
	return ChangeTestCatalog(func() error {
		db, err := sql.Open("mysql", TestDatabaseDSN(port))
		if err != nil {
			return fmt.Errorf("test catalog DDL: open: %w", err)
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), testDatabaseDDLTimeout)
		defer cancel()
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("test catalog DDL: connect to 127.0.0.1:%d: %w", port, err)
		}
		defer conn.Close()
		for _, s := range statements {
			if _, err := conn.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("test catalog DDL %q: %w", s, err)
			}
		}
		return nil
	})
}

// initTestDatabaseArg returns the --database value of a bd init argv minted for
// the test container (bdInitOnTestDatabase), or "".
func initTestDatabaseArg(args []string) string {
	if !bdInitOnTestDatabase(args) {
		return ""
	}
	for i, arg := range args {
		switch {
		case arg == "--database" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(arg, "--database="):
			return arg[len("--database="):]
		}
	}
	return ""
}

// TestDatabaseName is the database the last isolated Init (NewIsolatedWithPort)
// created on the test Dolt container, retries included, or "" before one ran.
// Test cleanup drops it with DropTestDatabase.
func (b *Beads) TestDatabaseName() string {
	return b.testDatabase
}

// TestServerPort is the test Dolt container port this wrapper was built with
// (NewIsolatedWithPort), or 0.
func (b *Beads) TestServerPort() int {
	return b.serverPort
}

// WithSharedTestCatalog runs fn — in-process work against the shared test Dolt
// container, such as a beadsdk store open and its schema migration — under the
// shared side of the catalog gate, the side every bd subprocess takes.
func WithSharedTestCatalog(fn func() error) error {
	release := shareTestCatalog()
	defer release()
	return fn()
}

// Catalog changes are batched, because each one waits for every bd call in
// flight — a schema migration included — and holds every new one back until it
// is done. One CREATE per init would put every init's migration behind the one
// before it; one exclusive section per batch lets a package's inits migrate
// side by side again. Batches double as the pool is drained, so a package that
// runs many inits (-count=5) stalls a handful of times, not once per eight.
const (
	// testDatabasePoolFirstBatch is how many empty databases the first
	// exclusive section creates for isolated inits to take; each later
	// refill doubles, up to testDatabasePoolMaxBatch.
	testDatabasePoolFirstBatch = 8
	testDatabasePoolMaxBatch   = 64
	// testDatabaseDropBatch is how many released databases wait before one
	// exclusive section drops them. A test database is about 2 MB on the
	// container's tmpfs, so drops exist to bound a runaway run, not to
	// reclaim space mid-suite; whatever is still queued when the test
	// process exits goes with its container.
	testDatabaseDropBatch = 128
)

var testDatabasePool struct {
	sync.Mutex
	port  int
	batch int      // size of the next refill
	ready []string // created, empty, not yet handed to an init
	drops []string // released by their tests, not yet dropped
}

// nextPoolBatch returns the size of the next refill of a pool whose previous
// refill was prev (0 for none), doubling up to testDatabasePoolMaxBatch.
func nextPoolBatch(prev int) int {
	if prev <= 0 {
		return testDatabasePoolFirstBatch
	}
	return min(prev*2, testDatabasePoolMaxBatch)
}

// takePooledTestDatabase returns an empty database on the shared test
// container on port, created under the exclusive side of the gate. When the
// pool is empty it creates a whole batch in one exclusive section.
// nextPoolBatch sizes the batch.
func takePooledTestDatabase(port int) (string, error) {
	testDatabasePool.Lock()
	defer testDatabasePool.Unlock()
	if testDatabasePool.port != port {
		testDatabasePool.port = port
		testDatabasePool.batch = 0
		testDatabasePool.ready = nil
		testDatabasePool.drops = nil
	}
	if len(testDatabasePool.ready) == 0 {
		n := nextPoolBatch(testDatabasePool.batch)
		names := make([]string, n)
		stmts := make([]string, n)
		for i := range names {
			names[i] = testDatabaseName()
			stmts[i] = "CREATE DATABASE IF NOT EXISTS `" + names[i] + "`"
		}
		if err := execTestCatalogDDL(port, stmts...); err != nil {
			return "", err
		}
		testDatabasePool.batch = n
		testDatabasePool.ready = names
	}
	name := testDatabasePool.ready[0]
	testDatabasePool.ready = testDatabasePool.ready[1:]
	return name, nil
}

// ReleaseTestDatabase queues name, a database a finished test created on the
// shared test container on port, for dropping. Drops run in batches under the
// exclusive side of the gate, for the reason takePooledTestDatabase creates in
// batches.
func ReleaseTestDatabase(port int, name string) error {
	testDatabasePool.Lock()
	defer testDatabasePool.Unlock()
	testDatabasePool.drops = append(testDatabasePool.drops, name)
	if len(testDatabasePool.drops) < testDatabaseDropBatch {
		return nil
	}
	stmts := make([]string, 0, len(testDatabasePool.drops)+1)
	for _, n := range testDatabasePool.drops {
		stmts = append(stmts, "DROP DATABASE IF EXISTS `"+n+"`")
	}
	stmts = append(stmts, "CALL dolt_purge_dropped_databases()")
	testDatabasePool.drops = nil
	return execTestCatalogDDL(port, stmts...)
}

// withInitDatabaseArg returns args with the --database value of a bd init
// argv replaced by name.
func withInitDatabaseArg(args []string, name string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, arg := range out {
		switch {
		case arg == "--database" && i+1 < len(out):
			out[i+1] = name
			return out
		case strings.HasPrefix(arg, "--database="):
			out[i] = "--database=" + name
			return out
		}
	}
	return out
}

// NextTestPoolBatch is nextPoolBatch for pools of test databases kept outside
// this package (testutil's in-process store pool).
func NextTestPoolBatch(prev int) int {
	return nextPoolBatch(prev)
}
