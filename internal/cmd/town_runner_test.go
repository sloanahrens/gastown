package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/forgejo"
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
		runs: func(context.Context) ([]runnerroll.Run, error) {
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

	want := []string{"hold:create", "landing", "runs", "apply", "hold:remove"}
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
				d.runs = func(context.Context) ([]runnerroll.Run, error) {
					return nil, errors.New("forgejo unreachable")
				}
			},
			wantCalls:  []string{"hold:create", "landing", "hold:remove"},
			wantRunErr: true,
		},
		{
			name: "runs wait times out",
			mutate: func(d *runnerRollDeps, _ context.CancelFunc) {
				d.runs = func(context.Context) ([]runnerroll.Run, error) {
					return []runnerroll.Run{{Repo: "town/gastown", Workflow: "gate.yml", ID: 7, Status: forgejo.RunStatusRunning, Created: d.now()}}, nil
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
		},
		{
			// A SIGINT while the runs wait sleeps: the gate run in flight is left
			// alone, nothing is applied, and the release still reaches the store.
			name: "context canceled during the runs wait",
			mutate: func(d *runnerRollDeps, cancel context.CancelFunc) {
				d.runs = func(context.Context) ([]runnerroll.Run, error) {
					return []runnerroll.Run{{Repo: "town/gastown", Workflow: "gate.yml", ID: 7, Status: forgejo.RunStatusRunning, Created: d.now()}}, nil
				}
				d.runsWait = 10 * time.Second
				d.clock.(*rollClock).onSleep = cancel
			},
			wantCalls:   []string{"hold:create", "landing", "hold:remove"},
			wantErr:     context.Canceled,
			wantRunErr:  true,
			wantNoApply: true,
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
		},
		{
			// A SIGINT during the apply step that still finishes: by the time the
			// release runs the roll's context is cancelled, and the hold must still go.
			name: "context canceled before the release runs",
			mutate: func(d *runnerRollDeps, cancel context.CancelFunc) {
				d.apply.(*rollApply).cancel = cancel
			},
			wantCalls: []string{"hold:create", "landing", "runs", "apply", "hold:remove"},
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
			if f.apply.cancel != nil {
				if !f.apply.sawStop {
					t.Error("the roll did not stop listening while it still ran: a second Ctrl-C would be swallowed")
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
	if len(k.calls) != 2 {
		t.Errorf("dry run made %d cluster calls, want the two reads", len(k.calls))
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

// TestTownRunnerRollApplyCallsInOrder: the ConfigMap, then the two claims, then
// the Deployment, then the rollout wait and the readiness read, and nothing
// else.
func TestTownRunnerRollApplyCallsInOrder(t *testing.T) {
	t.Parallel()
	k := &recordingKubectl{answer: func(call recordedKubectlCall) (string, int, error) {
		if len(call.args) > 0 && call.args[0] == "get" {
			return "True\n", 0, nil
		}
		return "", 0, nil
	}}

	if err := testRunnerApplier(k).Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
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

// TestTownRunnerRollApplyRefuses: each way the build, an apply, the rollout or
// the readiness read can fail returns an error and stops the call sequence.
func TestTownRunnerRollApplyRefuses(t *testing.T) {
	t.Parallel()
	failing := func(match func(recordedKubectlCall) bool, out string, exit int) func(recordedKubectlCall) (string, int, error) {
		return func(call recordedKubectlCall) (string, int, error) {
			if match(call) {
				return out, exit, nil
			}
			return "", 0, nil
		}
	}
	appliesDeployment := func(call recordedKubectlCall) bool {
		return len(call.args) > 0 && call.args[0] == "apply" && strings.Contains(call.stdin, "kind: Deployment")
	}
	isRollout := func(call recordedKubectlCall) bool { return len(call.args) > 0 && call.args[0] == "rollout" }
	isGet := func(call recordedKubectlCall) bool { return len(call.args) > 0 && call.args[0] == "get" }
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
			k.answer = failing(appliesDeployment, "", 1)
		}, want: "applying the deployment forgejo-runner", wantCalls: 4},
		{name: "rollout cannot start", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = func(call recordedKubectlCall) (string, int, error) {
				if isRollout(call) {
					return "", -1, errors.New("kubectl not found")
				}
				return "", 0, nil
			}
		}, want: "waiting for the rollout", wantCalls: 5},
		{name: "rollout not ready", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failing(isRollout, "", 1)
		}, want: "did not complete its rollout within", wantCalls: 5},
		{name: "readiness cannot start", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = func(call recordedKubectlCall) (string, int, error) {
				if isGet(call) {
					return "", -1, errors.New("kubectl not found")
				}
				return "", 0, nil
			}
		}, want: "reading the readiness", wantCalls: 6},
		{name: "readiness exits nonzero", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failing(isGet, "", 1)
		}, want: "reading the readiness", wantCalls: 6},
		{name: "Available is not True", mutate: func(_ *runnerApplier, k *recordingKubectl) {
			k.answer = failing(isGet, "False\n", 0)
		}, want: "is not ready", wantCalls: 6},
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
	got := runnerRollDepsFor("/town", &bytes.Buffer{}, time.Minute, time.Minute, time.Minute, false).holdPath
	if !strings.HasSuffix(got, runnerroll.LandingHoldName) {
		t.Errorf("the roll's hold path %q does not end with %q", got, runnerroll.LandingHoldName)
	}
}

// changeHoldDuringRuns makes the runs read write the store, as another process
// would between the take and the release.
func changeHoldDuringRuns(f *rollFixture, content string, present bool) {
	f.deps.runs = func(context.Context) ([]runnerroll.Run, error) {
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
