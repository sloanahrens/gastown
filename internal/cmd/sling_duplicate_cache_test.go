package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// tickOver is one tick of the daemon's pool fetch: a fresh cache loaded from
// path, as a new `gt spec dispatch` process loads it, read through store. It
// returns the pool, the counts the tick would report and what warn heard.
type cacheTick struct {
	pool                    []duplicateCandidate
	fetched, cached, unread int
	warnings                []string
}

func tickOver(t *testing.T, store beads.Client, path, owner string, limit int) cacheTick {
	t.Helper()
	cache := loadDuplicateDetailCache(path, owner, limit)
	var out cacheTick
	pool, err := fetchDuplicatePoolWith(store, func(_ string, err error) {
		out.warnings = append(out.warnings, err.Error())
	}, "rig", cache)
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
