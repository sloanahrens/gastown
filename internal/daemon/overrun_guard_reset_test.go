package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// resetWorld is a stopped seat's worktree in a gitfake world: origin holds main,
// and the worktree is on the seat's kept branch one commit ahead of it, the
// commit not yet pushed.
type resetWorld struct {
	d       *Daemon
	f       *gitfake.Fake
	origin  string
	work    string
	tip     string
	baseSHA string
}

func newResetWorld(t *testing.T) *resetWorld {
	t.Helper()
	d, _ := newOverrunDaemon(t, false)
	f := useGitfake(t, d)
	w := &resetWorld{d: d, f: f}
	w.origin = filepath.Join(t.TempDir(), "origin.git")
	w.work = filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	f.InitBare(t, w.origin)
	w.baseSHA = f.Commit(t, w.origin, "main", "initial", map[string]string{"a.txt": "a\n"})
	f.Clone(t, w.origin, w.work)
	if err := os.MkdirAll(filepath.Join(w.work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.SetRef(t, w.work, "refs/heads/"+stopSeat.Branch, w.baseSHA)
	if err := f.OpenBranchRepo(w.work).Checkout(stopSeat.Branch); err != nil {
		t.Fatal(err)
	}
	writeWorkFiles(t, w.work, map[string]string{"work.txt": "work\n"})
	w.tip = f.CommitWorktree(t, w.work, "the seat's work")
	return w
}

func (w *resetWorld) git() daemonGit { return w.d.gitAt(w.work) }

func (w *resetWorld) branches(t *testing.T) []string {
	t.Helper()
	got, err := w.git().ListBranches("")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A stop whose push the ls-remote read-back confirms leaves the worktree
// detached at origin's main with no local branch, and the hand-back that ran
// before still names the pushed branch.
func TestStop_ConfirmedPushResetsTheWorktreeToOriginMain(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	if got := w.branches(t); !slices.Contains(got, stopSeat.Branch) {
		t.Fatalf("setup: branches %v lack %s", got, stopSeat.Branch)
	}
	f := newOverrunFake(20*time.Minute, stopSeat)
	s := f.seams()
	s.push = func(seat overrunSeat) error {
		f.calls = append(f.calls, "push "+seat.Rig+"/"+seat.Polecat)
		return w.d.overrunPushBranch(seat)
	}
	s.resetWorktree = func(seat overrunSeat) error {
		f.calls = append(f.calls, "reset-worktree "+seat.Rig+"/"+seat.Polecat)
		return w.d.overrunResetWorktree(seat)
	}

	w.d.runOverrunGuard(s)

	if got := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); got != w.tip {
		t.Fatalf("origin holds %q for the branch, want the seat's tip %s", got, w.tip)
	}
	handBack := slices.Index(f.calls, "hand-back gastown/capable")
	reset := slices.Index(f.calls, "reset-worktree gastown/capable")
	record := slices.Index(f.calls, "record-cleanup gastown/capable")
	if handBack < 0 || reset < handBack || record < reset {
		t.Errorf("calls = %q, want hand-back, then reset-worktree, then record-cleanup", f.calls)
	}
	if len(f.handedBack) != 1 || !strings.HasPrefix(f.handedBack[0], "gt-aaa11 "+stopSeat.Branch+" ") {
		t.Errorf("hand-back %q, want one naming the pushed branch %s", f.handedBack, stopSeat.Branch)
	}
	g := w.git()
	if branch, _ := g.CurrentBranch(); branch != "" && branch != "HEAD" {
		t.Errorf("worktree is on branch %q, want detached", branch)
	}
	if head, err := g.Rev("HEAD"); err != nil || head != w.baseSHA {
		t.Errorf("HEAD = %q (%v), want origin/main %s", head, err, w.baseSHA)
	}
	if got := w.branches(t); slices.Contains(got, stopSeat.Branch) {
		t.Errorf("branches %v still hold %s", got, stopSeat.Branch)
	}
	if _, err := os.Stat(filepath.Join(w.work, "work.txt")); !os.IsNotExist(err) {
		t.Errorf("the branch's file is still in the worktree (stat err %v)", err)
	}
	if st, _ := readOverrunState(w.d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
		t.Errorf("state %+v, want the stop finished", st)
	}
}

// A stop whose push failed, or was given up on, leaves the worktree and its
// branch exactly as they were: the reset is never reached.
func TestStop_FailedPushLeavesTheWorktreeAndBranch(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	f := newOverrunFake(20*time.Minute, stopSeat)
	s := f.seams()
	s.push = func(seat overrunSeat) error {
		f.calls = append(f.calls, "push "+seat.Rig+"/"+seat.Polecat)
		return errors.New("remote unreachable")
	}
	s.resetWorktree = func(seat overrunSeat) error {
		f.calls = append(f.calls, "reset-worktree "+seat.Rig+"/"+seat.Polecat)
		return w.d.overrunResetWorktree(seat)
	}

	w.d.runOverrunGuard(s)
	for i := 2; i <= overrunPushMaxAttempts; i++ {
		f.seats = nil
		f.now = f.now.Add(time.Minute)
		w.d.runOverrunGuard(s)
	}

	if slices.Contains(f.calls, "reset-worktree gastown/capable") {
		t.Fatalf("calls = %q, want no reset after a push that failed", f.calls)
	}
	st, _ := readOverrunState(w.d.config.TownRoot, "gt-aaa11")
	if !st.PushGaveUp {
		t.Fatalf("state %+v, want the push given up on", st)
	}
	g := w.git()
	if head, _ := g.Rev("HEAD"); head != w.tip {
		t.Errorf("HEAD = %s, want the seat's tip %s", head, w.tip)
	}
	if got := w.branches(t); !slices.Contains(got, stopSeat.Branch) {
		t.Errorf("branches %v lost %s", got, stopSeat.Branch)
	}
	if _, err := os.Stat(filepath.Join(w.work, "work.txt")); err != nil {
		t.Errorf("the seat's file is gone: %v", err)
	}
}

// The reset itself confirms the push: origin must hold the branch at the
// worktree's HEAD, read back now. A branch origin lacks, holds at another
// commit, or cannot be asked about leaves the worktree alone.
func TestOverrunResetWorktree_NeedsOriginToHoldTheBranchTip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, w *resetWorld)
	}{
		{"origin lacks the branch", func(*testing.T, *resetWorld) {}},
		{"origin holds another commit", func(t *testing.T, w *resetWorld) {
			w.f.Commit(t, w.origin, stopSeat.Branch, "someone else's", map[string]string{"x.txt": "x\n"})
		}},
		{"origin cannot be asked", func(t *testing.T, w *resetWorld) {
			w.f.RemoveRemote(t, w.work, "origin")
		}},
		{"the worktree holds uncommitted work", func(t *testing.T, w *resetWorld) {
			if err := w.git().Push("origin", w.tip+":refs/heads/"+stopSeat.Branch, false); err != nil {
				t.Fatal(err)
			}
			writeWorkFiles(t, w.work, map[string]string{"dirty.txt": "unsaved\n"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newResetWorld(t)
			tc.prep(t, w)

			if err := w.d.overrunResetWorktree(stopSeat); err != nil {
				t.Fatalf("reset: %v, want nil (nothing to reset is not a failure)", err)
			}

			if head, _ := w.git().Rev("HEAD"); head != w.tip {
				t.Errorf("HEAD = %s, want the seat's tip %s", head, w.tip)
			}
			if got := w.branches(t); !slices.Contains(got, stopSeat.Branch) {
				t.Errorf("branches %v lost %s", got, stopSeat.Branch)
			}
		})
	}
}

// A seat with no worktree has nothing to reset.
func TestOverrunResetWorktree_NoWorktreeIsNothingToReset(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	if err := d.overrunResetWorktree(stopSeat); err != nil {
		t.Errorf("reset of a seat with no worktree: %v, want nil", err)
	}
}

// A failed reset does not hold the release back: the seat is still retired,
// the branch is on origin, and the log says the reset failed.
func TestStop_FailedWorktreeResetStillReleasesTheSeat(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.resetWorktree = func(overrunSeat) error { return errors.New("checkout refused") }

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "checkpoint gastown/capable", "push gastown/capable",
		"hand-back gastown/capable", "reset-agent-bead gastown/capable",
		"reset-worktree gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	calls := slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return strings.HasPrefix(c, "recheck") })
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if !strings.Contains(buf.String(), "worktree not reset after the push of "+stopSeat.Branch+": checkout refused") {
		t.Errorf("log lacks the reset failure:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
		t.Errorf("state %+v, want the stop finished", st)
	}
}
