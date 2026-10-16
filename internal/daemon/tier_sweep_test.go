package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/slot"
)

// newTierSweepDaemon is a daemon over a temp town whose rigs.json lists rigs,
// with the log, the feed and the outside effects faked. Each rig gets the
// .repo.git directory the cycle looks for.
func newTierSweepDaemon(t *testing.T, now time.Time, rigs ...string) (*Daemon, *tierSweepRunRecorder, *tierSweepFakeBeads, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	entries := make([]string, 0, len(rigs))
	for _, r := range rigs {
		entries = append(entries, `"`+r+`": {}`)
		if err := os.MkdirAll(filepath.Join(town, r, ".repo.git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(`{"rigs": {`+strings.Join(entries, ",")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	rec := &tierSweepRunRecorder{}
	bd := &tierSweepFakeBeads{}
	d := &Daemon{
		config:         &Config{TownRoot: town},
		patrolConfig:   &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true}}},
		logger:         log.New(&logs, "", 0),
		clock:          clockwork.NewFakeClockAt(now),
		dogFeedFn:      (&feedCapture{}).record,
		tierSweepSeams: tierSweepSeams{run: rec.run, beads: func(string) tierSweepBeadStore { return bd }},
	}
	return d, rec, bd, &logs
}

// tierSweepRunRecorder records each stage the cycle ran and answers it with
// whatever the test set, defaulting to no output (which reads as RED).
type tierSweepRunRecorder struct {
	stages []tierSweepStage
	result func(st tierSweepStage) tierSweepStageResult
}

func (r *tierSweepRunRecorder) run(_ context.Context, _ string, st tierSweepStage) tierSweepStageResult {
	r.stages = append(r.stages, st)
	if r.result != nil {
		return r.result(st)
	}
	return tierSweepStageResult{ran: true}
}

// green runs every stage green, with one passing unit.
func (r *tierSweepRunRecorder) green() {
	r.result = func(st tierSweepStage) tierSweepStageResult {
		var b strings.Builder
		for _, tier := range st.tiers {
			fmt.Fprintf(&b, "tier-sweep: %s GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n", tier)
		}
		return tierSweepStageResult{output: b.String(), exitCode: 0, ran: true}
	}
}

// tierSweepFakeBeads is the sweep's bead store: created beads, comments and
// closes are recorded, and List answers the open beads the test set (a
// comma-separated "title=id" list).
type tierSweepFakeBeads struct {
	open     []*beads.Issue
	created  []beads.CreateOptions
	comments []string
	closed   []string
}

func (b *tierSweepFakeBeads) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	b.created = append(b.created, opts)
	return &beads.Issue{ID: fmt.Sprintf("gt-new-%d", len(b.created)), Title: opts.Title}, nil
}

func (b *tierSweepFakeBeads) AddComment(id, text string) error {
	b.comments = append(b.comments, id+": "+text)
	return nil
}

func (b *tierSweepFakeBeads) List(beads.ListOptions) ([]*beads.Issue, error) {
	return b.open, nil
}

func (b *tierSweepFakeBeads) CloseWithReason(reason string, ids ...string) error {
	for _, id := range ids {
		b.closed = append(b.closed, id+" ("+reason+")")
	}
	return nil
}

func tierSweepTestState(t *testing.T, d *Daemon, rig string) tierSweepState {
	t.Helper()
	st, err := readTierSweepState(d.config.TownRoot, rig)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// at builds a fake-clock time at the given local hour, so a test can show the
// hour of day changes nothing about what a cycle runs.
func atHour(t time.Time, hour int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), hour, 0, 0, 0, time.Local)
}

// A pending upgrade restart keeps a cycle from starting at all, so the sweep
// never races the restarted daemon's own cycle (gt-ccyw0).
func TestRunTierSweep_SkipsWhileAnUpgradeRestartIsPending(t *testing.T) {
	t.Parallel()
	d, rec, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.upgradeRestartPending.Store(true)
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
		t.Fatal("a skipped cycle must not read origin/main")
		return "", nil
	}

	if d.runTierSweep() {
		t.Fatal("a skipped cycle accomplished nothing and must report no verdict, so the job stays due")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages run = %+v, want none while a restart is pending", rec.stages)
	}
	if !strings.Contains(logs.String(), "an upgrade restart is pending; skipping this cycle") {
		t.Fatalf("skip line missing:\n%s", logs.String())
	}
}

// waitTierSweepIdle blocks until the goroutine triggerTierSweep spawned has
// finished its cycle.
func waitTierSweepIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.tierSweepRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("tier_sweep cycle did not finish")
		}
		runtime.Gosched()
	}
}

// A cycle skipped for a pending upgrade restart must not record the hour as
// done: the patrol is still due on the next heartbeat, and once the flag
// clears the next trigger runs the sweep.
func TestTriggerTierSweep_PendingRestartDefersWithoutConsumingTheHour(t *testing.T) {
	t.Parallel()
	d, rec, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.upgradeRestartPending.Store(true)

	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep"); err != nil || found {
		t.Fatalf("last run after a skipped cycle: found=%v err=%v, want none recorded", found, err)
	}
	if dec := evaluatePatrolDue(d.config.TownRoot, "tier_sweep", time.Time{}, d.clk().Now(), tierSweepInterval(d.patrolConfig)); !dec.due {
		t.Fatal("tier_sweep is not due after a skipped cycle; the skip consumed the hour")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none while a restart is pending", rec.stages)
	}

	d.upgradeRestartPending.Store(false)
	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	if len(rec.stages) == 0 {
		t.Fatal("the sweep did not run once the restart cleared")
	}
	if _, found, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep"); err != nil || !found {
		t.Fatalf("last run after the sweep: found=%v err=%v, want it recorded", found, err)
	}
	if n := strings.Count(logs.String(), "an upgrade restart is pending"); n != 1 {
		t.Errorf("skip line logged %d times, want once:\n%s", n, logs.String())
	}
}

// withTierSweepLastRun writes rig's sweep record with the given last run, so a
// test can age a sweep without running one.
func withTierSweepLastRun(t *testing.T, d *Daemon, rig string, lastRun time.Time) {
	t.Helper()
	if err := writeTierSweepState(d.config.TownRoot, rig, tierSweepState{LastRun: lastRun}); err != nil {
		t.Fatal(err)
	}
}

// gt-zvqn8: every landing that changes the binary leaves a restart pending, so
// during a run of landings the flag is set almost continuously. The sweep — the
// only gate over the integration and race tiers — must not wait indefinitely
// for it: past the stale bound it runs anyway, and says why.
func TestRunTierSweep_StaleSweepRunsWhileARestartIsPending(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	withTierSweepLastRun(t, d, "gastown", now.Add(-defaultTierSweepStaleAfter-time.Minute))
	d.upgradeRestartPending.Store(true)

	if !d.runTierSweep() {
		t.Fatal("a sweep past the stale bound must not defer to the pending restart")
	}
	if len(rec.stages) == 0 {
		t.Fatalf("stages = %+v, want the stale sweep to run despite the pending restart", rec.stages)
	}
	if !strings.Contains(logs.String(), "past the 1h30m0s bound; sweeping now rather than waiting out the pending restart") {
		t.Fatalf("no line saying why the sweep ran under a pending restart:\n%s", logs.String())
	}
}

// A sweep inside the bound still defers, and the line carries its age
// (gt-zvqn8).
func TestRunTierSweep_YoungSweepStillDefersToTheRestart(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	withTierSweepLastRun(t, d, "gastown", now.Add(-20*time.Minute))
	d.upgradeRestartPending.Store(true)

	if d.runTierSweep() {
		t.Fatal("a deferred cycle accomplished nothing and must report no verdict, so the job stays due")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages run = %+v, want none inside the stale bound", rec.stages)
	}
	if !strings.Contains(logs.String(), "skipping this cycle (owed sweep 20m0s old, bound 1h30m0s)") {
		t.Fatalf("the skip line does not say how old the owed sweep was:\n%s", logs.String())
	}
}

// gt-v8j5x: a rig whose origin/main is the tip its last green sweep covered is
// skipped by every cycle, so its LastRun goes stale while the patrol runs. Its
// age must not stand for the sweep's age, or it retires the restart's deferral
// and the busy rig's sweep runs into the restart. The age of a rig that does
// owe a sweep is the owed interval (gt-icmdq), so a landing's tip counts from
// the landing, while a record that has been owed past the bound still retires
// the deferral.
func TestRunTierSweep_QuietRigDoesNotRetireTheRestartDeferral(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	const tip = "abc123"
	for _, tc := range []struct {
		name string
		// quietTip answers the quiet rig's origin/main: the green tip, a moved
		// tip, or an error.
		quietTip func(string) (string, error)
		// owedFor is how long ago the record says the quiet rig's owed
		// interval began; zero leaves the record undated.
		owedFor   time.Duration
		wantDefer bool
	}{
		{"quiet rig at the tip", func(string) (string, error) { return tip, nil }, 0, true},
		{"rig that just became owed defers", func(string) (string, error) { return "def456", nil }, 0, true},
		{"rig owed past the bound is still starved", func(string) (string, error) { return "def456", nil }, 3 * time.Hour, false},
		{"rig whose tip cannot be read is still starved", func(string) (string, error) { return "", errors.New("no ref") }, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _, _ := newTierSweepDaemon(t, now, "busy", "quiet")
			d.patrolConfig.Patrols.TierSweep.Rigs = []string{"busy", "quiet"}
			d.tierSweepSeams.localMainSHA = tc.quietTip
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
				t.Error("the restart deferral must not fetch origin/main")
				return "", errors.New("unexpected fetch")
			}
			withTierSweepLastRun(t, d, "busy", now.Add(-10*time.Minute))
			quiet := tierSweepState{LastRun: now.Add(-3 * time.Hour), LastSHA: tip, LastGreenSHA: tip}
			if tc.owedFor > 0 {
				quiet.OwedSince = now.Add(-tc.owedFor)
			}
			if err := writeTierSweepState(d.config.TownRoot, "quiet", quiet); err != nil {
				t.Fatal(err)
			}
			d.upgradeRestartPending.Store(true)

			if got := d.tierSweepDeferForRestart(now); got != tc.wantDefer {
				t.Fatalf("defer = %v, want %v", got, tc.wantDefer)
			}
			if !tc.wantDefer {
				return
			}
			if d.runTierSweep() {
				t.Fatal("a deferred cycle reached no verdict")
			}
			if len(rec.stages) != 0 {
				t.Fatalf("stages run = %+v, want none while the restart defers the sweep", rec.stages)
			}
		})
	}
}

// The bound is the named 90m constant and includes it: a sweep exactly at the
// bound runs, a fresher one defers. A rig with no record has no age to measure
// and defers as it did before the bound existed (gt-zvqn8).
func TestTierSweepDeferForRestart_StaleBound(t *testing.T) {
	t.Parallel()
	if defaultTierSweepStaleAfter != 90*time.Minute {
		t.Fatalf("defaultTierSweepStaleAfter = %s, want 90m", defaultTierSweepStaleAfter)
	}
	now := atHour(time.Now(), 15)
	for _, tc := range []struct {
		name    string
		age     time.Duration
		record  bool
		wantDef bool
	}{
		{"an hour old defers", time.Hour, true, true},
		{"just under the bound defers", defaultTierSweepStaleAfter - time.Second, true, true},
		{"at the bound runs", defaultTierSweepStaleAfter, true, false},
		{"a stale sweep runs", 3 * time.Hour, true, false},
		{"no record defers", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
			if tc.record {
				withTierSweepLastRun(t, d, "gastown", now.Add(-tc.age))
			}
			d.upgradeRestartPending.Store(true)

			if got := d.tierSweepDeferForRestart(now); got != tc.wantDef {
				t.Fatalf("defer = %v, want %v:\n%s", got, tc.wantDef, logs.String())
			}
			if tc.wantDef && !strings.Contains(logs.String(), "an upgrade restart is pending; skipping this cycle") {
				t.Fatalf("a deferred sweep must say why:\n%s", logs.String())
			}
			if !tc.wantDef && !strings.Contains(logs.String(), "sweeping now rather than waiting out the pending restart") {
				t.Fatalf("a sweep that ignored the restart must say why:\n%s", logs.String())
			}
		})
	}
}

// The override moves the bound: with restart_stale_after at 5m, a 10m-old
// sweep runs under a pending restart (gt-zvqn8).
func TestTierSweepDeferForRestart_ConfiguredBound(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.RestartStaleAfterStr = "5m"
	withTierSweepLastRun(t, d, "gastown", now.Add(-10*time.Minute))
	d.upgradeRestartPending.Store(true)

	if d.tierSweepDeferForRestart(now) {
		t.Fatal("a sweep past the configured 5m bound must not defer to the pending restart")
	}
}

// The skip line is written once per pending restart, not on every heartbeat,
// and a later restart logs it again.
func TestTriggerTierSweep_SkipLoggedOncePerPendingRestart(t *testing.T) {
	t.Parallel()
	d, _, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	count := func() int { return strings.Count(logs.String(), "an upgrade restart is pending") }

	d.upgradeRestartPending.Store(true)
	for i := 0; i < 3; i++ {
		d.triggerTierSweep()
		waitTierSweepIdle(t, d)
	}
	if got := count(); got != 1 {
		t.Fatalf("skip line logged %d times over three heartbeats, want 1:\n%s", got, logs.String())
	}

	// The flag clears (a heartbeat sees it clear), then a second restart.
	d.upgradeRestartPending.Store(false)
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	d.upgradeRestartPending.Store(true)
	// The sweep above recorded the hour; step past the interval so it is due,
	// and no further: a sweep past tierSweepStaleAfter runs despite the
	// restart instead of logging the skip (gt-zvqn8).
	d.clock = clockwork.NewFakeClockAt(d.clk().Now().Add(tierSweepInterval(d.patrolConfig) + time.Minute))
	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	if got := count(); got != 2 {
		t.Fatalf("skip line logged %d times across two restarts, want 2:\n%s", got, logs.String())
	}
}

// The line saying why a stale sweep ran under a pending restart is not the
// skip line's to suppress: a restart that first deferred a younger sweep must
// still hear, later on, that the bound retired the deferral (gt-icmdq).
func TestTierSweepStaleRunLogIsNotSuppressedByTheDeferralSkipLatch(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
	withTierSweepLastRun(t, d, "gastown", now.Add(-20*time.Minute))
	d.upgradeRestartPending.Store(true)

	if !d.tierSweepDeferForRestart(now) {
		t.Fatal("a sweep inside the bound must defer to the pending restart")
	}
	if !strings.Contains(logs.String(), "skipping this cycle") {
		t.Fatalf("the deferring cycle said nothing:\n%s", logs.String())
	}

	// The same pending restart, hours on: the sweep is owed past the bound and
	// runs, and the line that says so must reach the log.
	if d.tierSweepDeferForRestart(now.Add(2 * time.Hour)) {
		t.Fatal("a sweep past the stale bound must not defer to the pending restart")
	}
	if !strings.Contains(logs.String(), "past the 1h30m0s bound; sweeping now rather than waiting out the pending restart") {
		t.Fatalf("the deferral's latch swallowed the run line:\n%s", logs.String())
	}
}

// gt-icmdq: a rig caught up at the tip it last swept green keeps a LastRun
// hours old while it stays quiet. The landing that moves its main owes it a
// sweep now, and the pending restart has not starved it: the age is the owed
// interval, so the cycle still defers instead of sweeping at once.
func TestTierSweepAgeCountsFromWhenTheSweepBecameOwed(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "new1", nil }
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
		t.Error("a deferred cycle read origin/main")
		return "", errors.New("unexpected fetch")
	}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-6 * time.Hour), LastSHA: "green0", LastGreenSHA: "green0",
	}); err != nil {
		t.Fatal(err)
	}
	d.upgradeRestartPending.Store(true)

	d.triggerTierSweep()
	waitTierSweepIdle(t, d)

	age, swept, _ := d.tierSweepAge(now)
	if !swept || age >= defaultTierSweepStaleAfter {
		t.Fatalf("owed age = %s (swept = %v), want an interval below the %s bound", age, swept, defaultTierSweepStaleAfter)
	}
	if st := tierSweepTestState(t, d, "gastown"); !st.OwedSince.Equal(now) {
		t.Fatalf("OwedSince = %v, want %v: the read that first sees main move dates the interval", st.OwedSince, now)
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none: the restart defers a sweep that only just became owed", rec.stages)
	}
	if !strings.Contains(logs.String(), "an upgrade restart is pending; skipping this cycle") {
		t.Fatalf("the cycle deferred without saying why:\n%s", logs.String())
	}
}

// TestTierSweepStages_EveryHourRunsEveryTier: the hour of day decides nothing,
// so each of the day's 24 hours plans the shell stage and then the integration
// and race stage, and the cycle's "sweep started" line names all three tiers.
func TestTierSweepStages_EveryHourRunsEveryTier(t *testing.T) {
	t.Parallel()
	for hour := 0; hour < 24; hour++ {
		t.Run(fmt.Sprintf("hour %02d", hour), func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), hour)
			d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
			rec.green()
			sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
			d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

			if !d.runTierSweep() {
				t.Fatal("runTierSweep deferred; want a verdict")
			}
			if len(rec.stages) != 2 ||
				strings.Join(rec.stages[0].tiers, " ") != "shell" ||
				strings.Join(rec.stages[1].tiers, " ") != "integration race" {
				t.Fatalf("stages = %+v, want shell then integration race", rec.stages)
			}
			if want := "sweep started aaaaaaaa (shell, integration, race)"; !strings.Contains(logs.String(), want) {
				t.Errorf("log missing %q; got:\n%s", want, logs.String())
			}
			if got := tierSweepTestState(t, d, "gastown").LastGreenSHA; got != sha {
				t.Errorf("LastGreenSHA = %q, want %s: a green cycle at hour %d was a full cycle", got, sha, hour)
			}
		})
	}
}

func TestRunTierSweep_RunsEveryTierEveryCycle(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15) // odd
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	if got := len(rec.stages); got != 2 {
		t.Fatalf("stages = %+v, want shell then integration", rec.stages)
	}
	if strings.Join(rec.stages[1].tiers, " ") != "integration race" {
		t.Errorf("second stage tiers = %v, want integration race", rec.stages[1].tiers)
	}
	if !rec.stages[1].slot || rec.stages[1].role != "gastown/tier-sweep" {
		t.Errorf("integration stage = %+v, want a slot under gastown/tier-sweep", rec.stages[1])
	}
	for _, st := range rec.stages {
		if strings.Contains(st.command(), "make gate") {
			t.Errorf("stage command %q runs make gate; landings already gate the merged tree", st.command())
		}
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != sha || st.LastGreenSHA != sha {
		t.Errorf("state = %+v, want LastSHA and LastGreenSHA %s after a green cycle", st, sha)
	}

	// With a complete green sweep at this sha, the next cycle is silent.
	rec.stages = nil
	d.clock = clockwork.NewFakeClockAt(atHour(now, 16))
	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred on an unchanged sha")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none: origin/main is unchanged since the last green sweep", rec.stages)
	}

	// A moved sha runs every tier again, at an odd hour too.
	moved := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return moved, nil }
	d.clock = clockwork.NewFakeClockAt(atHour(now, 17))
	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred after a moved sha")
	}
	if len(rec.stages) != 2 {
		t.Fatalf("stages after a moved sha = %+v, want shell then integration", rec.stages)
	}
	st = tierSweepTestState(t, d, "gastown")
	if st.LastSHA != moved || st.LastGreenSHA != moved {
		t.Errorf("state = %+v, want LastSHA and LastGreenSHA %s", st, moved)
	}
}

func TestRunTierSweep_TimeoutIsRedWithTheReasonNeverGreen(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		return tierSweepStageResult{ran: true, exitCode: -1, timedOut: true}
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	d.runTierSweep()

	ts := tierSweepTestState(t, d, "gastown").Tiers["shell"]
	if ts.Verdict != tierSweepRed {
		t.Fatalf("verdict = %q, want RED for a stage killed by its budget", ts.Verdict)
	}
	if len(ts.FailedNames) != 1 || ts.FailedNames[0] != "timeout" {
		t.Errorf("failed names = %v, want [timeout]", ts.FailedNames)
	}
}

func TestRunTierSweep_AStageThatNeverRanDefersInsteadOfRed(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	// The stage never started: a slot the sweep could not take is not a verdict
	// about the tree, so the cycle must defer rather than record a red.
	rec.result = func(tierSweepStage) tierSweepStageResult {
		return tierSweepStageResult{err: context.DeadlineExceeded}
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	if d.runTierSweep() {
		t.Error("runTierSweep reached a verdict; want a defer")
	}
	if _, err := os.Stat(tierSweepStatePath(d.config.TownRoot, "gastown")); !os.IsNotExist(err) {
		t.Errorf("a sweep record was written for a cycle that ran nothing: %v", err)
	}
}

func TestRunTierSweep_FilesOneBeadPerFailingUnitAndClosesOnGreen(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, bd, _ := newTierSweepDaemon(t, now, "gastown")
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		var b strings.Builder
		for _, tier := range st.tiers {
			fmt.Fprintf(&b, "tier-sweep: %s RED passed=1 failed=2 skipped=0 failed: ./internal/cmd ./internal/daemon (logs /tmp/x)\n", tier)
		}
		return tierSweepStageResult{output: b.String(), exitCode: 1, ran: true}
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	d.runTierSweep()

	// Two failing packages in each of the shell, integration and race tiers.
	if len(bd.created) != 6 {
		t.Fatalf("created = %+v, want one bead per failing package per tier", bd.created)
	}
	// The tiers are filed from a map, so the order is not fixed.
	titles := map[string]bool{}
	for _, c := range bd.created {
		titles[c.Title] = true
		if len(c.Labels) != 1 || c.Labels[0] != tierSweepRedLabel {
			t.Errorf("labels = %v, want %s", c.Labels, tierSweepRedLabel)
		}
	}
	for _, tier := range []string{"shell", "integration", "race"} {
		for _, pkg := range []string{"./internal/cmd", "./internal/daemon"} {
			if want := "tier sweep (gastown): " + tier + ": " + pkg; !titles[want] {
				t.Errorf("no bead titled %q; got %v", want, titles)
			}
		}
	}

	// A second red run comments on the open beads instead of refiling them.
	bd.created = nil
	bd.open = []*beads.Issue{
		{ID: "gt-1", Title: "tier sweep (gastown): shell: ./internal/cmd"},
		{ID: "gt-2", Title: "tier sweep (gastown): shell: ./internal/daemon"},
		{ID: "gt-3", Title: "tier sweep (gastown): integration: ./internal/cmd"},
		{ID: "gt-4", Title: "tier sweep (gastown): integration: ./internal/daemon"},
		{ID: "gt-5", Title: "tier sweep (gastown): race: ./internal/cmd"},
		{ID: "gt-6", Title: "tier sweep (gastown): race: ./internal/daemon"},
	}
	d.runTierSweep()
	if len(bd.created) != 0 {
		t.Errorf("created = %+v, want comments on the open beads instead", bd.created)
	}
	if len(bd.comments) != 6 {
		t.Errorf("comments = %v, want one per open bead", bd.comments)
	}

	// Green closes the tier's open beads.
	rec.green()
	bd.comments, bd.open = nil, []*beads.Issue{
		{ID: "gt-1", Title: "tier sweep (gastown): shell: ./internal/cmd"},
		{ID: "gt-9", Title: "tier sweep (gastown): integration: ./internal/cmd"},
	}
	d.runTierSweep()
	if len(bd.closed) != 2 {
		t.Fatalf("closed = %v, want the shell and integration tiers' beads", bd.closed)
	}
	for _, id := range []string{"gt-1 ", "gt-9 "} {
		if !strings.HasPrefix(bd.closed[0], id) && !strings.HasPrefix(bd.closed[1], id) {
			t.Errorf("closed = %v, want %sclosed", bd.closed, id)
		}
	}
}

// A polecat that claims a red-tier bead moves it to in_progress or hooked; the
// next red sweep still comments on it instead of filing a second P1 bead, and
// a green sweep closes it (gt-9hzs5).
func TestTierSweepFileRed_ClaimedBead(t *testing.T) {
	t.Parallel()
	title := tierSweepBeadPrefix("gastown", "integration") + "./internal/x"
	for _, status := range []string{"open", "in_progress", "hooked"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
			bd := &byStatusBeads{byStatus: map[string][]*beads.Issue{
				status: {{ID: "gt-claimed", Title: title}},
			}}
			d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

			red := map[string]tierSweepTierResult{
				"integration": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"./internal/x"}},
			}
			d.tierSweepRecordBeads("gastown", "abc123", red)
			if len(bd.created) != 0 {
				t.Errorf("created = %+v, want a comment on the %s bead instead", bd.created, status)
			}
			if len(bd.comments) != 1 || !strings.HasPrefix(bd.comments[0], "gt-claimed: still red") {
				t.Errorf("comments = %v, want one \"still red\" comment on gt-claimed", bd.comments)
			}

			green := map[string]tierSweepTierResult{"integration": {Verdict: tierSweepGreen, Passed: 1}}
			d.tierSweepRecordBeads("gastown", "def456", green)
			if len(bd.closed) != 1 || !strings.HasPrefix(bd.closed[0], "gt-claimed ") {
				t.Errorf("closed = %v, want the %s bead closed on green", bd.closed, status)
			}
		})
	}
}

// failingStatusBeads answers List from byStatus and fails for the statuses in
// failStatus, so a lookup that reached one bead before the error is a partial one.
type failingStatusBeads struct {
	byStatusBeads
	failStatus string
}

func (b *failingStatusBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Status == b.failStatus {
		return nil, fmt.Errorf("bd list --status=%s: boom", opts.Status)
	}
	return b.byStatusBeads.List(opts)
}

// A status list that fails leaves the sweep unable to tell which units already
// have a bead, so the tier files, comments and closes nothing; the open bead it
// did find is not duplicated (gt-7useh).
func TestTierSweepOpenBeads_ListErrorFilesNoDuplicate(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	bd := &failingStatusBeads{
		byStatusBeads: byStatusBeads{byStatus: map[string][]*beads.Issue{
			"open": {{ID: "gt-open", Title: tierSweepBeadPrefix("gastown", "integration") + "./internal/x"}},
		}},
		failStatus: beads.StatusHooked,
	}
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Failed: 2, FailedNames: []string{"./internal/x", "./internal/y"}},
	}
	d.tierSweepRecordBeads("gastown", "abc123", red)
	if len(bd.created) != 0 {
		t.Errorf("created = %+v, want nothing filed from an unknown state", bd.created)
	}

	green := map[string]tierSweepTierResult{"integration": {Verdict: tierSweepGreen, Passed: 1}}
	d.tierSweepRecordBeads("gastown", "def456", green)
	if len(bd.closed) != 0 {
		t.Errorf("closed = %v, want nothing closed from a partial listing", bd.closed)
	}
}

// TestTierSweepSlotRoleIsNotGateClass pins the reentrancy contract: the
// sweep's role is not gate-class, so it takes a shared slot and the script's
// own `gt slot run` calls inherit that role and ride the one hold instead of
// competing for a slot each. It is full-suite class, so the sweep's whole-tree
// run counts against the pool's full-suite cap and a second one waits for it
// rather than piling onto the host (gt-dhcmp, acceptance 3).
func TestTierSweepSlotRoleIsNotGateClass(t *testing.T) {
	t.Parallel()
	stages := tierSweepStages("gastown")
	last := stages[len(stages)-1]
	if !last.slot {
		t.Fatal("the integration stage must hold the container-gate slot")
	}
	if last.role != "gastown/tier-sweep" {
		t.Errorf("role = %q, want gastown/tier-sweep", last.role)
	}
	if slot.IsGateRole(last.role) {
		t.Errorf("role %q is gate-class; the sweep must not take a gate-reserved slot", last.role)
	}
	if !slot.IsFullSuiteRole(last.role) {
		t.Errorf("role %q is not full-suite class; the sweep's whole-tree run would not take the cap", last.role)
	}
}

func TestTierSweepIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "tier_sweep") {
		t.Error("tier_sweep must be off with no config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "tier_sweep") {
		t.Error("tier_sweep must be off with no tier_sweep entry")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "tier_sweep") {
		t.Error("tier_sweep enabled:true must be on")
	}
}

func TestTierSweepDefaultsToOneHourAndGastown(t *testing.T) {
	t.Parallel()
	if got := tierSweepInterval(nil); got != time.Hour {
		t.Errorf("default interval = %v, want 1h", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{IntervalStr: "30m"}}}
	if got := tierSweepInterval(cfg); got != 30*time.Minute {
		t.Errorf("configured interval = %v, want 30m", got)
	}
	known := []string{"gastown", "otherrig"}
	if got := tierSweepRigs(nil, known); len(got) != 1 || got[0] != "gastown" {
		t.Errorf("default rigs = %v, want [gastown]", got)
	}
	cfg.Patrols.TierSweep.Rigs = []string{"otherrig"}
	if got := tierSweepRigs(cfg, known); len(got) != 1 || got[0] != "otherrig" {
		t.Errorf("configured rigs = %v, want [otherrig]", got)
	}
}

func TestTriggerTierSweep_IsOptIn(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}

	d.triggerTierSweep()
	// triggerTierSweep spawns a goroutine; with the patrol off it must not have
	// started one at all.
	if d.tierSweepRunning.Load() {
		t.Error("the sweep started with tier_sweep absent from the config")
	}
	if len(rec.stages) != 0 {
		t.Errorf("stages = %+v, want none with the patrol off", rec.stages)
	}
}

func TestParseTierSweepSummaries(t *testing.T) {
	t.Parallel()
	got := parseTierSweepSummaries(strings.Join([]string{
		"tier-sweep: shell ...",
		"tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.aB12) in 2m14s",
		"tier-sweep: integration GREEN passed=29 failed=0 skipped=2 in 45s",
		"tier-sweep: nonexistent tier RED",
	}, "\n"))
	if len(got) != 2 {
		t.Fatalf("summaries = %+v, want shell and integration", got)
	}
	sh := got["shell"]
	if sh.Verdict != tierSweepRed || sh.Passed != 5 || sh.Failed != 1 || len(sh.FailedNames) != 1 || sh.FailedNames[0] != "scripts/x.sh" {
		t.Errorf("shell = %+v", sh)
	}
	in := got["integration"]
	if in.Verdict != tierSweepGreen || in.Passed != 29 || len(in.FailedNames) != 0 {
		t.Errorf("integration = %+v", in)
	}
}

// TestRunTierSweep_LogsTheElapsedTimeOfTheSweep: a slow sweep must read as
// slow in the daemon log without opening anything (gt-iqzr0). The clock is the
// injected fake one and the stage seam advances it, so the test waits on no
// real time.
func TestRunTierSweep_LogsTheElapsedTimeOfTheSweep(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	clk := clockwork.NewFakeClockAt(now)
	d.clock = clk
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		// The stage's wall: 2m14s for the shell tier, the daemon log's own
		// rounding style.
		clk.Advance(2*time.Minute + 14*time.Second)
		var b strings.Builder
		for _, tier := range st.tiers {
			fmt.Fprintf(&b, "tier-sweep: %s GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 2m14s\n", tier)
		}
		return tierSweepStageResult{output: b.String(), exitCode: 0, ran: true}
	}
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	out := logs.String()
	for _, want := range []string{
		"tier_sweep: gastown: shell finished (exit 0) in 2m14s;",
		"tier_sweep: gastown: integration finished (exit 0) in 2m14s;",
		"tier_sweep: gastown: swept aaaaaaaa (shell GREEN, integration GREEN, race GREEN) in 4m28s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}
}

// TestRunTierSweep_LogsSweepStarted: the cycle's start names the rig's sha and
// every tier the cycle will run, so the dashboard's Tier sweeps pane reads an
// exact running state instead of inferring it (gt-rntre).
func TestRunTierSweep_LogsSweepStarted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hour int
		want string
	}{
		{"odd hour runs shell then the integration suite", 15, "tier_sweep: gastown: sweep started aaaaaaaa (shell, integration, race)"},
		{"even hour runs shell then the integration suite", 14, "tier_sweep: gastown: sweep started aaaaaaaa (shell, integration, race)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), tc.hour)
			d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
			rec.green()
			sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
			d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

			if !d.runTierSweep() {
				t.Fatal("runTierSweep deferred; want a verdict")
			}
			if out := logs.String(); !strings.Contains(out, tc.want) {
				t.Errorf("log missing %q; got:\n%s", tc.want, out)
			}
		})
	}
}

// TestRunTierSweep_SkippedSweepLogsNoDuration: nothing ran, so there is no
// elapsed time to report and the skip line must not imply one (gt-iqzr0).
func TestRunTierSweep_SkippedSweepLogsNoDuration(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	// The previous cycle was green at this sha, so there is nothing to sweep.
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{LastGreenSHA: sha}); err != nil {
		t.Fatal(err)
	}
	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a skip")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none: origin/main is unchanged", rec.stages)
	}
	out := logs.String()
	if !strings.Contains(out, "unchanged since the last green sweep; skipping") {
		t.Fatalf("log missing the skip line; got:\n%s", out)
	}
	if strings.Contains(out, "swept ") || strings.Contains(out, "finished (exit") {
		t.Errorf("a skipped sweep logged a stage or sweep line; got:\n%s", out)
	}
	if strings.Contains(out, "sweep started") {
		t.Errorf("a skipped cycle logged a start line; got:\n%s", out)
	}
}

// TestRunTierSweep_DeferredSweepLogsNoDuration: no stage ran, so the cycle is
// deferred and its line carries no elapsed time (gt-iqzr0).
func TestRunTierSweep_DeferredSweepLogsNoDuration(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	rec.result = func(tierSweepStage) tierSweepStageResult {
		return tierSweepStageResult{err: context.DeadlineExceeded}
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	if d.runTierSweep() {
		t.Error("runTierSweep reached a verdict; want a defer")
	}
	out := logs.String()
	if !strings.Contains(out, "did not run") {
		t.Fatalf("log missing the defer line; got:\n%s", out)
	}
	if strings.Contains(out, "swept ") || strings.Contains(out, "finished (exit") {
		t.Errorf("a deferred sweep logged a stage or sweep line; got:\n%s", out)
	}
}

// tierSweepCoverageTown writes the files TierSweepCoverageFor reads: a
// mayor/rigs.json naming rigs, and, when patrols is non-empty, a
// mayor/daemon.json whose patrols object is that JSON fragment.
func tierSweepCoverageTown(t *testing.T, patrols string, rigs ...string) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := make([]string, 0, len(rigs))
	for _, r := range rigs {
		entries = append(entries, `"`+r+`": {}`)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(`{"rigs":{`+strings.Join(entries, ",")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if patrols != "" {
		body := `{"type":"daemon-patrol-config","version":1,"patrols":{` + patrols + `}}`
		if err := os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// The sweep's coverage and its last green commit come from the town's patrol
// config and the rig's record, and a rig the sweep runs is not the same thing
// as a rig the town knows (gt-qk0pi).
func TestTierSweepCoverageForReadsTheConfigAndTheRecord(t *testing.T) {
	t.Parallel()
	town := tierSweepCoverageTown(t, `"tier_sweep":{"enabled":true,"rigs":["gastown"]}`, "gastown", "other")
	if err := writeTierSweepState(town, "gastown", tierSweepState{LastGreenSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}

	got, err := TierSweepCoverageFor(town, "gastown")
	if err != nil {
		t.Fatalf("TierSweepCoverageFor: %v", err)
	}
	if !got.Covered || got.LastGreenSHA != "abc123" {
		t.Errorf("coverage = %+v, want gastown covered at abc123", got)
	}
	if got, err := TierSweepCoverageFor(town, "other"); err != nil || got.Covered {
		t.Errorf("other = %+v (%v), want not covered", got, err)
	}
}

// A covered rig with no record yet is covered with no green commit, which is
// what makes every commit unpromotable.
func TestTierSweepCoverageForACoveredRigWithNoRecord(t *testing.T) {
	t.Parallel()
	town := tierSweepCoverageTown(t, `"tier_sweep":{"enabled":true}`, "gastown")
	got, err := TierSweepCoverageFor(town, "gastown")
	if err != nil {
		t.Fatalf("TierSweepCoverageFor: %v", err)
	}
	if !got.Covered || got.LastGreenSHA != "" {
		t.Errorf("coverage = %+v, want gastown covered with no green commit", got)
	}
}

// Coverage needs the patrol on, the rig among the sweep's rigs, and the rig
// known to the town. Anything less is "not covered", which leaves the
// promotion to the caller.
func TestTierSweepCoverageForIsOffUnlessTheSweepRunsTheRig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		patrols string
		rigs    []string
		rig     string
	}{
		{"sweep disabled", `"tier_sweep":{"enabled":false}`, []string{"gastown"}, "gastown"},
		{"no tier_sweep key", `"landing_worker":{"enabled":true}`, []string{"gastown"}, "gastown"},
		{"rig not among the sweep's rigs", `"tier_sweep":{"enabled":true,"rigs":["gastown"]}`, []string{"gastown", "other"}, "other"},
		{"rig the town does not know", `"tier_sweep":{"enabled":true,"rigs":["ghost"]}`, []string{"gastown"}, "ghost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := tierSweepCoverageTown(t, tc.patrols, tc.rigs...)
			got, err := TierSweepCoverageFor(town, tc.rig)
			if err != nil {
				t.Fatalf("TierSweepCoverageFor: %v", err)
			}
			if got.Covered {
				t.Errorf("coverage = %+v, want not covered", got)
			}
		})
	}

	// No daemon.json at all is "no patrol config", the same as the sweep being
	// off (IsPatrolEnabled's opt-in default).
	got, err := TierSweepCoverageFor(tierSweepCoverageTown(t, "", "gastown"), "gastown")
	if err != nil {
		t.Fatalf("TierSweepCoverageFor with no daemon.json: %v", err)
	}
	if got.Covered {
		t.Errorf("coverage = %+v, want not covered with no daemon.json", got)
	}
}

// A town whose disabled_patrols names the sweep is not covered, the same
// input the daemon's own predicate reads.
func TestTierSweepCoverageForHonoursTheTownDisabledList(t *testing.T) {
	t.Parallel()
	town := tierSweepCoverageTown(t, `"tier_sweep":{"enabled":true}`, "gastown")
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"disabled_patrols":["tier_sweep"]}`
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := TierSweepCoverageFor(town, "gastown")
	if err != nil {
		t.Fatalf("TierSweepCoverageFor: %v", err)
	}
	if got.Covered {
		t.Errorf("coverage = %+v, want not covered while the town disables the sweep", got)
	}
}

// A patrol config that does not parse is an error, never "not covered": the
// guard refuses to publish on this read, so it must not read a broken config
// as a town that promotes freely.
func TestTierSweepCoverageForRefusesAnUnreadableConfig(t *testing.T) {
	t.Parallel()
	town := tierSweepCoverageTown(t, "", "gastown")
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"bogus":true}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := TierSweepCoverageFor(town, "gastown"); err == nil {
		t.Error("TierSweepCoverageFor accepted a patrol config that does not parse")
	}
}

// The daemon's own predicate and the reader a promotion is guarded by answer
// from one rule, so they cannot drift (gt-qk0pi).
func TestTierSweepCoversRigAgreesWithTierSweepCoverageFor(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown", "other")
	town := d.config.TownRoot
	patrols := `"tier_sweep":{"enabled":true,"rigs":["gastown"]}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "daemon.json"), []byte(`{"type":"daemon-patrol-config","version":1,"patrols":{`+patrols+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{TierSweep: &TierSweepConfig{Enabled: true, Rigs: []string{"gastown"}}}}

	for _, rig := range []string{"gastown", "other"} {
		got, err := TierSweepCoverageFor(town, rig)
		if err != nil {
			t.Fatalf("TierSweepCoverageFor(%s): %v", rig, err)
		}
		if got.Covered != d.tierSweepCoversRig(rig) {
			t.Errorf("rig %s: TierSweepCoverageFor covered=%v, daemon predicate=%v", rig, got.Covered, d.tierSweepCoversRig(rig))
		}
	}
}

// gt-icmdq: the record's verdicts and the failing names on them belong to the
// tip LastSHA names — the pair the hour report prints and the dashboard matches
// a row's names to by sha. A cycle that could not finish records the verdicts
// it took under the tip it took them at and marks the record partial, so the
// RED it saw is kept, nothing is merged under the older sha, and the tip it
// never covered keeps owing a cycle.
func TestTierSweepRigCycleDeferredKeepsFailedNamesWithTheSHA(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, bd, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "new2", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "red1", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"TestSomethingInCmd"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The shell stage runs RED at the new tip, naming the unit that failed, and
	// the slot stage never starts, so the cycle defers.
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{err: errors.New("slot busy")}
		}
		return tierSweepStageResult{
			output:   "tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: TestDeferredThing (logs /tmp/tier-sweep.x)\n",
			exitCode: 1, ran: true,
		}
	}
	if d.runTierSweep() {
		t.Fatal("a partial cycle reached a verdict")
	}

	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "new2" || !st.Partial {
		t.Fatalf("record = %+v, want the deferred cycle's own tip new2, marked partial", st)
	}
	got := st.Tiers["shell"]
	if got.Verdict != tierSweepRed || len(got.FailedNames) != 1 || got.FailedNames[0] != "TestDeferredThing" {
		t.Fatalf("shell verdict = %+v, want the RED the deferred cycle took, with the name it carries: the sha beside it must name the results", got)
	}
	if _, ok := st.Tiers["integration"]; ok {
		t.Fatalf("tiers = %+v, want no integration verdict: that stage never ran", st.Tiers)
	}

	// The deferred cycle's own RED carries its names and the sha it was taken
	// at, in the bead it files.
	if len(bd.created) != 1 {
		t.Fatalf("beads = %+v, want one for the RED the deferred cycle saw", bd.created)
	}
	if want := "tier sweep (gastown): shell: TestDeferredThing"; bd.created[0].Title != want {
		t.Errorf("bead title = %q, want %q", bd.created[0].Title, want)
	}
	if !strings.Contains(bd.created[0].Description, "new2") {
		t.Errorf("bead = %q, want it filed against the tip the RED was taken at", bd.created[0].Description)
	}
}

// tierSweepItems is the attention the tick would raise for rig, read from the
// record the cycle left through the collector's own source (gt-euxf1).
func tierSweepItems(t *testing.T, d *Daemon, now time.Time, rig string) []attention.Item {
	t.Helper()
	src := &attentionSources{
		d:             d,
		now:           now,
		tierSweepRigs: func() []string { return []string{rig} },
		tierSweep:     func(r string) (tierSweepState, error) { return readTierSweepState(d.config.TownRoot, r) },
	}
	items, err := src.collectTierSweepRed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// tierSweepVerdictsAt renders a recorded verdict map as "tier=VERDICT@sha"
// lines the test's want can be written beside.
func tierSweepVerdictsAt(tiers map[string]tierSweepTierResult) string {
	lines := make([]string, 0, len(tiers))
	for tier, res := range tiers {
		lines = append(lines, fmt.Sprintf("%s=%s@%s", tier, res.Verdict, res.SHA))
	}
	sort.Strings(lines)
	return strings.Join(lines, " ")
}

// gt-icmdq and gt-euxf1 both turn on this map: every verdict a cycle takes
// carries that cycle's tip, a tier the cycle did not reach keeps the earlier
// verdict and the tip it was taken at, and a tier nobody will run again is
// dropped rather than carried forever.
func TestTierSweepRecordedTiers(t *testing.T) {
	t.Parallel()
	stages := []tierSweepStage{{tiers: []string{"shell"}}, {tiers: []string{"integration", "race"}}}
	prior := tierSweepState{LastSHA: "tipA", Tiers: map[string]tierSweepTierResult{
		"shell":       {Verdict: tierSweepGreen, SHA: "tipA"},
		"integration": {Verdict: tierSweepRed, SHA: "tipA"},
		"retired":     {Verdict: tierSweepRed, SHA: "tipA"},
	}}
	tests := []struct {
		name     string
		prior    tierSweepState
		results  map[string]tierSweepTierResult
		deferred bool
		want     string
	}{{
		name:  "a full cycle keeps only its own verdicts",
		prior: prior,
		results: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepGreen}, "integration": {Verdict: tierSweepGreen}, "race": {Verdict: tierSweepRed},
		},
		want: "integration=GREEN@tipB race=RED@tipB shell=GREEN@tipB",
	}, {
		name:     "a partial cycle keeps the earlier verdict for the tiers it never reached",
		prior:    prior,
		results:  map[string]tierSweepTierResult{"shell": {Verdict: tierSweepGreen}},
		deferred: true,
		want:     "integration=RED@tipA shell=GREEN@tipB",
	}, {
		// The carried verdict is whatever the tier last came back as: a GREEN
		// the cycle never reached stays GREEN at the tip that took it.
		name: "a partial cycle keeps an earlier GREEN the same way",
		prior: tierSweepState{LastSHA: "tipA", Tiers: map[string]tierSweepTierResult{
			"integration": {Verdict: tierSweepGreen, SHA: "tipA"},
		}},
		results:  map[string]tierSweepTierResult{"shell": {Verdict: tierSweepGreen}},
		deferred: true,
		want:     "integration=GREEN@tipA shell=GREEN@tipB",
	}, {
		// The upgrade boundary: a record written before a verdict carried its
		// own tip has none, and the record's LastSHA is the tip every tier in
		// it was judged at.
		name: "a carried verdict from a record with no per-tier tips takes that record's own",
		prior: tierSweepState{LastSHA: "tipA", Tiers: map[string]tierSweepTierResult{
			"integration": {Verdict: tierSweepRed},
		}},
		results:  map[string]tierSweepTierResult{"shell": {Verdict: tierSweepGreen}},
		deferred: true,
		want:     "integration=RED@tipA shell=GREEN@tipB",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tierSweepRecordedTiers(tc.results, tc.prior, stages, "tipB", tc.deferred)
			if rendered := tierSweepVerdictsAt(got); rendered != tc.want {
				t.Errorf("recorded = %s, want %s", rendered, tc.want)
			}
		})
	}
}

// gt-euxf1: a cycle that stopped early learned nothing about the stages it never
// reached, so the verdict the previous cycle took for those tiers stands — and
// the attention item for a RED one stands with it, dated at the tip that verdict
// was taken at. Dropping the tier read as a clear the sweep never took: the item
// retired while the red beads for that tier stayed open.
func TestTierSweepRigCycle_PartialCycleKeepsPriorRedTier(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tipB", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "tipA", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell":       {Verdict: tierSweepGreen, Passed: 3, SHA: "tipA"},
			"integration": {Verdict: tierSweepRed, Passed: 12, Failed: 1, FailedNames: []string{"TestThing"}, SHA: "tipA"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// tipB's shell stage runs GREEN; the stage that carries integration and
	// race never starts, so the cycle defers.
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{err: errors.New("slot busy")}
		}
		return tierSweepStageResult{
			output:   "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n",
			exitCode: 0, ran: true,
		}
	}
	if d.runTierSweep() {
		t.Fatal("a partial cycle reached a verdict")
	}

	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "tipB" || !st.Partial {
		t.Fatalf("record = %+v, want the deferred cycle's own tip tipB, marked partial", st)
	}
	if shell := st.Tiers["shell"]; shell.Verdict != tierSweepGreen || shell.SHA != "tipB" {
		t.Errorf("shell = %+v, want the GREEN this cycle took at tipB", shell)
	}
	got := st.Tiers["integration"]
	if got.Verdict != tierSweepRed || got.SHA != "tipA" {
		t.Fatalf("integration = %+v, want tipA's RED carried forward under the tip it was taken at", got)
	}
	if _, ok := st.Tiers["race"]; ok {
		t.Errorf("tiers = %+v, want no race verdict: no cycle has taken one", st.Tiers)
	}

	items := tierSweepItems(t, d, now, "gastown")
	if len(items) != 1 || items[0].Key != "sweep:gastown:integration" {
		t.Fatalf("items = %+v, want the carried RED's item still raised", items)
	}
	if items[0].SHA != "tipA" {
		t.Errorf("item = %+v, want it dated at the tip its verdict was taken at", items[0])
	}
}

// The upgrade boundary a carried verdict crosses: a record written before a
// verdict carried its own tip holds none, and the carry-over must date those
// tiers at the record's tip rather than at the cycle that did not run them. An
// empty tip is what the readers fall back to the newest tip for, which is the
// misdating gt-euxf1 is about.
func TestTierSweepRigCycle_CarriedLegacyVerdictKeepsTheRecordsTip(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tipB", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	// A record as an older daemon wrote it: the tiers carry no tip of their own.
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "tipA", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"integration": {Verdict: tierSweepRed, Passed: 12, Failed: 1, FailedNames: []string{"TestThing"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{err: errors.New("slot busy")}
		}
		return tierSweepStageResult{
			output:   "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n",
			exitCode: 0, ran: true,
		}
	}
	if d.runTierSweep() {
		t.Fatal("a partial cycle reached a verdict")
	}

	st := tierSweepTestState(t, d, "gastown")
	got := st.Tiers["integration"]
	if got.Verdict != tierSweepRed || got.SHA != "tipA" {
		t.Fatalf("integration = %+v, want the carried RED dated at tipA, the tip its record judged it at", got)
	}
	if items := tierSweepItems(t, d, now, "gastown"); len(items) != 1 || items[0].SHA != "tipA" {
		t.Errorf("items = %+v, want the carried RED's item dated at tipA", items)
	}
}

// The other half of gt-euxf1: a cycle that does run the tier replaces the
// carried verdict with its own and the item clears, so the carry-over cannot
// hold a tier red after the sweep has evidence it is not.
func TestTierSweepRigCycle_GreenCycleReplacesTheCarriedRedTier(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tipB", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "tipA", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell":       {Verdict: tierSweepGreen, Passed: 3, SHA: "tipA"},
			"integration": {Verdict: tierSweepRed, Passed: 12, Failed: 1, FailedNames: []string{"TestThing"}, SHA: "tipA"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	rec.green()
	if !d.runTierSweep() {
		t.Fatal("a cycle that ran every stage green deferred")
	}

	st := tierSweepTestState(t, d, "gastown")
	if st.Partial {
		t.Fatalf("record = %+v, want a cycle that ran every stage", st)
	}
	if got := st.Tiers["integration"]; got.Verdict != tierSweepGreen || got.SHA != "tipB" {
		t.Fatalf("integration = %+v, want the GREEN this cycle took at tipB", got)
	}
	if items := tierSweepItems(t, d, now, "gastown"); len(items) != 0 {
		t.Errorf("items = %+v, want the carried RED cleared by the GREEN that ran", items)
	}
}

// gt-icmdq: a deferred cycle learned nothing about the stages it could not run,
// so it must not restart the owed clock. A rig the sweep keeps partly running
// still ages past the stale bound, and the pending restart stops deferring it.
func TestTierSweepDeferredCycleDoesNotRestartTheOwedClock(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "new2", nil }
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "new2", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	owedSince := now.Add(-2 * time.Hour)
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: owedSince, LastSHA: "red1", LastGreenSHA: "green0", OwedSince: owedSince,
		Tiers: map[string]tierSweepTierResult{},
	}); err != nil {
		t.Fatal(err)
	}

	// The shell stage runs green and the slot stage never starts, so the cycle
	// defers with a partial record.
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{err: errors.New("slot busy")}
		}
		return tierSweepStageResult{
			output:   "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n",
			exitCode: 0, ran: true,
		}
	}
	if d.runTierSweep() {
		t.Fatal("a partial cycle reached a verdict")
	}

	st := tierSweepTestState(t, d, "gastown")
	if !st.OwedSince.Equal(owedSince) {
		t.Fatalf("OwedSince = %v after a deferred cycle, want the unchanged %v", st.OwedSince, owedSince)
	}
	d.upgradeRestartPending.Store(true)
	if age, swept, _ := d.tierSweepAge(now); !swept || age < defaultTierSweepStaleAfter {
		t.Fatalf("owed age = %s (swept = %v), want past the %s bound", age, swept, defaultTierSweepStaleAfter)
	}
	if d.tierSweepDeferForRestart(now) {
		t.Fatal("a sweep owed past the stale bound must not defer to the pending restart")
	}
}

// gt-icmdq: a rig that fails every cycle owes a sweep the whole time, so a full
// cycle that ends RED must not restamp OwedSince. Restamping would hold the age
// under the stale bound for good, and the pending restart would defer the sweep
// forever.
func TestTierSweepRedCycleDoesNotRestartTheOwedClock(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "red2", nil }
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "red2", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	owedSince := now.Add(-2 * time.Hour)
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: owedSince, LastSHA: "red1", LastGreenSHA: "green0", OwedSince: owedSince,
	}); err != nil {
		t.Fatal(err)
	}

	// Every stage runs (the recorder defaults to no verdict line, which reads
	// as RED), so the cycle reached a verdict and the rig still owes a sweep.
	if !d.runTierSweep() {
		t.Fatal("a cycle that ran every stage reached no verdict")
	}

	st := tierSweepTestState(t, d, "gastown")
	if !st.OwedSince.Equal(owedSince) {
		t.Fatalf("OwedSince = %v after a RED cycle, want the unchanged %v: only a green cycle ends the interval", st.OwedSince, owedSince)
	}
	d.upgradeRestartPending.Store(true)
	if age, swept, _ := d.tierSweepAge(now); !swept || age < defaultTierSweepStaleAfter {
		t.Fatalf("owed age = %s (swept = %v), want past the %s bound", age, swept, defaultTierSweepStaleAfter)
	}
	if d.tierSweepDeferForRestart(now) {
		t.Fatal("a sweep owed past the stale bound must not defer to the pending restart")
	}
}

// gt-icmdq: measuring the owed age dates nothing. The caller writes the stamp
// for the rigs the read saw start owing a sweep, so a read stays a read and the
// record has one writer on each path that touches it.
func TestTierSweepAgeReadsWithoutDatingTheOwedClock(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "new1", nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-6 * time.Hour), LastSHA: "green0", LastGreenSHA: "green0",
	}); err != nil {
		t.Fatal(err)
	}

	age, swept, owed := d.tierSweepAge(now)
	if !swept || age != 0 {
		t.Fatalf("owed age = %s (swept = %v), want the interval dated from now", age, swept)
	}
	if len(owed) != 1 || owed[0].rig != "gastown" || !owed[0].since.Equal(now) {
		t.Fatalf("owed = %+v, want the rig whose interval began at this read", owed)
	}
	if st := tierSweepTestState(t, d, "gastown"); !st.OwedSince.IsZero() {
		t.Fatalf("OwedSince = %v after the read alone, want the record untouched", st.OwedSince)
	}

	d.upgradeRestartPending.Store(true)
	d.tierSweepDeferForRestart(now)
	if st := tierSweepTestState(t, d, "gastown"); !st.OwedSince.Equal(now) {
		t.Fatalf("OwedSince = %v, want %v: the deferral check dates what the read saw", st.OwedSince, now)
	}
}

// gt-o3hb4: a record written before the owed stamp existed carries none, and
// what the deferral check dates it from is whatever the read could see. A
// record with no green sweep has only its last run, and that start is the
// record's own from then on; a record whose tip cannot be read is left undated,
// because a tip that cannot be read says nothing about whether a sweep is owed.
func TestTierSweepDeferForRestartDatesAPreStampRecord(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	const lastRun = 2 * time.Hour

	t.Run("no green sweep dates it from the last run", func(t *testing.T) {
		t.Parallel()
		d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
		withTierSweepLastRun(t, d, "gastown", now.Add(-lastRun))
		d.upgradeRestartPending.Store(true)

		if d.tierSweepDeferForRestart(now) {
			t.Fatal("a sweep owed past the stale bound must run rather than defer")
		}
		st := tierSweepTestState(t, d, "gastown")
		if !st.OwedSince.Equal(now.Add(-lastRun)) {
			t.Fatalf("OwedSince = %v, want the last run the age was measured from", st.OwedSince)
		}

		// Dated: the age moves with the clock from that start, and there is
		// nothing left for a later read to date.
		if age, swept, owed := d.tierSweepAge(now.Add(30 * time.Minute)); !swept || age != lastRun+30*time.Minute || len(owed) != 0 {
			t.Fatalf("age = %s (swept = %v, owed = %+v), want the dated interval and nothing left to date", age, swept, owed)
		}
	})

	t.Run("a tip that cannot be read dates nothing", func(t *testing.T) {
		t.Parallel()
		d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
		if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
			LastRun: now.Add(-lastRun), LastSHA: "green0", LastGreenSHA: "green0",
		}); err != nil {
			t.Fatal(err)
		}
		d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "", errors.New("no ref") }
		d.upgradeRestartPending.Store(true)

		// The last run measures the age, as it does for a record the read can
		// date from nothing else.
		if age, swept, owed := d.tierSweepAge(now); !swept || age != lastRun || len(owed) != 0 {
			t.Fatalf("age = %s (swept = %v, owed = %+v), want the last run measuring and nothing dated", age, swept, owed)
		}
		d.tierSweepDeferForRestart(now)
		if st := tierSweepTestState(t, d, "gastown"); !st.OwedSince.IsZero() {
			t.Fatalf("OwedSince = %v, want the record undated: a tip that cannot be read says nothing about whether a sweep is owed", st.OwedSince)
		}

		// Readable again and moved past the green sweep: the interval begins
		// now, and the next read dates the record from there.
		d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "new1", nil }
		if age, swept, owed := d.tierSweepAge(now); !swept || age != 0 || len(owed) != 1 || !owed[0].since.Equal(now) {
			t.Fatalf("age = %s (swept = %v, owed = %+v), want the interval beginning at this read", age, swept, owed)
		}
		d.tierSweepDeferForRestart(now)
		if st := tierSweepTestState(t, d, "gastown"); !st.OwedSince.Equal(now) {
			t.Fatalf("OwedSince = %v, want %v: the first read that can see the tip dates the interval from it", st.OwedSince, now)
		}
	})
}

// gt-o3hb4: the sweep record's read-modify-writes — the owed stamp and the
// cycle's own write — are safe because one goroutine owns both, and the
// single-flight guard is what keeps a second heartbeat out. A heartbeat
// arriving while a cycle is in flight must be turned away rather than start a
// cycle of its own.
func TestTriggerTierSweepRunsOneCycleAtATime(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if !d.tierSweepRunning.Load() {
			t.Error("a cycle ran outside the guard")
		}
		// The heartbeat arrives on the cycle's own goroutine, with the guard
		// held: nothing here waits on a second goroutine, so the guard is the
		// only thing that can turn it away.
		d.triggerTierSweep()
		return tierSweepStageResult{ran: true}
	}

	d.triggerTierSweep()
	waitTierSweepIdle(t, d)

	want := len(tierSweepStages("gastown"))
	if got := len(rec.stages); got != want {
		t.Fatalf("stages run = %d, want one cycle's %d: a second heartbeat started a second cycle", got, want)
	}
}

// A deferred early re-run records only the stages that ran, under the tip it
// reached them at, and marks the record partial. The tip is still owed a whole
// cycle, so the next early tick comes back for it rather than leaving it to the
// hourly one (gt-icmdq).
func TestRunTierSweepEarlyRetriesATipItsCycleCouldNotCover(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "new2", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-15 * time.Minute), LastSHA: "red1", LastGreenSHA: "green0",
	}); err != nil {
		t.Fatal(err)
	}

	// First attempt: the shell stage runs green but the integration stage
	// never starts (a slot the sweep could not take), so the cycle defers
	// with a partial record.
	green := &tierSweepRunRecorder{}
	green.green()
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{ran: false, err: errors.New("slot busy")}
		}
		return green.result(st)
	}
	d.runTierSweepEarly([]string{"gastown"}, now)
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "new2" || !st.Partial {
		t.Fatalf("record = %+v, want the partial cycle's own tip new2, marked partial", st)
	}
	if !st.LastRun.Equal(now) {
		t.Fatalf("LastRun = %v, want %v so the retry gap spaces the next attempt", st.LastRun, now)
	}

	// Next early tick, past the gap: new2 is only partly swept, so the re-run
	// comes back for it.
	later := now.Add(tierSweepRetryMinGap)
	rec.stages = nil
	rec.green()
	d.runTierSweepEarly([]string{"gastown"}, later)
	if len(rec.stages) == 0 {
		t.Fatal("the next early tick did not retry the tip the deferred re-run left partly swept")
	}
	if st := tierSweepTestState(t, d, "gastown"); st.LastSHA != "new2" || st.LastGreenSHA != "new2" || st.Partial {
		t.Fatalf("record = %+v, want the retry to land on new2", st)
	}
}

// The dashboard reads a swept line back and attaches a rig's failing units only
// to a tier the line gives a verdict. A tier a Partial cycle never reached
// carries a verdict from an earlier tip in the record, so the line has to name
// it with no verdict of its own: that, not a check at the reader, keeps the
// carried red off this tip's row (gt-euxf1, gt-he2fi).
func TestTierSweepSweptLineGivesAnUnrunTierNoVerdict(t *testing.T) {
	t.Parallel()
	got := TierSweepSweptLine("gastown", "tipb0001ffff", [][]string{{"shell"}, {"integration", "race"}},
		map[string]string{"shell": "GREEN", "race": "RED"}, 4*time.Minute)
	want := "tier_sweep: gastown: swept tipb0001 (shell GREEN, integration , race RED) in 4m0s"
	if got != want {
		t.Errorf("swept line = %q, want %q", got, want)
	}
}
