//go:build integration

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// timedOutCommand reports the killed-suite transcript and then keeps running,
// so only the deadline can end it. The exec replaces the shell with the sleep:
// the context's kill then reaches the process holding the output pipes, and the
// run returns at the deadline instead of waiting the sleep out.
func timedOutCommand(printed string) string {
	return "printf '%s' " + shellQuote(killedSuiteTranscript) + "; touch " + shellQuote(printed) + "; exec sleep 30"
}

// TestIntegrationRunCommandOnWorktree_TimeoutIsReportedAsTimeout is the gt-59yz
// acceptance case, against a real shell: the process-group kill must reach the
// sleep holding the output pipes so the run returns at its deadline. The unit
// twin, TestRunCommandOnWorktree_TimeoutIsReportedAsTimeout, pins the body on
// an in-process gate. The suite outran its budget, exec.CommandContext killed it,
// and the escalation the mayor reads must say that: before this, the body's
// first line — the verdict — was "test failed: signal: killed", which is
// character-for-character what a crash reports, while the transcript under it
// was green and the run had simply needed longer. The kill stays in the body,
// named as the deadline's doing, so the underlying error is not lost to the
// rewording.
//
// The signal that kill used is not pinned: SetProcessGroup escalates SIGTERM to
// SIGKILL, so the cause is "terminated" unless the group had to be forced, and
// the verdict is a timeout because the context's deadline expired, not because
// of which signal the group-kill reached for (gt-6t43).
func TestIntegrationRunCommandOnWorktree_TimeoutIsReportedAsTimeout(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	writeTree(t, workDir, map[string]string{
		"go.mod":                  "module github.com/steveyegge/gastown\n",
		"internal/daemon/scan.go": "package daemon\n",
		"internal/tmux/scan.go":   "package tmux\n",
	})
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
	}
	d.hostLoadFn = func() hostLoad { return hostLoad{IdlePercent: 3.5, Load1: 7.72, NumCPU: 8} }

	// The deadline passes once the suite has printed its transcript, not
	// after a fixed 2s of wall clock: a loaded host can take longer than that
	// to start sh at all, and the kill would then find no transcript to
	// report. The context still carries a 2s deadline for the budget line.
	printed := filepath.Join(t.TempDir(), "printed")
	ctx := newGatedDeadline()
	ctx.deadline = time.Now().Add(2 * time.Second)
	go func() {
		for {
			if _, err := os.Stat(printed); err == nil {
				ctx.expire()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	err := d.runCommandOnWorktree(ctx, "gastown", "deadbeef", workDir, "test", timedOutCommand(printed))
	if err == nil {
		t.Fatal("expected error from a command killed at its deadline")
	}

	body := err.Error()
	firstLine, _, _ := strings.Cut(body, "\n")
	if !strings.HasPrefix(firstLine, "test TIMED OUT after ") {
		t.Errorf("expected the verdict line to lead with the timeout, got: %q", firstLine)
	}
	if strings.Contains(firstLine, "failed:") {
		t.Errorf("expected no failure wording on a deadline kill, got: %q", firstLine)
	}
	for _, want := range []string{
		"of the run's budget",                          // not presented as a plain failure
		"still running when its deadline fired",        // that it was alive, not crashed
		"killed by: signal: ",                          // the raw cause, preserved
		"this is the runner's timeout, not a crash",    // and classified
		"run budget: patrols.main_branch_test.timeout", // so the fix (raise it) is visible
		"last package reported: ok  \t" + "github.com/steveyegge/gastown/internal/tmux",
		"the killed run's transcript is green", // the all-green tail, labeled
		"commit: deadbeef",
		"host at start: CPU idle 3.5%", // the contention context survives the refactor
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q, got:\n%s", want, body)
		}
	}

	// The budget is the clock this command was given, measured at its start.
	// Asserted as a range rather than an exact string: it is derived from a
	// real deadline minus however long the run took to reach this command, so
	// pinning "2s" would fail on a box slow enough to lose half a second
	// between the context and the first instruction.
	if got := parseBudgetSeconds(t, body); got < time.Second || got > 2*time.Second {
		t.Errorf("expected the reported budget to be within [1s, 2s] of the 2s timeout, got %v:\n%s", got, body)
	}
}

// TestIntegrationAcquireMainBranchTestSlot_TakesTheRealHold is the gt-off9
// regression test, in the integration tier because the marker lives in the
// process environment: the runner must acquire as a first-class holder — flock, owner file,
// docker-ps check — even when the daemon's own environment carries a marker
// naming this very role, which is what a marker inherited from a
// predecessor daemon process looks like. The kernel drops that process's
// flock when it dies, but the marker lives on in everything it spawned, so
// riding it would let every cycle "acquire" a slot nobody holds and run with
// no flock, no owner file and no docker-ps check: invisible to the refinery
// gates and gt done verifies it is supposed to queue behind.
func TestIntegrationAcquireMainBranchTestSlot_TakesTheRealHold(t *testing.T) {
	townRoot := t.TempDir()
	gate := slot.NewGate(slot.WithRuntime(noContainers{}))
	d := &Daemon{config: &Config{TownRoot: townRoot}}
	d.seams.slots = gate

	inherited := slot.SlotLockPath(townRoot, 0) + "|" + strconv.Itoa(os.Getpid()+100000) + "|gastown/main-branch-test"
	t.Setenv(slot.ReentrantEnvVar, inherited)

	h, err := d.acquireMainBranchTestSlot("gastown")
	if err != nil {
		t.Fatalf("acquireMainBranchTestSlot: %v", err)
	}

	rep, err := gate.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status while held: %v", err)
	}
	if !rep.Held {
		t.Fatalf("no flock taken — the daemon's suite is invisible to every other caller: %+v", rep)
	}
	if rep.Owner == nil || rep.Owner.Role != "gastown/main-branch-test" || rep.Owner.PID != os.Getpid() {
		t.Fatalf("owner file should name the daemon's own hold: %+v", rep.Owner)
	}

	// The marker the hold arms for its descendants names this runner's role,
	// overwriting the stale one — which is what keeps it harmless in the
	// unrelated processes the daemon spawns while holding: only a caller
	// doing the same role's work may ride it (gt-off9).
	want := slot.SlotLockPath(townRoot, h.Index) + "|" + strconv.Itoa(os.Getpid()) + "|gastown/main-branch-test"
	if got := os.Getenv(slot.ReentrantEnvVar); got != want {
		t.Fatalf("marker armed by the daemon's hold = %q, want %q", got, want)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if rep, _ = gate.Status(townRoot); rep.Held {
		t.Fatalf("the hold outlived Release: %+v", rep)
	}
}
