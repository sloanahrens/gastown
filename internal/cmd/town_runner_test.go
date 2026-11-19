package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
	removeErr error
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
	return s.content, s.present, nil
}

// Remove honours ctx as the pod exec does, so a release that runs on a canceled
// roll context fails here instead of passing.
func (s *rollStore) Remove(ctx context.Context) error {
	s.events.add("hold:remove")
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.removeErr != nil {
		return s.removeErr
	}
	s.present, s.content = false, ""
	return nil
}

// rollApply is the apply step as a fake: it records each call, and returns err.
type rollApply struct {
	events *rollEvents
	err    error
}

func (a *rollApply) Wired() error { return nil }

func (a *rollApply) Apply(context.Context) error {
	a.events.add("apply")
	return a.err
}

// rollClock is a clock that advances only when the wait sleeps, so no test sleeps.
type rollClock struct {
	now time.Time
}

func (c *rollClock) Now() time.Time { return c.now }

func (c *rollClock) Sleep(_ context.Context, d time.Duration) { c.now = c.now.Add(d) }

// rollFixture is a roll with every seam faked and every wait quiet and idle. A
// test changes the field it exercises.
type rollFixture struct {
	events *rollEvents
	store  *rollStore
	apply  *rollApply
	out    *bytes.Buffer
	deps   runnerRollDeps
}

func newRollFixture() *rollFixture {
	ev := &rollEvents{}
	f := &rollFixture{
		events: ev,
		store:  &rollStore{events: ev},
		apply:  &rollApply{events: ev},
		out:    &bytes.Buffer{},
	}
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f.deps = runnerRollDeps{
		store:    f.store,
		holdPath: "/town/landing.hold",
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
		apply:       f.apply,
		landingWait: 10 * time.Minute,
		runsWait:    20 * time.Minute,
		out:         f.out,
	}
	return f
}

func TestTownRunnerRollRunsStepsInOrder(t *testing.T) {
	f := newRollFixture()

	if err := runTownRunnerRoll(context.Background(), f.deps); err != nil {
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
	timeoutLanding := func(context.Context) (string, int, bool, error) {
		return "someone else", 2, true, nil
	}
	cases := []struct {
		name       string
		mutate     func(*runnerRollDeps, context.CancelFunc)
		wantCalls  []string
		wantErr    error
		wantRunErr bool
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
			wantCalls:  []string{"hold:create", "hold:remove"},
			wantErr:    context.Canceled,
			wantRunErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRollFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.mutate != nil {
				tc.mutate(&f.deps, cancel)
			}

			err := runTownRunnerRoll(ctx, f.deps)

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
		})
	}
}

func TestTownRunnerRollHeldByOtherRefusesBeforeAnyWait(t *testing.T) {
	f := newRollFixture()
	const holder = "ops rebuilding the image\nowner=ops token=abc123 at=2026-11-19T10:00:00Z"
	f.store.content, f.store.present = holder, true

	err := runTownRunnerRoll(context.Background(), f.deps)

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
	f := newRollFixture()
	f.deps.dryRun = true

	if err := runTownRunnerRoll(context.Background(), f.deps); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if len(f.events.calls) != 0 {
		t.Errorf("dry run made calls %v, want none", f.events.calls)
	}
	if f.store.present {
		t.Error("dry run wrote the landing hold")
	}
	out := f.out.String()
	for _, want := range []string{
		"step 1/5: take the landing hold",
		"step 2/5: wait for the landing pass in flight to end (bound 10m0s)",
		"step 3/5: wait for the gate and post-land runs to finish (bound 20m0s)",
		"step 4/5: apply",
		"step 5/5: release the landing hold",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q; got:\n%s", want, out)
		}
	}
}

func TestTownRunnerRollDefaultApplierRefusesBeforeHold(t *testing.T) {
	out := &bytes.Buffer{}
	deps := runnerRollDepsFor("/town", out, 10*time.Minute, 20*time.Minute, false)

	err := runTownRunnerRoll(context.Background(), deps)

	if !errors.Is(err, errApplyUnwired) {
		t.Fatalf("error = %v, want the unwired-apply refusal", err)
	}
	if !strings.Contains(err.Error(), "the apply step is not wired yet") {
		t.Errorf("error %q lacks the refusal text", err)
	}
	if strings.Contains(out.String(), "step 1/5") {
		t.Error("the roll printed step 1 before refusing")
	}
}

func TestTownRunnerRollRefusesNonPositiveWaitBeforeHold(t *testing.T) {
	f := newRollFixture()
	f.deps.landingWait = 0

	if err := runTownRunnerRoll(context.Background(), f.deps); err == nil {
		t.Fatal("roll with a zero --landing-wait succeeded")
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
}

func TestTownRunnerRollPrintsRemovalCommandWhenReleaseFails(t *testing.T) {
	f := newRollFixture()
	f.store.removeErr = errors.New("pod unreachable")

	err := runTownRunnerRoll(context.Background(), f.deps)

	if err == nil {
		t.Fatal("roll with a failed release succeeded")
	}
	if want := "rm -f -- /town/landing.hold"; !strings.Contains(f.out.String(), want) {
		t.Errorf("output lacks the removal command %q; got:\n%s", want, f.out.String())
	}
}
