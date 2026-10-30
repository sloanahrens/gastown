package daemon

import (
	"context"
	"errors"
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
// defect in its bead: the daemon labels it as any failed sling and records the
// kill as an attempt, which the dispatcher reads to clear the label and sling
// the bead again while retries remain (gt-vaivp).
func TestASlingKilledAtItsBoundRecordsTheAttempt(t *testing.T) {
	t.Parallel()
	fake, d, lines := boundKilledSling(t)
	bd := newWorkBD(t)
	bd.seed("gt-z", string(beads.StatusOpen), time.Now())
	d.openWorkBeads = bd.open

	d.runSpecDispatch()
	waitSlings(t, d)

	if got := fake.argvs("bead", "update"); len(got) != 1 || !slices.Equal(got[0], []string{"bead", "update", "gt-z", "--add-label", "spec-dispatch-failed"}) {
		t.Errorf("bead updates = %v, want gt-z labeled spec-dispatch-failed once", got)
	}
	notes := fake.argvs("bead", "note", "gt-z")
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one recording the kill", notes)
	}
	note := notes[0][len(notes[0])-1]
	if !strings.HasPrefix(note, specdispatch.TimeoutNoteMarker) || !strings.Contains(note, "attempt 1 of 3") {
		t.Errorf("note = %q, want it headed by the timeout marker and naming attempt 1 of 3", note)
	}
	if !strings.Contains(note, "the dispatcher clears this label") {
		t.Errorf("note = %q, want it to say the dispatcher clears the label while retries remain", note)
	}
	if n := countContaining(lines(), "gt-z: sling process failed"); n != 1 {
		t.Errorf("failure lines = %d, want one: %v", n, lines())
	}
}

// Each kill counts the ones before it, so the kill that spends the last retry
// says so and leaves the label for a person (gt-vaivp).
func TestASlingKilledAtItsBoundCountsTheAttemptsBeforeIt(t *testing.T) {
	t.Parallel()
	fake, d, _ := boundKilledSling(t)
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{
		ID:        "gt-z",
		Status:    string(beads.StatusOpen),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Labels:    []string{"spec-dispatch-failed"},
		Notes:     strings.Repeat(specdispatch.TimeoutNoteMarker+" an earlier kill\n", specdispatch.TimeoutRetries-1),
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
}

// boundKilledSling is a daemon whose sling is killed at its bound: the fake
// answers the tick, and the sling's context is already spent when its process is
// run and reports the kill the daemon's process-group kill leaves, so the bound
// is what the daemon reads without a real wait.
func boundKilledSling(t *testing.T) (*fakeCLI, *Daemon, func() []string) {
	t.Helper()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: string(wireOut(plannedTick(slingJob("gt-z", "claude-sonnet"))))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.specSlingCtx = func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
		return ctx, cancel
	}
	d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
		args := cmd.Args[1:]
		if len(args) >= 2 && args[0] == "spec" && args[1] == "sling" {
			return nil, []byte("signal: terminated"), errors.New("signal: terminated")
		}
		return fake.run(cmd)
	}
	return fake, d, lines
}
