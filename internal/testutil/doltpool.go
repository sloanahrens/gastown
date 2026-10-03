package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver for creating the pool

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testdb"
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
// plain SQL tests (TakePooledSQLDatabase) lease their database from the pool.
//
// The pool is a fixed size, whatever -count says. A lease is exclusive, and
// when it ends the database is reset to the commit it was handed out at and
// goes back to the pool (resetDoltDatabase): the branch is reset hard, and
// branches, tags, remotes and tables that commit does not hold are removed. No
// database is created or dropped for it. It used to hand every test a database
// of its own for the whole run, so the container held 128 databases per -count
// iteration: 722 databases and 9.9 GiB at -count=5 of internal/convoy, and
// every query on the single-core server got slower with the catalog.

// doltPoolSpares is how many spare databases the pool holds beyond the bd
// init templates. It bounds how many tests can hold one at once, not how many
// a run uses. Before leases were returned, one -count iteration used 35
// (daemon), 32 (convoy), 19 (refinery) and 12 (cmd), one after another or a
// handful at a time. Each database costs the container about 13 MiB whether
// or not it is used.
const doltPoolSpares = 32

// doltPoolSQLDatabases is how many plain SQL databases (TakePooledSQLDatabase)
// the pool holds. Daemon, their only user, took 7 per -count iteration.
const doltPoolSQLDatabases = 8

// doltSQLPoolPrefix names the plain SQL databases. Tests push them over real
// remotes, and pushDatabase refuses the "test" prefixes the store databases
// carry; "dolt_remotes_check_" is the prefix the orphan cleanups (the reaper's
// testPollutionPrefixes, jsonl_git_backup's discovery, gt dolt cleanup) already
// treat as test cruft, so a daemon test that lists the server's databases skips
// these as it skips the store databases.
const doltSQLPoolPrefix = testdb.RemotesCheckPrefix + "pool_"

// doltPoolDDLTimeout bounds each CREATE DATABASE of the pool, and each reset.
const doltPoolDDLTimeout = 2 * time.Minute

// doltPoolLeaseWait is how long a lease waits for a database to come back
// before it fails. The pool never grows: creating a database while tests run
// is the catalog change it exists to prevent.
const doltPoolLeaseWait = 2 * time.Minute

// doltPoolSessionWait is how long a release waits for its lessee's sessions
// on the database to end. A store's close and a bd subprocess's exit end
// theirs at once; the wait covers the server noticing a closed connection.
const doltPoolSessionWait = 10 * time.Second

// doltPoolReclaimPoll is how often a waiting lease looks again for a bd init
// lease whose owner directory is gone (see doltPoolEntry.ownerDir).
const doltPoolReclaimPoll = 100 * time.Millisecond

// doltLeaseKind is what a lease needs from its database.
type doltLeaseKind int

const (
	// leaseInit is an isolated bd init. It needs a database without an
	// identity, since bd init records a project identity and a prefix: a
	// template database (doltpool_template.go), or a spare one when every
	// template is leased.
	leaseInit doltLeaseKind = iota
	// leaseSQL is a plain SQL database (TakePooledSQLDatabase).
	leaseSQL
)

func (k doltLeaseKind) String() string {
	switch k {
	case leaseInit:
		return "bd init"
	default:
		return "SQL"
	}
}

// doltPoolEntry is one database of the pool.
type doltPoolEntry struct {
	name string

	initCommit string // the database's only commit when the pool created it
	head       string // the commit a release resets to

	leased bool
	used   bool   // leased at least once before
	owner  string // who holds the lease, for the exhaustion error
	// ownerDir is set for a bd init lease (beads.SetTestDatabaseSource), whose
	// caller has no testing.TB to end the lease with. The lease ends once the
	// directory, which is the test's (a t.TempDir), has existed and is gone:
	// the testing package removes a t.TempDir after the test's other cleanups.
	ownerDir     string
	ownerDirSeen bool
	broken       error // a reset failed; the entry is never leased again
}

// doltDBPool is a fixed set of databases on one Dolt server.
type doltDBPool struct {
	mu     sync.Mutex
	port   int
	base   string
	spares []*doltPoolEntry // bd init databases beyond the templates
	inits  []*doltPoolEntry // bd init databases cloned from the template (doltpool_template.go)
	sql    []*doltPoolEntry
	names  map[string]bool
	freed  chan struct{} // closed, and replaced, whenever a lease ends
	wait   time.Duration
	// sessionWait is how long a release waits for the lessee's sessions on
	// the database to end before it ends them itself.
	sessionWait time.Duration
	reset       func(e *doltPoolEntry, commit string) error
	db          *sql.DB

	leases, reuses, inUse, peak int
	reclaimErrs                 []error // failed resets of reclaimed bd init leases
}

// sharedDoltPool is the shared test container's pool; nil until it starts.
var sharedDoltPool struct {
	sync.Mutex
	p *doltDBPool
}

func currentDoltPool() *doltDBPool {
	sharedDoltPool.Lock()
	defer sharedDoltPool.Unlock()
	return sharedDoltPool.p
}

// newDoltDBPool lays out a pool of spares spare databases and sqlDBs plain SQL
// databases for the server at port, without touching the server.
func newDoltDBPool(port, spares, sqlDBs int) (*doltDBPool, error) {
	base, err := os.MkdirTemp("", "gt-doltpool-")
	if err != nil {
		return nil, fmt.Errorf("dolt test pool dir: %w", err)
	}
	p := &doltDBPool{
		port:  port,
		base:  base,
		names: make(map[string]bool, spares+sqlDBs),
		freed: make(chan struct{}),
		wait:  doltPoolLeaseWait,

		sessionWait: doltPoolSessionWait,
	}
	for i := range spares {
		p.spares = append(p.spares, &doltPoolEntry{name: fmt.Sprintf("%sspare_%04d", testdb.MintPrefix, i)})
	}
	for i := range sqlDBs {
		p.sql = append(p.sql, &doltPoolEntry{name: fmt.Sprintf("%s%04d", doltSQLPoolPrefix, i)})
	}
	for _, e := range p.all() {
		p.names[e.name] = true
	}
	return p, nil
}

func (p *doltDBPool) all() []*doltPoolEntry {
	return append(p.beadsEntries(), p.sql...)
}

// beadsEntries returns the entries a bd init can lease: the template
// databases and the spares behind them.
func (p *doltDBPool) beadsEntries() []*doltPoolEntry {
	return append(append([]*doltPoolEntry{}, p.inits...), p.spares...)
}

// create creates every database of the pool on its server and records each
// one's initial commit. It must run before any test can reach the server.
func (p *doltDBPool) create() error {
	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s", p.port))
	if err != nil {
		return fmt.Errorf("dolt test pool: %w", err)
	}
	p.db = db
	p.reset = func(e *doltPoolEntry, commit string) error {
		return resetDoltDatabase(db, e.name, commit, p.sessionWait)
	}
	entries := append(append([]*doltPoolEntry{}, p.spares...), p.sql...)
	for i, e := range entries {
		ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
		_, err := db.ExecContext(ctx, "CREATE DATABASE `"+e.name+"`")
		if err == nil {
			e.initCommit, err = doltHead(ctx, db, e.name)
			e.head = e.initCommit
		}
		cancel()
		if err != nil {
			return fmt.Errorf("dolt test pool: create database %d of %d (%s): %w", i+1, len(entries), e.name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
	defer cancel()
	if err := p.createInits(ctx); err != nil {
		return fmt.Errorf("dolt test pool: %w", err)
	}
	return nil
}

// createDoltPool creates the pool on the container at port. It runs inside the
// shared container's sync.Once, so no test can reach the container until it
// returns. It installs the pool as the source of isolated beads Inits.
func createDoltPool(port int) error {
	p, err := newDoltDBPool(port, doltPoolSpares, doltPoolSQLDatabases)
	if err != nil {
		return err
	}
	p.addInits(doltPoolInits)
	// The guard at teardown allows the image's own databases and the pool's,
	// nothing else; a fresh container holding anything more means the image
	// changed under doltImageDatabases, and the guard would misjudge it.
	if err := verifyDoltCatalogAt(port, nil); err != nil {
		return fmt.Errorf("dolt test pool: the fresh container is not what doltImageDatabases describes: %w", err)
	}
	if err := p.create(); err != nil {
		p.close()
		return err
	}
	sharedDoltPool.Lock()
	sharedDoltPool.p = p
	sharedDoltPool.Unlock()
	beads.SetTestDatabaseSource(p.initSource)
	return nil
}

// initSource is the pool's beads.SetTestDatabaseSource: it leases a bd init
// in ownerDir on port a database, until ownerDir is gone.
func (p *doltDBPool) initSource(port int, ownerDir string) (string, error) {
	if port != p.port {
		return "", nil
	}
	if !filepath.IsAbs(ownerDir) {
		// The lease ends when ownerDir is gone; an empty or relative one
		// could never be checked, and the database would be lost.
		return "", fmt.Errorf("dolt test pool: a bd init lease needs the absolute directory the init runs in, got %q", ownerDir)
	}
	e, err := p.acquire(leaseInit, "bd init in "+ownerDir, ownerDir)
	if err != nil {
		return "", err
	}
	return e.name, nil
}

// close forgets the pool's connection and store directories.
func (p *doltDBPool) close() {
	if p.db != nil {
		_ = p.db.Close()
	}
	if p.base != "" {
		_ = os.RemoveAll(p.base)
	}
}

// acquire leases a database of kind to owner, waiting up to p.wait for one to
// come back when none is free. ownerDir, when set, ends the lease once that
// directory has existed and is gone. It never creates a database.
func (p *doltDBPool) acquire(kind doltLeaseKind, owner, ownerDir string) (*doltPoolEntry, error) {
	deadline := time.Now().Add(p.wait)
	for {
		p.mu.Lock()
		stale := p.staleLocked()
		var e *doltPoolEntry
		if len(stale) == 0 {
			e = p.pickLocked(kind)
		}
		if e != nil {
			e.leased, e.owner, e.ownerDir, e.ownerDirSeen = true, owner, ownerDir, false
			if ownerDir != "" {
				if info, err := os.Stat(ownerDir); err == nil && info.IsDir() {
					e.ownerDirSeen = true
				}
			}
			p.leases++
			if e.used {
				p.reuses++
			}
			e.used = true
			p.inUse++
			p.peak = max(p.peak, p.inUse)
		}
		freed := p.freed
		p.mu.Unlock()

		if e != nil {
			return e, nil
		}
		if len(stale) > 0 {
			for _, s := range stale {
				if err := p.release(s); err != nil {
					p.mu.Lock()
					p.reclaimErrs = append(p.reclaimErrs, err)
					p.mu.Unlock()
				}
			}
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, p.exhausted(kind)
		}
		timer := time.NewTimer(min(remaining, doltPoolReclaimPoll))
		select {
		case <-freed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// staleLocked returns the bd init leases whose owner directory is gone,
// still marked leased so no other caller takes them before they are reset.
func (p *doltDBPool) staleLocked() []*doltPoolEntry {
	var stale []*doltPoolEntry
	for _, e := range p.beadsEntries() {
		if !e.leased || e.ownerDir == "" || e.broken != nil {
			continue
		}
		_, err := os.Stat(e.ownerDir)
		switch {
		case err == nil:
			e.ownerDirSeen = true
		case os.IsNotExist(err) && e.ownerDirSeen:
			e.ownerDir = "" // claimed by this sweep
			stale = append(stale, e)
		}
	}
	return stale
}

// pickLocked returns a free entry for kind, or nil.
func (p *doltDBPool) pickLocked(kind doltLeaseKind) *doltPoolEntry {
	if kind == leaseSQL {
		return firstFree(p.sql, func(*doltPoolEntry) bool { return true })
	}
	if kind == leaseInit {
		if e := firstFree(p.inits, func(*doltPoolEntry) bool { return true }); e != nil {
			return e
		}
	}
	return firstFree(p.spares, func(*doltPoolEntry) bool { return true })
}

func firstFree(entries []*doltPoolEntry, ok func(*doltPoolEntry) bool) *doltPoolEntry {
	for _, e := range entries {
		if !e.leased && e.broken == nil && ok(e) {
			return e
		}
	}
	return nil
}

// recordHead makes e's current head the commit a release returns it to.
func (p *doltDBPool) recordHead(e *doltPoolEntry) error {
	ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
	defer cancel()
	head, err := doltHead(ctx, p.db, e.name)
	if err != nil {
		return err
	}
	p.mu.Lock()
	e.head = head
	p.mu.Unlock()
	return nil
}

// release resets e to its head and ends its lease. An entry that cannot be
// reset is never leased again, and the error says why.
func (p *doltDBPool) release(e *doltPoolEntry) error {
	err := p.reset(e, e.head)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("dolt test pool: returning %s (leased by %s) to the pool: %w", e.name, e.owner, err)
		e.broken = err
	} else {
		e.leased, e.owner, e.ownerDir, e.ownerDirSeen = false, "", "", false
	}
	p.inUse--
	close(p.freed)
	p.freed = make(chan struct{})
	return err
}

// exhausted is the error for a lease that found no database in time.
func (p *doltDBPool) exhausted(kind doltLeaseKind) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries, knob := p.spares, "doltPoolSpares"
	if kind == leaseInit {
		entries, knob = p.beadsEntries(), "doltPoolInits or doltPoolSpares"
	}
	if kind == leaseSQL {
		entries, knob = p.sql, "doltPoolSQLDatabases"
	}
	var holders []string
	for _, e := range entries {
		switch {
		case e.broken != nil:
			holders = append(holders, e.name+": unusable, "+e.broken.Error())
		case e.leased:
			h := e.name + ": " + e.owner
			if e.ownerDir != "" && !e.ownerDirSeen {
				h += " (its directory never existed, so the lease cannot end)"
			}
			holders = append(holders, h)
		}
	}
	return fmt.Errorf("the test Dolt pool has no free database for a %s lease after %s: all %d are leased — "+
		"a test that holds more at once than %s allows, or leases that never end:\n  %s\n"+
		"Raise %s in internal/testutil/doltpool.go if the tests need more at once; creating a database while "+
		"tests run would reopen the catalog race the pool prevents",
		kind, p.wait, len(entries), knob, strings.Join(holders, "\n  "), knob)
}

// leaseForTest leases a database of kind for t, returned to the pool when t
// ends — failed or not. A reset that fails fails t.
func (p *doltDBPool) leaseForTest(t testing.TB, kind doltLeaseKind) *doltPoolEntry {
	t.Helper()
	e, err := p.acquire(kind, t.Name(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.release(e); err != nil {
			t.Errorf("%v", err)
		}
	})
	return e
}

// TakePooledSQLDatabase leases t an empty database on the shared test Dolt
// container, for a test that works with a database directly over SQL rather
// than through a beads store. The lease ends with t: the database is reset to
// its initial commit (resetDoltDatabase) and goes back to the pool. The test
// must not drop it: a DROP while other tests run is the catalog change the
// pool prevents. It must close every connection it opened on it before it
// ends, or the release kills them and fails t. Tables it creates are dropped
// at release, so their AUTO_INCREMENT counters go with them; a reset cannot
// restore the counter of a table it committed as a reset point
// (checkDoltDatabaseAt). Its name starts with doltSQLPoolPrefix.
func TakePooledSQLDatabase(t testing.TB) string {
	t.Helper()
	p := currentDoltPool()
	if p == nil {
		t.Fatal("TakePooledSQLDatabase: the shared test Dolt container has no pool; start it first (RequireDoltContainer or WithDolt)")
	}
	return p.leaseForTest(t, leaseSQL).name
}

// IsDoltPoolDatabase reports whether name is one of the databases the shared
// test container's pool created before tests ran.
func IsDoltPoolDatabase(name string) bool {
	p := currentDoltPool()
	return p != nil && p.names[name]
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
		"(beads.NewIsolatedWithPort + Init), and nothing may be dropped until the container goes: "+
		"a CREATE or DROP DATABASE while other tests run breaks their migrations (internal/testutil/doltpool.go)",
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

// verifyDoltCatalogAt runs verifyDoltCatalog against the server at port.
func verifyDoltCatalogAt(port int, pool map[string]bool) error {
	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s", port))
	if err != nil {
		return err
	}
	defer db.Close()
	return verifyDoltCatalog(db, pool)
}

// checkDoltPoolCatalog runs verifyDoltCatalog against the server p lives on.
func checkDoltPoolCatalog(p *doltDBPool) error {
	if err := verifyDoltCatalogAt(p.port, p.names); err != nil {
		if errors.Is(err, ErrDoltCatalogChanged) {
			return err
		}
		return fmt.Errorf("%w: cannot check it: %v", ErrDoltCatalogChanged, err)
	}
	return nil
}

// releaseDoltPool checks the shared container's catalog (checkDoltPoolCatalog),
// reports the pool's use, then forgets the pool. It runs while the container
// is still up, just before it is terminated, and its error fails the package:
// the catalog guard, and any reclaimed bd init lease that could not be reset.
func releaseDoltPool() error {
	sharedDoltPool.Lock()
	p := sharedDoltPool.p
	sharedDoltPool.p = nil
	sharedDoltPool.Unlock()
	if p == nil {
		return nil
	}
	beads.SetTestDatabaseSource(nil)
	catalogErr := checkDoltPoolCatalog(p)
	p.mu.Lock()
	var stuck []string
	for _, e := range p.beadsEntries() {
		if e.leased && e.ownerDir != "" {
			if _, err := os.Stat(e.ownerDir); err == nil {
				stuck = append(stuck, e.owner)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "testutil: Dolt test pool: %d leases (%d of a returned database), at most %d at once, of %d bd init, %d spare and %d SQL databases\n",
		p.leases, p.reuses, p.peak, len(p.inits), len(p.spares), len(p.sql))
	if len(stuck) > 0 {
		sort.Strings(stuck)
		fmt.Fprintf(os.Stderr, "testutil: Dolt test pool: %d bd init leases never ended, because their directory outlived the test: %s\n",
			len(stuck), strings.Join(stuck, ", "))
	}
	if total := len(p.inits) + len(p.spares) + len(p.sql); p.peak*4 >= total*3 {
		fmt.Fprintf(os.Stderr, "testutil: Dolt test pool: tests held %d of its %d databases at once; raise doltPoolSpares before leases start waiting\n", p.peak, total)
	}
	errs := append([]error{catalogErr}, p.reclaimErrs...)
	p.mu.Unlock()
	p.close()
	return errors.Join(errs...)
}
