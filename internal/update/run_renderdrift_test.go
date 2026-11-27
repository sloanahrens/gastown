package update

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRenderDriftAnswersAfterVerify runs the walk with a hook answer per row and
// checks the one line the answer makes, in the log and in the verify note, with
// the run's outcome unchanged.
func TestRenderDriftAnswersAfterVerify(t *testing.T) {
	t.Parallel()
	const (
		threeLine  = "render differs: 3 objects (run gt town cluster roll)"
		unreadable = "render drift: unreadable"
	)
	rows := []struct {
		name string
		hook func(context.Context) (int, error)
		want string
	}{
		{
			name: "count above zero prints the count",
			hook: func(context.Context) (int, error) { return 3, nil },
			want: threeLine,
		},
		{
			name: "count of zero prints nothing",
			hook: func(context.Context) (int, error) { return 0, nil },
			want: "",
		},
		{
			name: "error prints unreadable",
			hook: func(context.Context) (int, error) { return 0, errors.New("kubectl: exit 1") },
			want: unreadable,
		},
		{
			name: "negative count is unreadable, not clean",
			hook: func(context.Context) (int, error) { return -1, nil },
			want: unreadable,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newRunFixture(t)
			f.deps.RenderDrift = row.hook

			final, err := f.run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Outcome != OutcomeOK {
				t.Errorf("Outcome = %q, want %q: the hook must not change the run's outcome", final.Outcome, OutcomeOK)
			}
			if got := step(t, final, StepVerify).Detail; got != row.want {
				t.Errorf("verify Detail = %q, want %q", got, row.want)
			}
			out := f.log.String()
			if row.want == "" {
				if strings.Contains(out, "render") {
					t.Errorf("log = %q, want no render line for a clean count", out)
				}
				if strings.Contains(out, "\n\n") {
					t.Errorf("log = %q, want no blank line for a clean count", out)
				}
			} else if !strings.Contains(out, row.want+"\n") {
				t.Errorf("log = %q, want the line %q", out, row.want)
			}
		})
	}
}

// TestRenderDriftNilHookSkipsLine: with no hook the run prints no line and the
// verify note keeps what the verify step wrote.
func TestRenderDriftNilHookSkipsLine(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
	}
	if out := f.log.String(); strings.Contains(out, "render") {
		t.Errorf("log = %q, want no render line with a nil hook", out)
	}
	if got := step(t, final, StepVerify).Detail; got != "" {
		t.Errorf("verify Detail = %q, want nothing extra with a nil hook", got)
	}
}

// TestRenderDriftRestartPendingRunsNoHook: a verify step the restart skipped
// asks the hook nothing.
func TestRenderDriftRestartPendingRunsNoHook(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.verify.daemon = []verifyAnswer{{version: verifyTestOld}}
	calls := 0
	f.deps.RenderDrift = func(context.Context) (int, error) {
		calls++
		return 3, nil
	}

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomePartial {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomePartial)
	}
	if calls != 0 {
		t.Errorf("hook calls = %d, want 0 for a skipped verify", calls)
	}
}

// TestRenderDriftSkippedVerifyRunsNoHook: a verify step the plan leaves out
// asks the hook nothing.
func TestRenderDriftSkippedVerifyRunsNoHook(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.opts.Skip = map[StepName]bool{StepVerify: true}
	calls := 0
	f.deps.RenderDrift = func(context.Context) (int, error) {
		calls++
		return 3, nil
	}

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
	}
	if calls != 0 {
		t.Errorf("hook calls = %d, want 0 for a skipped verify", calls)
	}
}

// TestRenderDriftFailedVerifyKeepsPartialOutcome: a verify that fails still gets
// the line beside its failed checks, and the run stays partial.
func TestRenderDriftFailedVerifyKeepsPartialOutcome(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.verify.gt = verifyTestOld
	f.deps.RenderDrift = func(context.Context) (int, error) { return 3, nil }

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomePartial {
		t.Errorf("Outcome = %q, want %q: the hook must not change a failed verify", final.Outcome, OutcomePartial)
	}
	verify := step(t, final, StepVerify)
	if verify.Result != ResultFailed {
		t.Errorf("verify Result = %q, want %q", verify.Result, ResultFailed)
	}
	if want := "render differs: 3 objects (run gt town cluster roll)"; !strings.HasSuffix(verify.Detail, "; "+want) {
		t.Errorf("verify Detail = %q, want it to end with %q", verify.Detail, want)
	}
}

// runRenderDrift runs the fixture with hook and requires the run to end ok,
// returning the fixture and the verify step's detail.
func runRenderDrift(t *testing.T, hook func(context.Context) (int, error)) (*runFixture, string) {
	t.Helper()
	f := newRunFixture(t)
	f.deps.RenderDrift = hook
	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q: the hook must not change the run", final.Outcome, OutcomeOK)
	}
	return f, step(t, final, StepVerify).Detail
}

// TestRenderDriftPanicIsUnreadable: a hook that panics is recovered into the
// unreadable line, and its text stays out of the log.
func TestRenderDriftPanicIsUnreadable(t *testing.T) {
	t.Parallel()
	f, detail := runRenderDrift(t, func(context.Context) (int, error) {
		panic("kubeconfig read at /secret/cluster-token")
	})
	if want := "render drift: unreadable"; detail != want {
		t.Errorf("verify Detail = %q, want %q", detail, want)
	}
	if out := f.log.String(); strings.Contains(out, "secret") {
		t.Errorf("the panic's text was printed: log %q", out)
	}
}

// TestRenderDriftSilentHookPassesBound: a hook still waiting when the bound
// passes on the run's clock is canceled there, so the line is the unreadable one.
func TestRenderDriftSilentHookPassesBound(t *testing.T) {
	t.Parallel()
	// release is a safety net for a run that stops canceling the hook: the test
	// fails in seconds rather than hanging, and sawCancel names which ended it.
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer context.AfterFunc(ctx, func() { close(release) })()
	var sawCancel bool
	f, detail := runRenderDrift(t, func(ctx context.Context) (int, error) {
		select {
		case <-ctx.Done():
			sawCancel = true
			return 0, ctx.Err()
		case <-release:
			return 0, errors.New("the run never canceled the hook")
		}
	})
	if want := "render drift: unreadable"; detail != want {
		t.Errorf("verify Detail = %q, want %q", detail, want)
	}
	if !sawCancel {
		t.Error("the run never canceled the hook: the test's own release ended it")
	}
	if !slices.Contains(f.clock.sleeps, 20*time.Second) {
		t.Errorf("sleeps = %v, want the 20s bound on the run's clock", f.clock.sleeps)
	}
}

// TestRenderDriftNoRecordNamesTheRecorder: a hook error wrapping
// ErrNoRenderRecord prints the line naming the command that records a render.
func TestRenderDriftNoRecordNamesTheRecorder(t *testing.T) {
	t.Parallel()
	const want = "render drift: unreadable (no recorded render: run gt town cluster render --record-flags)"
	f, detail := runRenderDrift(t, func(context.Context) (int, error) {
		return 0, fmt.Errorf("no flags recorded under /secret/path: %w", ErrNoRenderRecord)
	})
	if detail != want {
		t.Errorf("verify Detail = %q, want %q", detail, want)
	}
	if out := f.log.String(); !strings.Contains(out, want+"\n") {
		t.Errorf("log = %q, want the line %q", out, want)
	}
}

// TestRenderDriftErrorTextIsNotPrinted: the hook's error text can name what the
// hook read, so it stays out of the log and the note.
func TestRenderDriftErrorTextIsNotPrinted(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.deps.RenderDrift = func(context.Context) (int, error) {
		return 0, errors.New("secret-cluster-token leaked")
	}

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	detail := step(t, final, StepVerify).Detail
	if strings.Contains(f.log.String(), "secret-cluster-token") || strings.Contains(detail, "secret-cluster-token") {
		t.Errorf("the hook's error text was printed: log %q, detail %q", f.log.String(), detail)
	}
}
