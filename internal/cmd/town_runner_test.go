package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

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
		apply:       f.apply,
		landingWait: 10 * time.Minute,
		runsWait:    20 * time.Minute,
		out:         f.out,
	}
	return f
}

func TestTownRunnerRollRunsStepsInOrder(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newRollFixture()
	f.deps.landingWait = 0

	if err := runTownRunnerRoll(context.Background(), f.deps); err == nil {
		t.Fatal("roll with a zero --landing-wait succeeded")
	}
	if len(f.events.calls) != 0 {
		t.Errorf("calls = %v, want none", f.events.calls)
	}
}

// TestTownRunnerRollHoldPathUsesTheExportedName: the roll's hold path is built from runnerroll's one exported name.
func TestTownRunnerRollHoldPathUsesTheExportedName(t *testing.T) {
	t.Parallel()
	got := runnerRollDepsFor("/town", &bytes.Buffer{}, time.Minute, time.Minute, false).holdPath
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
			err := runTownRunnerRoll(context.Background(), f.deps)
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

	if err := runTownRunnerRoll(context.Background(), f.deps); err == nil {
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
	if missing != "dry-run" {
		cmd.Flags().Bool("dry-run", false, "")
	}
	return cmd
}

func TestTownRunnerRollRefusesAFlagItCannotRead(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"landing-wait", "runs-wait", "dry-run"} {
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
