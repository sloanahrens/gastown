package sling

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestSeatBranchRefusal pins which refusals after a spawn carry
// ErrSeatBranchRefused: the seat's own branch is unreadable or names another
// bead's work, and not a bead held at the claim. The sling give-back turns on
// this distinction to hand the respawn count back (gt-znvgef).
func TestSeatBranchRefusal(t *testing.T) {
	t.Parallel()
	const branchErrText = "git rev-parse: not a repository"
	for _, tc := range []struct {
		name      string
		setUp     func(*fakeSlingDeps)
		wantTyped bool
	}{
		{
			name:      "the worktree branch cannot be read",
			setUp:     func(f *fakeSlingDeps) { f.branchErr = errors.New(branchErrText) },
			wantTyped: true,
		},
		{
			name:      "the seat holds another bead's branch",
			setUp:     func(f *fakeSlingDeps) { f.branch = "polecat/coma/gt-ug7zed+mwmuimru" },
			wantTyped: true,
		},
		{
			name: "the bead is held at the claim",
			setUp: func(f *fakeSlingDeps) {
				f.reads = []holdTestRead{
					{bead: &Bead{Status: "open"}},
					{bead: &Bead{Status: "open", Labels: []string{"needs-human"}}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSlingDeps{}
			tc.setUp(f)

			result, err := Run(context.Background(), f.deps(), hookBranchTestOptions())
			if err == nil {
				t.Fatalf("the dispatch committed (result %+v), want a refusal after the spawn", result)
			}
			if !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
				t.Errorf("err = %v, want the refusal marker %q: an automatic dispatcher must defer the bead", err, dispatch.SlingRefusalMarker)
			}
			if got := errors.Is(err, ErrSeatBranchRefused); got != tc.wantTyped {
				t.Errorf("errors.Is(err, ErrSeatBranchRefused) = %v, want %v: err = %v", got, tc.wantTyped, err)
			}
			if f.rollback != 1 {
				t.Errorf("rolled back %d spawn(s), want 1: the refusal is past the spawn", f.rollback)
			}
		})
	}
}
