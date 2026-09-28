package testutil

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver for creating the pool

	"github.com/steveyegge/gastown/internal/beads"
)

// The shared test Dolt container's database pool.
//
// A Dolt sql-server (dolthub/dolt-sql-server:2.0.7) fails a session's
// information_schema reads and savepoints with "could not resolve initial root
// for database X/" or "no root value found in session" while another session
// runs CREATE or DROP DATABASE. bd issues exactly those statements on every
// store open and all through a schema migration, so one test's bd init (a
// CREATE) or cleanup (a DROP) broke the migration of the test beside it; the
// migration left its working set dirty, bd retried its own open, and the retry
// refused with "pending schema migrations alter pre-existing dirty tables"
// (gastownhall/beads#4566) — the top flake family of the gate.
//
// So the catalog never changes while tests run. Every database a test process
// needs is created when its container starts, before any test can reach the
// container, and nothing is dropped: the container, and every database in it,
// goes at teardown. Isolated beads Inits (beads.SetTestDatabaseSource) and
// in-process test stores (OpenTestStore) take their database from the pool.
//
// Each entry is a store path whose BEADS_TEST_MODE database name
// ("testdb_" + FNV-64a of the path, beads internal/storage/dolt
// applyConfigDefaults) is what the pool created, so the same entry serves a
// bd init (the name) and a beadsdk.Open (the path).

// doltPoolPerRun is how many databases the pool holds per -count iteration.
// Measured per iteration on 2026-09-28: daemon 35, convoy 32, refinery 19,
// cmd 12, mail 1 — so a package can nearly quadruple its store tests before it
// runs out, and teardown warns once a run uses three quarters. Creating the
// 128 empty databases takes about 0.4s.
const doltPoolPerRun = 128

// doltSQLPoolPerRun is how many plain SQL databases (TakePooledSQLDatabase)
// the pool holds per -count iteration. Measured 2026-09-28: daemon uses 7 per
// iteration; the rest of the pool's users take store databases.
const doltSQLPoolPerRun = 16

// doltSQLPoolPrefix names the plain SQL databases. Tests push them over real
// remotes, and pushDatabase refuses the "test" prefixes the store databases
// carry; "dolt_remotes_check_" is the prefix the orphan cleanups (the reaper's
// testPollutionPrefixes, jsonl_git_backup's discovery, gt dolt cleanup) already
// treat as test cruft, so a daemon test that lists the server's databases skips
// these as it skips the store databases.
const doltSQLPoolPrefix = "dolt_remotes_check_pool_"

// doltPoolDDLTimeout bounds each CREATE DATABASE of the pool.
const doltPoolDDLTimeout = 2 * time.Minute

var doltPool struct {
	sync.Mutex
	port    int
	base    string
	entries []string // store paths, in creation order
	next    int      // index of the next entry to hand out
	sql     []string // plain SQL database names, in creation order
	sqlNext int      // index of the next plain SQL database to hand out
	names   map[string]bool
}

// testCount is this test binary's -test.count. The shared container can start
// from TestMain, before the testing package parses its flags, so os.Args is
// read when the flag is still at its default.
func testCount() int {
	if f := flag.Lookup("test.count"); f != nil && flag.Parsed() {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return testCountFromArgs(os.Args[1:])
}

func testCountFromArgs(args []string) int {
	for i, a := range args {
		for _, name := range []string{"-test.count", "--test.count"} {
			v := ""
			switch {
			case a == name && i+1 < len(args):
				v = args[i+1]
			case strings.HasPrefix(a, name+"="):
				v = a[len(name)+1:]
			default:
				continue
			}
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// beadsTestModeDatabase is the database name beadsdk.Open picks for dbPath
// under BEADS_TEST_MODE=1.
func beadsTestModeDatabase(dbPath string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(dbPath))
	return fmt.Sprintf("testdb_%x", h.Sum64())
}

// createDoltPool creates the pool on the container at port. It runs inside the
// shared container's sync.Once, so no test can reach the container until it
// returns. It installs the pool as the source of isolated beads Inits.
func createDoltPool(port int) error {
	base, err := os.MkdirTemp("", "gt-doltpool-")
	if err != nil {
		return fmt.Errorf("dolt test pool dir: %w", err)
	}
	size := doltPoolPerRun * testCount()
	entries := make([]string, size)
	sqlNames := make([]string, doltSQLPoolPerRun*testCount())
	names := make(map[string]bool, size+len(sqlNames))
	create := make([]string, 0, size+len(sqlNames))
	for i := range entries {
		entries[i] = filepath.Join(base, fmt.Sprintf("s%04d", i), ".beads", "dolt")
		create = append(create, beadsTestModeDatabase(entries[i]))
	}
	for i := range sqlNames {
		sqlNames[i] = fmt.Sprintf("%s%04d", doltSQLPoolPrefix, i)
		create = append(create, sqlNames[i])
	}
	for _, name := range create {
		names[name] = true
	}

	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s", port))
	if err != nil {
		return fmt.Errorf("dolt test pool: %w", err)
	}
	defer db.Close()
	// The guard at teardown allows the image's own databases and the pool's,
	// nothing else; a fresh container holding anything more means the image
	// changed under doltImageDatabases, and the guard would misjudge it.
	if err := verifyDoltCatalog(db, nil); err != nil {
		return fmt.Errorf("dolt test pool: the fresh container is not what doltImageDatabases describes: %w", err)
	}
	for i, name := range create {
		ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
		_, err := db.ExecContext(ctx, "CREATE DATABASE `"+name+"`")
		cancel()
		if err != nil {
			return fmt.Errorf("dolt test pool: create database %d of %d (%s): %w", i+1, len(create), name, err)
		}
	}

	doltPool.Lock()
	doltPool.port, doltPool.base, doltPool.entries, doltPool.next, doltPool.names = port, base, entries, 0, names
	doltPool.sql, doltPool.sqlNext = sqlNames, 0
	doltPool.Unlock()
	beads.SetTestDatabaseSource(func(p int) (string, error) {
		if p != port {
			return "", nil
		}
		path, err := takeDoltPoolPath()
		if err != nil {
			return "", err
		}
		return beadsTestModeDatabase(path), nil
	})
	return nil
}

// takeDoltPoolPath hands out the next pool entry's store path, created on disk.
func takeDoltPoolPath() (string, error) {
	doltPool.Lock()
	defer doltPool.Unlock()
	if doltPool.next >= len(doltPool.entries) {
		return "", fmt.Errorf("the shared test Dolt container's database pool is exhausted (%d databases, %d per -count iteration): "+
			"raise doltPoolPerRun in internal/testutil/doltpool.go — creating a database while tests run would reopen the catalog race it prevents",
			len(doltPool.entries), doltPoolPerRun)
	}
	p := doltPool.entries[doltPool.next]
	doltPool.next++
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("dolt test pool store dir: %w", err)
	}
	return p, nil
}

// TakePooledSQLDatabase hands t an empty database on the shared test Dolt
// container, created with the pool before any test ran, for a test that works
// with a database directly over SQL rather than through a beads store. The
// test owns it for the rest of the run and must not drop it: it goes with the
// container, and a DROP while other tests run is the catalog change the pool
// prevents. Its name starts with doltSQLPoolPrefix.
func TakePooledSQLDatabase(t testing.TB) string {
	t.Helper()
	doltPool.Lock()
	defer doltPool.Unlock()
	if doltPool.port == 0 {
		t.Fatal("TakePooledSQLDatabase: the shared test Dolt container has no pool; start it first (RequireDoltContainer or WithDolt)")
	}
	if doltPool.sqlNext >= len(doltPool.sql) {
		t.Fatalf("the shared test Dolt container's SQL database pool is exhausted (%d databases, %d per -count iteration): "+
			"raise doltSQLPoolPerRun in internal/testutil/doltpool.go — creating a database while tests run would reopen the catalog race it prevents",
			len(doltPool.sql), doltSQLPoolPerRun)
	}
	name := doltPool.sql[doltPool.sqlNext]
	doltPool.sqlNext++
	return name
}

// IsDoltPoolDatabase reports whether name is one of the databases the shared
// test container's pool created before tests ran.
func IsDoltPoolDatabase(name string) bool {
	doltPool.Lock()
	defer doltPool.Unlock()
	return doltPool.names[name]
}

// DoltPoolUsage reports how many pool databases have been handed out, and the
// pool's size.
func DoltPoolUsage() (used, size int) {
	doltPool.Lock()
	defer doltPool.Unlock()
	return doltPool.next, len(doltPool.entries)
}

// doltImageDatabases are the databases a fresh container of DoltDockerImage
// holds, started with doltContainerOpts: the two the server always has, and
// gt_test, which dolt.WithDatabase creates. Measured on
// dolthub/dolt-sql-server:2.0.7 (2026-09-28); createDoltPool checks a fresh
// container against it, so an image that adds one fails at startup rather
// than at the guard.
var doltImageDatabases = []string{"gt_test", "information_schema", "mysql"}

// ErrDoltCatalogChanged marks a teardown that found the shared test
// container's catalog changed after its pool was created.
var ErrDoltCatalogChanged = errors.New("the shared test Dolt container's catalog changed while tests ran")

// verifyDoltCatalog checks that the server behind db holds exactly the image's
// databases plus pool, and that none was dropped. A database created while
// tests run is the catalog change the pool exists to prevent: Dolt fails
// other sessions' store opens and migrations while it happens (see the top of
// this file). A drop is the same change, so a database that is gone, or that
// Dolt still holds for dolt_undrop, fails too; that also catches a database
// created and dropped again between checks.
func verifyDoltCatalog(db *sql.DB, pool map[string]bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
	defer cancel()
	present, err := showDatabases(ctx, db)
	if err != nil {
		return err
	}
	dropped, err := undroppableDatabases(ctx, db)
	if err != nil {
		return err
	}
	return catalogViolations(present, dropped, pool)
}

// catalogViolations is verifyDoltCatalog's verdict on the databases the
// server showed (present) and the ones it holds as dropped.
func catalogViolations(present, dropped []string, pool map[string]bool) error {
	allowed := make(map[string]bool, len(pool)+len(doltImageDatabases))
	for name := range pool {
		allowed[name] = true
	}
	for _, name := range doltImageDatabases {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(present))
	var problems []string
	for _, name := range present {
		seen[name] = true
		if !allowed[name] {
			problems = append(problems, fmt.Sprintf("database %q was created, and is neither the image's nor the pool's", name))
		}
	}
	var missing []string
	for name := range allowed {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		problems = append(problems, fmt.Sprintf("database %q was dropped", name))
	}
	for _, name := range dropped {
		if !allowed[name] || !seen[name] {
			problems = append(problems, fmt.Sprintf("database %q was dropped (Dolt still holds it for dolt_undrop)", name))
		} else {
			problems = append(problems, fmt.Sprintf("database %q was dropped and created again", name))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s\nEvery database a test uses must come from the pool created before tests run "+
		"(beads.NewIsolatedWithPort + Init, testutil.OpenTestStore), and nothing may be dropped until the container goes: "+
		"a CREATE or DROP DATABASE while other tests run breaks their store opens and migrations (internal/testutil/doltpool.go)",
		ErrDoltCatalogChanged, strings.Join(problems, "\n  - "))
}

func showDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("SHOW DATABASES: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("SHOW DATABASES: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SHOW DATABASES: %w", err)
	}
	return names, nil
}

// Dolt keeps a dropped database for dolt_undrop until it is purged, and
// dolt_undrop called with no name refuses with the list of them. That refusal
// is the only place the server names them (dolt 2.0.7).
const (
	undropNoneMarker = "there are no databases currently available to be undropped"
	undropListMarker = "available databases that can be undropped: "
)

// undroppableDatabases returns the databases Dolt holds as dropped.
func undroppableDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	_, err := db.ExecContext(ctx, "CALL dolt_undrop()")
	if err == nil {
		return nil, errors.New("CALL dolt_undrop() with no name succeeded; expected Dolt to refuse and list the dropped databases")
	}
	return parseUndropRefusal(err.Error())
}

// parseUndropRefusal reads the dropped databases out of dolt_undrop's refusal.
func parseUndropRefusal(msg string) ([]string, error) {
	if strings.Contains(msg, undropNoneMarker) {
		return nil, nil
	}
	_, list, ok := strings.Cut(msg, undropListMarker)
	if !ok {
		return nil, fmt.Errorf("CALL dolt_undrop(): unrecognized answer, cannot tell which databases were dropped: %s", msg)
	}
	var names []string
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// checkDoltPoolCatalog runs verifyDoltCatalog against the shared container
// the pool lives on. It is a no-op when no pool was created.
func checkDoltPoolCatalog() error {
	doltPool.Lock()
	port, names := doltPool.port, doltPool.names
	doltPool.Unlock()
	if port == 0 {
		return nil
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s", port))
	if err != nil {
		return fmt.Errorf("%w: cannot check it: %v", ErrDoltCatalogChanged, err)
	}
	defer db.Close()
	if err := verifyDoltCatalog(db, names); err != nil {
		if errors.Is(err, ErrDoltCatalogChanged) {
			return err
		}
		return fmt.Errorf("%w: cannot check it: %v", ErrDoltCatalogChanged, err)
	}
	return nil
}

// releaseDoltPool checks the shared container's catalog (checkDoltPoolCatalog),
// then forgets the pool and removes the store directories it handed out. It
// runs while the container is still up, just before it is terminated, and its
// error fails the package: the catalog guard.
func releaseDoltPool() error {
	catalogErr := checkDoltPoolCatalog()
	doltPool.Lock()
	base := doltPool.base
	used, size := doltPool.next, len(doltPool.entries)
	sqlUsed, sqlSize := doltPool.sqlNext, len(doltPool.sql)
	doltPool.port, doltPool.base, doltPool.entries, doltPool.next, doltPool.names = 0, "", nil, 0, nil
	doltPool.sql, doltPool.sqlNext = nil, 0
	doltPool.Unlock()
	beads.SetTestDatabaseSource(nil)
	if size > 0 && used*4 >= size*3 {
		fmt.Fprintf(os.Stderr, "testutil: shared Dolt container used %d of its %d pooled databases; raise doltPoolPerRun before it runs out\n", used, size)
	}
	if sqlSize > 0 && sqlUsed*4 >= sqlSize*3 {
		fmt.Fprintf(os.Stderr, "testutil: shared Dolt container used %d of its %d pooled SQL databases; raise doltSQLPoolPerRun before it runs out\n", sqlUsed, sqlSize)
	}
	if base != "" {
		_ = os.RemoveAll(base)
	}
	return catalogErr
}
