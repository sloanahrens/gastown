package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/git"
)

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
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) {
		close(blocked)
		<-release
		return func() {}, nil
	}

	done := make(chan bool, 1)
	go func() { done <- d.runTierSweep() }()
	<-blocked

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
	if !<-done {
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
