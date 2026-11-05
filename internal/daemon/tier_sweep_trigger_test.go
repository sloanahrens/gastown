package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// awaitOrFailTimeout bounds every channel wait in this file. The acceptance
// cap is five seconds for a silent channel to fail its test.
const awaitOrFailTimeout = 2 * time.Second

// awaitTarget is the part of *testing.T awaitOrFail uses. It is an interface
// so the timeout branch has a test of its own: a *testing.T cannot be failed
// and then keep running.
type awaitTarget interface {
	Helper()
	Fatalf(format string, args ...any)
}

// awaitOrFail is the only way these tests wait on a channel: a signal answers
// at once, and a channel that never signals fails the test with the name of
// what it waited for instead of hanging the package until the go test timeout.
// The bound is a context deadline rather than time.After, which the tree's
// test policy refuses in a unit test.
func awaitOrFail[T any](t awaitTarget, ch <-chan T, what string) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), awaitOrFailTimeout)
	defer cancel()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		var zero T
		t.Fatalf("waited %s for %s; it never came", awaitOrFailTimeout, what)
		return zero
	}
}

// awaitFailSentinel is what the fake target's Fatalf panics with, so the helper
// stops where a real Fatalf would and the test can read what was reported.
var awaitFailSentinel = errors.New("awaitOrFail called Fatalf")

// awaitFailFake records the failure awaitOrFail reports.
type awaitFailFake struct{ msg string }

func (f *awaitFailFake) Helper() {}

func (f *awaitFailFake) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(awaitFailSentinel)
}

// A never-signalled channel fails its waiter in bounded time, naming what was
// awaited: no passing test reaches that branch, so it is driven here.
func TestAwaitOrFailFailsOnASilentChannel(t *testing.T) {
	t.Parallel()
	if awaitOrFailTimeout >= 5*time.Second {
		t.Fatalf("awaitOrFailTimeout = %s, want under the 5s acceptance bound", awaitOrFailTimeout)
	}
	f := &awaitFailFake{}
	func() {
		defer func() {
			if r := recover(); r != awaitFailSentinel {
				t.Fatalf("awaitOrFail answered a silent channel instead of failing: %v", r)
			}
		}()
		awaitOrFail(f, make(chan struct{}), "the checkout seam")
	}()
	if !strings.Contains(f.msg, "the checkout seam") {
		t.Fatalf("failure message %q does not name what was awaited", f.msg)
	}
}

// gt-gg9hv.37: the trigger used to return without a word at four points, and a
// town that logged nothing for seventy minutes could not say which one held it.
// Each reason now logs on the first heartbeat it holds and stays quiet until it
// changes.
func TestTriggerTierSweepLogsEachSkipReasonOnce(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
	count := func(sub string) int { return strings.Count(logs.String(), sub) }

	d.patrolConfig.Patrols.TierSweep.Enabled = false
	for range 3 {
		d.triggerTierSweep()
	}
	if n := count("tier_sweep: not starting: patrol inactive (enabled=false)"); n != 1 {
		t.Fatalf("the disabled patrol logged %d times, want once:\n%s", n, logs.String())
	}

	d.patrolConfig.Patrols.TierSweep.Enabled = true
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		d.triggerTierSweep()
	}
	if n := count("tier_sweep: not starting: not due ("); n != 1 {
		t.Fatalf("the not-due trigger logged %d times, want once:\n%s", n, logs.String())
	}
	if want := "next due " + now.Add(defaultTierSweepInterval).Format(time.RFC3339); !strings.Contains(logs.String(), want) {
		t.Fatalf("the not-due line does not hold %q:\n%s", want, logs.String())
	}

	// Due, with a cycle in flight: the age in the line moves every heartbeat
	// and the line does not.
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	d.tierSweepStartedAt.Store(now.Add(-5 * time.Minute).UnixNano())
	d.tierSweepRunning.Store(true)
	for range 3 {
		d.triggerTierSweep()
		d.clock.(*clockwork.FakeClock).Advance(time.Minute)
	}
	if n := count("tier_sweep: not starting: a cycle is already running"); n != 1 {
		t.Fatalf("the in-flight reason logged %d times, want once:\n%s", n, logs.String())
	}
	d.tierSweepRunning.Store(false)
}

// TestTriggerTierSweepEverySilentReturnLogsOncePerReason drives every silent
// return through the real trigger as a pair of reasons: the first is held for
// three heartbeats and logs once, and the second logs at once when it replaces
// the first, then stays quiet. The patrol-off arms are split so a reason that
// drifts from the check that held it (enabled=false mistaken for
// disabled_patrols) fails its row.
func TestTriggerTierSweepEverySilentReturnLogsOncePerReason(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	inactive := func(setting []string, container bool) func(*Daemon) {
		return func(d *Daemon) {
			d.disabledPatrols = nil
			d.containerMode = container
			d.patrolConfig.ContainerOffSetting = setting
			d.patrolConfig.Patrols.TierSweep.Enabled = true
		}
	}
	inFlight := func(t *testing.T, d *Daemon, at time.Time) {
		t.Helper()
		if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", at.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		d.tierSweepRunning.Store(true)
		d.tierSweepStartedAt.Store(at.Add(-5 * time.Minute).UnixNano())
	}
	notDue := func(t *testing.T, d *Daemon, at time.Time) {
		t.Helper()
		d.tierSweepRunning.Store(false)
		if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", at); err != nil {
			t.Fatal(err)
		}
	}
	estopHeld := func(t *testing.T, d *Daemon) {
		t.Helper()
		if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill"); err != nil {
			t.Fatal(err)
		}
	}
	ringWaned := func(t *testing.T, d *Daemon, at time.Time) {
		t.Helper()
		withStandbyRingKey(t, d)
		ringtest.Waned(t, d.config.TownRoot, standbyKey, at)
	}

	for _, tc := range []struct {
		name  string
		setA  func(t *testing.T, d *Daemon, at time.Time)
		setB  func(t *testing.T, d *Daemon, at time.Time)
		lineA string
		lineB string
	}{
		{
			name: "enabled=false then disabled_patrols",
			setA: func(_ *testing.T, d *Daemon, _ time.Time) {
				d.patrolConfig.Patrols.TierSweep.Enabled = false
			},
			setB: func(_ *testing.T, d *Daemon, _ time.Time) {
				d.disabledPatrols = map[string]bool{"tier_sweep": true}
			},
			lineA: "patrol inactive (enabled=false)",
			lineB: "patrol inactive (disabled_patrols)",
		},
		{
			name: "disabled_patrols then container_off",
			setA: func(_ *testing.T, d *Daemon, _ time.Time) {
				d.disabledPatrols = map[string]bool{"tier_sweep": true}
			},
			setB: func(_ *testing.T, d *Daemon, _ time.Time) {
				inactive([]string{"tier_sweep"}, true)(d)
			},
			lineA: "patrol inactive (disabled_patrols)",
			lineB: "patrol inactive (container_off)",
		},
		{
			name: "container_off then enabled=false",
			setA: func(_ *testing.T, d *Daemon, _ time.Time) {
				inactive([]string{"tier_sweep"}, true)(d)
			},
			setB: func(_ *testing.T, d *Daemon, _ time.Time) {
				inactive(nil, false)(d)
				d.patrolConfig.Patrols.TierSweep.Enabled = false
			},
			lineA: "patrol inactive (container_off)",
			lineB: "patrol inactive (enabled=false)",
		},
		{
			name:  "not due then a cycle in flight",
			setA:  notDue,
			setB:  inFlight,
			lineA: "not due (",
			lineB: "a cycle is already running",
		},
		{
			name:  "a cycle in flight then not due",
			setA:  inFlight,
			setB:  notDue,
			lineA: "a cycle is already running",
			lineB: "not due (",
		},
		{
			name: "town ESTOP then a Waned Ring",
			setA: func(t *testing.T, d *Daemon, _ time.Time) {
				estopHeld(t, d)
			},
			setB: func(t *testing.T, d *Daemon, at time.Time) {
				if err := estop.Deactivate(d.config.TownRoot, false); err != nil {
					t.Fatal(err)
				}
				ringWaned(t, d, at)
			},
			lineA: "standby (town ESTOP active",
			lineB: "standby (the Ring waned",
		},
		{
			name: "a Waned Ring then the town ESTOP",
			setA: func(t *testing.T, d *Daemon, at time.Time) {
				ringWaned(t, d, at)
			},
			setB: func(t *testing.T, d *Daemon, _ time.Time) {
				estopHeld(t, d)
			},
			lineA: "standby (the Ring waned",
			lineB: "standby (town ESTOP active",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
			count := func(sub string) int { return strings.Count(logs.String(), sub) }
			advance := func() { d.clock.(*clockwork.FakeClock).Advance(time.Minute) }

			tc.setA(t, d, d.clk().Now())
			for range 3 {
				d.triggerTierSweep()
				advance()
			}
			if n := count("tier_sweep: not starting: " + tc.lineA); n != 1 {
				t.Fatalf("reason A logged %d times over three heartbeats, want once:\n%s", n, logs.String())
			}
			if n := count("tier_sweep: not starting: "); n != 1 {
				t.Fatalf("three heartbeats logged %d not-starting lines, want one:\n%s", n, logs.String())
			}

			tc.setB(t, d, d.clk().Now())
			d.triggerTierSweep()
			if n := count("tier_sweep: not starting: " + tc.lineB); n != 1 {
				t.Fatalf("the changed reason logged %d times, want once at once:\n%s", n, logs.String())
			}
			if n := count("tier_sweep: not starting: "); n != 2 {
				t.Fatalf("the changed reason left %d not-starting lines, want two:\n%s", n, logs.String())
			}

			for range 2 {
				d.triggerTierSweep()
				advance()
			}
			if n := count("tier_sweep: not starting: "); n != 2 {
				t.Fatalf("the held reason logged again: %d lines, want two:\n%s", n, logs.String())
			}
		})
	}
}

// The not-due line's detail reads the last-run record, so it is built only on
// the heartbeat that logs: a held reason must not read the file on every tick.
func TestLogTierSweepNotStartingBuildsTheDetailOnlyWhenItLogs(t *testing.T) {
	t.Parallel()
	d, _, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	builds := 0
	detail := func() string {
		builds++
		return "last run now"
	}

	for range 3 {
		d.logTierSweepNotStarting("not-due", detail)
	}
	if builds != 1 {
		t.Fatalf("the detail was built %d times over three held heartbeats, want once", builds)
	}
	if n := strings.Count(logs.String(), "tier_sweep: not starting: last run now"); n != 1 {
		t.Fatalf("the held reason logged %d times, want once:\n%s", n, logs.String())
	}

	d.logTierSweepNotStarting("", nil)
	d.logTierSweepNotStarting("not-due", detail)
	if builds != 2 {
		t.Fatalf("the detail was built %d times after the reason was lifted and held again, want twice", builds)
	}
}

// A cycle blocked before "sweep started" names the pre-stage step it is in: the
// checkout seam holds the cycle, and the log already holds the cycle-started
// and fetch lines.
func TestTierSweepCycleNamesTheStepItIsIn(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	blocked := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) {
		close(blocked)
		<-release
		return func() {}, nil
	}

	done := make(chan bool, 1)
	go func() { done <- d.runTierSweep() }()
	awaitOrFail(t, blocked, "the cycle to reach the checkout seam")

	if got := d.tierSweepStepName(); got != "checkout" {
		t.Errorf("step while the checkout blocks = %q, want checkout", got)
	}
	for _, want := range []string{
		"tier_sweep: cycle started (due: no last-run record)",
		"tier_sweep: gastown: fetch in",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log while the checkout blocks does not hold %q:\n%s", want, logs.String())
		}
	}

	close(release)
	released = true
	if ok := awaitOrFail(t, done, "the cycle to reach a verdict"); !ok {
		t.Error("the cycle did not reach a verdict")
	}
}

// A cycle past tierSweepStuckAfter raises one attention item, logs at most once
// an hour, and leaves the queue when the cycle ends.
func TestTierSweepStuckRaisesOneAttentionItem(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	f := newAttentionFixture(t, now)
	f.d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		TierSweep: &TierSweepConfig{Enabled: true},
	}}
	f.d.tierSweepRunning.Store(true)
	f.d.tierSweepStartedAt.Store(now.Add(-(tierSweepStuckAfter + time.Minute)).UnixNano())
	f.d.setTierSweepStep("checkout")

	st := f.tick(t, now)
	items := itemsWithKey(st, tierSweepStuckKey)
	if len(items) != 1 {
		t.Fatalf("items keyed %s = %d, want exactly one: %+v", tierSweepStuckKey, len(items), st.Items)
	}
	if !strings.Contains(items[0].Summary, "checkout") {
		t.Errorf("stuck item summary %q does not name the step", items[0].Summary)
	}

	f.d.triggerTierSweep()
	if n := strings.Count(f.logs.String(), "tier_sweep: cycle running"); n != 1 {
		t.Fatalf("the stuck line logged %d times, want once:\n%s", n, f.logs.String())
	}
	f.d.clock.(*clockwork.FakeClock).Advance(30 * time.Minute)
	f.d.triggerTierSweep()
	if n := strings.Count(f.logs.String(), "tier_sweep: cycle running"); n != 1 {
		t.Fatalf("the stuck line repeated inside the hour: %d times:\n%s", n, f.logs.String())
	}
	f.d.clock.(*clockwork.FakeClock).Advance(31 * time.Minute)
	f.d.triggerTierSweep()
	if n := strings.Count(f.logs.String(), "tier_sweep: cycle running"); n != 2 {
		t.Fatalf("the stuck line did not repeat after the hour: %d times:\n%s", n, f.logs.String())
	}

	f.d.tierSweepRunning.Store(false)
	st = f.tick(t, now.Add(2*time.Hour))
	if items := itemsWithKey(st, tierSweepStuckKey); len(items) != 0 {
		t.Fatalf("the item survived the cycle ending: %+v", items)
	}
	events, err := attention.ReadEvents(f.d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	cleared := false
	for _, e := range events {
		if e.Key == tierSweepStuckKey && e.State == attention.EventCleared {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("no cleared transition for %s: %+v", tierSweepStuckKey, events)
	}
}

// itemsWithKey returns the queue's items under key.
func itemsWithKey(st attention.State, key string) []attention.Item {
	var out []attention.Item
	for _, it := range st.Items {
		if it.Key == key {
			out = append(out, it)
		}
	}
	return out
}

// tierSweepFetchBudget hands the fetch what is left of the cycle's budget and
// refuses an exhausted one: FetchBranchWithTimeout reads a zero timeout as "no
// timeout", so a spent deadline must be an error.
func TestTierSweepFetchBudget(t *testing.T) {
	t.Parallel()
	if got, err := tierSweepFetchBudget(context.Background()); err != nil || got != git.RemoteQueryTimeout {
		t.Errorf("no deadline = %s, %v; want %s, nil", got, err, git.RemoteQueryTimeout)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if got, err := tierSweepFetchBudget(ctx); err != nil || got <= 0 || got > time.Hour {
		t.Errorf("1h deadline = %s, %v; want a bound inside it", got, err)
	}
	spent, spend := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer spend()
	if _, err := tierSweepFetchBudget(spent); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("spent deadline error = %v, want context.DeadlineExceeded", err)
	}
}
