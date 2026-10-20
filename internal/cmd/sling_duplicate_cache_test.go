package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// cacheTick is what one tick of the daemon's pool fetch reports: the pool, the
// counts it would report, and what warn heard.
type cacheTick struct {
	pool                    []duplicateCandidate
	fetched, cached, unread int
	warnings                []string
}

// tickOver is one tick of the daemon's pool fetch for the rig "rig".
func tickOver(t *testing.T, store beads.Client, path, owner string, limit int) cacheTick {
	t.Helper()
	return tickOverRig(t, store, path, owner, "rig", limit)
}

// tickOverRig is one tick of the daemon's pool fetch for the rig at beadsDir: a
// fresh cache loaded from path, as a new `gt spec dispatch` process loads it,
// read through store.
func tickOverRig(t *testing.T, store beads.Client, path, owner, beadsDir string, limit int) cacheTick {
	t.Helper()
	cache := loadDuplicateDetailCache(path, owner, limit)
	var out cacheTick
	pool, err := fetchDuplicatePoolWith(store, func(_ string, err error) {
		out.warnings = append(out.warnings, err.Error())
	}, beadsDir, cache)
	if err != nil {
		t.Fatalf("fetchDuplicatePoolWith: %v", err)
	}
	out.pool = pool
	out.fetched, out.cached, out.unread = cache.Counts()
	return out
}

func shownIDs(s *countingStore) []string {
	var ids []string
	for _, batch := range s.multis {
		ids = append(ids, batch...)
	}
	return ids
}

// A second tick over the same pool with no edits makes no bd show call for
// design or notes, and reads the same pool the first tick did.
func TestDuplicateDetailCacheSecondTickShowsNothing(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")

	first := newCountingStore(listLikeBD{Client: db})
	one := tickOver(t, first, path, "daemon-1", 40)
	if got := len(shownIDs(first)); got != 8 {
		t.Fatalf("the cold tick showed %d beads, want the pool's 8", got)
	}
	if one.fetched != 8 || one.cached != 0 {
		t.Errorf("cold tick counts = %d fetched, %d cached, want 8 and 0", one.fetched, one.cached)
	}

	second := newCountingStore(listLikeBD{Client: db})
	two := tickOver(t, second, path, "daemon-1", 40)
	if len(second.multis) != 0 || len(second.shows) != 0 {
		t.Fatalf("the warm tick asked bd to show %v and %v, want nothing", second.multis, second.shows)
	}
	if two.fetched != 0 || two.cached != 8 {
		t.Errorf("warm tick counts = %d fetched, %d cached, want 0 and 8", two.fetched, two.cached)
	}
	if fmt.Sprintf("%+v", one.pool) != fmt.Sprintf("%+v", two.pool) {
		t.Errorf("the warm pool differs from the cold one:\n%+v\n%+v", two.pool, one.pool)
	}
	if len(two.warnings) != 0 {
		t.Errorf("a warm tick warned: %v", two.warnings)
	}
}

// A bead whose updated_at moved is shown again, with its new text, and a bead
// whose updated_at did not is not.
func TestDuplicateDetailCacheRereadsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 40)

	db.Seed(beads.Issue{ID: "gt-p1", Type: "task", Title: "Alpha flake", Status: "open", Priority: 1,
		Description: "Unrelated prose.", Design: "Root cause is now TestOmega.",
		CreatedAt: "2026-10-15T09:00:00Z", UpdatedAt: "2026-10-19T09:00:00Z"})

	store := newCountingStore(listLikeBD{Client: db})
	tick := tickOver(t, store, path, "daemon-1", 40)
	if got := shownIDs(store); len(got) != 1 || got[0] != "gt-p1" {
		t.Fatalf("shown = %v, want only the edited gt-p1", got)
	}
	if tick.fetched != 1 || tick.cached != 7 {
		t.Errorf("counts = %d fetched, %d cached, want 1 and 7", tick.fetched, tick.cached)
	}
	for _, c := range tick.pool {
		if c.ID != "gt-p1" {
			continue
		}
		if len(c.Refs.Tests) != 1 || c.Refs.Tests[0] != "TestOmega" {
			t.Errorf("gt-p1 refs = %v, want the edited design's TestOmega", c.Refs.Tests)
		}
	}
}

// One tick never shows more than its limit: the rest are compared on list text
// and the warning names how many, and the following ticks read them.
func TestDuplicateDetailCacheBoundsMissesPerTick(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	for i := 0; i < 8; i++ {
		db.Seed(beads.Issue{ID: fmt.Sprintf("gt-b%d", i), Type: "task", Title: "bead", Status: "open",
			Design: fmt.Sprintf("TestShared%d fails.", i), UpdatedAt: "2026-10-18T09:00:00Z"})
	}
	path := filepath.Join(t.TempDir(), "cache.json")

	var shown int
	for tickNo, want := range []struct{ fetched, cached, unread int }{{3, 0, 5}, {3, 3, 2}, {2, 6, 0}, {0, 8, 0}} {
		store := newCountingStore(listLikeBD{Client: db})
		tick := tickOver(t, store, path, "daemon-1", 3)
		shown += len(shownIDs(store))
		if tick.fetched != want.fetched || tick.cached != want.cached || tick.unread != want.unread {
			t.Errorf("tick %d counts = %d fetched, %d cached, %d unread; want %d, %d, %d",
				tickNo+1, tick.fetched, tick.cached, tick.unread, want.fetched, want.cached, want.unread)
		}
		if len(shownIDs(store)) > 3 {
			t.Errorf("tick %d showed %d beads, over the limit of 3", tickNo+1, len(shownIDs(store)))
		}
		if want.unread == 0 {
			if len(tick.warnings) != 0 {
				t.Errorf("tick %d warned though it read everything: %v", tickNo+1, tick.warnings)
			}
			continue
		}
		wantWarn := fmt.Sprintf("design/notes of %d of 8 bead(s) not read this tick", want.unread)
		if len(tick.warnings) != 1 || !strings.Contains(tick.warnings[0], wantWarn) {
			t.Errorf("tick %d warnings = %v, want one naming %q", tickNo+1, tick.warnings, wantWarn)
		}
		unread := 0
		for _, c := range tick.pool {
			if c.Refs.Empty() {
				unread++
			}
		}
		if unread != want.unread {
			t.Errorf("tick %d: %d beads compared on list text alone, want %d", tickNo+1, unread, want.unread)
		}
	}
	if shown != 8 {
		t.Errorf("the four ticks showed %d beads in all, want each of the 8 once", shown)
	}
}

// The daemon's cache is the daemon's: a file another owner wrote, or one that
// does not parse, holds nothing.
func TestDuplicateDetailCacheDroppedOnRestart(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 40)

	restarted := newCountingStore(listLikeBD{Client: db})
	tick := tickOver(t, restarted, path, "daemon-2", 40)
	if len(shownIDs(restarted)) != 8 || tick.cached != 0 {
		t.Errorf("a restarted daemon showed %d beads and answered %d from cache, want 8 and 0", len(shownIDs(restarted)), tick.cached)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := newCountingStore(listLikeBD{Client: db})
	tickOver(t, broken, path, "daemon-2", 40)
	if len(shownIDs(broken)) != 8 {
		t.Errorf("an unreadable cache file left %d beads shown, want all 8", len(shownIDs(broken)))
	}
}

// A blocker closing does not touch the bead it blocks, so the cached edge is
// refreshed from the pool: the bead is no longer held, with no new show.
func TestDuplicateDetailCacheRefreshesBlockerStatus(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	openOf := func(pool []duplicateCandidate, id string) []string {
		for _, c := range pool {
			if c.ID == id {
				return c.Ready.OpenBlockers
			}
		}
		t.Fatalf("%s is not in the pool", id)
		return nil
	}
	if got := openOf(tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 40).pool, "gt-p3"); len(got) != 1 || got[0] != "gt-b1" {
		t.Fatalf("gt-p3 open blockers = %v, want gt-b1", got)
	}

	// The fake stamps a close with its own clock, outside the pool's lookback,
	// so the closed blocker is seeded as bd would list it just after a close.
	closedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	db.Seed(beads.Issue{ID: "gt-b1", Type: "task", Title: "open blocker", Status: "closed", ClosedAt: closedAt,
		CloseReason: "done", Description: "Nothing shared.", CreatedAt: "2026-10-15T09:04:00Z", UpdatedAt: closedAt})
	store := newCountingStore(listLikeBD{Client: db})
	tick := tickOver(t, store, path, "daemon-1", 40)
	if got := openOf(tick.pool, "gt-p3"); len(got) != 0 {
		t.Errorf("gt-p3 open blockers = %v after gt-b1 closed, want none", got)
	}
	if got := shownIDs(store); len(got) > 1 {
		t.Errorf("shown = %v, want at most the closed blocker itself", got)
	}
}

// The overlap guard decides and prints what it did before the cache, cold and
// warm: the golden was written before the change.
func TestDuplicateGuardDecisionsMatchGoldenThroughTheCache(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	want := readDuplicateGuardGolden(t)
	for _, tick := range []string{"cold", "warm"} {
		cache := loadDuplicateDetailCache(path, "daemon-1", 40)
		pools := newDuplicatePools(func(beadsDir string) ([]duplicateCandidate, error) {
			return fetchDuplicatePoolWith(listLikeBD{Client: db}, func(dir string, err error) {
				t.Errorf("%s tick: enrichment of %s failed: %v", tick, dir, err)
			}, beadsDir, cache)
		}, duplicateGuardHolds())
		if got := duplicateGuardTranscript(t, pools); got != want {
			t.Fatalf("%s tick: decisions differ from testdata/%s:\n--- got\n%s--- want\n%s", tick, duplicateGuardGoldenPath, got, want)
		}
	}
}

// A tick no daemon started keeps no cache and reports no counts.
func TestSpecDetailCacheForNeedsAnOwner(t *testing.T) {
	t.Parallel()
	if specDetailCacheFor(t.TempDir(), "") != nil {
		t.Error("a tick run by hand got a cache")
	}
	cache := specDetailCacheFor(t.TempDir(), "daemon-1")
	if cache == nil || cache.limit != duplicateDetailMissLimit {
		t.Fatalf("a daemon's tick cache = %+v, want one limited to %d", cache, duplicateDetailMissLimit)
	}
	var none *duplicateDetailCache
	if f, c, u := none.Counts(); f+c+u != 0 {
		t.Errorf("a nil cache counted %d, %d, %d", f, c, u)
	}
}

// The daemon's ticks alternate between rigs over one cache file. One rig's pool
// fetch prunes only its own entries, so the other rig's beads stay cached.
func TestDuplicateDetailCacheKeepsEachRigsEntries(t *testing.T) {
	t.Parallel()
	alpha := duplicateGuardFixture(t)
	beta := beadsfake.New()
	beta.Seed(
		beads.Issue{ID: "be-1", Type: "task", Title: "Beta flake", Status: "open", Design: "TestBeta fails.", UpdatedAt: "2026-10-18T09:00:00Z"},
		beads.Issue{ID: "be-2", Type: "task", Title: "Beta rework", Status: "open", Notes: "Touches internal/beta.go.", UpdatedAt: "2026-10-18T09:01:00Z"},
	)
	path := filepath.Join(t.TempDir(), "cache.json")

	steps := []struct {
		beadsDir string
		db       beads.Client
		pool     int
		shown    int
	}{
		{"alpha", alpha, 8, 8},
		{"beta", beta, 2, 2},
		{"alpha", alpha, 8, 0},
		{"beta", beta, 2, 0},
	}
	for i, s := range steps {
		store := newCountingStore(s.db)
		tick := tickOverRig(t, store, path, "daemon-1", s.beadsDir, 40)
		if len(tick.pool) != s.pool || len(shownIDs(store)) != s.shown {
			t.Errorf("tick %d (%s): pool %d, shown %d; want pool %d, shown %d",
				i+1, s.beadsDir, len(tick.pool), len(shownIDs(store)), s.pool, s.shown)
		}
	}
}

// A blocker closed before the lookback leaves the pool, and with it the hold on
// its dependent: the cached edge reads as resolved, with no show.
func TestDuplicateDetailCacheBlockerLeavingThePoolUnblocks(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 40)

	old := time.Now().Add(-100 * time.Hour).UTC().Format(time.RFC3339)
	db.Seed(beads.Issue{ID: "gt-b1", Type: "task", Title: "open blocker", Status: "closed", ClosedAt: old,
		CloseReason: "done", Description: "Nothing shared.", CreatedAt: "2026-10-15T09:04:00Z", UpdatedAt: old})
	store := newCountingStore(listLikeBD{Client: db})
	tick := tickOver(t, store, path, "daemon-1", 40)
	if got := openBlockersOf(t, tick.pool, "gt-p3"); len(got) != 0 {
		t.Errorf("gt-p3 open blockers = %v after gt-b1 left the pool, want none", got)
	}
	if got := shownIDs(store); len(got) != 0 {
		t.Errorf("shown = %v, want none", got)
	}
}

// A show that fails leaves the pool compared on list text and caches nothing,
// so the next tick shows every bead again.
func TestDuplicateDetailCacheShowFailureCachesNothing(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")

	failing := tickOver(t, listLikeBD{Client: db, showErr: errors.New("bd show: boom")}, path, "daemon-1", 40)
	if len(failing.pool) != 8 {
		t.Errorf("the pool under a failed show has %d beads, want all 8 on list text", len(failing.pool))
	}
	// The active and the closed list each fail their show; the two are one warning.
	if len(failing.warnings) != 1 || !strings.Contains(failing.warnings[0], "fetching design/notes for 6 bead(s)") ||
		!strings.Contains(failing.warnings[0], "fetching design/notes for 2 bead(s)") {
		t.Errorf("warnings = %v, want one naming the failed shows of 6 and 2 beads", failing.warnings)
	}

	store := newCountingStore(listLikeBD{Client: db})
	healed := tickOver(t, store, path, "daemon-1", 40)
	if got := len(shownIDs(store)); got != 8 {
		t.Errorf("the tick after a failed show showed %d beads, want all 8", got)
	}
	if len(healed.warnings) != 0 {
		t.Errorf("the tick after a failed show warned: %v", healed.warnings)
	}
}

// A tick that runs out of its limit across both lists warns once, counting the
// beads left unread in both.
func TestDuplicateDetailCacheWarnsOnceAcrossLists(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")

	tick := tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 3)
	if len(tick.warnings) != 1 || !strings.Contains(tick.warnings[0], "design/notes of 5 of 8 bead(s) not read this tick") {
		t.Errorf("warnings = %v, want one naming 5 of 8 beads unread", tick.warnings)
	}
	if tick.fetched != 3 || tick.unread != 5 {
		t.Errorf("counts = %d fetched, %d unread, want 3 and 5", tick.fetched, tick.unread)
	}
}

// A limit of zero reads every bead a tick sees: there is no miss budget.
func TestDuplicateDetailCacheZeroLimitIsUnlimited(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")

	tick := tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 0)
	if tick.fetched != 8 || tick.unread != 0 || len(tick.warnings) != 0 {
		t.Errorf("counts = %d fetched, %d unread, %d warnings; want 8, 0 and none", tick.fetched, tick.unread, len(tick.warnings))
	}
}

// A row with no updated_at has nothing to key the cache by, so it is shown on
// every tick.
func TestDuplicateDetailCacheRowWithoutUpdatedAtIsShownEveryTick(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-n1", Type: "task", Title: "No stamp", Status: "open", Design: "TestNoStamp fails."})
	path := filepath.Join(t.TempDir(), "cache.json")
	for tickNo := 1; tickNo <= 2; tickNo++ {
		store := newCountingStore(listLikeBD{Client: db})
		tickOver(t, store, path, "daemon-1", 40)
		if got := shownIDs(store); len(got) != 1 || got[0] != "gt-n1" {
			t.Errorf("tick %d shown = %v, want gt-n1", tickNo, got)
		}
	}
}

// A bead that leaves the pool loses its entry, so a bead that comes back at the
// same updated_at is shown again rather than answered from a stale entry.
func TestDuplicateDetailCacheDropsBeadsThatLeaveThePool(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 40)

	delta := beads.Issue{ID: "gt-p6", Type: "task", Title: "Delta in flight", Status: "hooked", Priority: 1,
		Assignee: "gastown/polecats/capable", Description: "TestDelta fails.",
		CreatedAt: "2026-10-15T09:07:00Z", UpdatedAt: "2026-10-18T09:07:00Z"}
	closed := delta
	closed.Status = "closed"
	closed.ClosedAt = time.Now().Add(-100 * time.Hour).UTC().Format(time.RFC3339)
	closed.CloseReason = "done"
	db.Seed(closed)
	gone := newCountingStore(listLikeBD{Client: db})
	tickOver(t, gone, path, "daemon-1", 40)
	if got := shownIDs(gone); len(got) != 0 {
		t.Fatalf("shown = %v while gt-p6 is out of the pool, want none", got)
	}

	db.Seed(delta)
	back := newCountingStore(listLikeBD{Client: db})
	tickOver(t, back, path, "daemon-1", 40)
	if got := shownIDs(back); len(got) != 1 || got[0] != "gt-p6" {
		t.Errorf("shown = %v after gt-p6 came back, want gt-p6 read again", got)
	}
}

// A cache file that cannot be written costs the next tick its reads and nothing
// else: the pool is read in full and the tick is not failed.
func TestDuplicateDetailCacheUnwritableFileCostsOnlyTheCache(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "cache.json")
	for tickNo := 1; tickNo <= 2; tickNo++ {
		store := newCountingStore(listLikeBD{Client: db})
		tick := tickOver(t, store, path, "daemon-1", 40)
		if len(tick.pool) != 8 || len(shownIDs(store)) != 8 {
			t.Errorf("tick %d: pool %d, shown %d; want 8 and 8", tickNo, len(tick.pool), len(shownIDs(store)))
		}
	}
}

// The cache holds design and notes text, so its file is readable by its owner
// alone, and the directory the daemon creates for it is too.
func TestDuplicateDetailCacheFileIsOwnerOnly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not POSIX permissions on Windows")
	}
	db := duplicateGuardFixture(t)
	dir := filepath.Join(t.TempDir(), "daemon")
	path := filepath.Join(dir, "spec_detail_cache.json")
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", 40)

	assertPerm(t, path, 0o600)
	assertPerm(t, dir, 0o700)
}

// openBlockersOf is the open blockers the pool gives the bead id.
func openBlockersOf(t *testing.T, pool []duplicateCandidate, id string) []string {
	t.Helper()
	for _, c := range pool {
		if c.ID == id {
			return c.Ready.OpenBlockers
		}
	}
	t.Fatalf("%s is not in the pool", id)
	return nil
}

// assertPerm fails unless path has exactly the permission bits want.
func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}
