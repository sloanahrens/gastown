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
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
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
	if !strings.Contains(logs.String(), "skipping this cycle (last sweep 20m0s ago, bound 1h30m0s)") {
		t.Fatalf("the skip line does not say how old the sweep was:\n%s", logs.String())
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

// A deferred early re-run (the stage never started) learned nothing about the
// new tip, so it must leave LastSHA at the old one: runTierSweepEarly skips a
// tip equal to LastSHA, and advancing it would lose the retry to the hourly
// cycle.
func TestRunTierSweepEarlyKeepsLastSHAOnDeferredVerdict(t *testing.T) {
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
	if st.LastSHA != "red1" {
		t.Fatalf("LastSHA = %q after a deferred early re-run, want the old tip red1", st.LastSHA)
	}
	if !st.LastRun.Equal(now) {
		t.Fatalf("LastRun = %v, want %v so the retry gap spaces the next attempt", st.LastRun, now)
	}

	// Next early tick, past the gap: the retry runs and records the tip.
	later := now.Add(tierSweepRetryMinGap)
	rec.stages = nil
	rec.green()
	d.runTierSweepEarly([]string{"gastown"}, later)
	if len(rec.stages) == 0 {
		t.Fatal("the next early tick did not retry the deferred re-run")
	}
	if st := tierSweepTestState(t, d, "gastown"); st.LastSHA != "new2" || st.LastGreenSHA != "new2" {
		t.Fatalf("record = %+v, want the retry to land on new2", st)
	}
}
