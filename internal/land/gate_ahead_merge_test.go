package land

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingMerge fails the worktree's merge, so a test can drive mergeWork's
// error branch.
type failingMerge struct {
	Repo
	err error
}

func (r failingMerge) MergeNoFF(string, string) error { return r.err }

// TestGateAheadMergeMergesInSlotOne is train 3b-1: the merge sits in slot 1's
// worktree, and the cleanup removes that worktree.
func TestGateAheadMergeMergesInSlotOne(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	dir, merged, cleanup, err := l.gateAheadMerge(l.open(f.repo), f.work, f.base)
	if err != nil {
		t.Fatalf("gateAheadMerge: %v", err)
	}
	if cleanup == nil {
		t.Fatal("gateAheadMerge returned no cleanup")
	}
	if want := filepath.Join(f.workRoot, landWorktreeName+"-1"); dir != want {
		t.Errorf("dir = %s, want slot 1's %s", dir, want)
	}
	if parents := f.git.Parents(merged); len(parents) != 2 || parents[0] != f.base || parents[1] != f.work.Head {
		t.Errorf("merged parents = %v, want [%s %s]", parents, f.base, f.work.Head)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("slot 1 worktree before cleanup: %v, want it in place", statErr)
	}
	cleanup()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("slot 1 worktree after cleanup: stat err = %v, want it removed", statErr)
	}
	f.assertUntouched(t)
}

// TestGateAheadMergeFailuresLeaveNoWorktree is train 3b-1: each failure returns
// the reason with a nil cleanup, writes nothing to the bead and leaves no
// worktree.
func TestGateAheadMergeFailuresLeaveNoWorktree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, f *landFixture, l *Lander) string
		check func(t *testing.T, err error)
	}{
		{"failed identity read",
			func(t *testing.T, f *landFixture, l *Lander) string {
				l.authorOf = func(Work) (Identity, error) { return Identity{}, errors.New("head has no author") }
				return f.base
			},
			func(t *testing.T, err error) { wantStage(t, err, "head author") }},
		{"failed worktree add",
			func(t *testing.T, f *landFixture, l *Lander) string {
				return strings.Repeat("0", 40) // no commit has this id
			},
			func(t *testing.T, err error) { wantStage(t, err, "worktree") }},
		{"conflict",
			func(t *testing.T, f *landFixture, l *Lander) string {
				// The branch and main both write b.txt; the caller fetched main.
				base := f.pushMain("b.txt", "main's b\n")
				if err := f.git.Open(f.repo).FetchRefspecWithTimeout("origin", "+refs/heads/main:refs/remotes/origin/main", 0); err != nil {
					t.Fatal(err)
				}
				return base
			},
			func(t *testing.T, err error) {
				var rej *Rejection
				if !errors.As(err, &rej) || rej.Kind != RejectConflict {
					t.Fatalf("error = %T %v, want a conflict rejection", err, err)
				}
				if len(rej.Conflicting) != 1 || rej.Conflicting[0] != "b.txt" {
					t.Errorf("conflicting = %v, want [b.txt]", rej.Conflicting)
				}
			}},
		{"failed merge",
			func(t *testing.T, f *landFixture, l *Lander) string {
				open := l.openRepo
				l.openRepo = func(dir string) Repo {
					r := open(dir)
					if filepath.Dir(dir) == f.workRoot {
						return failingMerge{Repo: r, err: errors.New("merge refused")}
					}
					return r
				}
				return f.base
			},
			func(t *testing.T, err error) { wantStage(t, err, "merge") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			base := tc.setup(t, f, l)
			dir, merged, cleanup, err := l.gateAheadMerge(l.open(f.repo), f.work, base)
			if err == nil {
				t.Fatal("gateAheadMerge = nil error, want the failure")
			}
			if cleanup != nil {
				t.Error("cleanup is non-nil after a failure")
			}
			if dir != "" || merged != "" {
				t.Errorf("dir = %q, merged = %q, want both empty on failure", dir, merged)
			}
			tc.check(t, err)
			assertNoWorkRootEntries(t, f)
			f.assertUntouched(t)
		})
	}
}

// wantStage asserts err is the *InfraError of stage.
func wantStage(t *testing.T, err error, stage string) {
	t.Helper()
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != stage {
		t.Fatalf("error = %T %v, want an *InfraError at %s", err, err, stage)
	}
}

// assertNoWorkRootEntries fails when workRoot still holds the worktree or the
// land- parent directory a failed call made.
func assertNoWorkRootEntries(t *testing.T, f *landFixture) {
	t.Helper()
	entries, err := os.ReadDir(f.workRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading work root: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 0 {
		t.Errorf("work root holds %v after a failure, want nothing", names)
	}
}
