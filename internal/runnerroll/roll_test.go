package runnerroll

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// stepLog is the ordered record of a run's calls to its fakes.
type stepLog struct {
	calls []string
}

func (l *stepLog) add(call string) { l.calls = append(l.calls, call) }

// loggedStore is the landing hold in memory, recording each take and release.
// Remove records the context state it ran on, so a release on the roll's
// cancelled context fails here instead of passing.
type loggedStore struct {
	log           *stepLog
	content       string
	present       bool
	removeErr     error
	releaseCtxErr error
}

func (s *loggedStore) Create(_ context.Context, content string) error {
	s.log.add("hold:take")
	if s.present {
		return ErrExists
	}
	s.content, s.present = content, true
	return nil
}

func (s *loggedStore) Read(context.Context) (string, bool, error) {
	return s.content, s.present, nil
}

func (s *loggedStore) Remove(ctx context.Context) error {
	s.log.add("hold:release")
	s.releaseCtxErr = ctx.Err()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.removeErr != nil {
		return s.removeErr
	}
	s.content, s.present = "", false
	return nil
}

// recordingTarget is the roll's target as a fake: it records each call and
// returns the configured diff or error.
type recordingTarget struct {
	log      *stepLog
	diff     []string
	diffErr  error
	applyErr error
	readyErr error
}

func (t *recordingTarget) Name() string { return "runner" }

func (t *recordingTarget) Diff(context.Context) ([]string, error) {
	t.log.add("diff")
	return t.diff, t.diffErr
}

func (t *recordingTarget) Apply(context.Context) error {
	t.log.add("apply")
	return t.applyErr
}

func (t *recordingTarget) WaitReady(context.Context) error {
	t.log.add("ready")
	return t.readyErr
}

// rollFixture is a roll with every fake quiet and idle. A test changes the field
// it exercises.
type rollFixture struct {
	log    *stepLog
	store  *loggedStore
	target *recordingTarget
	out    *bytes.Buffer
	deps   RollDeps
	opts   RollOptions
}

func newRollFixture() *rollFixture {
	log := &stepLog{}
	store := &loggedStore{log: log}
	target := &recordingTarget{log: log}
	out := &bytes.Buffer{}
	f := &rollFixture{log: log, store: store, target: target, out: out}
	f.deps = RollDeps{
		Store:    store,
		HoldPath: "/town/" + LandingHoldName,
		Owner:    "roll",
		Reason:   RollPause,
		Landing: func(context.Context) (string, int, bool, error) {
			log.add("landing")
			return RollPause, 0, true, nil
		},
		Runs: func(context.Context) ([]WorkflowRun, error) {
			log.add("runs")
			return nil, nil
		},
		Clock: &fakeClock{now: fixedNow},
		Now:   func() time.Time { return fixedNow },
		Out:   out,
	}
	f.opts = RollOptions{
		LandingWait: 10 * time.Minute,
		RunsWait:    20 * time.Minute,
		RolloutWait: 10 * time.Minute,
		Step:        func(n int) string { return fmt.Sprintf("step name %d", n) },
	}
	return f
}

// The steps the roll runs, in order, as the fixture's target and store log them.
var rollStepOrder = []string{"hold:take", "landing", "runs", "apply", "ready", "hold:release"}

func TestRunCallsTheStepsInOrder(t *testing.T) {
	t.Parallel()
	f := newRollFixture()

	if err := Run(context.Background(), f.deps, f.target, f.opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !slices.Equal(f.log.calls, rollStepOrder) {
		t.Errorf("calls = %v, want %v", f.log.calls, rollStepOrder)
	}
	if f.store.present {
		t.Error("the landing hold is still in place after a clean roll")
	}
	want := []string{"step 1/5: step name 1", "step 2/5: step name 2", "step 3/5: step name 3", "step 4/5: step name 4", "step 5/5: step name 5"}
	for _, line := range want {
		if !strings.Contains(f.out.String(), line) {
			t.Errorf("output lacks %q; got:\n%s", line, f.out.String())
		}
	}
}

// TestRunReleasesTheHoldAfterAFailureInEveryStep: each step that can fail stops
// the roll, and the hold the roll took is released anyway.
func TestRunReleasesTheHoldAfterAFailureInEveryStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		mutate      func(*rollFixture)
		wantErr     error
		wantCalls   []string
		wantNoApply bool
	}{
		{
			name: "the landing read fails",
			mutate: func(f *rollFixture) {
				f.deps.Landing = func(context.Context) (string, int, bool, error) {
					f.log.add("landing")
					return "", 0, false, errors.New("health report unreadable")
				}
			},
			wantCalls:   []string{"hold:take", "landing", "hold:release"},
			wantNoApply: true,
		},
		{
			name: "the landing wait times out",
			mutate: func(f *rollFixture) {
				f.deps.Landing = func(context.Context) (string, int, bool, error) {
					f.log.add("landing")
					return "someone else", 2, true, nil
				}
				f.opts.LandingWait = 10 * time.Second
			},
			wantErr:     ErrWaitTimeout,
			wantNoApply: true,
		},
		{
			name: "the runs list fails",
			mutate: func(f *rollFixture) {
				f.deps.Runs = func(context.Context) ([]WorkflowRun, error) {
					f.log.add("runs")
					return nil, errors.New("forgejo unreachable")
				}
			},
			wantCalls:   []string{"hold:take", "landing", "runs", "hold:release"},
			wantNoApply: true,
		},
		{
			name: "the runs wait times out",
			mutate: func(f *rollFixture) {
				f.deps.Runs = func(context.Context) ([]WorkflowRun, error) {
					f.log.add("runs")
					return []WorkflowRun{{Repo: "town/gastown", Workflow: "gate.yml", ID: 7, Status: forgejo.RunStatusRunning, Created: fixedNow}}, nil
				}
				f.opts.RunsWait = 10 * time.Second
			},
			wantErr:     ErrWaitTimeout,
			wantNoApply: true,
		},
		{
			name: "apply fails",
			mutate: func(f *rollFixture) {
				f.target.applyErr = errors.New("apply refused")
			},
			wantCalls: []string{"hold:take", "landing", "runs", "apply", "hold:release"},
		},
		{
			name: "the ready wait fails",
			mutate: func(f *rollFixture) {
				f.target.readyErr = errors.New("the Deployment is not Available")
			},
			wantCalls: []string{"hold:take", "landing", "runs", "apply", "ready", "hold:release"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRollFixture()
			tc.mutate(f)

			err := Run(context.Background(), f.deps, f.target, f.opts)

			if err == nil {
				t.Fatal("run succeeded, want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if tc.wantCalls != nil && !slices.Equal(f.log.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", f.log.calls, tc.wantCalls)
			}
			if got := f.log.calls; got[0] != "hold:take" || got[len(got)-1] != "hold:release" {
				t.Errorf("calls = %v, want the take first and the release last", got)
			}
			if tc.wantNoApply && slices.Contains(f.log.calls, "apply") {
				t.Errorf("calls = %v: something was applied after the failure", f.log.calls)
			}
			if f.store.present {
				t.Error("the landing hold was not released")
			}
			if f.store.releaseCtxErr != nil {
				t.Errorf("the release ran on a cancelled context (%v); the hold could outlive the roll", f.store.releaseCtxErr)
			}
		})
	}
}

// TestRunReleasesTheHoldOnACancelledContext: a signal that cancels the roll
// during a wait stops the roll before it applies, and the release still reaches
// the store on a context the cancel does not reach.
func TestRunReleasesTheHoldOnACancelledContext(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.deps.Runs = func(context.Context) ([]WorkflowRun, error) {
		f.log.add("runs")
		cancel()
		return nil, nil
	}

	err := Run(ctx, f.deps, f.target, f.opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if want := []string{"hold:take", "landing", "runs", "hold:release"}; !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v", f.log.calls, want)
	}
	if f.store.present {
		t.Error("the landing hold was not released")
	}
	if f.store.releaseCtxErr != nil {
		t.Errorf("the release ran on a cancelled context (%v); the hold could outlive the roll", f.store.releaseCtxErr)
	}
}

// TestRunReleasesTheHoldCancelledDuringApply: a cancel that lands during the
// apply step leaves the roll's context cancelled for the release, which must
// still remove the hold.
func TestRunReleasesTheHoldCancelledDuringApply(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The cancel lands after the take and waits, as a SIGINT during the apply does.
	cancelDuringApply := &cancellingTarget{target: f.target, cancel: cancel}
	if err := Run(ctx, f.deps, cancelDuringApply, f.opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	if f.store.present {
		t.Error("the landing hold was not released")
	}
	if f.store.releaseCtxErr != nil {
		t.Errorf("the release ran on a cancelled context (%v); the hold could outlive the roll", f.store.releaseCtxErr)
	}
}

// cancellingTarget cancels the roll's context in Apply, then behaves as its
// target does, so the roll continues into the ready wait and the release on a
// cancelled context.
type cancellingTarget struct {
	target *recordingTarget
	cancel context.CancelFunc
}

func (t *cancellingTarget) Name() string { return t.target.Name() }

func (t *cancellingTarget) Diff(ctx context.Context) ([]string, error) {
	return t.target.Diff(ctx)
}

func (t *cancellingTarget) WaitReady(ctx context.Context) error {
	return t.target.WaitReady(ctx)
}

func (t *cancellingTarget) Apply(ctx context.Context) error {
	err := t.target.Apply(ctx)
	t.cancel()
	return err
}

// TestRunRefusesAHeldLandingBeforeAnyWait: a take refused by another holder
// leaves the file as it was and runs no step after it.
func TestRunRefusesAHeldLandingBeforeAnyWait(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	const other = "ops rebuilding the image\nowner=ops token=abc123 at=2026-11-19T10:00:00Z"
	f.store.content, f.store.present = other, true

	err := Run(context.Background(), f.deps, f.target, f.opts)

	if !errors.Is(err, ErrHeldByOther) {
		t.Fatalf("error = %v, want ErrHeldByOther", err)
	}
	if want := []string{"hold:take"}; !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v: nothing after the refused take", f.log.calls, want)
	}
	if !f.store.present || f.store.content != other {
		t.Errorf("the other holder's file was touched: present=%v content=%q", f.store.present, f.store.content)
	}
}

func TestRunDryRunTakesNoHoldAndPrintsTheDiff(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.opts.DryRun = true
	f.target.diff = []string{"deployment/forgejo-runner: replicas", "configmap/forgejo-runner-config: data"}

	if err := Run(context.Background(), f.deps, f.target, f.opts); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if want := []string{"diff"}; !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v: a dry run takes nothing and applies nothing", f.log.calls, want)
	}
	if f.store.present {
		t.Error("dry run wrote the landing hold")
	}
	out := f.out.String()
	for _, want := range append([]string{
		"dry run: no hold is taken and nothing is applied",
		"step 1/5: step name 1",
		"step 5/5: step name 5",
		"the live runner differs from the rendered manifests:",
	}, f.target.diff...) {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q; got:\n%s", want, out)
		}
	}
}

func TestRunDryRunSaysWhenThereIsNoDiff(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.opts.DryRun = true

	if err := Run(context.Background(), f.deps, f.target, f.opts); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if out := f.out.String(); !strings.Contains(out, "no differences") {
		t.Errorf("dry run output does not say there are no differences; got:\n%s", out)
	}
}

// TestRunDryRunRefusesAnUnreadableDiff: a dry run that cannot compare exits
// non-zero instead of reporting no difference, and still takes nothing.
func TestRunDryRunRefusesAnUnreadableDiff(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.opts.DryRun = true
	f.target.diffErr = errors.New("the live Deployment could not be read")

	err := Run(context.Background(), f.deps, f.target, f.opts)

	if err == nil || !strings.Contains(err.Error(), "comparing the runner with the live cluster") {
		t.Fatalf("error = %v, want it to name the compare", err)
	}
	if want := []string{"diff"}; !slices.Equal(f.log.calls, want) {
		t.Errorf("calls = %v, want %v", f.log.calls, want)
	}
	if f.store.present {
		t.Error("dry run wrote the landing hold")
	}
}

// TestRunRefusesARollItCannotRun: every seam and bound the run reads has a nil
// or zero value that refuses before the hold instead of failing mid-roll.
func TestRunRefusesARollItCannotRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(*rollFixture)
		nilTarget bool
		want      string
	}{
		{name: "no landing wait", mutate: func(f *rollFixture) { f.opts.LandingWait = 0 }, want: "longer than zero"},
		{name: "no runs wait", mutate: func(f *rollFixture) { f.opts.RunsWait = -time.Second }, want: "longer than zero"},
		{name: "no rollout wait", mutate: func(f *rollFixture) { f.opts.RolloutWait = 0 }, want: "longer than zero"},
		{name: "no step names", mutate: func(f *rollFixture) { f.opts.Step = nil }, want: "no step names"},
		{name: "no target", nilTarget: true, want: "no target"},
		{name: "nowhere to print", mutate: func(f *rollFixture) { f.deps.Out = nil }, want: "nowhere to print"},
		{name: "no hold store", mutate: func(f *rollFixture) { f.deps.Store = nil }, want: "no landing hold store"},
		{name: "no landing read", mutate: func(f *rollFixture) { f.deps.Landing = nil }, want: "no landing health read"},
		{name: "no run list", mutate: func(f *rollFixture) { f.deps.Runs = nil }, want: "no run list"},
		{name: "no clock", mutate: func(f *rollFixture) { f.deps.Clock = nil }, want: "no clock"},
		{name: "no clock reading", mutate: func(f *rollFixture) { f.deps.Now = nil }, want: "no clock reading"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRollFixture()
			if tc.mutate != nil {
				tc.mutate(f)
			}
			target := Target(f.target)
			if tc.nilTarget {
				target = nil
			}

			err := Run(context.Background(), f.deps, target, f.opts)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
			if len(f.log.calls) != 0 {
				t.Errorf("calls = %v, want none: a refused roll takes nothing", f.log.calls)
			}
			if f.store.present {
				t.Error("a refused roll wrote the landing hold")
			}
		})
	}
}

// TestRunReturnsAReleaseFailure: a release that fails is the roll's error and
// prints what to do with the hold. A step that already failed keeps its own
// error, since it names the cause.
func TestRunReturnsAReleaseFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*rollFixture)
		wantErr string
		notErr  string
	}{
		{name: "the release fails after a clean roll", wantErr: "releasing the landing hold"},
		{
			name: "a step failed first, so its error is the roll's",
			mutate: func(f *rollFixture) {
				f.target.applyErr = errors.New("apply refused")
			},
			wantErr: "apply refused",
			notErr:  "releasing the landing hold",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRollFixture()
			f.store.removeErr = errors.New("pod unreachable")
			if tc.mutate != nil {
				tc.mutate(f)
			}

			err := Run(context.Background(), f.deps, f.target, f.opts)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
			}
			if tc.notErr != "" && strings.Contains(err.Error(), tc.notErr) {
				t.Errorf("error = %v, want it not to name %q", err, tc.notErr)
			}
			out := f.out.String()
			if !strings.Contains(out, "still this roll's") || !strings.Contains(out, f.deps.HoldPath) {
				t.Errorf("output does not give the removal command for this roll's hold; got:\n%s", out)
			}
		})
	}
}
