package sweeprunner

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// exitError is a failed command's error: it carries the exit status the way
// the daemon's command seam's errors do.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e exitError) ExitCode() int { return e.code }

// stepAnswer is what the fake answers one command with.
type stepAnswer struct {
	stdout, stderr string
	err            error
}

// fakeSteps stands in for the command seam: it records each command's argv
// and answers it from answer, without starting a process.
type fakeSteps struct {
	calls  [][]string
	answer func(argv []string) stepAnswer
}

func (f *fakeSteps) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	f.calls = append(f.calls, cmd.Args)
	a := f.answer(cmd.Args)
	return []byte(a.stdout), []byte(a.stderr), a.err
}

// pauseClock is a fake clock whose waits end at once and whose wait lengths
// are recorded, so a test reads the pause a retry took without sleeping.
type pauseClock struct {
	clockwork.Clock
	waits []time.Duration
}

func newPauseClock() *pauseClock {
	return &pauseClock{Clock: clockwork.NewFakeClockAt(time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC))}
}

func (c *pauseClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}

// syncCopyStep is the repo copy as the Kubernetes backend renders it.
func syncCopyStep() Step {
	return Step{Argv: []string{"kubectl", "--context", "ctx", "cp", "/repo/.", "gt-sweep-sync-gastown:/repo.git"}, Retry: true}
}

// syncExecStep is the chown that follows the copy, which runs once the copy
// has landed.
func syncExecStep() Step {
	return Step{Argv: []string{"kubectl", "--context", "ctx", "exec", "gt-sweep-sync-gastown", "--", "sh", "-c", "chown"}}
}

// verbCount is how many of the calls carry word as one of their arguments.
func verbCount(calls [][]string, word string) int {
	n := 0
	for _, argv := range calls {
		if slices.Contains(argv, word) {
			n++
		}
	}
	return n
}

// TestSweepSyncRenderedRepoCopyIsTheOnlyRetry reads the plan the Kubernetes
// backend renders, not a hand-built step: the copy into the sync pod's repo
// claim is the one step marked Retry, and no create, wait, exec or the
// result copies are.
func TestSweepSyncRenderedRepoCopyIsTheOnlyRetry(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	if plan.Poll == nil {
		t.Fatal("the rendered plan carries no poll, so its result copies are not checked")
	}
	var retried []string
	for _, step := range slices.Concat(plan.Apply, plan.Steps, plan.Poll.Collect) {
		if step.Retry {
			retried = append(retried, strings.Join(step.Argv, " "))
		}
	}
	want := syncPodName(spec) + ":" + RepoMount
	if len(retried) != 1 || !strings.HasSuffix(retried[0], want) || !slices.Contains(strings.Fields(retried[0]), "cp") {
		t.Errorf("the steps marked Retry are %q, want only the copy into %s", retried, want)
	}
}

// TestSweepSyncCopyFailureShowsTheOutput is the failed copy's message: a copy
// that fails both attempts reports each attempt's error line and the exit
// code, and stops the run before the step after it.
func TestSweepSyncCopyFailureShowsTheOutput(t *testing.T) {
	t.Parallel()
	const line = "error: unable to copy into the sync pod"
	fake := &fakeSteps{answer: func(argv []string) stepAnswer {
		if slices.Contains(argv, "cp") {
			return stepAnswer{stderr: line + "\n", err: exitError{code: 1}}
		}
		return stepAnswer{}
	}}

	out, code := RunSteps(context.Background(), newPauseClock(), fake.run, []Step{syncCopyStep(), syncExecStep()})

	if code != 1 {
		t.Errorf("exit code = %d, want the copy's exit status 1", code)
	}
	if n := strings.Count(out, line); n != 2 {
		t.Errorf("the output carries the copy's error line %d times, want once per attempt:\n%s", n, out)
	}
	if n := verbCount(fake.calls, "exec"); n != 0 {
		t.Errorf("the chown ran %d times after the copy failed, want 0", n)
	}
}

// TestSweepSyncCopyRetriesOnce is the transient copy: a copy that fails once
// and then succeeds continues to the step after it, and the retry waits the
// stepRetryPause on the clock first.
func TestSweepSyncCopyRetriesOnce(t *testing.T) {
	t.Parallel()
	copies := 0
	fake := &fakeSteps{answer: func(argv []string) stepAnswer {
		if slices.Contains(argv, "cp") {
			copies++
			if copies == 1 {
				return stepAnswer{stderr: "error: tar: file changed as we read it\n", err: exitError{code: 1}}
			}
		}
		return stepAnswer{}
	}}
	clock := newPauseClock()

	out, code := RunSteps(context.Background(), clock, fake.run, []Step{syncCopyStep(), syncExecStep()})

	if code != 0 {
		t.Fatalf("exit code = %d, want the run to continue past the copy that succeeded on retry:\n%s", code, out)
	}
	if got := len(fake.calls); got != 3 {
		t.Fatalf("commands run = %d, want the copy twice and the chown once: %q", got, fake.calls)
	}
	if n := verbCount(fake.calls, "exec"); n != 1 {
		t.Errorf("the chown ran %d times, want once after the copy landed", n)
	}
	if want := []time.Duration{stepRetryPause}; !slices.Equal(clock.waits, want) {
		t.Errorf("waits = %v, want one pause of %v before the retry", clock.waits, stepRetryPause)
	}
}

// TestSweepSyncCopyCanceledDuringPauseIsNotRetried: a run canceled while a
// Retry step waits out its pause does not run the second attempt, and reports
// the failed first attempt rather than a clean exit. The fake clock's pause
// never ends unless advanced, so only the cancellation can end the wait.
func TestSweepSyncCopyCanceledDuringPauseIsNotRetried(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	copies := 0
	fake := &fakeSteps{answer: func(argv []string) stepAnswer {
		if slices.Contains(argv, "cp") {
			copies++
			if copies == 1 {
				cancel()
				return stepAnswer{stderr: "error: tar: file changed as we read it\n", err: exitError{code: 1}}
			}
		}
		return stepAnswer{}
	}}
	clock := clockwork.NewFakeClockAt(time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC))

	out, code := RunSteps(ctx, clock, fake.run, []Step{syncCopyStep(), syncExecStep()})

	if code == 0 {
		t.Errorf("exit code = 0 for a copy that failed and was cut short by cancellation:\n%s", out)
	}
	if copies != 1 {
		t.Errorf("the copy ran %d times, want once: a canceled run does not retry", copies)
	}
}

// TestSweepSyncOtherStepsAreNotRetried is the rest of the sync stage: a create,
// a wait or an exec that fails is attempted once, and the run stops there.
func TestSweepSyncOtherStepsAreNotRetried(t *testing.T) {
	t.Parallel()
	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	steps := slices.Concat(plan.Apply, plan.Steps)
	for _, verb := range []string{"create", "wait", "exec"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			fake := &fakeSteps{answer: func(argv []string) stepAnswer {
				if slices.Contains(argv, verb) {
					return stepAnswer{stderr: "error: refused\n", err: exitError{code: 1}}
				}
				return stepAnswer{}
			}}
			clock := newPauseClock()

			_, code := RunSteps(context.Background(), clock, fake.run, steps)

			if code == 0 {
				t.Errorf("exit code = 0 for a run whose %s step failed", verb)
			}
			if n := verbCount(fake.calls, verb); n != 1 {
				t.Errorf("the %s step was attempted %d times, want once", verb, n)
			}
			if len(clock.waits) != 0 {
				t.Errorf("waits = %v, want no pause before a step that is not retried", clock.waits)
			}
		})
	}
}

// TestSweepSyncCopyFailureTailIsLastEightLines: a failed attempt carries the
// last stepTailLines lines of its output and no earlier ones.
func TestSweepSyncCopyFailureTailIsLastEightLines(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&stderr, "stderr line %02d\n", i)
	}
	fake := &fakeSteps{answer: func(argv []string) stepAnswer {
		if slices.Contains(argv, "cp") {
			return stepAnswer{stderr: stderr.String(), err: exitError{code: 1}}
		}
		return stepAnswer{}
	}}

	out, _ := RunSteps(context.Background(), newPauseClock(), fake.run, []Step{syncCopyStep()})

	if strings.Contains(out, "stderr line 04\n") {
		t.Errorf("the output carries line 04, which is before the last %d lines:\n%s", stepTailLines, out)
	}
	if !strings.Contains(out, "stderr line 05\n") || !strings.Contains(out, "stderr line 12\n") {
		t.Errorf("the output lacks the last %d lines of the failed copy:\n%s", stepTailLines, out)
	}
}

// TestSweepSyncCopyFailureRedactsTokens: a token-looking value in the copy's
// output is left out of the failure message, and the rest of its line is kept.
func TestSweepSyncCopyFailureRedactsTokens(t *testing.T) {
	t.Parallel()
	const keyed = "ghp_0123456789abcdefABCD"
	const bare = "xoxb-1234567890abcdefghij"
	fake := &fakeSteps{answer: func(argv []string) stepAnswer {
		if slices.Contains(argv, "cp") {
			return stepAnswer{stderr: "error: GITHUB_TOKEN=" + keyed + " refused\npushed with " + bare + "\n", err: exitError{code: 1}}
		}
		return stepAnswer{}
	}}

	out, _ := RunSteps(context.Background(), newPauseClock(), fake.run, []Step{syncCopyStep()})

	for _, secret := range []string{keyed, bare} {
		if strings.Contains(out, secret) {
			t.Errorf("the output carries the token %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "GITHUB_TOKEN=[redacted] refused") || !strings.Contains(out, "pushed with [redacted]") {
		t.Errorf("the output does not keep the redacted lines around the token:\n%s", out)
	}
}
