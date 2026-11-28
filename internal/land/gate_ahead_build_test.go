package land

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestGateAheadBuildAllowsAnApprovedMerge is train 3b-3: a submission passing
// the check, the merge and a stored approval comes back merged with the
// cleanup that removes slot 1's worktree, and nothing is written to the bead.
func TestGateAheadBuildAllowsAnApprovedMerge(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	store := TownAheadStore(f.town, "gastown")
	store.Now = l.Now
	l.Ahead, l.ReuseExpiry = store, time.Hour
	// The candidate is merged once to learn the key its review would carry. The
	// build's own merge has the same key: it holds the patch-id, not the commit.
	origin := f.git.Open(f.origin)
	dir, merged, cleanup, err := l.gateAheadMerge(origin, f.work, f.base)
	must(t, err)
	issue, err := l.Beads.Show(f.work.BeadID)
	must(t, err)
	in, _, _, err := l.reviewInputFor(issue, f.work, f.base, merged, dir, origin, true)
	must(t, err)
	key, _, err := l.reviewKey(context.Background(), origin, l.open(dir), f.work, in, f.base, merged)
	must(t, err)
	cleanup()
	must(t, store.Put(reuseAskRecord(key, f.base, l.now().Add(-time.Minute))))

	before := aheadBeadWrites(t, f.bd)
	dir, merged, cleanup, err = l.gateAheadBuild(l.open(f.repo), f.work, f.base)
	if err != nil {
		t.Fatalf("gateAheadBuild: %v", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("gateAheadBuild wrote %d bead records, want 0", n)
	}
	if cleanup == nil {
		t.Fatal("gateAheadBuild returned no cleanup")
	}
	if want := filepath.Join(f.workRoot, landWorktreeName+"-1"); dir != want {
		t.Errorf("dir = %s, want slot 1's %s", dir, want)
	}
	if got, want := f.git.Parents(merged), []string{f.base, f.work.Head}; !slices.Equal(got, want) {
		t.Errorf("merged parents = %v, want %v", got, want)
	}
	cleanup()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("slot 1 worktree after cleanup: stat err = %v, want it removed", statErr)
	}
}

// TestGateAheadBuildFailuresLeaveNoWorktree is train 3b-3: a refused check,
// merge or approval returns the reason with no candidate, no worktree and no
// bead write.
func TestGateAheadBuildFailuresLeaveNoWorktree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, f *landFixture, l *Lander) string
		want  string // substring of the refusal
		stage string // *InfraError stage, instead of want
	}{
		{"check refuses", func(t *testing.T, f *landFixture, l *Lander) string {
			i, err := f.bd.Show(f.work.BeadID)
			must(t, err)
			i.Labels = nil
			f.bd.Seed(*i)
			return f.base
		}, "has no " + LabelReadyToLand + " label", ""},
		{"merge refuses", func(t *testing.T, f *landFixture, l *Lander) string {
			return strings.Repeat("0", 40)
		}, "", "worktree"},
		{"approval refuses", func(t *testing.T, f *landFixture, l *Lander) string {
			store := TownAheadStore(f.town, "gastown")
			store.Now = l.Now
			l.Ahead, l.ReuseExpiry = store, time.Hour
			return f.base
		}, "no stored review is kept", ""},
		{"no store", func(t *testing.T, f *landFixture, l *Lander) string {
			return f.base
		}, "no review-ahead store", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, _ := keyFixture(t)
			before := aheadBeadWrites(t, f.bd)
			dir, merged, cleanup, err := l.gateAheadBuild(l.open(f.repo), f.work, tc.setup(t, f, l))
			if err == nil {
				t.Fatal("gateAheadBuild = nil error, want the refusal")
			}
			if dir != "" || merged != "" || cleanup != nil {
				t.Errorf("dir = %q, merged = %q, cleanup = %v, want all empty", dir, merged, cleanup != nil)
			}
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("gateAheadBuild wrote %d bead records, want 0", n)
			}
			if tc.stage != "" {
				wantStage(t, err, tc.stage)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			assertNoWorkRootEntries(t, f)
		})
	}
}
