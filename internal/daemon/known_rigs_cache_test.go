package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// TestGetKnownRigs_CachedBetweenInvalidations verifies that d.getKnownRigs()
// memoizes rigs.json reads and only re-reads after invalidation. This is the
// regression test for #3463 — without the cache the ~10 per-tick callers each
// read and parse the file independently.
func TestGetKnownRigs_CachedBetweenInvalidations(t *testing.T) {
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	if err := os.WriteFile(rigsPath, []byte(`{"rigs":{"alpha":{},"beta":{}}}`), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	d := &Daemon{config: &Config{TownRoot: townRoot}}

	// First call populates the cache.
	first := d.getKnownRigs()
	slices.Sort(first)
	if !slices.Equal(first, []string{"alpha", "beta"}) {
		t.Fatalf("first call: got %v, want [alpha beta]", first)
	}

	// Delete the file on disk. A cached call must still return the old list.
	if err := os.Remove(rigsPath); err != nil {
		t.Fatalf("remove rigs.json: %v", err)
	}
	cached := d.getKnownRigs()
	slices.Sort(cached)
	if !slices.Equal(cached, []string{"alpha", "beta"}) {
		t.Fatalf("cached call after delete: got %v, want [alpha beta] (cache bypassed?)", cached)
	}

	// Invalidate — next call must re-read from disk (now empty).
	d.invalidateKnownRigsCache()
	if got := d.getKnownRigs(); len(got) != 0 {
		t.Fatalf("post-invalidate call: got %v, want empty", got)
	}

	// A subsequent write should still not surface until the next invalidation.
	if err := os.WriteFile(rigsPath, []byte(`{"rigs":{"gamma":{}}}`), 0o644); err != nil {
		t.Fatalf("rewrite rigs.json: %v", err)
	}
	if got := d.getKnownRigs(); len(got) != 0 {
		t.Fatalf("cached-empty call after rewrite: got %v, want empty (cache bypassed?)", got)
	}
	d.invalidateKnownRigsCache()
	if got := d.getKnownRigs(); !slices.Equal(got, []string{"gamma"}) {
		t.Fatalf("post-invalidate call after rewrite: got %v, want [gamma]", got)
	}
}

// TestGetKnownRigs_ConcurrentInvalidation is the regression test for gt-f18v.
// The cache is a heartbeat-tick memo, but a tick's main_branch_test cycle runs
// on its own goroutine (gt-uvxy) and calls getKnownRigs, so that goroutine's
// read races the next tick's invalidateKnownRigsCache at the top of the
// heartbeat. Run under -race (CI does): without the mutex the reader and the
// invalidator hit both cache fields unsynchronized and the detector reports
// the race; the value assertions hold either way.
//
// One reader against one invalidator per round, racing only each other: a
// reader spinning against a long invalidation loop races the same fields
// hundreds of times, which is all the detector needs to not report, and a
// round where the two never overlap is a round that proves nothing.
func TestGetKnownRigs_ConcurrentInvalidation(t *testing.T) {
	townRoot := t.TempDir()
	writeRigsJSON(t, townRoot, []string{"alpha"})
	d := &Daemon{config: &Config{TownRoot: townRoot}}

	for range 500 {
		start := make(chan struct{})
		var round sync.WaitGroup
		round.Add(2)
		go func() {
			defer round.Done()
			<-start
			d.invalidateKnownRigsCache()
		}()
		go func() {
			defer round.Done()
			<-start
			// rigs.json never changes here, so every read must see alpha
			// regardless of where the invalidation lands.
			if got := d.getKnownRigs(); !slices.Equal(got, []string{"alpha"}) {
				t.Errorf("getKnownRigs() = %v, want [alpha]", got)
			}
		}()
		close(start)
		round.Wait()
	}
}
