package refinery

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/rig"
)

// stubLintLockRetryDelay shortens the waits between lock retries so a test can
// drive the retry path without sleeping the real tens of seconds.
func stubLintLockRetryDelay(t *testing.T, delays ...time.Duration) {
	t.Helper()
	prev := lintlock.RetryDelay
	lintlock.RetryDelay = delays
	t.Cleanup(func() { lintlock.RetryDelay = prev })
}

func newLintGateEngineer(t *testing.T) *Engineer {
	t.Helper()
	e := NewEngineer(&rig.Rig{Name: "test-rig", Path: t.TempDir()})
	e.workDir = t.TempDir()
	e.output = io.Discard
	return e
}

// contendedThen is a gate command that fails its first run with the output a
// contended lint produces, then on the run that follows does what a lint that
// got the lock does. It leaves "waited" behind if the retry happened at all.
//
// The first run counts for detecting the retry; the file is written before the
// output so a test can tell "no attempt" from "one attempt".
func contendedThen(t *testing.T, e *Engineer, contendedOutput, thenShell string) (cmd, firstRun, waited string) {
	t.Helper()
	firstRun = filepath.Join(e.workDir, "first-run")
	waited = filepath.Join(e.workDir, "waited")
	cmd = fmt.Sprintf("if [ -f '%s' ]; then touch '%s'; %s; fi; touch '%s'; echo '%s'; exit 2",
		firstRun, waited, thenShell, firstRun, contendedOutput)
	return cmd, firstRun, waited
}

// TestRunGate_GolangciLintGateWaitsOutTheLock is the batch gate's half of
// gt-ijqw: a lint that lost the lock to a concurrent one is re-run rather than
// reported as the MR's own failure.
func TestRunGate_GolangciLintGateWaitsOutTheLock(t *testing.T) {
	stubLintLockRetryDelay(t, time.Millisecond)
	e := newLintGateEngineer(t)

	cmd, firstRun, waited := contendedThen(t, e, lintlock.Marker, "exit 0")
	result := e.runGate(context.Background(), "lint", &GateConfig{Cmd: cmd, Timeout: lintlock.MinBudget()})

	if !result.Success {
		t.Errorf("gate failed on a lint that only lost the lock: %s", result.Error)
	}
	for _, f := range []string{firstRun, waited} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected %s to exist after the retry: %v", filepath.Base(f), err)
		}
	}
}

// TestRunGate_LintFindingAfterContentionIsReportedAsFindings is the regression
// the om gate asked for (om-gate attempt 1 major): the verdict comes from what
// the FINAL attempt did, not from the retry count. A lint that contended once
// and then found real errors must be forwarded as findings — reporting it as
// "nothing was linted" sends the polecat to re-run a lint that fails
// identically.
func TestRunGate_LintFindingAfterContentionIsReportedAsFindings(t *testing.T) {
	stubLintLockRetryDelay(t, time.Millisecond)
	e := newLintGateEngineer(t)

	cmd, _, waited := contendedThen(t, e, lintlock.Marker, "echo 'pkga/a.go:1:1: a real finding (fakecheck)'; exit 1")
	result := e.runGate(context.Background(), "lint", &GateConfig{Cmd: cmd, Timeout: lintlock.MinBudget()})

	if result.Success {
		t.Fatal("gate passed on a lint that reported a finding")
	}
	if _, err := os.Stat(waited); err != nil {
		t.Errorf("expected the contended attempt to be retried: %v", err)
	}
	if !strings.Contains(result.Error, "fix the lint findings") {
		t.Errorf("error does not ask for the findings to be fixed: %s", result.Error)
	}
	if !strings.Contains(result.Error, "fakecheck") {
		t.Errorf("error does not carry the final attempt's own finding: %s", result.Error)
	}
	for _, unwanted := range []string{"held the lock", "nothing was linted"} {
		if strings.Contains(result.Error, unwanted) {
			t.Errorf("error claims %q for a lint that ran and found something: %s", unwanted, result.Error)
		}
	}
}

// TestRunGate_GolangciLintGateWithoutABudget pins the boundary the gate cannot
// cross on its own: the retry measures itself against the gate's deadline, so a
// gate with no budget fails closed after one attempt instead of looping.
func TestRunGate_GolangciLintGateWithoutABudget(t *testing.T) {
	stubLintLockRetryDelay(t, time.Millisecond)
	e := newLintGateEngineer(t)

	cmd, _, waited := contendedThen(t, e, lintlock.Marker, "exit 0")
	result := e.runGate(context.Background(), "lint", &GateConfig{Cmd: cmd})

	if result.Success {
		t.Error("gate passed a contended lint it never retried")
	}
	if _, err := os.Stat(waited); err == nil {
		t.Error("the gate retried a lint with no budget to finish the retry")
	}
	if !strings.Contains(result.Error, "held the lock") {
		t.Errorf("a contended lint was not attributed to the lock: %s", result.Error)
	}
}

func TestIsGolangciLintGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		gateName string
		cmd      string
		want     bool
	}{
		{"the command names the tool", "fast-lint", "golangci-lint run ./...", true},
		{"a wrapper around it leaves only the name", "lint", "make lint", true},
		{"an unrelated linter is not routed through it", "eslint", "npm run eslint", false},
		{"nor a same-suffixed name", "lintian", "lintian pkg.deb", false},
		{"nor a gate that merely mentions linting", "lint-check", "true", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := &GateConfig{Cmd: tc.cmd}
			if got := isGolangciLintGate(tc.gateName, gate); got != tc.want {
				t.Errorf("isGolangciLintGate(%q, %q) = %v, want %v", tc.gateName, tc.cmd, got, tc.want)
			}
		})
	}
}

// TestRunBatchSteps_NamesTheGateThatFailed is the diagnostic half of gt-ijqw:
// one chained shell command could not say which step broke, so a failing lint
// read as the batch's own test failure.
func TestRunBatchSteps_NamesTheGateThatFailed(t *testing.T) {
	e := newLintGateEngineer(t)
	testRan := filepath.Join(e.workDir, "test-ran")

	result := e.runBatchSteps(context.Background(), []GateStep{
		{Name: "lint", Cmd: "exit 1"},
		{Name: "test", Cmd: "touch '" + testRan + "'"},
	})

	if result.Success {
		t.Fatal("batch verification passed with a failing lint step")
	}
	if !strings.Contains(result.Error, "quality gates failed: lint") {
		t.Errorf("the failing step is not named in %q", result.Error)
	}
	if _, err := os.Stat(testRan); err == nil {
		t.Error("a later step ran after an earlier one failed")
	}
}

// TestRunBatchSteps_WaitsOutTheLintLock covers the path the reported incident
// actually took: gastown configures lint_command rather than a gates map, so
// the batch reaches the lint through these steps.
func TestRunBatchSteps_WaitsOutTheLintLock(t *testing.T) {
	stubLintLockRetryDelay(t, time.Millisecond)
	e := newLintGateEngineer(t)
	e.config.RunTests = true

	cmd, _, waited := contendedThen(t, e, lintlock.Marker, "exit 0")
	result := e.runBatchSteps(context.Background(), []GateStep{{Name: "lint", Cmd: cmd}})

	if !result.Success {
		t.Errorf("batch verification failed on a lint that only lost the lock: %s", result.Error)
	}
	if _, err := os.Stat(waited); err != nil {
		t.Errorf("the batch's lint step did not wait out the lock: %v", err)
	}
}

// TestRunBatchStep_TestStepKeepsTheFlakeRetry keeps what the chained command
// got from RetryFlakyTests: the batch's test step still re-runs a flaky
// failure, which is the other way an innocent MR gets ejected.
func TestRunBatchStep_TestStepKeepsTheFlakeRetry(t *testing.T) {
	e := newLintGateEngineer(t)
	e.config.RetryFlakyTests = 2
	firstRun := filepath.Join(e.workDir, "first-run")

	result := e.runBatchStep(context.Background(), GateStep{
		Name: "test",
		Cmd:  fmt.Sprintf("if [ ! -f '%s' ]; then touch '%s'; exit 1; fi; exit 0", firstRun, firstRun),
	})

	if !result.Success {
		t.Errorf("a flaky test step was not retried: %s", result.Error)
	}
}
