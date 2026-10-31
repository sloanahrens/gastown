package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// The sling child's duplicate check (gt-nebs9). A background sling runs as its
// own `gt spec sling` process, which used to show every bead of the pool for
// its design and notes: the tick's cache lived in the tick's process, so the
// child passed none and the fetch fell back to one show of the whole pool,
// 39 s a sling on average. The child now reads the cache file the tick that
// planned it saved, through the env it runs its job with (specSlingEnv), and
// writes nothing back.

// slingChildEnv is the env a `gt spec sling` child runs with, on a town whose
// cache file is whatever this test left there, and the cache the child's pool
// fetch reads once the env is built. These tests do not run in parallel: the
// env is the process's cache wiring (specDetailCache), as in the child.
func slingChildEnv(t *testing.T, townRoot, owner string) *duplicateDetailCache {
	t.Helper()
	specSlingEnv(townRoot, owner, &specSlingTraces{})
	t.Cleanup(func() { specDetailCache.Store(nil) })
	return specDetailCache.Load()
}

// slingChildPool is the pool fetch the child's duplicate check makes, over
// store, reporting what bd was asked and what warn heard.
func slingChildPool(t *testing.T, store beads.Client, details *duplicateDetailCache) ([]duplicateCandidate, []string) {
	t.Helper()
	var warnings []string
	pool, err := fetchDuplicatePoolWith(store, func(_ string, err error) {
		warnings = append(warnings, err.Error())
	}, "rig", details)
	if err != nil {
		t.Fatalf("fetchDuplicatePoolWith: %v", err)
	}
	return pool, warnings
}

// A child with a warm cache file shows nothing: every pool bead's list row
// carries the updated_at the entry was read at, so the design and notes come
// from the file. The mutation this pins is the child handing its pool fetch no
// cache (specSlingDetailCacheFor returning nil, or the env storing nil): the
// fetch then shows all eight pool beads and the counts below are its.
//
//testpolicy:allow parallel — installs the process-wide design/notes cache the sling's pool fetch reads, which another test must not see mid-run
func TestSlingChildDuplicatePoolUsesDetailCache(t *testing.T) {
	db := duplicateGuardFixture(t)
	townRoot := t.TempDir()
	path := dispatch.SpecDetailCachePath(townRoot)
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", duplicateDetailMissLimit)

	details := slingChildEnv(t, townRoot, "daemon-1")
	if details == nil {
		t.Fatal("the child got no cache from a daemon's owner")
	}

	store := newCountingStore(listLikeBD{Client: db})
	pool, warnings := slingChildPool(t, store, details)
	if len(store.multis) != 0 || len(store.shows) != 0 {
		t.Fatalf("the child showed %d batches (%v) and %d beads, want none: every row's updated_at matches the cache",
			len(store.multis), store.multis, len(store.shows))
	}
	if len(pool) != 8 {
		t.Errorf("pool = %d candidates, want the 8 the tick's pool has", len(pool))
	}
	if len(warnings) != 0 {
		t.Errorf("a warm child warned: %v", warnings)
	}
}

// A child whose cache file is missing — a restarted daemon, or one that never
// finished a tick — falls back to showing the pool itself, but at most
// duplicateDetailMissLimit beads, and says how many it left unread, as the tick
// does. The pool keeps every bead, compared on list text where it has no
// detail.
//
//testpolicy:allow parallel — installs the process-wide design/notes cache the sling's pool fetch reads, which another test must not see mid-run
func TestSlingChildDuplicatePoolCapsColdCache(t *testing.T) {
	const poolSize = duplicateDetailMissLimit + 5
	db := beadsfake.New()
	for i := range poolSize {
		db.Seed(beads.Issue{
			ID:          fmt.Sprintf("gt-c%03d", i),
			Type:        "task",
			Title:       fmt.Sprintf("Child pool bead %d", i),
			Status:      "open",
			Priority:    2,
			Description: "Nothing shared.",
			Notes:       fmt.Sprintf("Touches internal/child%d.go.", i),
			CreatedAt:   "2026-10-15T09:00:00Z",
			UpdatedAt:   fmt.Sprintf("2026-10-18T09:00:%02dZ", i),
		})
	}
	townRoot := t.TempDir()
	if _, err := os.Stat(dispatch.SpecDetailCachePath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("the town starts with a cache file (%v); this test needs none", err)
	}

	details := slingChildEnv(t, townRoot, "daemon-1")
	store := newCountingStore(listLikeBD{Client: db})
	pool, warnings := slingChildPool(t, store, details)

	shown := shownIDs(store)
	if len(shown) > duplicateDetailMissLimit {
		t.Fatalf("the child showed %d beads, over its limit of %d", len(shown), duplicateDetailMissLimit)
	}
	if len(shown) != duplicateDetailMissLimit {
		t.Errorf("the child showed %d beads, want the limit of %d spent", len(shown), duplicateDetailMissLimit)
	}
	if len(pool) != poolSize {
		t.Errorf("pool = %d candidates, want all %d: an unread bead is compared on list text", len(pool), poolSize)
	}
	want := fmt.Sprintf("design/notes of %d of %d bead(s) not read this tick, over its limit of %d",
		poolSize-duplicateDetailMissLimit, poolSize, duplicateDetailMissLimit)
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, want) }) {
		t.Errorf("warnings = %q, want one naming the unread count: %q", warnings, want)
	}
}

// The child writes nothing back: a bead the tick has not seen is shown by the
// child, and the file keeps exactly what the tick saved. Writing it here would
// race the daemon's next tick, whose newer entries the child's older view
// would overwrite.
//
//testpolicy:allow parallel — installs the process-wide design/notes cache the sling's pool fetch reads, which another test must not see mid-run
func TestSlingChildDuplicatePoolLeavesTheCacheFileAlone(t *testing.T) {
	db := duplicateGuardFixture(t)
	townRoot := t.TempDir()
	path := dispatch.SpecDetailCachePath(townRoot)
	tickOver(t, newCountingStore(listLikeBD{Client: db}), path, "daemon-1", duplicateDetailMissLimit)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the tick's cache: %v", err)
	}

	// An edit the tick has not seen: the child shows this bead and would save
	// the entry it just read if it owned the file.
	db.Seed(beads.Issue{ID: "gt-p1", Type: "task", Title: "Alpha flake", Status: "open", Priority: 1,
		Description: "Unrelated prose.", Design: "Root cause is now TestOmega.",
		CreatedAt: "2026-10-15T09:00:00Z", UpdatedAt: "2026-10-19T09:00:00Z"})
	details := slingChildEnv(t, townRoot, "daemon-1")
	store := newCountingStore(listLikeBD{Client: db})
	slingChildPool(t, store, details)
	if shown := shownIDs(store); !slices.Equal(shown, []string{"gt-p1"}) {
		t.Fatalf("the child showed %v, want only the edited gt-p1", shown)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the tick's cache after the child: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the child rewrote the tick's cache file:\n%s\n%s", before, after)
	}
}

// A sling no daemon planned reads no cache: the behaviour every sling had
// before gt-nebs9.
func TestSlingChildDetailCacheNeedsAnOwner(t *testing.T) {
	t.Parallel()
	if cache := specSlingDetailCacheFor(t.TempDir(), ""); cache != nil {
		t.Errorf("a child with no owner got cache %+v, want none", cache)
	}
}
