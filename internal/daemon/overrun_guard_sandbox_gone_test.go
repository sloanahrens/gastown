package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
)

// The bead's stated gate is -run OverrunSandboxGone, and a test named for this
// rule carries that string so the gate selects it.

// overrunGoneClone is the clone directory the sandbox-gone fixture's record
// names: absolute, with no "..", so only the sandbox's answer can refuse it.
const overrunGoneClone = "/sandbox/gt/gastown/polecats/capable/gastown"

// overrunSandboxGoneFixture is a daemon whose seat runs in a sandbox that is
// gone: every in-sandbox git call answers session.ErrNothingToPush, as a
// sandbox deleted under the stop does. The calls are recorded, and the host
// worktree's opener fails the test if the stop opens it.
func overrunSandboxGoneFixture(t *testing.T) (*Daemon, *[]overrunSandboxGitCall) {
	t.Helper()
	d, _ := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	d.readSandboxRecord = func(townRoot, name string) (session.SandboxRecord, bool, error) {
		return *overrunStopSandboxRecord(overrunGoneClone), true, nil
	}
	var calls []overrunSandboxGitCall
	d.sandboxGitRunnerFn = func(townRoot string, rec session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		return overrunSandboxGitRecorder(&calls, nil, session.ErrNothingToPush), nil
	}
	d.openGitFn = func(dir string) daemonGit {
		t.Errorf("the stop opened the host worktree %q for a sandbox seat", dir)
		return nil
	}
	seedHostWorktreeStub(t, d)
	return d, &calls
}

// TestOverrunSandboxGoneErrorSaveIsReturnedNotSuccess: a sandbox that has gone
// is never a successful save. The checkpoint reads a failed status as a
// refusal, so the save asks the sandbox handle first and returns
// errSandboxGone, which the stop routes to the sandbox-gone rule.
func TestOverrunSandboxGoneErrorSaveIsReturnedNotSuccess(t *testing.T) {
	t.Parallel()
	d, calls := overrunSandboxGoneFixture(t)

	got, err := d.overrunSaveWork(stopSeat)

	if !errors.Is(err, errSandboxGone) {
		t.Fatalf("save = %+v, %v; want errSandboxGone: a vanished sandbox is not nothing to save", got, err)
	}
	if len(*calls) == 0 {
		t.Fatal("the save made no in-sandbox call, so it could not have learned the sandbox was gone")
	}
}

// TestOverrunSandboxGoneErrorDirtyCheckIsReturnedNotClean: the dirty check
// returns errSandboxGone, never a clean tree, when the sandbox handle reports
// the sandbox gone: the work may have gone with it.
func TestOverrunSandboxGoneErrorDirtyCheckIsReturnedNotClean(t *testing.T) {
	t.Parallel()
	d, _ := overrunSandboxGoneFixture(t)

	dirty, err := d.overrunWorktreeDirty(stopSeat)

	if !errors.Is(err, errSandboxGone) {
		t.Fatalf("dirty check = %v, %v; want errSandboxGone: a vanished sandbox is not clean", dirty, err)
	}
	if dirty {
		t.Error("a vanished sandbox was read as dirty, want the error alone")
	}
}

// TestOverrunSandboxGoneErrorStopKeepsTheSandboxAndHoldsTheBead: until the
// sandbox-gone rule lands, the stop treats errSandboxGone as a refused save.
// It holds the stop with the sandbox in place and does not hand the bead back.
func TestOverrunSandboxGoneErrorStopKeepsTheSandboxAndHoldsTheBead(t *testing.T) {
	t.Parallel()
	d, _ := overrunSandboxGoneFixture(t)

	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = func(overrunSeat) error { return nil }
	s := f.seams()
	s.saveWork = d.overrunSaveWork

	d.runOverrunGuard(s)

	if len(f.stops) != 0 || len(f.handedBack) != 0 || slices.Contains(f.calls, "sandbox gastown/capable") {
		t.Fatalf("stops %q, hand-backs %q, calls %q; want the refused save held with the sandbox kept",
			f.stops, f.handedBack, f.calls)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatal(err)
	}
	if !st.SavedAt.IsZero() || st.SaveFailures != 1 {
		t.Errorf("state = SavedAt %v, SaveFailures %d; want the save unset and the refusal counted",
			st.SavedAt, st.SaveFailures)
	}
	if !strings.Contains(st.SaveFailure, "sandbox is gone") {
		t.Errorf("SaveFailure = %q, want it to name the vanished sandbox", st.SaveFailure)
	}
}

// TestOverrunSandboxGoneErrorBranchNotABranchNameIsRefusedFirst: the in-sandbox
// runner cannot tell a branch from another argument, so a branch that fails the
// rule session.validCloneBranch applies is refused before any git call reaches
// the sandbox.
func TestOverrunSandboxGoneErrorBranchNotABranchNameIsRefusedFirst(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{
		"--upload-pack=rsh",
		"polecat/capable/gt-aaa11..x1",
		"polecat/capable/gt-aaa11 x1",
		"/polecat/capable/gt-aaa11+x1",
		"polecat/capable/gt-aaa11\nx1",
	} {
		t.Run(branch, func(t *testing.T) {
			t.Parallel()
			d, calls := overrunSandboxGoneFixture(t)
			seat := stopSeat
			seat.Branch = branch

			_, err := d.overrunSaveWork(seat)

			if err == nil {
				t.Fatal("save = nil, want the branch refused")
			}
			if errors.Is(err, errSandboxGone) {
				t.Errorf("save = %v; a branch name git cannot take is not the sandbox going away", err)
			}
			if !strings.Contains(err.Error(), "not a branch name") {
				t.Errorf("error %q does not say the branch is not a branch name", err)
			}
			if len(*calls) != 0 {
				t.Errorf("in-sandbox calls = %v, want none before the branch is refused", *calls)
			}
		})
	}
}

// The three answers an origin read can give part ways in originBranchRead, and
// originBranchSHA keeps reading the unanswerable one as an absent branch for the
// callers that only ask what origin holds (gt-lahjz4).
func TestOverrunSandboxGoneOriginBranchReadSplitsTheThreeAnswers(t *testing.T) {
	t.Parallel()
	ref := "refs/heads/" + stopSeat.Branch
	readErr := errors.New("could not resolve host: origin")
	tests := []struct {
		name    string
		refs    []git.RemoteRef
		err     error
		wantSHA string
		wantErr error
	}{
		{name: "origin_holds_the_branch", refs: []git.RemoteRef{{Name: ref, Hash: "aaa111"}}, wantSHA: "aaa111"},
		{name: "origin_has_no_such_branch", refs: []git.RemoteRef{{Name: "refs/heads/main", Hash: "bbb222"}}},
		{name: "origin_cannot_be_asked", err: readErr, wantErr: readErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &pushStubGit{originRefs: tt.refs, originErr: tt.err}

			sha, err := originBranchRead(g, stopSeat.Branch)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("originBranchRead error = %v, want %v", err, tt.wantErr)
			}
			if sha != tt.wantSHA {
				t.Errorf("originBranchRead = %q, want %q", sha, tt.wantSHA)
			}
			// originBranchSHA answers the callers that cannot act on the error:
			// a read that failed is an empty answer there, as before.
			if got := originBranchSHA(g, stopSeat.Branch); got != tt.wantSHA {
				t.Errorf("originBranchSHA = %q, want %q", got, tt.wantSHA)
			}
		})
	}
}

// overrunGoneSandboxStop is the stop's sandbox step over d: the pusher answers
// wantPush, and the deleter records that it ran.
func overrunGoneSandboxStop(d *Daemon, wantPush error, deleted *int) func(overrunSeat) error {
	return func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat,
			func(string, session.SandboxRecord) error { return wantPush },
			func(string, session.SandboxRecord) error {
				*deleted++
				return nil
			})
	}
}

// A gone sandbox whose origin read fails is the case the stop must not answer on
// its own: the seat's commits may be nowhere. The stop logs the WARNING with the
// error, raises the escalation the attention queue shows the overseer, and holds
// the bead, keeping the sandbox's record so the next tick retries (gt-lahjz4).
func TestOverrunSandboxGoneOriginReadFailureKeepsTheBead(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	seedHostWorktreeStub(t, d)
	readErr := errors.New("could not resolve host: origin")
	d.openGitFn = func(string) daemonGit { return &pushStubGit{originErr: readErr} }
	rec := notifyfake.New()
	d.notifier = rec

	deleted := 0
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = overrunGoneSandboxStop(d, session.ErrNothingToPush, &deleted)

	d.runOverrunGuard(f.seams())

	if deleted != 0 {
		t.Errorf("the sandbox record was removed though origin could not be read")
	}
	if len(f.handedBack) != 0 {
		t.Errorf("the bead was handed back over an unreadable origin: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "WARNING") || !strings.Contains(buf.String(), readErr.Error()) {
		t.Errorf("the log does not warn with the origin read's error:\n%s", buf.String())
	}
	raised := rec.Escalations()
	if len(raised) != 1 {
		t.Fatalf("escalations = %+v, want one for the seat whose origin could not be read", raised)
	}
	if !strings.Contains(raised[0].Escalation.Reason, stopSeat.Bead) || !strings.Contains(raised[0].Escalation.Reason, stopSeat.Branch) {
		t.Errorf("escalation reason %q does not name the bead %s and its branch %s",
			raised[0].Escalation.Reason, stopSeat.Bead, stopSeat.Branch)
	}
	if _, ok, err := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); !ok || err != nil {
		t.Errorf("the sandbox record is gone (ok %v, err %v); the stop needs it to retry", ok, err)
	}
	st, err := readOverrunState(d.config.TownRoot, stopSeat.Bead)
	if err != nil {
		t.Fatal(err)
	}
	if st.StopBeganAt.IsZero() || st.SessionStoppedAt.IsZero() || !st.StoppedAt.IsZero() {
		t.Errorf("state %+v: want the stop still under way with the bead still the seat's", st)
	}
}

// A gone sandbox whose branch origin holds is nothing to alarm about: the stop
// removes the sandbox's record and hands the bead back exactly as it did before
// the origin read existed (gt-lahjz4).
func TestOverrunSandboxGoneOriginHeldProceedsAsBefore(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	seedHostWorktreeStub(t, d)
	d.openGitFn = func(string) daemonGit {
		return &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + stopSeat.Branch, Hash: "aaa111"}}}
	}
	rec := notifyfake.New()
	d.notifier = rec

	deleted := 0
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = overrunGoneSandboxStop(d, session.ErrNothingToPush, &deleted)

	d.runOverrunGuard(f.seams())

	if deleted != 1 {
		t.Errorf("the sandbox record was removed %d times, want once: origin holds the branch", deleted)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the bead handed back once", f.handedBack)
	}
	if raised := rec.Escalations(); len(raised) != 0 {
		t.Errorf("escalations = %+v, want none: origin holds the branch", raised)
	}
	if strings.Contains(buf.String(), "WARNING") {
		t.Errorf("the stop warned about a branch origin holds:\n%s", buf.String())
	}
}

// A gone sandbox whose branch origin does not hold is the case the stop can
// neither see nor undo: the sandbox is gone and origin has no copy of the work.
// The stop logs the WARNING naming the seat, the branch and origin's answer,
// raises the escalation the attention queue shows the overseer, then removes the
// record and hands the bead back once. It never says the push was done, because
// the guard cannot see that it was (gt-mp9zrg).
func TestOverrunSandboxGoneOriginBranchAbsentWarnsRaisesAndHandsBackOnce(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	seedHostWorktreeStub(t, d)
	// Origin answers, and holds another branch but not the seat's.
	d.openGitFn = func(string) daemonGit {
		return &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/main", Hash: "bbb222"}}}
	}
	notified := notifyfake.New()
	d.notifier = notified

	deleted := 0
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = overrunGoneSandboxStop(d, session.ErrNothingToPush, &deleted)

	d.runOverrunGuard(f.seams())

	if deleted != 1 {
		t.Errorf("the sandbox record was removed %d times, want once: origin answered that it has no such branch", deleted)
	}
	if len(f.handedBack) != 1 {
		t.Errorf("hand-backs = %q, want the bead handed back once", f.handedBack)
	}
	log := buf.String()
	for _, want := range []string{
		"WARNING",
		stopSeat.Rig + "/" + stopSeat.Polecat,
		stopSeat.Branch,
		"origin holds no branch " + stopSeat.Branch,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not carry %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "treating the push as done") {
		t.Errorf("the log treats the push as done on a path where the guard cannot see it:\n%s", log)
	}
	raised := notified.Escalations()
	if len(raised) != 1 {
		t.Fatalf("escalations = %+v, want one for the branch origin lacks", raised)
	}
	reason := raised[0].Escalation.Reason
	if !strings.Contains(reason, stopSeat.Bead) || !strings.Contains(reason, stopSeat.Branch) {
		t.Errorf("escalation reason %q does not name the bead %s and its branch %s",
			reason, stopSeat.Bead, stopSeat.Branch)
	}
	if _, ok, err := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); ok || err != nil {
		t.Errorf("the sandbox record survived the stop (ok %v, err %v); the bead was handed back", ok, err)
	}
}

// A sandbox whose record names no branch is an unknown answer too: the stop
// cannot ask origin what it holds for the seat, so it keeps the record and holds
// the bead rather than reading silence as an absent branch (gt-lahjz4).
func TestOverrunSandboxGoneOriginReadWithNoBranchKeepsTheBead(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	// The seat carries no branch and its record names none either.
	rec := session.SandboxRecord{Session: overrunStopSessionName, Rig: stopSeat.Rig, Polecat: stopSeat.Polecat}
	if err := session.WriteSandboxRecord(d.config.TownRoot, rec); err != nil {
		t.Fatal(err)
	}
	seedHostWorktreeStub(t, d)
	d.openGitFn = func(string) daemonGit {
		t.Error("the stop read origin for a seat whose branch nobody names")
		return &pushStubGit{}
	}
	notified := notifyfake.New()
	d.notifier = notified
	seat := stopSeat
	seat.Branch = ""

	deleted := 0
	f := newOverrunFake(20*time.Minute, seat)
	f.removeSandbox = overrunGoneSandboxStop(d, session.ErrNothingToPush, &deleted)

	d.runOverrunGuard(f.seams())

	if deleted != 0 || len(f.handedBack) != 0 {
		t.Errorf("deleted %d, hand-backs %q; want the bead held with its record kept", deleted, f.handedBack)
	}
	if !strings.Contains(buf.String(), "names no branch") {
		t.Errorf("the log does not say the stop had no branch to ask origin about:\n%s", buf.String())
	}
	if raised := notified.Escalations(); len(raised) != 1 {
		t.Errorf("escalations = %+v, want one for the unknown answer", raised)
	}
}

// A seat whose worktree does not resolve leaves the stop with nowhere to ask
// origin from, which is the same unknown answer as a read that fails: the stop
// warns, raises the item and keeps the record instead of reading silence as an
// absent branch (gt-lahjz4).
func TestOverrunSandboxGoneOriginUnresolvedWorktreeKeepsTheBead(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	// No host worktree is seeded under the town root, so the seat's worktree
	// does not resolve and the stop has no directory to read origin from.
	d.openGitFn = func(dir string) daemonGit {
		t.Errorf("the stop read origin from %q for a seat whose worktree did not resolve", dir)
		return &pushStubGit{}
	}
	notified := notifyfake.New()
	d.notifier = notified

	deleted := 0
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = overrunGoneSandboxStop(d, session.ErrNothingToPush, &deleted)

	d.runOverrunGuard(f.seams())

	if deleted != 0 {
		t.Errorf("the sandbox record was removed for a seat the stop could not place")
	}
	if len(f.handedBack) != 0 {
		t.Errorf("the bead was handed back with no origin read: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "could not be read for branch "+stopSeat.Branch) {
		t.Errorf("the log does not name the branch the stop could not read origin for:\n%s", buf.String())
	}
	if raised := notified.Escalations(); len(raised) != 1 {
		t.Errorf("escalations = %+v, want one for the unknown answer", raised)
	}
	if _, ok, err := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); !ok || err != nil {
		t.Errorf("the sandbox record is gone (ok %v, err %v); the stop needs it to retry", ok, err)
	}
}

// The origin read's error is a git error, which carries the command's captured
// output. The stop's WARNING, its escalation and the error it returns each get
// one bounded line of it, never the whole capture (gt-lahjz4).
func TestOverrunSandboxGoneOriginReadErrorIsBounded(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seedStopSandbox(t, d)
	seedHostWorktreeStub(t, d)
	const tail = "captured-output-tail"
	readErr := errors.New("git ls-remote failed\n" + strings.Repeat("x", 400) + "\n" + tail)
	d.openGitFn = func(string) daemonGit { return &pushStubGit{originErr: readErr} }
	notified := notifyfake.New()
	d.notifier = notified

	deleted := 0
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.removeSandbox = overrunGoneSandboxStop(d, session.ErrNothingToPush, &deleted)

	d.runOverrunGuard(f.seams())

	if !strings.Contains(buf.String(), "git ls-remote failed") {
		t.Errorf("the log does not name the read that failed:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), tail) {
		t.Errorf("the log carries the whole capture; the reason must be bounded:\n%s", buf.String())
	}
	raised := notified.Escalations()
	if len(raised) != 1 {
		t.Fatalf("escalations = %+v, want one for the unknown answer", raised)
	}
	reason := raised[0].Escalation.Reason
	if !strings.Contains(reason, "git ls-remote failed") {
		t.Errorf("escalation reason %q does not name the read that failed", reason)
	}
	if strings.Contains(reason, tail) {
		t.Errorf("escalation reason carries the whole capture; it must be bounded: %q", reason)
	}
}
