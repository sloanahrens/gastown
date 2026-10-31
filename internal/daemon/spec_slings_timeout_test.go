package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A sling killed at its own bound is the host's load reaching the sling, not a
// defect in its bead: the daemon labels it as any failed sling, records the
// kill as an attempt in the bead's metadata and marks it with the timeout
// label, which the dispatcher reads to clear the label and sling the bead again
// while retries remain (gt-vaivp).
func TestASlingKilledAtItsBoundRecordsTheAttempt(t *testing.T) {
	t.Parallel()
	fake, d, lines := boundKilledSling(t)
	bd := newWorkBD(t)
	bd.seed("gt-z", string(beads.StatusOpen), time.Now())
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	want := []string{"bead", "update", "gt-z",
		"--add-label", "spec-dispatch-failed",
		"--add-label", specdispatch.TimeoutLabel,
		"--set-metadata", "spec_dispatch_timeout_attempts=1"}
	if got := fake.argvs("bead", "update"); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("bead updates = %v, want %v", got, want)
	}
	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one recording the kill", notes)
	}
	note := notes[0][len(notes[0])-1]
	if !strings.Contains(note, "attempt 1 of 3") {
		t.Errorf("note = %q, want it naming attempt 1 of 3", note)
	}
	if !strings.Contains(note, "the dispatcher clears this label") {
		t.Errorf("note = %q, want it to say the dispatcher clears the label while retries remain", note)
	}
	if n := countContaining(lines(), "gt-z: sling process failed"); n != 1 {
		t.Errorf("failure lines = %d, want one: %v", n, lines())
	}
}

// Each kill counts the ones before it from the bead's metadata, so the kill
// that spends the last retry says so and leaves the label for a person
// (gt-vaivp).
func TestASlingKilledAtItsBoundCountsTheAttemptsBeforeIt(t *testing.T) {
	t.Parallel()
	fake, d, _ := boundKilledSling(t)
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.StatusOpen),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Labels:    []string{specdispatch.DispatchFailedLabel, specdispatch.TimeoutLabel},
		Metadata:  []byte(`{"spec_dispatch_timeout_attempts":"2"}`),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one recording the last attempt", notes)
	}
	note := notes[0][len(notes[0])-1]
	if !strings.Contains(note, "attempt 3 of 3") || !strings.Contains(note, "the retries are spent") {
		t.Errorf("note = %q, want attempt 3 of 3 and the retries spent", note)
	}
}

// A failure that is not a kill at the bound drops the timeout label and the
// count a kill may have left: the dispatcher would otherwise sling that bead
// again on the kill's retries (gt-vaivp).
func TestAFailureOfItsOwnDropsTheTimeoutRetry(t *testing.T) {
	t.Parallel()
	fake, d, _ := failedSling(t)
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.StatusOpen),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Labels:    []string{specdispatch.TimeoutLabel},
		Metadata:  []byte(`{"spec_dispatch_timeout_attempts":"1"}`),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	want := []string{"bead", "update", "gt-z",
		"--add-label", "spec-dispatch-failed",
		"--remove-label", specdispatch.TimeoutLabel,
		"--unset-metadata", specdispatch.TimeoutAttemptsKey}
	if got := fake.argvs("bead", "update"); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("bead updates = %v, want %v", got, want)
	}
	if got := fake.argvs("bead", "note", "gt-z"); len(got) != 1 {
		t.Errorf("notes = %v, want one recording the failure", got)
	}
}

// A sling killed after it hooked its bead but before its polecat's session
// started leaves a hook nothing works: the daemon hands the bead back from that
// polecat, so the retry slings it rather than leaving it hooked and idle
// (gt-vaivp).
func TestASlingKilledAtItsBoundHandsBackAHalfHookedBead(t *testing.T) {
	t.Parallel()
	fake, d, _ := boundKilledSling(t)
	d.tmux = newFakeTmux(clockwork.NewRealClock())
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.IssueStatusHooked),
		Assignee:  "gastown/polecats/p",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("unsling"); len(got) != 1 || !slices.Equal(got[0], []string{"unsling", "gt-z", "gastown/polecats/p"}) {
		t.Errorf("unsling calls = %v, want gt-z handed back from its sessionless polecat once", got)
	}
	if got := fake.argvs("bead", "note", "gt-z"); len(got) != 1 {
		t.Errorf("notes = %v, want the kill recorded on the handed-back bead", got)
	}
}

// A polecat that has a session owns the bead it was hooked: unslinging it would
// take work away from an agent mid-task, so the daemon leaves it (gt-nosod).
func TestASlingKilledAtItsBoundLeavesABeadItsPolecatIsWorking(t *testing.T) {
	t.Parallel()
	fake, d, _ := boundKilledSling(t)
	tm := newFakeTmux(clockwork.NewRealClock())
	tm.addSession("gt-p", "", time.Now())
	d.tmux = tm
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.IssueStatusHooked),
		Assignee:  "gastown/polecats/p",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("unsling"); len(got) != 0 {
		t.Errorf("unsling calls = %v, want the bead left to its running polecat", got)
	}
	if got := fake.argvs("bead", "update"); len(got) != 0 {
		t.Errorf("bead updates = %v, want the bead left to its running polecat", got)
	}
	if got := fake.argvs("bead", "note", "gt-z"); len(got) != 0 {
		t.Errorf("notes = %v, want none for a bead its live polecat owns: nothing is half-done", got)
	}
}

// A failure that is not a kill at the bound leaves the hook alone: the bound is
// what makes a half-made sling certain, and unslinging on every failure takes a
// bead away from a sling that may still be working (gt-vaivp).
func TestAFailureOfItsOwnLeavesAHookedBeadHooked(t *testing.T) {
	t.Parallel()
	fake, d, _ := failedSling(t)
	d.tmux = newFakeTmux(clockwork.NewRealClock())
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.IssueStatusHooked),
		Assignee:  "gastown/polecats/p",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("unsling"); len(got) != 0 {
		t.Errorf("unsling calls = %v, want no hand-back for a failure that is not the bound", got)
	}
	if got := fake.argvs("bead", "update"); len(got) != 0 {
		t.Errorf("bead updates = %v, want the bead left hooked to its polecat", got)
	}
	if got := fake.argvs("bead", "note", "gt-z"); len(got) != 0 {
		t.Errorf("notes = %v, want none for a failure that is not the bound", got)
	}
}

// A daemon with no tmux cannot tell a dead seat from a busy one, so the bead a
// killed sling hooked is not unslung: an unanswered session read is no proof
// the polecat is gone. The hook may still be a dead one, so the bead carries a
// note saying so and how to clear it, and the label is left off because that
// polecat may own the bead (gt-nosod, gt-vaivp).
func TestASlingKilledAtItsBoundLeavesAHookedBeadNoted(t *testing.T) {
	t.Parallel()
	fake, d, lines := boundKilledSling(t)
	d.tmux = nil
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.IssueStatusHooked),
		Assignee:  "gastown/polecats/p",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("unsling"); len(got) != 0 {
		t.Errorf("unsling calls = %v, want no hand-back without a tmux to prove the seat dead", got)
	}
	if got := fake.argvs("bead", "update"); len(got) != 0 {
		t.Errorf("bead updates = %v, want no label on a bead whose holder may own it", got)
	}
	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one recording the unconfirmed hand-back", notes)
	}
	note := notes[0][len(notes[0])-1]
	if !strings.Contains(note, "could not be handed back") {
		t.Errorf("note = %q, want it to say the hand-back was not confirmed", note)
	}
	if !strings.Contains(note, "gt unsling gt-z gastown/polecats/p") {
		t.Errorf("note = %q, want it to name the release a person can make", note)
	}
	if n := countContaining(lines(), "cannot tell whether gastown/polecats/p has a session"); n != 1 {
		t.Errorf("unanswered-session lines = %d, want one: %v", n, lines())
	}
}

// A hand-back that fails leaves the bead hooked and says so once: the bead is
// still on a hook nothing works, but the label is the one write only an
// operator clears, and a polecat the daemon could not unsling may still own it
// (gt-nosod, gt-vaivp). The note is the record that the hook is half-done.
func TestASlingKilledAtItsBoundNotesAFailedHandBack(t *testing.T) {
	t.Parallel()
	fake, d, lines := daemonWhoseSlingFailed(t, slingFailure{
		took:       specSlingBound(),
		unslingErr: errors.New("exit status 1"),
	})
	d.tmux = newFakeTmux(clockwork.NewRealClock())
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.IssueStatusHooked),
		Assignee:  "gastown/polecats/p",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("unsling"); len(got) != 1 {
		t.Errorf("unsling calls = %v, want the hand-back tried once", got)
	}
	if n := countContaining(lines(), "handing the bead back from gastown/polecats/p failed"); n != 1 {
		t.Errorf("hand-back failure lines = %d, want one: %v", n, lines())
	}
	if got := fake.argvs("bead", "update"); len(got) != 0 {
		t.Errorf("bead updates = %v, want no label on a bead whose hand-back failed", got)
	}
	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one recording the refused hand-back", notes)
	}
	note := notes[0][len(notes[0])-1]
	if !strings.Contains(note, "gt unsling refused it") {
		t.Errorf("note = %q, want it to say the release was refused", note)
	}
}

// An assignee that is not a polecat's seat address names nothing the daemon can
// unsling, so the bead is left to a person and the note says why (gt-vaivp).
func TestASlingKilledAtItsBoundLeavesAMalformedHolderToAPerson(t *testing.T) {
	t.Parallel()
	for _, holder := range []string{"gastown/polecats", "gastown/crew/alice", "alice"} {
		t.Run(holder, func(t *testing.T) {
			t.Parallel()
			fake, d, _ := boundKilledSling(t)
			bd := newWorkBD(t)
			bd.db.Seed(beads.Issue{
				ID:        "gt-z",
				Status:    string(beads.IssueStatusHooked),
				Assignee:  holder,
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			})
			d.openWorkBeads = bd.open

			d.runSpecDispatch()
			waitSlings(t, d)

			if got := fake.argvs("unsling"); len(got) != 0 {
				t.Errorf("unsling calls = %v, want the malformed holder %q left alone", got, holder)
			}
			if got := fake.argvs("bead", "update"); len(got) != 0 {
				t.Errorf("bead updates = %v, want no label on a bead a person must release", got)
			}
			notes := fake.argvs("bead", "note", "gt-z")
			if len(notes) != 1 {
				t.Fatalf("notes = %v, want one recording the hook a person must release", notes)
			}
			if note := notes[0][len(notes[0])-1]; !strings.Contains(note, "is not a polecat seat") {
				t.Errorf("note = %q, want it to say the holder is not a polecat seat", note)
			}
		})
	}
}

// boundKilledSling is a daemon whose sling is killed at its bound: the fake
// answers the tick, and the fake clock is carried past the sling's whole bound
// while the sling's process is run and reports the kill the daemon's
// process-group kill leaves, so the daemon reads the kill's own timing without
// waiting one out (gt-vaivp).
func boundKilledSling(t *testing.T) (*fakeCLI, *Daemon, func() []string) {
	t.Helper()
	return daemonWhoseSlingFailed(t, slingFailure{took: specSlingBound()})
}

// failedSling is a daemon whose sling fails without running out of time: the
// failure that must not be read as a kill at the bound.
func failedSling(t *testing.T) (*fakeCLI, *Daemon, func() []string) {
	t.Helper()
	return daemonWhoseSlingFailed(t, slingFailure{})
}

// slingFailure is how one sling ends in these tests: took is how far the fake
// clock is carried while its process runs, and unslingErr refuses the hand-back
// of a bead the sling hooked.
type slingFailure struct {
	took       time.Duration
	unslingErr error
}

// daemonWhoseSlingFailed is a daemon whose one sling exits with the error the
// daemon's process-group kill leaves, after the clock is carried o.took: a
// whole bound is a kill, and no time at all is a failure of its own.
func daemonWhoseSlingFailed(t *testing.T, o slingFailure) (*fakeCLI, *Daemon, func() []string) {
	t.Helper()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	clock := clockwork.NewFakeClockAt(time.Now())
	d.clock = clock
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		args := cmd.Args[1:]
		switch {
		case len(args) >= 2 && args[0] == "spec" && args[1] == "sling":
			clock.Advance(o.took)
			return nil, []byte("signal: terminated"), errors.New("signal: terminated")
		case len(args) >= 1 && args[0] == "unsling" && o.unslingErr != nil:
			// The fake records the call it answers, so the test can see the
			// hand-back was tried before it is refused.
			_, _, _ = fake.run(cmd)
			return nil, []byte("the hand-back was refused"), o.unslingErr
		}
		return fake.run(cmd)
	}
	return fake, d, lines
}

// exitCoded is an error that carries a process exit code, as exec.ExitError does.
type exitCoded struct{ code int }

func (e exitCoded) Error() string { return "exit status" }
func (e exitCoded) ExitCode() int { return e.code }

// A run that took its whole bound and then failed with a normal exit code was
// not killed by the bound; a signal death (no exit code) and a plain error are.
func TestExitedOnItsOwnSeparatesANormalExitFromAKill(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"exit status 1", exitCoded{1}, true},
		{"exit status 0 reported with an error", exitCoded{0}, true},
		{"wrapped exit status 2", fmt.Errorf("sling: %w", exitCoded{2}), true},
		{"killed by a signal (-1)", exitCoded{-1}, false},
		{"a plain error", errors.New("signal: terminated"), false},
		{"no error", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := exitedOnItsOwn(c.err); got != c.want {
				t.Errorf("exitedOnItsOwn(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
