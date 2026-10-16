package land

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// probeRepo is a Repo whose probe steps fail on demand and which records the
// calls that undo the probe.
type probeRepo struct {
	Repo
	mergeErr, treesErr, resetErr error
	aborted, reset               bool
}

func (r *probeRepo) MergeNoFF(branch, message string) error {
	if r.mergeErr != nil {
		return r.mergeErr
	}
	return r.Repo.MergeNoFF(branch, message)
}

func (r *probeRepo) AbortMerge() error {
	r.aborted = true
	return r.Repo.AbortMerge()
}

func (r *probeRepo) TreesIdentical(a, b string) (bool, error) {
	if r.treesErr != nil {
		return false, r.treesErr
	}
	return r.Repo.TreesIdentical(a, b)
}

func (r *probeRepo) ResetHard(ref string) error {
	r.reset = true
	if r.resetErr != nil {
		return r.resetErr
	}
	return r.Repo.ResetHard(ref)
}

// probeWorktree is a detached worktree at f.base with the head fetched, the
// state mergeWork hands mergeChangesNothing.
func probeWorktree(t *testing.T, f *landFixture) *probeRepo {
	t.Helper()
	clone := f.git.Open(f.repo)
	for _, branch := range []string{"main", fixtureBranch} {
		if err := clone.FetchRefspecWithTimeout("origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "wt")
	if err := clone.WorktreeAddDetached(dir, f.base); err != nil {
		t.Fatal(err)
	}
	return &probeRepo{Repo: f.git.Open(dir)}
}

// TestMergeChangesNothing pins the probe's answer on each branch and that it
// hands the worktree back where it found it: a probe that merged cleanly
// committed, and the next reader of the worktree must not see that commit
// (gt-1eblc).
func TestMergeChangesNothing(t *testing.T) {
	t.Parallel()
	// The head's work is on a main that has moved on, so the probe's merge is
	// clean and writes nothing.
	alreadyThere := func(t *testing.T) *landFixture {
		f := newLandFixture(t)
		f.squashRange(t)
		f.pushMain("b.txt", "work\n")
		f.pushMain("c.txt", "wip\n")
		f.base = f.pushMain("d.txt", "someone else landed\n")
		return f
	}
	boom := errors.New("git: transient failure")
	tests := []struct {
		name        string
		fixture     func(t *testing.T) *landFixture
		setup       func(r *probeRepo)
		want        bool
		wantAborted bool
		wantReset   bool
		wantAtBase  bool
	}{
		{name: "the head's work is already on the base", fixture: alreadyThere, want: true, wantReset: true, wantAtBase: true},
		{name: "the merge would write the head's work", fixture: newLandFixture, want: false, wantReset: true, wantAtBase: true},
		{name: "the probe merge fails", fixture: alreadyThere, setup: func(r *probeRepo) { r.mergeErr = boom },
			want: false, wantAborted: true, wantAtBase: true},
		{name: "the tree comparison fails", fixture: alreadyThere, setup: func(r *probeRepo) { r.treesErr = boom },
			want: false, wantReset: true, wantAtBase: true},
		{name: "the reset fails", fixture: alreadyThere, setup: func(r *probeRepo) { r.resetErr = boom },
			want: false, wantReset: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := tt.fixture(t)
			wt := probeWorktree(t, f)
			if tt.setup != nil {
				tt.setup(wt)
			}
			if got := mergeChangesNothing(wt, f.work, f.base, "probe"); got != tt.want {
				t.Errorf("mergeChangesNothing = %v, want %v", got, tt.want)
			}
			if wt.aborted != tt.wantAborted || wt.reset != tt.wantReset {
				t.Errorf("aborted = %v, reset = %v; want %v, %v", wt.aborted, wt.reset, tt.wantAborted, tt.wantReset)
			}
			if tt.wantAtBase {
				if head, err := wt.Rev("HEAD"); err != nil || head != f.base {
					t.Errorf("HEAD after the probe = %s (%v), want the base %s", head, err, f.base)
				}
			}
		})
	}
}
