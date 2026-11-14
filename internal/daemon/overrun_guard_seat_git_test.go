package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// overrunSeatGit is the one seam that picks the git handle the stop's save,
// dirty check, push and reset run over. A host seat gets its worktree's handle;
// a seat whose worktree cannot be resolved is errOverrunNoWorktree, never a nil
// handle that would read as nothing to save.
func TestOverrunSeatGit_HostSeatGetsItsWorktreeHandle(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	wantDir, err := w.git().GitDir()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		seat  overrunSeat
		fails bool
	}{
		{name: "host seat", seat: stopSeat},
		{name: "unresolvable worktree", seat: overrunSeat{Rig: stopSeat.Rig, Polecat: "ghost", Bead: stopSeat.Bead}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := w.d.overrunSeatGit(tc.seat)
			if tc.fails {
				if !errors.Is(err, errOverrunNoWorktree) || g != nil {
					t.Fatalf("overrunSeatGit = (%v, %v), want %v and no handle", g, err, errOverrunNoWorktree)
				}
				return
			}
			if err != nil {
				t.Fatalf("overrunSeatGit = %v, want the host worktree's handle", err)
			}
			gotDir, err := g.GitDir()
			if err != nil {
				t.Fatalf("the returned handle cannot read its git directory: %v", err)
			}
			if gotDir != wantDir {
				t.Errorf("handle's git directory = %q, want the host worktree's %q", gotDir, wantDir)
			}
		})
	}
}

// recordingSeatGit records the calls the stop's steps make on the handle
// overrunSeatGit returned for the seat, so a test can tell that handle apart
// from any other runner.
type recordingSeatGit struct {
	daemonGit
	statuses int
	commits  []string
	pushes   []string
	deleted  []string
}

func (r *recordingSeatGit) Status() (*git.GitStatus, error) {
	r.statuses++
	return r.daemonGit.Status()
}

func (r *recordingSeatGit) Commit(message string) error {
	r.commits = append(r.commits, message)
	return r.daemonGit.Commit(message)
}

func (r *recordingSeatGit) Push(remote, refspec string, force bool) error {
	r.pushes = append(r.pushes, refspec)
	return r.daemonGit.Push(remote, refspec, force)
}

func (r *recordingSeatGit) DeleteBranch(name string, force bool) error {
	r.deleted = append(r.deleted, name)
	return r.daemonGit.DeleteBranch(name, force)
}

// The save, the dirty check, the push and the reset each run on the handle
// overrunSeatGit returns. The daemon's own opener fails the test, so a step
// that resolved the worktree and opened its own handle again would be caught
// there instead of reaching the recording handle.
func TestOverrunSeatGit_IsTheHandleTheStopStepsUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, w *resetWorld, rec *recordingSeatGit)
	}{
		{
			name: "save",
			run: func(t *testing.T, w *resetWorld, rec *recordingSeatGit) {
				writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})
				if _, err := w.d.overrunSaveWork(stopSeat); err != nil {
					t.Fatalf("save = %v, want nil", err)
				}
				if !slices.Equal(rec.commits, []string{overrunSaveMessage(stopSeat.Bead)}) {
					t.Errorf("commits on the seat's handle = %q, want the WIP commit on the handle overrunSeatGit returned", rec.commits)
				}
				if len(rec.pushes) != 1 {
					t.Errorf("pushes on the seat's handle = %q, want the WIP commit pushed through it", rec.pushes)
				}
			},
		},
		{
			name: "dirty check",
			run: func(t *testing.T, w *resetWorld, rec *recordingSeatGit) {
				writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})
				dirty, err := w.d.overrunWorktreeDirty(stopSeat)
				if err != nil || !dirty {
					t.Fatalf("dirty check = %v, %v; want the handle overrunSeatGit returned read", dirty, err)
				}
				if rec.statuses != 1 {
					t.Errorf("status reads on the seat's handle = %d, want 1", rec.statuses)
				}
			},
		},
		{
			name: "push",
			run: func(t *testing.T, w *resetWorld, rec *recordingSeatGit) {
				if err := w.d.overrunPushBranch(stopSeat); err != nil {
					t.Fatalf("push = %v, want nil", err)
				}
				if len(rec.pushes) != 1 || rec.pushes[0] != w.tip+":refs/heads/"+stopSeat.Branch {
					t.Errorf("pushes on the seat's handle = %q, want the seat's tip pushed through it", rec.pushes)
				}
			},
		},
		{
			name: "reset",
			run: func(t *testing.T, w *resetWorld, rec *recordingSeatGit) {
				if err := w.d.overrunPushBranch(stopSeat); err != nil {
					t.Fatalf("setup: push = %v", err)
				}
				if err := w.d.overrunResetWorktree(stopSeat); err != nil {
					t.Fatalf("reset = %v, want nil", err)
				}
				if !slices.Equal(rec.deleted, []string{stopSeat.Branch}) {
					t.Errorf("branches deleted through the seat's handle = %q, want %s", rec.deleted, stopSeat.Branch)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newResetWorld(t)
			rec := &recordingSeatGit{daemonGit: w.git()}
			w.d.openGitFn = func(dir string) daemonGit {
				t.Fatalf("%s opened a git handle for %s; want only the handle overrunSeatGit returned", tc.name, dir)
				return nil
			}
			w.d.overrunSeatGitFn = func(seat overrunSeat) (daemonGit, error) {
				return rec, nil
			}
			tc.run(t, w, rec)
		})
	}
}

// A seat whose worktree cannot be resolved keeps answering as it did before the
// handle was routed: the save and the dirty check refuse it, and the push and
// the reset treat it as nothing to do.
func TestOverrunSeatGit_UnresolvableWorktreeKeepsItsAnswer(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	ghost := overrunSeat{Rig: stopSeat.Rig, Polecat: "ghost", Bead: stopSeat.Bead, Branch: stopSeat.Branch}

	if _, err := w.d.overrunSaveWork(ghost); !errors.Is(err, errOverrunNoWorktree) {
		t.Errorf("save of a seat with no worktree = %v, want %v", err, errOverrunNoWorktree)
	}
	if dirty, err := w.d.overrunWorktreeDirty(ghost); !errors.Is(err, errOverrunNoWorktree) || dirty {
		t.Errorf("dirty check of a seat with no worktree = %v, %v; want %v and not dirty", dirty, err, errOverrunNoWorktree)
	}
	if err := w.d.overrunPushBranch(ghost); err != nil {
		t.Errorf("push of a seat with no worktree = %v, want nothing to push", err)
	}
	if err := w.d.overrunResetWorktree(ghost); err != nil {
		t.Errorf("reset of a seat with no worktree = %v, want nothing to reset", err)
	}
}

// A seam failure that is not a missing worktree is not read as nothing to do:
// the push and the reset return it, so the push is retried and the reset is
// warned of, instead of the seat being treated as empty.
func TestOverrunSeatGit_OtherFailuresAreNotReadAsNoWorktree(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	boom := errors.New("sandbox runner unavailable")
	w.d.overrunSeatGitFn = func(seat overrunSeat) (daemonGit, error) {
		return nil, boom
	}

	if _, err := w.d.overrunSaveWork(stopSeat); !errors.Is(err, boom) {
		t.Errorf("save = %v, want the seam's failure", err)
	}
	if dirty, err := w.d.overrunWorktreeDirty(stopSeat); !errors.Is(err, boom) || dirty {
		t.Errorf("dirty check = %v, %v; want the seam's failure and not dirty", dirty, err)
	}
	if err := w.d.overrunPushBranch(stopSeat); !errors.Is(err, boom) {
		t.Errorf("push = %v, want the seam's failure", err)
	}
	if err := w.d.overrunResetWorktree(stopSeat); !errors.Is(err, boom) {
		t.Errorf("reset = %v, want the seam's failure", err)
	}
}
