package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// The test names in this file carry OverrunSandboxGoneError so the bead's
// stated gate (-run OverrunSandboxGoneError) selects them.

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
