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

// A CLEAN worktree whose HEAD is off the kept branch is saved only when the
// branch already holds HEAD's commits. With the branch equal to origin, comparing
// the branch ref alone would call the seat saved while commits reachable only
// from HEAD are lost with the sandbox.
func TestOverrunSaveWork_CleanDetachedHeadIsSavedOnlyIfTheBranchHoldsIt(t *testing.T) {
	t.Parallel()
	t.Run("commits only HEAD holds are refused", func(t *testing.T) {
		t.Parallel()
		w := newResetWorld(t)
		w.pushKeptBranch(t)
		if err := w.git().CheckoutDetach(w.tip); err != nil {
			t.Fatal(err)
		}
		writeWorkFiles(t, w.work, map[string]string{"only-head.txt": "only head\n"})
		if w.d.checkpointWorktreeAs(w.work, stopSeat.Rig, stopSeat.Polecat, "work on a detached head") != checkpointCreated {
			t.Fatal("setup: the commit on the detached HEAD was not created")
		}

		got, err := w.d.overrunSaveWork(stopSeat)
		if err == nil {
			t.Fatalf("save = %+v, nil; want a refusal: HEAD holds a commit the kept branch lacks", got)
		}
		if got != (overrunSave{}) {
			t.Errorf("save result = %+v on a refusal, want nothing reported as saved", got)
		}
	})
	t.Run("a HEAD the branch already holds is saved", func(t *testing.T) {
		t.Parallel()
		w := newResetWorld(t)
		w.pushKeptBranch(t)
		if err := w.git().CheckoutDetach(w.baseSHA); err != nil {
			t.Fatal(err)
		}

		got, err := w.d.overrunSaveWork(stopSeat)
		if err != nil || got != (overrunSave{}) {
			t.Errorf("save = %+v, %v; want nothing to save: the branch holds HEAD", got, err)
		}
	})
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
		{name: "HEAD unreadable on a clean worktree", seat: stopSeat.Branch, read: unreadableRefGit{failRev: "HEAD"}},
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

// tickUntilSaveCap runs the guard for the ticks a refusing save holds the stop
// before its cap, asserting the bead is not handed back meanwhile, then runs the
// cap tick. A refusal that never clears parks the seat on that tick.
func tickUntilSaveCap(t *testing.T, w *resetWorld, f *overrunFake, s overrunSeams) {
	t.Helper()
	for tick := 1; tick < overrunSaveMaxAttempts; tick++ {
		w.d.runOverrunGuard(s)
		if len(f.handedBack) != 0 {
			t.Fatalf("tick %d of %d: hand-backs %q; want the stop held until the cap",
				tick, overrunSaveMaxAttempts, f.handedBack)
		}
	}
	w.d.runOverrunGuard(s)
}

// A refused save holds the stop: nothing is stopped and the bead is not handed
// back, and the next tick saves again. When the refusal clears the save lands,
// the stop finishes, and the refusal is no longer recorded.
func TestOverrunStop_RefusedSaveIsRetriedOnTheNextTick(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	w.divergeOriginBranch(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork

	w.d.runOverrunGuard(s)

	if len(f.stops) != 0 || len(f.handedBack) != 0 || slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Fatalf("first tick: stops %q, hand-backs %q, calls %q; want the refused stop held",
			f.stops, f.handedBack, f.calls)
	}
	st, err := readOverrunState(w.d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if !st.SavedAt.IsZero() || st.SaveFailure == "" || st.SaveFailures != 1 {
		t.Errorf("state after the refusal = SavedAt %v, SaveFailure %q, SaveFailures %d; want unset, the reason, and 1",
			st.SavedAt, st.SaveFailure, st.SaveFailures)
	}

	// The refusal clears: origin's branch no longer holds a commit the seat's
	// tip lacks, so the next tick's push goes through.
	w.f.SetRef(t, w.origin, "refs/heads/"+stopSeat.Branch, w.baseSHA)
	w.d.runOverrunGuard(s)

	head, err := w.git().Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.f.Ref(w.origin, "refs/heads/"+stopSeat.Branch); got != head {
		t.Errorf("origin holds %q for the branch, want the saved head %s", got, head)
	}
	if !slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q, want the sandbox removed once the save landed", f.calls)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the stop to finish on the retry", f.handedBack)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], head) {
		t.Errorf("hand-back note = %q, want it to name the saved head %s", f.handBackNotes, head)
	}
	st, err = readOverrunState(w.d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if st.SaveFailure != "" || st.SaveFailures != 0 || st.SaveGaveUp {
		t.Errorf("state after the save landed = SaveFailure %q, SaveFailures %d, SaveGaveUp %v; want all cleared",
			st.SaveFailure, st.SaveFailures, st.SaveGaveUp)
	}
}

// A refusal that never clears parks the seat once, on the cap tick: the seat is
// stopped, the sandbox is kept, and the hand-back names the reason. Later ticks
// add no hand-back.
func TestOverrunStop_PermanentlyRefusedSaveParksAfterTheCap(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)
	w.divergeOriginBranch(t)
	writeWorkFiles(t, w.work, map[string]string{"more.txt": "more\n"})

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork

	tickUntilSaveCap(t, w, f, s)

	if !slices.Contains(f.stops, "gastown/capable") {
		t.Errorf("stops = %q, want the seat stopped on the cap tick", f.stops)
	}
	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though the work was never saved", f.calls)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], "could not be saved before the stop") {
		t.Fatalf("hand-back note = %q, want the refused save named", f.handBackNotes)
	}
	st, err := readOverrunState(w.d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if !st.SaveGaveUp || st.SaveFailure == "" || !strings.Contains(f.handBackNotes[0], st.SaveFailure) {
		t.Errorf("state = SaveGaveUp %v, SaveFailure %q; want the park recorded with the reason named in the note %q",
			st.SaveGaveUp, st.SaveFailure, f.handBackNotes[0])
	}

	w.d.runOverrunGuard(s)
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q after a later tick, want the seat parked once", f.handedBack)
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
// save does: the stop is held until the cap, then hands the bead back with the
// refusal named.
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

	tickUntilSaveCap(t, w, f, s)

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

// A late edit the re-save keeps refusing holds the stop until the cap, then parks
// the seat with the sandbox kept. The refused WIP commit stays in the worktree
// with its push undone, and the worktree then reads clean: the re-save is still
// tried on each tick, so the sandbox never goes on that unpushed commit.
func TestOverrunStop_LateEditRefusedForeverParksAfterTheCap(t *testing.T) {
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

	tickUntilSaveCap(t, w, f, s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed with the late edit's commit unpushed", f.calls)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], "could not be saved before the stop") {
		t.Errorf("hand-back note = %q, want the refused late save named", f.handBackNotes)
	}
}

// The seat's worktree in production is a linked worktree of the rig's clone: its
// .git is a file naming the clone's worktrees directory, not a .git directory of
// its own. The save commits and pushes that shape the same way.
func TestOverrunSaveWork_LinkedWorktreeIsSavedAndPushed(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := useGitfake(t, d)
	origin := filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, origin)
	base := f.Commit(t, origin, "main", "initial", map[string]string{"a.txt": "a\n"})
	clone := filepath.Join(t.TempDir(), "clone")
	f.Clone(t, origin, clone)
	work := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	if err := f.OpenBranchRepo(clone).WorktreeAddFromRef(work, stopSeat.Branch, base); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(work, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("setup: .git in the linked worktree is not a file (%v, %v)", info, err)
	}
	writeWorkFiles(t, work, map[string]string{"work.txt": "work\n"})
	w := &resetWorld{d: d, f: f, origin: origin, work: work, baseSHA: base}

	if _, err := d.overrunSaveWork(stopSeat); err != nil {
		t.Fatalf("save = %v, want the linked worktree saved", err)
	}

	head, err := w.git().Rev("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.f.Ref(origin, "refs/heads/"+stopSeat.Branch); got != head {
		t.Errorf("origin holds %q for the branch, want the WIP commit %s", got, head)
	}
	msgs := w.saveCommits(t, base)
	if len(msgs) != 1 || msgs[0] != overrunSaveMessage(stopSeat.Bead) {
		t.Errorf("commits above main = %q, want one WIP commit named for the bead", msgs)
	}
}

// statusUnreadableAfterStop is the seat's git with its status unreadable once
// the session has stopped: the late re-check (step 2a) reads the worktree then,
// and must not read an unreadable status as clean.
type statusUnreadableAfterStop struct {
	daemonGit
	stopped *bool
}

func (g statusUnreadableAfterStop) Status() (*git.GitStatus, error) {
	if *g.stopped {
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
	stopped := false
	open := w.d.openGitFn
	w.d.openGitFn = func(dir string) daemonGit {
		return statusUnreadableAfterStop{daemonGit: open(dir), stopped: &stopped}
	}
	stopSession := s.stopSession
	s.stopSession = func(seat overrunSeat) error {
		if err := stopSession(seat); err != nil {
			return err
		}
		stopped = true
		return nil
	}

	w.d.runOverrunGuard(s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though the worktree's status was unread", f.calls)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("hand-backs = %q, want the stop held with the sandbox kept", f.handedBack)
	}
}

// A late check that keeps failing counts toward the save cap: the stop holds
// with the sandbox kept on each tick before the cap, then parks the seat once.
// The note names the reason and says the check was a late one, not one before
// the stop.
func TestOverrunStop_LateDirtyCheckThatKeepsFailingParksAfterTheCap(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork
	stopped := false
	open := w.d.openGitFn
	w.d.openGitFn = func(dir string) daemonGit {
		return statusUnreadableAfterStop{daemonGit: open(dir), stopped: &stopped}
	}
	stopSession := s.stopSession
	s.stopSession = func(seat overrunSeat) error {
		if err := stopSession(seat); err != nil {
			return err
		}
		stopped = true
		return nil
	}

	tickUntilSaveCap(t, w, f, s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though the worktree was never read", f.calls)
	}
	if len(f.handBackNotes) != 1 {
		t.Fatalf("hand-back notes = %q, want the seat parked with one note", f.handBackNotes)
	}
	note := f.handBackNotes[0]
	if !strings.Contains(note, "late check") || !strings.Contains(note, "status unreadable") {
		t.Errorf("hand-back note = %q, want the late check and its reason named", note)
	}
	if strings.Contains(note, "could not be saved before the stop") {
		t.Errorf("hand-back note = %q, want the late check named, not a refused save before the stop", note)
	}

	w.d.runOverrunGuard(s)
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q after a later tick, want the seat parked once", f.handedBack)
	}
}

// A seat whose worktree cannot be resolved at the late check is refused, not
// read as clean. It counts toward the same cap, and at the cap is parked with
// the reason in the hand-back note. This is the decision for such a seat: its
// work cannot be seen, so the sandbox that may hold it is kept.
func TestOverrunStop_LateUnresolvableWorktreeParksWithItsReasonNoted(t *testing.T) {
	t.Parallel()
	w := newResetWorld(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	// The session stop fails on the first tick, so the save (1b) runs while the
	// worktree is still there and the late check (2a) does not run yet.
	f.failOn = map[string]error{"stop": errors.New("supervisor unavailable")}
	s := f.seams()
	s.saveWork = w.d.overrunSaveWork

	w.d.runOverrunGuard(s)
	if len(f.handedBack) != 0 || slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Fatalf("setup: first tick handed back %q, calls %q; want the stop held before the late check", f.handedBack, f.calls)
	}
	if err := os.RemoveAll(w.work); err != nil {
		t.Fatal(err)
	}
	f.failOn = nil

	tickUntilSaveCap(t, w, f, s)

	if slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Errorf("calls = %q: the sandbox was removed though its worktree could not be seen", f.calls)
	}
	if len(f.handBackNotes) != 1 {
		t.Fatalf("hand-back notes = %q, want the seat parked with one note", f.handBackNotes)
	}
	note := f.handBackNotes[0]
	if !strings.Contains(note, "late check") || !strings.Contains(note, "cannot be resolved") {
		t.Errorf("hand-back note = %q, want the late check and the unresolvable worktree named", note)
	}
	if strings.Contains(note, "could not be saved before the stop") {
		t.Errorf("hand-back note = %q, want the late check named, not a refused save before the stop", note)
	}

	w.d.runOverrunGuard(s)
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q after a later tick, want the seat parked once", f.handedBack)
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
