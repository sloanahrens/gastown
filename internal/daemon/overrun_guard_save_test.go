package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// The save step (overrunSaveWork) runs on newResetWorld: the seat's worktree
// is on its kept branch, one commit ahead of origin's main and, unless a test
// pushes it, not yet on origin. origin has no such branch until one is pushed.

// saveCommits lists the commit subjects the worktree holds above base.
func (w *resetWorld) saveCommits(t *testing.T, base string) []string {
	t.Helper()
	msgs, err := w.f.Open(w.work).CommitMessages(base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, m.Message)
	}
	return got
}

// pushKeptBranch puts the seat's tip on origin, as a stop's push would.
func (w *resetWorld) pushKeptBranch(t *testing.T) {
	t.Helper()
	if err := w.git().Push("origin", w.tip+":refs/heads/"+stopSeat.Branch, false); err != nil {
		t.Fatal(err)
	}
}

// divergeOriginBranch gives origin a different history for the kept branch: a
// commit on main's base that the seat's tip does not contain, so the seat's
// unforced push is refused and the tip adds work origin lacks.
func (w *resetWorld) divergeOriginBranch(t *testing.T) {
	t.Helper()
	w.f.SetRef(t, w.origin, "refs/heads/"+stopSeat.Branch, w.baseSHA)
	w.f.Commit(t, w.origin, stopSeat.Branch, "origin's own history", map[string]string{"origin.txt": "origin\n"})
}

func TestOverrunSaveWork_DirtyWorktreeIsOneWIPCommitPushed(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})

	got, err := w.d.overrunSaveWork(stopSeat)
	if err != nil {
		t.Fatalf("save = %v, want nil", err)
	}
	head, err := w.git().Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Committed || got.Head != head {
		t.Errorf("save = %+v, want the WIP commit %s", got, head)
	}
	if msgs := w.saveCommits(t, w.tip); len(msgs) != 1 || msgs[0] != "WIP overrun stop: gt-aaa11" {
		t.Errorf("commits above the seat's tip = %q, want one WIP commit naming the bead", msgs)
	}
	if origin := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); origin != head {
		t.Errorf("origin holds %q for the branch, want the WIP commit %s", origin, head)
	}
}

func TestOverrunSaveWork_CleanPushedWorktreeChangesNothing(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	w.pushKeptBranch(t)

	got, err := w.d.overrunSaveWork(stopSeat)
	if err != nil || got != (overrunSave{}) {
		t.Errorf("save = %+v, %v; want nothing saved", got, err)
	}
	head, err := w.git().Rev("HEAD")
	if err != nil || head != w.tip {
		t.Errorf("HEAD = %s (%v), want the seat's tip %s unchanged", head, err, w.tip)
	}
	if msgs := w.saveCommits(t, w.tip); len(msgs) != 0 {
		t.Errorf("commits above the tip = %q, want none", msgs)
	}
}

// A clean worktree with a commit origin lacks is pushed as it is: no commit is
// made, and the head is the seat's own.
func TestOverrunSaveWork_UnpushedCommitIsPushedWithoutACommit(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)

	got, err := w.d.overrunSaveWork(stopSeat)
	if err != nil {
		t.Fatalf("save = %v, want nil", err)
	}
	if got.Committed || got.Head != w.tip {
		t.Errorf("save = %+v, want the seat's tip %s pushed with no WIP commit", got, w.tip)
	}
	if origin := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); origin != w.tip {
		t.Errorf("origin holds %q for the branch, want the tip %s", origin, w.tip)
	}
	if msgs := w.saveCommits(t, w.tip); len(msgs) != 0 {
		t.Errorf("commits above the tip = %q, want none", msgs)
	}
}

// Each refusal of the save is an error that names nothing as pushed, so the
// stop keeps the sandbox. The push refused as non-fast-forward with work
// origin lacks is the divergence error, and nothing is forced.
func TestOverrunSaveWork_RefusedPushIsAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		dirty bool
	}{
		{name: "clean worktree with an unpushed commit", dirty: false},
		{name: "dirty worktree committed first", dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newResetWorld(t)
			w.divergeOriginBranch(t)
			if tc.dirty {
				writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})
			}

			got, err := w.d.overrunSaveWork(stopSeat)
			var diverged *overrunPushDivergedError
			if !errors.As(err, &diverged) {
				t.Fatalf("save = %+v, %v; want the divergence error", got, err)
			}
			if got != (overrunSave{}) {
				t.Errorf("save result = %+v on a refused push, want nothing reported as pushed", got)
			}
			if origin := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); origin == w.tip {
				t.Errorf("origin moved to the seat's tip: the refused push must not land")
			}
		})
	}
}

// A worktree mid-merge is refused before anything is committed, so the
// conflict is not recorded as work and the worktree is left as it is.
func TestOverrunSaveWork_MidMergeRefusesWithoutCommitting(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})
	if err := os.WriteFile(filepath.Join(w.work, ".git", "MERGE_HEAD"), []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := w.d.overrunSaveWork(stopSeat); err == nil {
		t.Fatal("save = nil, want a refusal for a worktree mid-merge")
	}
	if msgs := w.saveCommits(t, w.tip); len(msgs) != 0 {
		t.Errorf("commits above the tip = %q, want none", msgs)
	}
	if origin := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); origin == w.tip {
		t.Error("origin holds the branch: a refused save must not push")
	}
}

// A dirty worktree whose WIP commit cannot reach the kept branch is refused:
// a commit on a detached HEAD, or on another branch than the seat's, is not
// the branch's work.
func TestOverrunSaveWork_WIPCommitOffTheKeptBranchIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		branch string
	}{
		{name: "detached HEAD with the seat's branch named", branch: stopSeat.Branch},
		{name: "detached HEAD with no branch named", branch: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newResetWorld(t)
			if err := w.git().CheckoutDetach(w.tip); err != nil {
				t.Fatal(err)
			}
			writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})
			seat := stopSeat
			seat.Branch = tc.branch

			if _, err := w.d.overrunSaveWork(seat); err == nil {
				t.Fatal("save = nil, want a refusal: the WIP commit is not on the kept branch")
			}
			if origin := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); origin != "" {
				t.Errorf("origin holds %q for the branch, want none: nothing may be pushed", origin)
			}
		})
	}
}

// unreadableRefGit is the seat's git with one read failing: the current branch,
// or the head of the ref failRev names. Every other call goes to the fake.
type unreadableRefGit struct {
	daemonGit
	failCurrentBranch bool
	failRev           string
}

func (g unreadableRefGit) CurrentBranch() (string, error) {
	if g.failCurrentBranch {
		return "", errors.New("current branch unreadable")
	}
	return g.daemonGit.CurrentBranch()
}

func (g unreadableRefGit) Rev(ref string) (string, error) {
	if g.failRev != "" && ref == g.failRev {
		return "", errors.New("ref unreadable")
	}
	return g.daemonGit.Rev(ref)
}

// A branch the save cannot read is an error, never a clean worktree: the save
// would otherwise report nothing saved and the sandbox would go.
func TestOverrunSaveWork_UnreadableBranchIsAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seat string
		read unreadableRefGit
	}{
		{name: "current branch unreadable", seat: "", read: unreadableRefGit{failCurrentBranch: true}},
		{name: "kept branch head unreadable", seat: stopSeat.Branch, read: unreadableRefGit{failRev: "refs/heads/" + stopSeat.Branch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newResetWorld(t)
			open := w.d.openGitFn
			w.d.openGitFn = func(dir string) daemonGit {
				read := tc.read
				read.daemonGit = open(dir)
				return read
			}
			seat := stopSeat
			seat.Branch = tc.seat

			if _, err := w.d.overrunSaveWork(seat); err == nil {
				t.Fatal("save = nil, want the unreadable branch to be refused")
			}
		})
	}
}

// The stop saves the seat's work before it stops the session and before it
// removes the sandbox, so the pushed WIP commit is on origin when the seat
// stops, and the hand-back's comment names the pushed head.
func TestOverrunStop_SaveRunsBeforeTheSessionStopAndTheSandbox(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = func(seat overrunSeat) (overrunSave, error) {
		f.calls = append(f.calls, "save "+seat.Rig+"/"+seat.Polecat)
		return w.d.overrunSaveWork(seat)
	}
	stopSession := s.stopSession
	var onOriginAtStop string
	s.stopSession = func(seat overrunSeat) error {
		onOriginAtStop = w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch)
		return stopSession(seat)
	}

	w.d.runOverrunGuard(s)

	save := slices.Index(f.calls, "save gastown/capable")
	stop := slices.Index(f.calls, "stop gastown/capable")
	sandbox := slices.Index(f.calls, "sandbox gastown/capable")
	if save < 0 || stop < save || sandbox < stop {
		t.Fatalf("calls = %q, want save, then stop, then the sandbox removal", f.calls)
	}
	head, err := w.git().Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if onOriginAtStop != head {
		t.Errorf("origin held %q when the seat stopped, want the WIP commit %s pushed before the stop", onOriginAtStop, head)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], "WIP commit") || !strings.Contains(f.handBackNotes[0], head) {
		t.Errorf("hand-back note = %q, want it to name the pushed WIP commit %s", f.handBackNotes, head)
	}
}

// A refused save is noted in the hand-back and keeps the sandbox, but it does
// not hold the stop: the session is stopped and the bead handed back, and the
// seat stays paused so nothing reuses the worktree.
func TestOverrunStop_RefusedSaveKeepsTheSandboxAndHandsBack(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	w.divergeOriginBranch(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork

	w.d.runOverrunGuard(s)

	if !slices.Contains(f.stops, "gastown/capable") {
		t.Errorf("stops = %q, want the seat stopped: a refused save does not hold the stop", f.stops)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the bead handed back", f.handedBack)
	}
	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed before the save's result was known to be pushed", f.calls)
	}
	if slices.Contains(f.calls, "retire gastown/capable") {
		t.Errorf("calls = %q: the seat was released although its work was not saved", f.calls)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], "could not be saved before the stop") {
		t.Errorf("hand-back note = %q, want the refused save named", f.handBackNotes)
	}
}

// A clean, pushed seat leaves the stop's comment without a note about a save.
func TestOverrunStop_CleanPushedSeatAddsNoSaveNote(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	w.pushKeptBranch(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork

	w.d.runOverrunGuard(s)

	if len(f.handBackNotes) != 1 || f.handBackNotes[0] != "" {
		t.Errorf("hand-back note = %q, want none", f.handBackNotes)
	}
}

// The save runs once per stop: a stop whose session stop failed is retried on
// a later tick, and the work is not saved again.
func TestOverrunStop_SaveRunsOnceAcrossAFailedSessionStop(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"stop": errors.New("supervisor unavailable")}
	s := f.seams()
	saves := 0
	s.saveWork = func(seat overrunSeat) (overrunSave, error) {
		saves++
		return w.d.overrunSaveWork(seat)
	}

	w.d.runOverrunGuard(s)
	if len(f.handedBack) != 0 {
		t.Fatalf("setup: first tick handed back %q, want the stop to fail", f.handedBack)
	}
	f.failOn = nil
	w.d.runOverrunGuard(s)

	if saves != 1 {
		t.Errorf("save ran %d times across the stop's ticks, want once", saves)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the stop to finish on the retry", f.handedBack)
	}
}

// A worktree the save cannot resolve is a refused save, never a clean one: the
// save would otherwise report nothing to save and the sandbox would go.
func TestOverrunSaveWork_UnresolvableWorktreeIsAnError(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	if err := os.RemoveAll(w.work); err != nil {
		t.Fatal(err)
	}

	if _, err := w.d.overrunSaveWork(stopSeat); err == nil {
		t.Fatal("save = nil, want the unresolvable worktree refused")
	}
}

// A refused save of an unresolvable worktree keeps the sandbox, as any refused
// save does: the stop hands the bead back with the refusal named.
func TestOverrunStop_UnresolvableWorktreeKeepsTheSandbox(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	if err := os.RemoveAll(w.work); err != nil {
		t.Fatal(err)
	}

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork

	w.d.runOverrunGuard(s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though the worktree could not be saved", f.calls)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the bead handed back", f.handedBack)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], "could not be saved before the stop") {
		t.Errorf("hand-back note = %q, want the refused save named", f.handBackNotes)
	}
}

// The save runs before the session stops, so an edit made while the session is
// still stopping is not in it. The sandbox must not go with that edit only in
// the worktree: a worktree dirty again is saved before the sandbox is removed.
func TestOverrunStop_LateEditIsSavedBeforeTheSandboxGoes(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	var headAtSandbox, originAtSandbox string
	var cleanAtSandbox bool
	f.removeSandbox = func(overrunSeat) error {
		headAtSandbox, _ = w.git().Rev("HEAD")
		originAtSandbox = w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch)
		status, err := w.git().Status()
		if err != nil {
			t.Fatal(err)
		}
		cleanAtSandbox = status.Clean
		return nil
	}
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork
	stopSession := s.stopSession
	s.stopSession = func(seat overrunSeat) error {
		writeWorkFiles(t, w.work, map[string]string{"late.txt": "late\n"})
		return stopSession(seat)
	}

	w.d.runOverrunGuard(s)

	if !slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Fatalf("calls = %q, want the sandbox removed", f.calls)
	}
	if !cleanAtSandbox {
		t.Errorf("worktree was dirty when the sandbox went: the late edit would be lost")
	}
	if headAtSandbox == w.tip {
		t.Errorf("head at the sandbox removal is still the seat's tip, want the late edit saved as a commit")
	}
	if originAtSandbox != headAtSandbox {
		t.Errorf("origin held %q when the sandbox went, want the head %s pushed first", originAtSandbox, headAtSandbox)
	}
}

// A late edit the save cannot push holds the stop: the sandbox, the only copy
// of that edit, is kept and the bead is not handed back.
func TestOverrunStop_LateEditThatCannotBeSavedKeepsTheSandbox(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork
	stopSession := s.stopSession
	s.stopSession = func(seat overrunSeat) error {
		writeWorkFiles(t, w.work, map[string]string{"late.txt": "late\n"})
		w.divergeOriginBranch(t)
		return stopSession(seat)
	}

	w.d.runOverrunGuard(s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though the late edit was not saved", f.calls)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("hand-backs = %q, want the stop held with the sandbox kept", f.handedBack)
	}
}

// statusFailsOnSecondRead is the seat's git with its second status read failing.
// The first read is the save's (step 1b); the second is the late re-check's
// (step 2a), which must not read a dirty worktree as clean.
type statusFailsOnSecondRead struct {
	daemonGit
	reads *int
}

func (g statusFailsOnSecondRead) Status() (*git.GitStatus, error) {
	*g.reads++
	if *g.reads == 2 {
		return nil, errors.New("status unreadable")
	}
	return g.daemonGit.Status()
}

// A late re-check that cannot read the worktree's status holds the stop with the
// sandbox kept: a worktree that cannot be read is not known to be clean.
func TestOverrunStop_UnreadableLateStatusKeepsTheSandbox(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork
	reads := 0
	open := w.d.openGitFn
	w.d.openGitFn = func(dir string) daemonGit {
		return statusFailsOnSecondRead{daemonGit: open(dir), reads: &reads}
	}

	w.d.runOverrunGuard(s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though the worktree's status was unread", f.calls)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("hand-backs = %q, want the stop held with the sandbox kept", f.handedBack)
	}
}

// The late re-check of a worktree it cannot resolve is an error, never a clean
// answer that would let the sandbox go.
func TestOverrunWorktreeDirty_UnresolvableWorktreeIsAnError(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	if err := os.RemoveAll(w.work); err != nil {
		t.Fatal(err)
	}

	if _, err := w.d.overrunWorktreeDirty(stopSeat); err == nil {
		t.Fatal("dirty = nil error, want the unresolvable worktree refused")
	}
}
