package testutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResets stands in for resetDoltDatabase in the pool's unit tests.
type fakeResets struct {
	mu    sync.Mutex
	calls []string // "<database>@<commit>"
	fail  map[string]error
}

func (r *fakeResets) reset(e *doltPoolEntry, commit string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, e.name+"@"+commit)
	return r.fail[e.name]
}

func (r *fakeResets) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// newFakeDoltPool is a pool whose databases were never created: each entry's
// initial commit is "init<i>", resets are recorded, and a lease that finds
// nothing free fails at once.
func newFakeDoltPool(t *testing.T, stores, sqlDBs int) (*doltDBPool, *fakeResets) {
	t.Helper()
	p, err := newDoltDBPool(0, stores, sqlDBs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	for i, e := range p.all() {
		e.initCommit = fmt.Sprintf("init%d", i)
		e.head = e.initCommit
	}
	r := &fakeResets{fail: map[string]error{}}
	p.reset = r.reset
	p.wait = 0
	return p, r
}

func TestDoltPoolReturnsALeaseResetWhenItsTestEnds(t *testing.T) {
	p, resets := newFakeDoltPool(t, 1, 0)
	var first, second string
	t.Run("first", func(t *testing.T) { first = p.leaseForTest(t, leaseStore).name })
	if got, want := resets.all(), []string{first + "@init0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resets after the first test = %q, want %q", got, want)
	}
	t.Run("second", func(t *testing.T) { second = p.leaseForTest(t, leaseStore).name })
	if second != first {
		t.Fatalf("second lease = %s, want the one database %s back", second, first)
	}
	if p.leases != 2 || p.reuses != 1 || p.peak != 1 || p.inUse != 0 {
		t.Errorf("leases %d reuses %d peak %d inUse %d, want 2 1 1 0", p.leases, p.reuses, p.peak, p.inUse)
	}
}

// cleanupTB records the cleanups a lease registers, so a test can run them
// the way the testing package does when a test ends — failed or not.
type cleanupTB struct {
	testing.TB
	cleanups []func()
	failed   bool
}

func (c *cleanupTB) Cleanup(f func()) { c.cleanups = append(c.cleanups, f) }
func (c *cleanupTB) Name() string     { return "TestThatFails" }
func (c *cleanupTB) Errorf(string, ...any) {
	c.failed = true
}

func TestDoltPoolReturnsTheLeaseOfAFailedTest(t *testing.T) {
	p, resets := newFakeDoltPool(t, 1, 0)
	failing := &cleanupTB{TB: t}
	e := p.leaseForTest(failing, leaseStore)
	if len(failing.cleanups) != 1 {
		t.Fatalf("lease registered %d cleanups, want 1: the release must be a t.Cleanup, which runs for a failed test too", len(failing.cleanups))
	}
	failing.failed = true // the test fails; the testing package still runs its cleanups
	for i := len(failing.cleanups) - 1; i >= 0; i-- {
		failing.cleanups[i]()
	}
	if got := resets.all(); len(got) != 1 || got[0] != e.name+"@init0" {
		t.Fatalf("resets = %q, want the failed test's lease reset", got)
	}
	if _, err := p.acquire(leaseStore, "next", ""); err != nil {
		t.Fatalf("lease after a failed test: %v", err)
	}
}

func TestDoltPoolExhaustedFailsWithoutCreatingADatabase(t *testing.T) {
	p, resets := newFakeDoltPool(t, 1, 1)
	if _, err := p.acquire(leaseStore, "TestHolder", ""); err != nil {
		t.Fatal(err)
	}
	_, err := p.acquire(leaseStore, "TestWaiter", "")
	if err == nil {
		t.Fatal("second lease from a one-database pool succeeded; an exhausted pool must fail, never create a database mid-run")
	}
	for _, want := range []string{"no free database for a store lease", "TestHolder", "doltPoolStores"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exhaustion error %q does not say %q", err, want)
		}
	}
	if len(p.stores) != 1 || len(resets.all()) != 0 {
		t.Errorf("exhaustion changed the pool: %d stores, resets %q", len(p.stores), resets.all())
	}
	// The SQL databases are a pool of their own.
	if _, err := p.acquire(leaseSQL, "TestSQL", ""); err != nil {
		t.Errorf("SQL lease while every store database is leased: %v", err)
	}
}

func TestDoltPoolLeaseWaitsForARelease(t *testing.T) {
	p, _ := newFakeDoltPool(t, 1, 0)
	p.wait = time.Hour
	held, err := p.acquire(leaseStore, "TestHolder", "")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *doltPoolEntry, 1)
	go func() {
		e, err := p.acquire(leaseStore, "TestWaiter", "")
		if err != nil {
			t.Errorf("waiting lease: %v", err)
		}
		got <- e
	}()
	// The pool holds one database, so the waiter cannot have it until this
	// release.
	if err := p.release(held); err != nil {
		t.Fatal(err)
	}
	if e := <-got; e != held {
		t.Fatalf("waiter got %v, want the released database", e)
	}
}

func TestDoltPoolReclaimsABdInitLeaseOnceItsDirIsGone(t *testing.T) {
	p, resets := newFakeDoltPool(t, 1, 0)
	owner := filepath.Join(t.TempDir(), "rig")
	if err := os.Mkdir(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := p.acquire(leaseInit, "bd init in "+owner, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.acquire(leaseInit, "other", ""); err == nil {
		t.Fatal("a bd init lease whose directory still exists was taken back")
	}
	if err := os.RemoveAll(owner); err != nil {
		t.Fatal(err)
	}
	again, err := p.acquire(leaseInit, "other", "")
	if err != nil {
		t.Fatalf("lease after the owner's directory went: %v", err)
	}
	if again != e {
		t.Fatalf("got %s, want the reclaimed %s", again.name, e.name)
	}
	if got, want := resets.all(), []string{e.name + "@init0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("resets = %q, want %q", got, want)
	}
}

func TestDoltPoolKeepsABdInitLeaseWhoseDirNeverExisted(t *testing.T) {
	p, _ := newFakeDoltPool(t, 1, 0)
	if _, err := p.acquire(leaseInit, "bd init in nowhere", filepath.Join(t.TempDir(), "never")); err != nil {
		t.Fatal(err)
	}
	_, err := p.acquire(leaseInit, "other", "")
	if err == nil || !strings.Contains(err.Error(), "never existed") {
		t.Fatalf("lease = %v, want an exhaustion error naming the lease that cannot end", err)
	}
}

func TestDoltPoolStoreAndInitLeasesPickTheirState(t *testing.T) {
	p, resets := newFakeDoltPool(t, 2, 0)
	migrated := p.stores[1]
	migrated.head, migrated.migrated = "mig1", true

	store, err := p.acquire(leaseStore, "store", "")
	if err != nil {
		t.Fatal(err)
	}
	if store != migrated {
		t.Errorf("store lease got %s, want the migrated %s", store.name, migrated.name)
	}
	if err := p.release(store); err != nil {
		t.Fatal(err)
	}
	if got := resets.all(); len(got) != 1 || got[0] != migrated.name+"@mig1" {
		t.Errorf("release of a migrated store reset to %q, want its migration commit", got)
	}

	fresh, err := p.acquire(leaseInit, "init 1", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh != p.stores[0] {
		t.Errorf("bd init lease got %s, want the unmigrated %s", fresh.name, p.stores[0].name)
	}
	// Only the migrated database is left: a bd init gets it reset to its
	// initial commit, since bd init needs an empty database.
	last, err := p.acquire(leaseInit, "init 2", "")
	if err != nil {
		t.Fatal(err)
	}
	if last != migrated || last.migrated || last.head != "init1" {
		t.Errorf("bd init lease got %s (migrated %v, head %s), want %s reset to init1", last.name, last.migrated, last.head, migrated.name)
	}
	if got := resets.all(); got[len(got)-1] != migrated.name+"@init1" {
		t.Errorf("resets = %q, want the last one to take %s back to init1", got, migrated.name)
	}
}

func TestDoltPoolNeverLendsADatabaseItCouldNotReset(t *testing.T) {
	p, resets := newFakeDoltPool(t, 1, 0)
	e, err := p.acquire(leaseStore, "TestDirtier", "")
	if err != nil {
		t.Fatal(err)
	}
	resets.fail[e.name] = errors.New("reset left uncommitted changes")
	if err := p.release(e); err == nil || !strings.Contains(err.Error(), "TestDirtier") {
		t.Fatalf("release = %v, want the reset failure naming the lessee", err)
	}
	if _, err := p.acquire(leaseStore, "next", ""); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("lease after a failed reset = %v, want exhaustion naming the unusable database", err)
	}
}

func TestCatalogViolations(t *testing.T) {
	pool := map[string]bool{"testdb_a": true, "testdb_b": true}
	image := []string{"gt_test", "information_schema", "mysql"}
	with := func(extra ...string) []string { return append(append([]string{}, image...), extra...) }
	cases := []struct {
		name    string
		present []string
		dropped []string
		want    []string // fragments the error must carry; nil = no error
	}{
		{name: "clean", present: with("testdb_a", "testdb_b")},
		{name: "stray", present: with("testdb_a", "testdb_b", "beads"),
			want: []string{`database "beads" was created`}},
		{name: "pool database dropped", present: with("testdb_a"),
			want: []string{`database "testdb_b" was dropped`}},
		{name: "image database dropped", present: []string{"information_schema", "mysql", "testdb_a", "testdb_b"},
			want: []string{`database "gt_test" was dropped`}},
		{name: "created then dropped", present: with("testdb_a", "testdb_b"), dropped: []string{"hq"},
			want: []string{`database "hq" was dropped (Dolt still holds it for dolt_undrop)`}},
		{name: "dropped and recreated", present: with("testdb_a", "testdb_b"), dropped: []string{"testdb_a"},
			want: []string{`database "testdb_a" was dropped and created again`}},
		{name: "several", present: with("testdb_a", "x", "y"),
			want: []string{`"x" was created`, `"y" was created`, `"testdb_b" was dropped`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := catalogViolations(c.present, c.dropped, pool)
			if c.want == nil {
				if err != nil {
					t.Fatalf("catalogViolations = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrDoltCatalogChanged) {
				t.Fatalf("catalogViolations = %v, want an error wrapping ErrDoltCatalogChanged", err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// The refusals are dolt 2.0.7's own words, captured from DoltDockerImage;
// TestDoltCatalogGuardFiresOnRealServer checks them against the real server.
func TestParseUndropRefusal(t *testing.T) {
	cases := []struct {
		msg     string
		want    []string
		wantErr bool
	}{
		{msg: "Error 1105 (HY000): no database name specified. there are no databases currently available to be undropped"},
		{msg: "Error 1105 (HY000): no database name specified. available databases that can be undropped: stray2",
			want: []string{"stray2"}},
		{msg: "Error 1105 (HY000): no database name specified. available databases that can be undropped: gt_x-y, stray2",
			want: []string{"gt_x-y", "stray2"}},
		{msg: "Error 1045: access denied", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseUndropRefusal(c.msg)
		if (err != nil) != c.wantErr {
			t.Errorf("parseUndropRefusal(%q) err = %v, wantErr %v", c.msg, err, c.wantErr)
			continue
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("parseUndropRefusal(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}
