package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/runnerroll"
)

// rollEvents is the ordered record of the roll's calls to its fakes.
type rollEvents struct {
	calls []string
}

func (e *rollEvents) add(call string) { e.calls = append(e.calls, call) }

// rollStore is an in-memory landing hold. It records each create and remove in
// events, and can be told to fail either call.
type rollStore struct {
	events    *rollEvents
	content   string
	present   bool
	createErr error
	readErr   error
	removeErr error
	// releaseCtxErr is the Remove call's context state: a release on the roll's
	// cancelled context would see the cancel here.
	releaseCtxErr error
}

func (s *rollStore) Create(ctx context.Context, content string) error {
	s.events.add("hold:create")
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.createErr != nil {
		return s.createErr
	}
	if s.present {
		return runnerroll.ErrExists
	}
	s.content, s.present = content, true
	return nil
}

func (s *rollStore) Read(context.Context) (string, bool, error) {
	if s.readErr != nil {
		return "", false, s.readErr
	}
	return s.content, s.present, nil
}

// Remove honours ctx as the pod exec does, so a release that runs on a canceled
// roll context fails here instead of passing.
func (s *rollStore) Remove(ctx context.Context) error {
	s.events.add("hold:remove")
	s.releaseCtxErr = ctx.Err()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.removeErr != nil {
		return s.removeErr
	}
	s.present, s.content = false, ""
	return nil
}

// stopWaitBound bounds a test's wait for the roll to stop listening, so a roll
// that never stops fails the test instead of hanging it.
const stopWaitBound = 10 * time.Second

// rollApply is the apply step as a fake: it records each call, returns the
// configured error or diff, and can cancel the roll's context and wait for the
// roll to stop listening.
type rollApply struct {
	events   *rollEvents
	err      error
	readyErr error
	diff     []string
	diffErr  error
	cancel   context.CancelFunc
	waitStop <-chan struct{}
	sawStop  bool
}

func (a *rollApply) Wired() error { return nil }

func (a *rollApply) Apply(context.Context) error {
	a.events.add("apply")
	if a.cancel == nil {
		return a.err
	}
	a.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), stopWaitBound)
	defer cancel()
	select {
	case <-a.waitStop:
		a.sawStop = true
	case <-ctx.Done():
	}
	return a.err
}

func (a *rollApply) WaitReady(context.Context) error {
	a.events.add("ready")
	return a.readyErr
}

func (a *rollApply) Diff(context.Context) ([]string, error) {
	a.events.add("diff")
	return a.diff, a.diffErr
}

// recordedKubectlCall is one recorded call to the cluster client: its argv and the
// stdin the update.RunFunc seam carries on the context.
type recordedKubectlCall struct {
	name  string
	args  []string
	stdin string
}

// recordingKubectl records every call and answers through answer.
type recordingKubectl struct {
	calls  []recordedKubectlCall
	answer func(recordedKubectlCall) (string, int, error)
}

func (k *recordingKubectl) run(ctx context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	call := recordedKubectlCall{name: name, args: append([]string(nil), args...), stdin: string(runStdinOf(ctx))}
	k.calls = append(k.calls, call)
	return k.answer(call)
}

// testRunnerApplier is the real applier over a recording cluster client, built
// from the checked-in control values.
func testRunnerApplier(k *recordingKubectl) runnerApplier {
	return runnerApplier{
		kubectl:     k.run,
		values:      forgejorunner.ControlValues,
		build:       forgejorunner.Build,
		encode:      forgejorunner.YAML,
		rolloutWait: 10 * time.Minute,
		out:         io.Discard,
	}
}

// rollClock is a clock that advances only when the wait sleeps, so no test sleeps.
type rollClock struct {
	now     time.Time
	onSleep context.CancelFunc
}

func (c *rollClock) Now() time.Time { return c.now }

func (c *rollClock) Sleep(_ context.Context, d time.Duration) {
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep()
	}
}

// fakeRollSignals is the roll's signal source in a test: the test cancels ctx,
// and stop counts its calls and records whether the signal had cancelled ctx.
type fakeRollSignals struct {
	ctx    context.Context
	cancel context.CancelFunc
	stops  atomic.Int32
	stopCh chan struct{}
	// cancelledAtStop is written before stopCh closes.
	cancelledAtStop bool
}

func (s *fakeRollSignals) source() (context.Context, context.CancelFunc) {
	return s.ctx, func() {
		if s.stops.Add(1) == 1 {
			s.cancelledAtStop = s.ctx.Err() != nil
			close(s.stopCh)
		}
	}
}

// rollFixture is a roll with every seam faked and every wait quiet and idle. A
// test changes the field it exercises.
type rollFixture struct {
	events  *rollEvents
	store   *rollStore
	apply   *rollApply
	signals *fakeRollSignals
	out     *bytes.Buffer
	deps    runnerRollDeps
}

func newRollFixture() *rollFixture {
	ev := &rollEvents{}
	ctx, cancel := context.WithCancel(context.Background())
	signals := &fakeRollSignals{ctx: ctx, cancel: cancel, stopCh: make(chan struct{})}
	f := &rollFixture{
		events:  ev,
		store:   &rollStore{events: ev},
		apply:   &rollApply{events: ev},
		signals: signals,
		out:     &bytes.Buffer{},
	}
	f.apply.waitStop = signals.stopCh
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f.deps = runnerRollDeps{
		store:    f.store,
		holdPath: filepath.Join("/town", runnerroll.LandingHoldName),
		landing: func(context.Context) (string, int, bool, error) {
			ev.add("landing")
			return runnerroll.RollPause, 0, true, nil
		},
		runs: func(context.Context) ([]runnerroll.WorkflowRun, error) {
			ev.add("runs")
			return nil, nil
		},
		clock:       &rollClock{now: t0},
		now:         func() time.Time { return t0 },
		signals:     signals.source,
		apply:       f.apply,
		landingWait: 10 * time.Minute,
		runsWait:    20 * time.Minute,
		rolloutWait: 10 * time.Minute,
		out:         f.out,
	}
	return f
}

func TestTownRunnerRollRunsStepsInOrder(t *testing.T) {
	t.Parallel()
	f := newRollFixture()

	if err := runTownRunnerRoll(f.deps); err != nil {
		t.Fatalf("roll: %v", err)
	}

	want := []string{"hold:create", "landing", "runs", "apply", "ready", "hold:remove"}
	if !slices.Equal(f.events.calls, want) {
		t.Errorf("calls = %v, want %v", f.events.calls, want)
	}
	if f.store.present {
		t.Error("the landing hold is still in place after a clean roll")
	}
}

func TestTownRunnerRollReleasesHoldOnEveryExitPath(t *testing.T) {
	t.Parallel()
	timeoutLanding := func(context.Context) (string, int, bool, error) {
		return "someone else", 2, true, nil
	}
	cases := []struct {
		name        string
		mutate      func(*runnerRollDeps, context.CancelFunc)
		wantCalls   []string
		wantErr     error
		wantRunErr  bool
		wantNoApply bool // a cancel means nothing is applied after it
		wantStop    bool // the row delivers a signal, so stop must run exactly once
	}{
		{
			name: "landing read fails",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.landing = func(context.Context) (string, int, bool, error) {
					return "", 0, false, errors.New("health report unreadable")
				}
			},
			wantCalls:  []string{"hold:create", "hold:remove"},
			wantRunErr: true,
		},
		{
			name: "landing wait times out",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.landing = timeoutLanding
				d.landingWait = 10 * time.Second
			},
			wantCalls:  []string{"hold:create", "hold:remove"},
			wantErr:    runnerroll.ErrWaitTimeout,
			wantRunErr: true,
		},
		{
			name: "runs list fails",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.runs = func(context.Context) ([]runnerroll.WorkflowRun, error) {
					return nil, errors.New("forgejo unreachable")
				}
			},
			wantCalls:  []string{"hold:create", "landing", "hold:remove"},
			wantRunErr: true,
		},
		{
			name: "runs wait times out",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.runs = func(context.Context) ([]runnerroll.WorkflowRun, error) {
					return []runnerroll.WorkflowRun{{Repo: "town/gastown", Workflow: "gate.yml", ID: 7, Status: forgejo.RunStatusRunning, Created: d.now()}}, nil
				}
				d.runsWait = 10 * time.Second
			},
			wantCalls:  []string{"hold:create", "landing", "hold:remove"},
			wantErr:    runnerroll.ErrWaitTimeout,
			wantRunErr: true,
		},
		{
			name: "apply fails",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.apply.(*rollApply).err = errors.New("rollout refused")
			},
			wantCalls:  []string{"hold:create", "landing", "runs", "apply", "hold:remove"},
			wantRunErr: true,
		},
		{
			name: "the ready wait fails",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.apply.(*rollApply).readyErr = errors.New("the Deployment did not become Available")
			},
			wantCalls:  []string{"hold:create", "landing", "runs", "apply", "ready", "hold:remove"},
			wantRunErr: true,
		},
		{
			// A SIGINT cancels the roll's context while the landing wait runs. The
			// release must still reach the store, so the hold does not outlive the roll.
			name: "context canceled during the landing wait",
			mutate: func(d *runnerRollDeps, cancel context.CancelFunc) {
				d.landing = func(context.Context) (string, int, bool, error) {
					cancel()
					return "someone else", 2, true, nil
				}
			},
			wantCalls:   []string{"hold:create", "hold:remove"},
			wantErr:     context.Canceled,
			wantRunErr:  true,
			wantNoApply: true,
			wantStop:    true,
		},
		{
			// A SIGINT while the runs wait sleeps: the gate run in flight is left
			// alone, nothing is applied, and the release still reaches the store.
			name: "context canceled during the runs wait",
			mutate: func(d *runnerRollDeps, cancel context.CancelFunc) {
				d.runs = func(context.Context) ([]runnerroll.WorkflowRun, error) {
					return []runnerroll.WorkflowRun{{Repo: "town/gastown", Workflow: "gate.yml", ID: 7, Status: forgejo.RunStatusRunning, Created: d.now()}}, nil
				}
				d.runsWait = 10 * time.Second
				d.clock.(*rollClock).onSleep = cancel
			},
			wantCalls:   []string{"hold:create", "landing", "hold:remove"},
			wantErr:     context.Canceled,
			wantRunErr:  true,
			wantNoApply: true,
			wantStop:    true,
		},
		{
			// A SIGINT during the apply step: the roll fails, and the release still
			// reaches the store on a context the cancel does not reach.
			name: "context canceled during the apply step",
			mutate: func(d *runnerRollDeps, cancel context.CancelFunc) {
				d.apply.(*rollApply).cancel = cancel
				d.apply.(*rollApply).err = context.Canceled
			},
			wantCalls:  []string{"hold:create", "landing", "runs", "apply", "hold:remove"},
			wantErr:    context.Canceled,
			wantRunErr: true,
			wantStop:   true,
		},
		{
			// A SIGINT during the apply step that still finishes: by the time the
			// release runs the roll's context is cancelled, and the hold must still go.
			name: "context canceled before the release runs",
			mutate: func(d *runnerRollDeps, cancel context.CancelFunc) {
				d.apply.(*rollApply).cancel = cancel
			},
			wantCalls: []string{"hold:create", "landing", "runs", "apply", "ready", "hold:remove"},
			wantStop:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRollFixture()
			if tc.mutate != nil {
				tc.mutate(&f.deps, f.signals.cancel)
			}

			err := runTownRunnerRoll(f.deps)

			if tc.wantRunErr && err == nil {
				t.Fatal("roll succeeded, want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !slices.Equal(f.events.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", f.events.calls, tc.wantCalls)
			}
			if f.store.present {
				t.Error("the landing hold was not released")
			}
			if tc.wantNoApply && slices.Contains(f.events.calls, "apply") {
				t.Errorf("calls = %v: something was applied after the cancel", f.events.calls)
			}
			if f.store.releaseCtxErr != nil {
				t.Errorf("the release ran on a cancelled context (%v); the hold could outlive the roll", f.store.releaseCtxErr)
			}
			if f.apply.cancel != nil && !f.apply.sawStop {
				t.Error("the roll did not stop listening while it still ran: a second Ctrl-C would be swallowed")
			}
			if tc.wantStop {
				// The stop for the first signal may run on the context's AfterFunc
				// goroutine, so wait for it to reach stopCh with a bound.
				waitCtx, waitCancel := context.WithTimeout(context.Background(), stopWaitBound)
				defer waitCancel()
				select {
				case <-f.signals.stopCh:
				case <-waitCtx.Done():
					t.Fatal("stop was not called within the bound after the signal cancelled the roll")
				}
				if got := f.signals.stops.Load(); got != 1 {
					t.Errorf("stop was called %d times, want once after the signal", got)
				}
				if !f.signals.cancelledAtStop {
					t.Error("stop was called before the context was cancelled")
				}
			}
		})
	}
}

func TestTownRunnerRollHeldByOtherRefusesBeforeAnyWait(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	const holder = "ops rebuilding the image\nowner=ops token=abc123 at=2026-11-19T10:00:00Z"
	f.store.content, f.store.present = holder, true

	err := runTownRunnerRoll(f.deps)

	if !errors.Is(err, runnerroll.ErrHeldByOther) {
		t.Fatalf("error = %v, want ErrHeldByOther", err)
	}
	if !strings.Contains(err.Error(), "ops rebuilding the image") {
		t.Errorf("error %q does not name the holder's line", err)
	}
	if want := []string{"hold:create"}; !slices.Equal(f.events.calls, want) {
		t.Errorf("calls = %v, want %v: nothing after the refused take", f.events.calls, want)
	}
	if !f.store.present || f.store.content != holder {
		t.Errorf("the other holder's file was touched: present=%v content=%q", f.store.present, f.store.content)
	}
}

func TestTownRunnerRollDryRunTakesNoHoldAndPrintsSteps(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.dryRun = true

	if err := runTownRunnerRoll(f.deps); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if !slices.Equal(f.events.calls, []string{"diff"}) {
		t.Errorf("dry run calls = %v, want only the diff", f.events.calls)
	}
	if f.store.present {
		t.Error("dry run wrote the landing hold")
	}
	out := f.out.String()
	for _, want := range []string{
		"step 1/5: take the landing hold",
		"step 2/5: wait for the landing pass in flight to end (bound 10m0s)",
		"step 3/5: wait for the gate and post-land runs to finish (bound 20m0s)",
		"step 4/5: apply the manifests, then wait for the rollout (bound 10m0s) and the Deployment's readiness condition",
		"step 5/5: release the landing hold",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q; got:\n%s", want, out)
		}
	}
}

func TestTownRunnerRollDryRunSaysWhenThereIsNoDiff(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.dryRun = true

	if err := runTownRunnerRoll(f.deps); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if out := f.out.String(); !strings.Contains(out, "no differences") {
		t.Errorf("dry run output does not say there are no differences; got:\n%s", out)
	}
}

// TestTownRunnerRollDryRunPrintsTheDriftDiff: the dry run reads the live runner
// through the cluster client and prints the drift compare's lines, and makes no
// apply call.
func TestTownRunnerRollDryRunPrintsTheDriftDiff(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	k := &recordingKubectl{answer: func(recordedKubectlCall) (string, int, error) {
		return `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"live"}}`, 0, nil
	}}
	deps := runnerRollDeps{
		apply:       testRunnerApplier(k),
		landingWait: 10 * time.Minute,
		runsWait:    20 * time.Minute,
		rolloutWait: 10 * time.Minute,
		dryRun:      true,
		out:         out,
	}

	if err := runTownRunnerRoll(deps); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	for _, call := range k.calls {
		if len(call.args) > 0 && call.args[0] == "apply" {
			t.Errorf("dry run made an apply call: %v", call.args)
		}
	}
	printed := out.String()
	for _, want := range []string{
		"the live runner differs from the rendered manifests:",
		"deployment/forgejo-runner: ",
		"configmap/forgejo-runner-config: ",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("dry run output lacks %q; got:\n%s", want, printed)
		}
	}
}

// TestTownRunnerRollDryRunRefusesAnUnreadableCompare: a dry run whose values or
// live object cannot be read exits non-zero instead of reporting no difference.
func TestTownRunnerRollDryRunRefusesAnUnreadableCompare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*runnerApplier, *recordingKubectl)
		want   string
	}{
		{name: "values unreadable", mutate: func(a *runnerApplier, _ *recordingKubectl) {
			a.values = func() (forgejorunner.Values, error) {
				return forgejorunner.Values{}, errors.New("file unreadable")
			}
		}, want: "reading the control cluster's runner values"},
		{name: "the live object cannot be read", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = func(recordedKubectlCall) (string, int, error) { return "", 1, nil }
		}, want: "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &recordingKubectl{answer: func(recordedKubectlCall) (string, int, error) { return "{}", 0, nil }}
			a := testRunnerApplier(k)
			tc.mutate(&a, k)
			deps := runnerRollDeps{
				apply:       a,
				landingWait: 10 * time.Minute,
				runsWait:    20 * time.Minute,
				rolloutWait: 10 * time.Minute,
				dryRun:      true,
				out:         io.Discard,
			}

			err := runTownRunnerRoll(deps)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			for _, call := range k.calls {
				if len(call.args) > 0 && call.args[0] == "apply" {
					t.Errorf("dry run made an apply call: %v", call.args)
				}
			}
		})
	}
}

// TestTownRunnerRollRefusesWithoutAWiredApplier: a real roll and a dry run both
// refuse an applier that cannot work, and neither touches the hold.
func TestTownRunnerRollRefusesWithoutAWiredApplier(t *testing.T) {
	t.Parallel()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry run %v", dryRun), func(t *testing.T) {
			f := newRollFixture()
			f.deps.apply = runnerApplier{}
			f.deps.dryRun = dryRun

			err := runTownRunnerRoll(f.deps)

			if err == nil || !strings.Contains(err.Error(), "no cluster client is wired") {
				t.Fatalf("error = %v, want the missing-cluster-client refusal", err)
			}
			if len(f.events.calls) != 0 {
				t.Errorf("calls = %v, want none", f.events.calls)
			}
			if !dryRun && strings.Contains(f.out.String(), "step 1/5") {
				t.Error("the real roll printed step 1 before refusing")
			}
		})
	}
}

// TestTownRunnerApplierWiredRefusesAMissingSeam: every seam the applier reads
// has a nil or zero value that refuses rather than panicking mid-apply.
func TestTownRunnerApplierWiredRefusesAMissingSeam(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{answer: func(recordedKubectlCall) (string, int, error) { return "", 0, nil }}
	if err := testRunnerApplier(k).Wired(); err != nil {
		t.Fatalf("the complete applier refuses: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*runnerApplier)
		want   string
	}{
		{name: "no cluster client", mutate: func(a *runnerApplier) { a.kubectl = nil }, want: "no cluster client"},
		{name: "no values", mutate: func(a *runnerApplier) { a.values = nil }, want: "no runner values or renderer"},
		{name: "no renderer", mutate: func(a *runnerApplier) { a.build = nil }, want: "no runner values or renderer"},
		{name: "no encoder", mutate: func(a *runnerApplier) { a.encode = nil }, want: "no manifest encoder"},
		{name: "no rollout wait", mutate: func(a *runnerApplier) { a.rolloutWait = 0 }, want: "rollout wait must be longer than zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testRunnerApplier(k)
			tc.mutate(&a)
			err := a.Wired()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Wired() = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestTownRunnerRollApplyAndReadyCallInOrder: the ConfigMap, then the two
// claims, then the Deployment, then the rollout wait and the readiness read, and
// nothing else.
func TestTownRunnerRollApplyAndReadyCallInOrder(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{answer: func(call recordedKubectlCall) (string, int, error) {
		if len(call.args) > 0 && call.args[0] == "get" {
			return "True\n", 0, nil
		}
		return "", 0, nil
	}}

	a := testRunnerApplier(k)
	if err := a.Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := a.WaitReady(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}

	if len(k.calls) != 6 {
		t.Fatalf("made %d cluster calls, want 6: %v", len(k.calls), k.calls)
	}
	applied := []struct {
		kind string
		name string
	}{
		{"kind: ConfigMap", "name: forgejo-runner-config"},
		{"kind: PersistentVolumeClaim", "name: forgejo-runner-dind"},
		{"kind: PersistentVolumeClaim", "name: forgejo-runner-data"},
		{"kind: Deployment", "name: forgejo-runner"},
	}
	for i, want := range applied {
		call := k.calls[i]
		if !slices.Equal(call.args, []string{"apply", "-f", "-"}) {
			t.Errorf("call %d args = %v, want kubectl apply -f -", i, call.args)
		}
		if !strings.Contains(call.stdin, want.kind) || !strings.Contains(call.stdin, want.name) {
			t.Errorf("call %d stdin does not hold %q and %q; got:\n%s", i, want.kind, want.name, call.stdin)
		}
	}
	if want := []string{"rollout", "status", "deployment/forgejo-runner", "-n", "forgejo", "--timeout=10m0s"}; !slices.Equal(k.calls[4].args, want) {
		t.Errorf("call 4 = %v, want %v", k.calls[4].args, want)
	}
	if want := []string{"get", "deployment", "forgejo-runner", "-n", "forgejo", "-o", readyConditionJSONPath}; !slices.Equal(k.calls[5].args, want) {
		t.Errorf("call 5 = %v, want %v", k.calls[5].args, want)
	}
}

// failingKubectl answers a matching call with out and exit, the rest with success.
func failingKubectl(match func(recordedKubectlCall) bool, out string, exit int) func(recordedKubectlCall) (string, int, error) {
	return func(call recordedKubectlCall) (string, int, error) {
		if match(call) {
			return out, exit, nil
		}
		return "", 0, nil
	}
}

// TestTownRunnerRollApplyRefuses: each way the build, an apply, the rollout or
// the readiness read can fail returns an error and stops the call sequence.
func TestTownRunnerRollApplyRefuses(t *testing.T) {
	t.Parallel()
	appliesDeployment := func(call recordedKubectlCall) bool {
		return len(call.args) > 0 && call.args[0] == "apply" && strings.Contains(call.stdin, "kind: Deployment")
	}
	cases := []struct {
		name      string
		mutate    func(*runnerApplier, *recordingKubectl)
		want      string
		wantCalls int
	}{
		{name: "values unreadable", mutate: func(a *runnerApplier, _ *recordingKubectl) {
			a.values = func() (forgejorunner.Values, error) { return forgejorunner.Values{}, errors.New("file unreadable") }
		}, want: "reading the control cluster's runner values", wantCalls: 0},
		{name: "render refuses", mutate: func(a *runnerApplier, _ *recordingKubectl) {
			a.build = func(forgejorunner.Values) (forgejorunner.Manifests, error) {
				return forgejorunner.Manifests{}, errors.New("no labels")
			}
		}, want: "rendering the runner", wantCalls: 0},
		{name: "encode fails", mutate: func(a *runnerApplier, _ *recordingKubectl) {
			a.encode = func(io.Writer, any) error { return errors.New("cannot write") }
		}, want: "building the configmap", wantCalls: 0},
		{name: "apply cannot start", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = func(recordedKubectlCall) (string, int, error) { return "", -1, errors.New("kubectl not found") }
		}, want: "applying the configmap", wantCalls: 1},
		{name: "deployment apply exits nonzero", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failingKubectl(appliesDeployment, "", 1)
		}, want: "applying the deployment forgejo-runner", wantCalls: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &recordingKubectl{answer: func(recordedKubectlCall) (string, int, error) { return "", 0, nil }}
			a := testRunnerApplier(k)
			tc.mutate(&a, k)
			err := a.Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(k.calls) != tc.wantCalls {
				t.Errorf("made %d cluster calls, want %d: %v", len(k.calls), tc.wantCalls, k.calls)
			}
		})
	}
}

// TestTownRunnerRollWaitReadyRefuses: the ready wait, the roll's step 5, has its
// own refusals, each driven from WaitReady alone.
func TestTownRunnerRollWaitReadyRefuses(t *testing.T) {
	t.Parallel()
	isRollout := func(call recordedKubectlCall) bool { return len(call.args) > 0 && call.args[0] == "rollout" }
	isGet := func(call recordedKubectlCall) bool { return len(call.args) > 0 && call.args[0] == "get" }
	cases := []struct {
		name      string
		mutate    func(*runnerApplier, *recordingKubectl)
		want      string
		wantCalls int
	}{
		{name: "rollout cannot start", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = func(call recordedKubectlCall) (string, int, error) {
				if isRollout(call) {
					return "", -1, errors.New("kubectl not found")
				}
				return "", 0, nil
			}
		}, want: "waiting for the rollout", wantCalls: 1},
		{name: "rollout not ready", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failingKubectl(isRollout, "", 1)
		}, want: "did not complete its rollout within", wantCalls: 1},
		{name: "readiness cannot start", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = func(call recordedKubectlCall) (string, int, error) {
				if isGet(call) {
					return "", -1, errors.New("kubectl not found")
				}
				return "", 0, nil
			}
		}, want: "reading the readiness", wantCalls: 2},
		{name: "readiness exits nonzero", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failingKubectl(isGet, "", 1)
		}, want: "reading the readiness", wantCalls: 2},
		{name: "Available is not True", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failingKubectl(isGet, "False\n", 0)
		}, want: "is not ready", wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &recordingKubectl{answer: func(recordedKubectlCall) (string, int, error) { return "", 0, nil }}
			a := testRunnerApplier(k)
			tc.mutate(&a, k)
			err := a.WaitReady(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(k.calls) != tc.wantCalls {
				t.Errorf("made %d cluster calls, want %d: %v", len(k.calls), tc.wantCalls, k.calls)
			}
		})
	}
}

// TestTownRunnerRollWaitReadyRefusesAManifestItCannotBuild: the ready wait finds
// the Deployment by rendering the manifests again, so values or a render that
// fail there are an error and no cluster call.
func TestTownRunnerRollWaitReadyRefusesAManifestItCannotBuild(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*runnerApplier)
		want   string
	}{
		{name: "values unreadable", mutate: func(a *runnerApplier) {
			a.values = func() (forgejorunner.Values, error) { return forgejorunner.Values{}, errors.New("file unreadable") }
		}, want: "reading the control cluster's runner values"},
		{name: "render refuses", mutate: func(a *runnerApplier) {
			a.build = func(forgejorunner.Values) (forgejorunner.Manifests, error) {
				return forgejorunner.Manifests{}, errors.New("no labels")
			}
		}, want: "rendering the runner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := &recordingKubectl{answer: func(recordedKubectlCall) (string, int, error) { return "", 0, nil }}
			a := testRunnerApplier(k)
			tc.mutate(&a)

			err := a.WaitReady(context.Background())

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(k.calls) != 0 {
				t.Errorf("made %d cluster calls, want none: %v", len(k.calls), k.calls)
			}
		})
	}
}

// TestTownRunnerRollReleasesTheHoldWhenTheRolloutIsNotReady: a rollout that does
// not become ready fails the roll after the hold is taken, and the hold is still
// released.
func TestTownRunnerRollReleasesTheHoldWhenTheRolloutIsNotReady(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		answer func(recordedKubectlCall) (string, int, error)
		want   string
	}{
		{name: "rollout status times out", answer: func(call recordedKubectlCall) (string, int, error) {
			if call.args[0] == "rollout" {
				return "", 1, nil
			}
			return "", 0, nil
		}, want: "did not complete its rollout within"},
		{name: "the Available condition is False", answer: func(call recordedKubectlCall) (string, int, error) {
			if call.args[0] == "get" {
				return "False\n", 0, nil
			}
			return "", 0, nil
		}, want: "is not ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRollFixture()
			f.deps.apply = testRunnerApplier(&recordingKubectl{answer: tc.answer})

			err := runTownRunnerRoll(f.deps)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
			if want := []string{"hold:create", "landing", "runs", "hold:remove"}; !slices.Equal(f.events.calls, want) {
				t.Errorf("calls = %v, want %v", f.events.calls, want)
			}
			if f.store.present {
				t.Error("the landing hold was not released")
			}
		})
	}
}

func TestTownRunnerRollRefusesNonPositiveWaitBeforeHold(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.landingWait = 0

	if err := runTownRunnerRoll(f.deps); err == nil {
		t.Fatal("roll with a zero --landing-wait succeeded")
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
}

// TestTownRunnerRollHoldPathUsesTheExportedName: the roll's hold path is built from runnerroll's one exported name.
func TestTownRunnerRollHoldPathUsesTheExportedName(t *testing.T) {
	t.Parallel()
	got := runnerRollDepsFor("/town", nil, fixedRunsSeam(), &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false).holdPath
	if !strings.HasSuffix(got, runnerroll.LandingHoldName) {
		t.Errorf("the roll's hold path %q does not end with %q", got, runnerroll.LandingHoldName)
	}
}

// TestRunnerRollDepsForWiresThePodHoldStore: the store the production deps carry
// is the pod store over the exec runnerRollDepsFor was given, so a Create through
// deps.store reaches that exec with the hold path under the town root and the
// content on stdin. A deps that wired another exec or path would not.
func TestRunnerRollDepsForWiresThePodHoldStore(t *testing.T) {
	t.Parallel()
	var argv []string
	var stdin []byte
	fakePodExec := func(_ context.Context, got []string, in []byte) ([]byte, int, error) {
		argv, stdin = got, in
		return []byte("created\n"), 0, nil
	}
	deps := runnerRollDepsFor("/town", fakePodExec, fixedRunsSeam(), &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)
	if deps.err != nil {
		t.Fatalf("deps error = %v, want none", deps.err)
	}
	if deps.store == nil {
		t.Fatal("the production deps have no landing hold store")
	}
	if err := deps.store.Create(t.Context(), "owner furiosa\n"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !slices.Contains(argv, filepath.Join("/town", runnerroll.LandingHoldName)) {
		t.Errorf("argv %q does not name the hold path under the town root", argv)
	}
	if string(stdin) != "owner furiosa\n" {
		t.Errorf("stdin = %q, want the hold content", stdin)
	}
}

// TestRunnerRollDepsForWiresTheSweepPreflight: the production deps build the
// sweep preflight from the town settings the daemon pod holds. Building the deps
// reads nothing, and the seam's call runs exactly one cat of the settings path
// through the exec the deps were given.
func TestRunnerRollDepsForWiresTheSweepPreflight(t *testing.T) {
	t.Parallel()
	var calls [][]string
	exec := func(_ context.Context, argv []string, _ []byte) ([]byte, int, error) {
		calls = append(calls, argv)
		settings := `{"daemon": {"patrols": {"tier_sweep": {"runner": {"backend": "kubernetes", "cpus": 8, "memory": 16, "jobs": 4, "output_root": "/out", "context": "k3s-sweep"}}}}}`
		return []byte(settings), 0, nil
	}
	var out bytes.Buffer
	deps := runnerRollDepsFor("/town", exec, fixedRunsSeam(), &out, time.Minute, time.Minute, time.Minute, false)
	if deps.err != nil {
		t.Fatalf("deps error = %v, want none", deps.err)
	}
	if len(calls) != 0 {
		t.Fatalf("building the deps ran %d pod calls, want none", len(calls))
	}
	applier, ok := deps.apply.(runnerApplier)
	if !ok {
		t.Fatalf("deps.apply is %T, want runnerApplier", deps.apply)
	}
	if applier.sweepPreflight == nil {
		t.Fatal("the production applier carries no sweep preflight seam")
	}
	if p := applier.sweepPreflight(); p == nil {
		t.Fatalf("the seam built no preflight from the pod's settings; out: %s", out.String())
	}
	if len(calls) != 1 {
		t.Fatalf("the seam ran %d pod calls, want one", len(calls))
	}
	wantTail := []string{"cat", "--", config.TownSettingsPath("/town")}
	if got := calls[0]; len(got) < len(wantTail) || !slices.Equal(got[len(got)-len(wantTail):], wantTail) {
		t.Errorf("pod argv = %q, want it to end with a cat of %q", got, config.TownSettingsPath("/town"))
	}
}

// TestRunnerRollDepsForCarriesAStoreErrorBeforeTheHold: a town root with no hold
// store is carried as the deps error, and a real roll refuses on it before it
// takes the hold or runs any step.
func TestRunnerRollDepsForCarriesAStoreErrorBeforeTheHold(t *testing.T) {
	t.Parallel()
	deps := runnerRollDepsFor("", nil, fixedRunsSeam(), &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)
	if deps.err == nil {
		t.Fatal("deps for an empty town root carry no error")
	}

	err := runTownRunnerRoll(deps)

	if !errors.Is(err, deps.err) {
		t.Fatalf("error = %v, want the store error wrapped", err)
	}
}

// TestRunnerRollDepsForWiresTheRunsRead: the production deps list the seam's
// rig repo through its viewer client, so the roll waits on a real run list.
func TestRunnerRollDepsForWiresTheRunsRead(t *testing.T) {
	t.Parallel()
	lister := &rollLister{}
	var townRoot string
	seam := runnerRollRunsSeam{
		client: func() (runnerroll.ForgejoRunLister, error) { return lister, nil },
		repos:  func(got string) ([]string, error) { townRoot = got; return []string{"acme/widgets"}, nil },
	}

	deps := runnerRollDepsFor("/town", (&fakePodExec{}).exec, seam, &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)

	if deps.err != nil {
		t.Fatalf("deps error = %v, want none", deps.err)
	}
	if deps.runs == nil {
		t.Fatal("the production deps have no runs read")
	}
	runs, err := deps.runs(t.Context())
	if err != nil {
		t.Fatalf("runs read: %v", err)
	}
	if townRoot != "/town" {
		t.Errorf("the repos were read for town root %q, want /town", townRoot)
	}
	var asked []string
	for _, request := range lister.requests {
		fields := strings.Fields(request)
		if len(fields) < 2 || fields[0] != "acme/widgets" {
			t.Fatalf("list request %q is not for acme/widgets", request)
		}
		asked = append(asked, fields[1])
	}
	slices.Sort(asked)
	if want := []string{"gate.yml", "post-land.yml"}; !slices.Equal(asked, want) {
		t.Errorf("workflows listed = %q, want %q for the one repo", asked, want)
	}
	if len(runs) != 6 || runs[0].Repo != "acme/widgets" {
		t.Errorf("runs read returned %d runs, first %+v; want 6 from acme/widgets", len(runs), runs)
	}
}

// TestRunnerRollDepsForAFailedRunsBuildIsNotAnEmptyRead: a client that cannot
// be built, and a town that names no repos, each leave a read that returns the
// reason, never a nil read or an empty listing; the no-repos case reads no token.
func TestRunnerRollDepsForAFailedRunsBuildIsNotAnEmptyRead(t *testing.T) {
	t.Parallel()
	noToken := errors.New("no viewer token file")
	cases := []struct {
		name string
		seam runnerRollRunsSeam
		want string
	}{
		{name: "the client cannot be built", want: noToken.Error(), seam: runnerRollRunsSeam{
			client: func() (runnerroll.ForgejoRunLister, error) { return nil, noToken },
			repos:  func(string) ([]string, error) { return []string{"acme/widgets"}, nil },
		}},
		{name: "the repo list cannot be built", want: "registry unreadable", seam: runnerRollRunsSeam{
			client: func() (runnerroll.ForgejoRunLister, error) { return &rollLister{}, nil },
			repos:  func(string) ([]string, error) { return nil, errors.New("registry unreadable") },
		}},
		{name: "the town names no repos", want: "names no Forgejo rig repos", seam: runnerRollRunsSeam{
			client: func() (runnerroll.ForgejoRunLister, error) {
				t.Error("the viewer client was built for a town that names no repos")
				return &rollLister{}, nil
			},
			repos: func(string) ([]string, error) { return nil, nil },
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := runnerRollDepsFor("/town", (&fakePodExec{}).exec, tc.seam, &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)

			if deps.runs == nil {
				t.Fatal("a failed runs build left the deps with no read at all")
			}
			runs, err := deps.runs(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("read error = %v, want it to name %q", err, tc.want)
			}
			if runs != nil {
				t.Errorf("read returned %v, want no runs", runs)
			}
		})
	}
}

// TestRunnerRollDepsForClientRefuses: a viewer client that cannot be built is a
// deps error, so a real roll refuses before it takes the hold or runs a step. The
// error names the token file's path and never its contents.
func TestRunnerRollDepsForClientRefuses(t *testing.T) {
	t.Parallel()
	tokenPath := filepath.Join(t.TempDir(), "forgejo-viewer.env")
	const token = "viewer-token-that-must-not-print"
	if err := os.WriteFile(tokenPath, []byte(forgejo.EnvKey+"="+token+"\n"), 0o644); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	seam := runnerRollRunsSeam{
		client: func() (runnerroll.ForgejoRunLister, error) {
			return viewerRunListerVia(func(role string, opts ...forgejo.Option) (*forgejo.Client, error) {
				return forgejo.NewClient(role, append(opts, forgejo.WithTokenFile(tokenPath))...)
			})
		},
		repos: func(string) ([]string, error) { return []string{"acme/widgets"}, nil },
	}
	events := &rollEvents{}
	exec := func(context.Context, []string, []byte) ([]byte, int, error) {
		events.add("pod:exec")
		return []byte("created\n"), 0, nil
	}

	deps := runnerRollDepsFor("/town", exec, seam, &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)

	if deps.err == nil {
		t.Fatal("a viewer client that cannot be built left the deps with no error")
	}
	if strings.Contains(deps.err.Error(), token) {
		t.Errorf("the deps error carries the token: %v", deps.err)
	}
	if !strings.Contains(deps.err.Error(), tokenPath) {
		t.Errorf("the deps error %q does not name the token file %s", deps.err, tokenPath)
	}

	err := runTownRunnerRoll(deps)

	if !errors.Is(err, deps.err) {
		t.Fatalf("roll error = %v, want the deps error wrapped", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the roll error carries the token: %v", err)
	}
	if len(events.calls) != 0 {
		t.Errorf("calls = %v, want none: the roll ran a step before refusing", events.calls)
	}
}

// TestRunnerRollDepsForNoReposRefuses: a rig repo list that comes back empty is a
// deps error, so the roll refuses before the hold and builds no viewer client; no
// token is in either error.
func TestRunnerRollDepsForNoReposRefuses(t *testing.T) {
	t.Parallel()
	const token = "viewer-token-that-must-not-print"
	seam := runnerRollRunsSeam{
		client: func() (runnerroll.ForgejoRunLister, error) {
			t.Error("the viewer client was built for a town that names no repos")
			return nil, errors.New("reading the token file")
		},
		repos: func(string) ([]string, error) { return nil, nil },
	}
	events := &rollEvents{}
	exec := func(context.Context, []string, []byte) ([]byte, int, error) {
		events.add("pod:exec")
		return []byte("created\n"), 0, nil
	}

	deps := runnerRollDepsFor("/town", exec, seam, &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false)

	if deps.err == nil {
		t.Fatal("an empty rig repo list left the deps with no error")
	}
	if !strings.Contains(deps.err.Error(), "names no Forgejo rig repos") {
		t.Errorf("the deps error %q does not name the empty repo list", deps.err)
	}
	if strings.Contains(deps.err.Error(), token) {
		t.Errorf("the deps error carries the token: %v", deps.err)
	}

	err := runTownRunnerRoll(deps)

	if !errors.Is(err, deps.err) {
		t.Fatalf("roll error = %v, want the deps error wrapped", err)
	}
	if len(events.calls) != 0 {
		t.Errorf("calls = %v, want none: the roll ran a step before refusing", events.calls)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the roll error carries the token: %v", err)
	}
}

// fixedRunsSeam is the runs seam of a test that does not drive the runs read: a
// fixed repo list and a client that lists nothing, so the unit tier reads no rig
// settings and no token file.
func fixedRunsSeam() runnerRollRunsSeam {
	return runnerRollRunsSeam{
		client: func() (runnerroll.ForgejoRunLister, error) { return &rollLister{}, nil },
		repos:  func(string) ([]string, error) { return []string{"acme/widgets"}, nil },
	}
}

// TestRollForgejoRepos drives the production repo builder on a town root written
// to a temp directory (the registry read is the real one; only the Forgejo
// resolve, which reads rig settings through git, is injected): one repo per rig,
// and a fail-closed error naming the rig, or the registry, whenever the list
// could not be complete.
func TestRollForgejoRepos(t *testing.T) {
	t.Parallel()
	const rigs = `{"version":1,"rigs":{"alpha":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"al"}},"zeta":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"ze"}}}}`
	remotes := func(alpha, zeta string) func(string, string) (*config.ForgejoConfig, error) {
		return func(_, name string) (*config.ForgejoConfig, error) {
			switch name {
			case "alpha":
				return &config.ForgejoConfig{RemoteURL: alpha}, nil
			case "zeta":
				if zeta == "" {
					return nil, nil
				}
				return &config.ForgejoConfig{RemoteURL: zeta}, nil
			}
			return nil, nil
		}
	}
	unreadable := func(string, string) (*config.ForgejoConfig, error) {
		return nil, errors.New("settings/config.json: permission denied")
	}
	cases := []struct {
		name    string
		rigs    string
		resolve func(string, string) (*config.ForgejoConfig, error)
		want    []string
		wantErr string
	}{
		{name: "every rig resolves", rigs: rigs, resolve: remotes("https://forge.test/acme/alpha.git", "https://forge.test/acme/zeta"), want: []string{"acme/alpha", "acme/zeta"}},
		{name: "a rig with no Forgejo config", rigs: rigs, resolve: remotes("https://forge.test/acme/alpha.git", ""), wantErr: "rig zeta"},
		{name: "a rig whose remote does not parse", rigs: rigs, resolve: remotes("not a url", "https://forge.test/acme/zeta"), wantErr: "rig alpha"},
		{name: "a settings tier that cannot be read", rigs: rigs, resolve: unreadable, wantErr: "rig alpha: its Forgejo settings cannot be read"},
		{name: "a registry that is not there", rigs: "", resolve: remotes("", ""), wantErr: "cannot list the rigs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.rigs != "" {
				if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(tc.rigs), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := rollForgejoRepos(town, tc.resolve)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
					t.Fatalf("rollForgejoRepos = %q, %v; want no list and an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("rollForgejoRepos = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// noRepoFile is a repo-file opener whose repository holds no tracked file, so the
// repo tier of a rig's settings names nothing and only the rig-local tiers count.
type noRepoFile struct{}

func (noRepoFile) ShowFileAtRev(string, string) (string, error) { return "", errors.New("not tracked") }

// TestProductionRunsSeamReadsStrictSettings drives the production seam's resolver
// (rig.ResolveForgejoConfigWithError) on a temp town root: a rig whose settings tier
// exists but cannot be read (a directory where settings/config.json should be) is an
// error naming the rig and the file, not a shorter list; the same rig without that
// tier is only a rig that names no remote, so the error is about the remote.
func TestProductionRunsSeamReadsStrictSettings(t *testing.T) {
	t.Parallel()
	strict := strictForgejoResolver(func(string) rig.RepoFileReader { return noRepoFile{} })
	town := func(t *testing.T) string {
		dir := t.TempDir()
		for path, content := range map[string]string{"mayor/rigs.json": `{"version":1,"rigs":{"alpha":{"git_url":"u","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"al"}}}}`} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Run("an unreadable settings tier is an error naming the rig", func(t *testing.T) {
		t.Parallel()
		dir := town(t)
		if err := os.MkdirAll(filepath.Join(dir, "alpha", "settings", "config.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := rollForgejoRepos(dir, strict)
		if err == nil || got != nil || !strings.Contains(err.Error(), "rig alpha") || !strings.Contains(err.Error(), "settings") {
			t.Fatalf("rollForgejoRepos = %q, %v; want no list and an error naming rig alpha and its settings", got, err)
		}
	})
	t.Run("the same rig without that tier only names no remote", func(t *testing.T) {
		t.Parallel()
		got, err := rollForgejoRepos(town(t), strict)
		if err == nil || got != nil || !strings.Contains(err.Error(), "names no Forgejo remote") {
			t.Fatalf("rollForgejoRepos = %q, %v; want no list and a names-no-remote error", got, err)
		}
	})
}

// TestViewerRunListerReadsTheViewerRole: the roll's client is built for the viewer
// role, the read-only account, and a build error is returned with no client.
func TestViewerRunListerReadsTheViewerRole(t *testing.T) {
	t.Parallel()
	var role string
	fake := func(r string, _ ...forgejo.Option) (*forgejo.Client, error) {
		role = r
		return nil, errors.New("no token")
	}
	if lister, err := viewerRunListerVia(fake); err == nil || lister != nil {
		t.Errorf("viewerRunListerVia = %v, %v; want no client and the build error", lister, err)
	}
	if role != config.ForgejoRoleViewer {
		t.Errorf("the client was built for role %q, want %q", role, config.ForgejoRoleViewer)
	}
}

// changeHoldDuringRuns makes the runs read write the store, as another process
// would between the take and the release.
func changeHoldDuringRuns(f *rollFixture, content string, present bool) {
	f.deps.runs = func(context.Context) ([]runnerroll.WorkflowRun, error) {
		f.events.add("runs")
		f.store.content, f.store.present = content, present
		return nil, nil
	}
}

// TestTownRunnerRollReleaseFailureAdvice: a failed release prints what to do by
// whose hold the file is now. Only this roll's own hold gets a removal command.
func TestTownRunnerRollReleaseFailureAdvice(t *testing.T) {
	t.Parallel()
	const otherHold = "overseer pause\nowner=ops token=tok-2 at=2026-11-19T11:00:00Z"
	cases := []struct {
		name     string
		mutate   func(*rollFixture)
		want     []string
		wantPath bool
	}{
		{name: "this roll's own hold", mutate: func(f *rollFixture) { f.store.removeErr = errors.New("pod unreachable") },
			want: []string{"still this roll's", "remove it with: rm -f -- "}, wantPath: true},
		{name: "another roll's hold", mutate: func(f *rollFixture) { changeHoldDuringRuns(f, otherHold, true) },
			want: []string{`owner "ops"`, `reason "overseer pause"`, "do not remove it; ask that roll"}},
		{name: "a file that is not a whole hold", mutate: func(f *rollFixture) { changeHoldDuringRuns(f, "overseer pause\nowner=ops", true) },
			want: []string{`first line is "overseer pause"`, "left in place"}},
		{name: "a file already gone", mutate: func(f *rollFixture) { changeHoldDuringRuns(f, "", false) },
			want: []string{"already gone"}},
		{name: "a file that cannot be read", mutate: func(f *rollFixture) { f.store.readErr = errors.New("pod unreachable") },
			want: []string{"could not be read back", "pod unreachable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRollFixture()
			tc.mutate(f)
			err := runTownRunnerRoll(f.deps)
			if err == nil {
				t.Fatal("roll with a failed release succeeded")
			}
			out := f.out.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q; got:\n%s", want, out)
				}
			}
			if tc.wantPath {
				_, after, _ := strings.Cut(out, "rm -f -- ")
				printed, _, _ := strings.Cut(after, "\n")
				printed = strings.TrimSpace(printed)
				if !strings.HasSuffix(printed, runnerroll.LandingHoldName) {
					t.Errorf("printed hold path %q does not end with %q", printed, runnerroll.LandingHoldName)
				}
			} else if strings.Contains(out, "rm") {
				t.Errorf("output holds a removal command; got:\n%s", out)
			}
		})
	}
}

func TestTownRunnerRollRefusesWithoutAHoldStoreBeforeTheHold(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.store = nil

	if err := runTownRunnerRoll(f.deps); err == nil {
		t.Fatal("roll with no landing hold store succeeded")
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
}

// TestTownRunnerRollRefusesWithoutAnApplier: a roll whose applier is not wired
// at all refuses before it reads the town or takes the hold, instead of
// panicking on the nil applier.
func TestTownRunnerRollRefusesWithoutAnApplier(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.apply = nil

	err := runTownRunnerRoll(f.deps)

	if err == nil || !strings.Contains(err.Error(), "no applier") {
		t.Fatalf("error = %v, want the missing-applier refusal", err)
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
}

// TestTownRunnerRollRefusesWithoutASignalSourceBeforeTheHold: a real roll with
// no signal source cannot release the hold on Ctrl-C, so it refuses before the
// take.
func TestTownRunnerRollRefusesWithoutASignalSourceBeforeTheHold(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.signals = nil

	err := runTownRunnerRoll(f.deps)

	if err == nil || !strings.Contains(err.Error(), "no signal source") {
		t.Fatalf("error = %v, want the missing-signal-source refusal", err)
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
}

// TestTownRunnerRollRefusesOnADepsError: a production read that could not be
// built is returned before the roll takes the hold, so nothing runs and the
// operator sees why.
func TestTownRunnerRollRefusesOnADepsError(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	buildErr := errors.New("no token file")
	f.deps.err = buildErr

	err := runTownRunnerRoll(f.deps)

	if !errors.Is(err, buildErr) {
		t.Fatalf("error = %v, want the deps error wrapped", err)
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
	if f.store.present {
		t.Error("the landing hold was taken despite the deps error")
	}
}

// TestTownRunnerRollDryRunIgnoresADepsError: a dry run needs none of the reads
// the deps error came from, so it still prints its steps.
func TestTownRunnerRollDryRunIgnoresADepsError(t *testing.T) {
	t.Parallel()
	f := newRollFixture()
	f.deps.dryRun = true
	f.deps.err = errors.New("no token file")

	if err := runTownRunnerRoll(f.deps); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(f.out.String(), "step 1/5: take the landing hold") {
		t.Errorf("dry run printed no steps; got:\n%s", f.out.String())
	}
}

// rollFlagsCmd is the roll command with every flag defined except missing.
func rollFlagsCmd(missing string) *cobra.Command {
	cmd := &cobra.Command{}
	if missing != "landing-wait" {
		cmd.Flags().Duration("landing-wait", 10*time.Minute, "")
	}
	if missing != "runs-wait" {
		cmd.Flags().Duration("runs-wait", 20*time.Minute, "")
	}
	if missing != "rollout-wait" {
		cmd.Flags().Duration("rollout-wait", 10*time.Minute, "")
	}
	if missing != "dry-run" {
		cmd.Flags().Bool("dry-run", false, "")
	}
	return cmd
}

func TestTownRunnerRollRefusesAFlagItCannotRead(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"landing-wait", "runs-wait", "rollout-wait", "dry-run"} {
		findTown := func() (string, error) {
			t.Errorf("the town was looked up although --%s could not be read", name)
			return "", nil
		}

		err := runTownRunnerRollIn(rollFlagsCmd(name), findTown)

		if err == nil || !strings.Contains(err.Error(), "reading --"+name) {
			t.Errorf("--%s missing: error = %v, want it to name reading --%s", name, err, name)
		}
	}
}

func TestTownRunnerRollRefusesWhenTheTownIsNotFound(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	cmd := rollFlagsCmd("")
	cmd.SetOut(out)
	findTown := func() (string, error) { return "", errors.New("no town above the working directory") }

	err := runTownRunnerRollIn(cmd, findTown)

	if err == nil || !strings.Contains(err.Error(), "finding the town root") {
		t.Fatalf("error = %v, want it to name finding the town root", err)
	}
	if out.Len() != 0 {
		t.Errorf("the roll printed %q before refusing", out.String())
	}
}
