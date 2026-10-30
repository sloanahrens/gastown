package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// newTierSweepDaemon is a daemon over a temp town whose rigs.json lists rigs,
// with the log, the feed and the outside effects faked. Each rig gets the
// .repo.git directory the cycle looks for, and the daemon's git seam reads a
// fake world rather than starting git (the cycle resolves each rig's
// repo-committed settings tier, gt-yj6g1).
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
		tierSweepSeams: tierSweepSeams{run: rec.run, beads: func(string) tierSweepBeadStore { return bd }, treeHas: goTree},
	}
	useGitfake(t, d)
	return d, rec, bd, &logs
}

// goTree is a checked-out tree that ships every path, so each stage runs the Go
// command. A test reaches the tree check through the seam, never a real stat.
func goTree(string, string) (bool, error) { return true, nil }

// treeWith is a checked-out tree that ships exactly the paths listed in paths.
func treeWith(paths ...string) func(dir, rel string) (bool, error) {
	return func(_, rel string) (bool, error) { return slices.Contains(paths, rel), nil }
}

// treeFailing is a checked-out tree whose every path fails to check with err.
func treeFailing(err error) func(dir, rel string) (bool, error) {
	return func(_, _ string) (bool, error) { return false, err }
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
	updated  map[string]beads.UpdateOptions
}

func (b *tierSweepFakeBeads) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	b.created = append(b.created, opts)
	return &beads.Issue{ID: fmt.Sprintf("gt-new-%d", len(b.created)), Title: opts.Title}, nil
}

func (b *tierSweepFakeBeads) AddComment(id, text string) error {
	b.comments = append(b.comments, id+": "+text)
	return nil
}

func (b *tierSweepFakeBeads) Update(id string, opts beads.UpdateOptions) error {
	if b.updated == nil {
		b.updated = map[string]beads.UpdateOptions{}
	}
	b.updated[id] = opts
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
	d.landingHolds.hold(landingHoldUpgradeRestart)
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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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

	d.landingHolds.release(landingHoldUpgradeRestart)
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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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
			d.landingHolds.hold(landingHoldUpgradeRestart)

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
			d.landingHolds.hold(landingHoldUpgradeRestart)

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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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

	d.landingHolds.hold(landingHoldUpgradeRestart)
	for i := 0; i < 3; i++ {
		d.triggerTierSweep()
		waitTierSweepIdle(t, d)
	}
	if got := count(); got != 1 {
		t.Fatalf("skip line logged %d times over three heartbeats, want 1:\n%s", got, logs.String())
	}

	// The flag clears (a heartbeat sees it clear), then a second restart.
	d.landingHolds.release(landingHoldUpgradeRestart)
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	d.landingHolds.hold(landingHoldUpgradeRestart)
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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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

// The skip line and the stale-run line each keep a latch of their own, so
// neither one's first log swallows the other's, and a restart that is no
// longer pending resets both (gt-kp9fo).
func TestTierSweepDeferForRestart_StaleLatch(t *testing.T) {
	t.Parallel()
	const staleLine = "sweeping now rather than waiting out the pending restart"
	const skipLine = "skipping this cycle"
	now := atHour(time.Now(), 15)

	t.Run("skip then stale", func(t *testing.T) {
		t.Parallel()
		d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
		withTierSweepLastRun(t, d, "gastown", now.Add(-20*time.Minute))
		d.landingHolds.hold(landingHoldUpgradeRestart)
		d.tierSweepDeferForRestart(now)
		if got := strings.Count(logs.String(), skipLine); got != 1 {
			t.Fatalf("skip line logged %d times, want 1:\n%s", got, logs.String())
		}
		d.tierSweepDeferForRestart(now.Add(2 * time.Hour))
		d.tierSweepDeferForRestart(now.Add(2*time.Hour + time.Minute))
		if got := strings.Count(logs.String(), staleLine); got != 1 {
			t.Fatalf("stale line logged %d times, want 1:\n%s", got, logs.String())
		}
	})

	t.Run("stale then skip", func(t *testing.T) {
		t.Parallel()
		d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
		withTierSweepLastRun(t, d, "gastown", now.Add(-2*time.Hour))
		d.landingHolds.hold(landingHoldUpgradeRestart)
		if d.tierSweepDeferForRestart(now) {
			t.Fatal("a sweep past the bound must not defer")
		}
		if got := strings.Count(logs.String(), staleLine); got != 1 {
			t.Fatalf("stale line logged %d times, want 1:\n%s", got, logs.String())
		}
		// Earlier than the bound again (the clock stepped back): the skip line
		// must still be heard after the stale line was logged.
		if !d.tierSweepDeferForRestart(now.Add(-100 * time.Minute)) {
			t.Fatal("a sweep inside the bound must defer")
		}
		if got := strings.Count(logs.String(), skipLine); got != 1 {
			t.Fatalf("the stale latch swallowed the skip line (%d):\n%s", got, logs.String())
		}
	})

	t.Run("both latches clear once no restart is pending", func(t *testing.T) {
		t.Parallel()
		d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
		withTierSweepLastRun(t, d, "gastown", now.Add(-2*time.Hour))
		d.landingHolds.hold(landingHoldUpgradeRestart)
		d.tierSweepDeferForRestart(now)
		if !d.tierSweepStaleLogged.Load() {
			t.Fatal("the stale run did not latch its line")
		}
		// The clock steps back inside the bound: the same restart now defers,
		// so both latches are set by the code that owns them.
		d.tierSweepDeferForRestart(now.Add(-100 * time.Minute))
		if !d.tierSweepSkipLogged.Load() {
			t.Fatal("the deferral did not latch its skip line")
		}
		d.landingHolds.release(landingHoldUpgradeRestart)
		d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
		d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
		d.triggerTierSweep()
		waitTierSweepIdle(t, d)
		if d.tierSweepSkipLogged.Load() || d.tierSweepStaleLogged.Load() {
			t.Fatal("a heartbeat with no restart pending left a latch set; the next restart would log nothing")
		}
	})
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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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

// TestTierSweepStages_ShellStageRunsTheGoRunner: the shell stage runs the Go
// runner with --all, the integration and race stage keeps its script, and only
// the integration stage holds the container-gate slot.
func TestTierSweepStages_ShellStageRunsTheGoRunner(t *testing.T) {
	t.Parallel()
	stages := tierSweepStages("gastown")
	if len(stages) != 2 {
		t.Fatalf("stages = %+v, want shell then integration race", stages)
	}
	shell, integration := stages[0], stages[1]
	// The runner gets the shell tier's per-script limit as --budget, 10m, so a
	// slow script is held to the same bound the script's go test had.
	if got, want := shell.command(), "go run ./internal/shelltier/cmd/shelltier --all --budget 10m"; got != want {
		t.Errorf("shell command = %q, want %q", got, want)
	}
	if got, want := integration.command(), "go run ./cmd/tier-sweep integration race"; got != want {
		t.Errorf("integration command = %q, want %q", got, want)
	}
	if shell.slot || !integration.slot {
		t.Errorf("slot: shell = %v, integration = %v; want only the integration stage to hold the slot", shell.slot, integration.slot)
	}
}

// TestTierSweepCommand_ArgvPerTier: the integration and race stage runs the Go
// command with the tiers it names as arguments, one tier or both, and never the
// script. Restoring the script path turns this red.
func TestTierSweepCommand_ArgvPerTier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tiers []string
		want  []string
	}{
		{tiers: []string{"integration"}, want: []string{"go", "run", "./cmd/tier-sweep", "integration"}},
		{tiers: []string{"race"}, want: []string{"go", "run", "./cmd/tier-sweep", "race"}},
		{tiers: []string{"integration", "race"}, want: []string{"go", "run", "./cmd/tier-sweep", "integration", "race"}},
	}
	for _, tc := range tests {
		st := tierSweepStage{tiers: tc.tiers}
		if got := strings.Fields(st.command()); !slices.Equal(got, tc.want) {
			t.Errorf("tiers %v: argv = %q, want %q", tc.tiers, got, tc.want)
		}
	}
}

// TestTierSweepCommand_ExitOverrulesGreen: the integration and race stage reads
// its output line by line, and only the RED exit (1, which cmd/tier-sweep
// reports when any tier it ran was RED) is excused by a RED line in that
// output: the run is one command for both
// tiers, so a RED integration leaves the race tier's GREEN standing (gt-xyigz).
// Exit 2, a signal's -1 and exit 1 with no RED line behind it all withhold
// GREEN, as does a decline the rig's script is alone in making (gt-qgrdy).
func TestTierSweepCommand_ExitOverrulesGreen(t *testing.T) {
	t.Parallel()
	const (
		green = "tier-sweep: integration GREEN passed=4 failed=0 skipped=0 (logs /tmp/tier-sweep.y)\n"
		red   = "tier-sweep: integration RED passed=4 failed=1 skipped=0 failed: pkg/x (logs /tmp/tier-sweep.y)\n"
		race  = "tier-sweep: race GREEN passed=2 failed=0 skipped=0 (logs /tmp/tier-sweep.z)\n"
		decl  = "tier-sweep: unknown tier race (want shell, integration or race)\n"
	)
	tests := []struct {
		name     string
		script   bool // true when the rig's script runs the stage, not the Go command
		output   string
		exitCode int
		want     map[string]string // tier -> verdict; a tier absent here has no verdict
	}{
		{name: "GREEN, exit 0", output: green + race, exitCode: 0,
			want: map[string]string{"integration": tierSweepGreen, "race": tierSweepGreen}},
		{name: "GREEN, exit 1 with no RED line is withheld", output: green + race, exitCode: 1,
			want: map[string]string{}},
		{name: "GREEN, exit 2 with no decline line is withheld", output: green, exitCode: 2,
			want: map[string]string{}},
		{name: "a RED line stands and leaves the other tier's GREEN standing", output: red + race, exitCode: 1,
			want: map[string]string{"integration": tierSweepRed, "race": tierSweepGreen}},
		{name: "a RED line leaves an unreported tier unreported", output: red, exitCode: 1,
			want: map[string]string{"integration": tierSweepRed}},
		{name: "a RED line at exit 0 stands beside the other tier's GREEN", output: red + race, exitCode: 0,
			want: map[string]string{"integration": tierSweepRed, "race": tierSweepGreen}},
		{name: "a RED line at exit 2 does not excuse the run's own failure", output: red + race, exitCode: 2,
			want: map[string]string{"integration": tierSweepRed}},
		{name: "a RED line at a signal's exit -1 does not excuse it", output: red + race, exitCode: -1,
			want: map[string]string{"integration": tierSweepRed}},
		{name: "a RED line at exit 137 does not excuse it", output: red + race, exitCode: 137,
			want: map[string]string{"integration": tierSweepRed}},
		{name: "go command: a decline line at exit 1 excuses nothing", output: green + decl, exitCode: 1,
			want: map[string]string{}},
		{name: "go command: a decline line at exit 2 excuses nothing", output: green + decl, exitCode: 2,
			want: map[string]string{}},
		{name: "script: decline leaves the GREEN before it standing", script: true, output: green + decl, exitCode: 2,
			want: map[string]string{"integration": tierSweepGreen}},
		{name: "script: decline line at exit 1 is withheld", script: true, output: green + decl, exitCode: 1,
			want: map[string]string{}},
		{name: "script: decline line at exit 137 is withheld", script: true, output: green + decl, exitCode: 137,
			want: map[string]string{}},
		{name: "script: decline line at exit 0 is withheld, not the decline shape", script: true, output: green + decl, exitCode: 0,
			want: map[string]string{}},
		{name: "script: exit 2 with no decline line is withheld", script: true, output: green, exitCode: 2,
			want: map[string]string{}},
		{name: "script: a RED line stands beside the other tier's GREEN", script: true, output: red + race, exitCode: 1,
			want: map[string]string{"integration": tierSweepRed, "race": tierSweepGreen}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stage := tierSweepStage{tiers: []string{"integration", "race"}, script: tc.script}
			reported := tierSweepStageReported(stage, tierSweepStageResult{output: tc.output, exitCode: tc.exitCode, ran: true})
			got := map[string]string{}
			for tier, ts := range reported {
				got[tier] = ts.Verdict
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("reported = %v, want %v", got, tc.want)
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

// TestRunTierSweep_ShellVerdictReadsTheRunnersExit drives the shell stage's
// output and exit code through a cycle. The runner prints its summary and exits
// 2 on a usage or log-dir failure with no summary, so a non-zero exit must
// never read as GREEN, even beside a GREEN line.
func TestRunTierSweep_ShellVerdictReadsTheRunnersExit(t *testing.T) {
	t.Parallel()
	const green = "tier-sweep: shell GREEN passed=28 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 3m02s"
	const red = "tier-sweep: shell RED passed=27 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.x) in 3m02s"
	tests := []struct {
		name     string
		output   string
		exitCode int
		want     string
		// names is the tier's failed names, joined by a space.
		names string
	}{
		{name: "GREEN summary, exit 0", output: green + "\n", exitCode: 0, want: tierSweepGreen},
		{name: "RED summary, exit 1", output: red + "\n", exitCode: 1, want: tierSweepRed, names: "scripts/x.sh"},
		{name: "no summary, exit 2", output: "tier-sweep: shell scripts/x.sh ...\nusage: shelltier [--all]\n", exitCode: 2, want: tierSweepRed, names: "no verdict line (exit 2)"},
		{name: "GREEN summary, exit 2", output: green + "\n", exitCode: 2, want: tierSweepRed, names: "no verdict line (exit 2)"},
		{name: "last summary GREEN, exit 0", output: red + "\n" + green + "\n", exitCode: 0, want: tierSweepGreen},
		{name: "last summary RED, exit 1", output: green + "\n" + red + "\n", exitCode: 1, want: tierSweepRed, names: "scripts/x.sh"},
		{name: "empty output, exit 0", output: "", exitCode: 0, want: tierSweepRed, names: "no verdict line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
			rec.green()
			greenStage := rec.result
			rec.result = func(st tierSweepStage) tierSweepStageResult {
				if st.runner == "" {
					return greenStage(st)
				}
				return tierSweepStageResult{output: tc.output, exitCode: tc.exitCode, ran: true}
			}
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
			d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

			d.runTierSweep()

			ts := tierSweepTestState(t, d, "gastown").Tiers["shell"]
			if ts.Verdict != tc.want {
				t.Fatalf("shell verdict = %+v, want %s", ts, tc.want)
			}
			if got := strings.Join(ts.FailedNames, " "); got != tc.names {
				t.Errorf("shell failed names = %q, want %q", got, tc.names)
			}
		})
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

// gt-zq1pb: the retry of a cycle that deferred does not re-run a rig whose
// record already carries this tip's verdict. Re-deriving it costs the rig its
// 20m and 90m stages, and a deferral leaves the job due on every heartbeat.
func TestRunTierSweepDeferredRigSkipsUnchangedRedRigs(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown", "other")
	d.patrolConfig.Patrols.TierSweep.Rigs = []string{"gastown", "other"}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "redsha", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"TestRed"}, SHA: "redsha"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The record above is newer than the sweep's own clock, which is what says
	// a cycle that never judged every rig wrote it: this run is that cycle's
	// retry.
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// other's origin/main cannot be read, so its cycle defers: the job stays
	// due and the next heartbeat retries the cycle.
	d.tierSweepSeams.mainSHA = func(_ context.Context, repo string) (string, error) {
		if filepath.Base(filepath.Dir(repo)) == "other" {
			return "", errors.New("fetch failed")
		}
		return "redsha", nil
	}
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	if d.runTierSweep() {
		t.Fatal("runTierSweep reached a verdict with a rig deferring")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none: gastown's tip was already judged and other never reached a stage", rec.stages)
	}
	if want := "origin/main redsha already judged by the last cycle"; !strings.Contains(logs.String(), want) {
		t.Errorf("log missing %q; got:\n%s", want, logs.String())
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "redsha" || !st.LastRun.Equal(now.Add(-time.Hour)) {
		t.Errorf("record = %+v, want the skipped rig's record untouched", st)
	}
	if len(bd.created) != 0 {
		t.Errorf("beads = %+v, want none: no stage ran for either rig", bd.created)
	}
}

// gt-zq1pb: the judged-tip skip is scoped to the retry of a deferred cycle. A
// tip a cycle that judged every rig already saw is still re-swept on the next
// cadence run, which is how an unchanged red recovers without main moving.
func TestRunTierSweepReSweepsATipACompletedCycleJudged(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "redsha", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"TestRed"}, SHA: "redsha"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The sweep's clock is newer than the record, which is what says a cycle
	// that judged every rig wrote it: origin/main stands still, and the tip is
	// swept again anyway.
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-time.Hour).Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "redsha", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	d.runTierSweep()

	if len(rec.stages) == 0 {
		t.Error("no stage ran, want a re-sweep: only a deferred cycle's retry skips a judged tip")
	}
}

// A cycle that judged the red tip and then failed to save its patrol clock is
// not a deferral: the trigger reads that completion from memory, and the judged
// check must read it the same way, so the cadence run re-sweeps the tip rather
// than skipping it for another interval (gt-zq1pb).
func TestRunTierSweepReSweepsATipWhenTheClockSaveFailed(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-40 * time.Minute), LastSHA: "redsha", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"TestRed"}, SHA: "redsha"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The disk clock is the last save that landed; the completion after it did
	// not, so only this process remembers it.
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	d.tierSweepRun.record(now.Add(-30 * time.Minute))
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "redsha", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	d.runTierSweep()

	if len(rec.stages) == 0 {
		t.Error("no stage ran, want a re-sweep: the judged cycle completed, so its verdict is not a deferral's")
	}
}

// gt-1911b: an early re-run writes its record outside the town-wide cycle, so
// the cadence run that reads it re-derives the tip. Read as a deferred cycle's
// record, an unchanged red would be skipped for another interval.
func TestRunTierSweepReSweepsAfterAnEarlyReRunRecord(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "new2", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-15 * time.Minute), LastSHA: "red1", LastGreenSHA: "green0",
	}); err != nil {
		t.Fatal(err)
	}

	// A fix landed and main moved, so the early re-run takes new2 and comes
	// back red: the record is that re-run's, written at now.
	d.runTierSweepEarly([]string{"gastown"}, now)
	if st := tierSweepTestState(t, d, "gastown"); st.LastSHA != "new2" {
		t.Fatalf("record = %+v, want the early re-run's tip new2", st)
	}
	// The town's clock stands two hours behind that record: no town-wide cycle
	// has judged every rig, which is what a deferred cycle's record looks like.
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	rec.stages = nil
	d.runTierSweep()

	if len(rec.stages) == 0 {
		t.Errorf("no stage ran for the tip an early re-run judged; the cadence run read it as a deferred cycle's, log:\n%s", logs.String())
	}
}

// gt-1911b: the retry's skip is a concession to the one cycle being retried, so
// it lapses with the sweep's staleness bound. A rig that defers on every
// heartbeat never lets a cycle complete, and the rigs that cycle already judged
// would hold their verdicts until main moved.
func TestRunTierSweepReSweepsARedWhileASiblingRigDefers(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, logs := newTierSweepDaemon(t, now, "gastown", "other")
	d.patrolConfig.Patrols.TierSweep.Rigs = []string{"gastown", "other"}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "redsha", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"TestRed"}, SHA: "redsha"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The town's clock stands two hours behind that record, and other is the
	// rig that keeps every cycle since from judging every rig.
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	d.tierSweepSeams.mainSHA = func(_ context.Context, repo string) (string, error) {
		if filepath.Base(filepath.Dir(repo)) == "other" {
			return "", errors.New("fetch failed")
		}
		return "redsha", nil
	}
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	// The heartbeat after the deferral is that cycle's retry: gastown's tip is
	// already judged, and stands.
	if d.runTierSweep() {
		t.Fatal("runTierSweep reached a verdict with a rig deferring")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none on the retry of the cycle that judged gastown", rec.stages)
	}

	clk, ok := d.clock.(*clockwork.FakeClock)
	if !ok {
		t.Fatalf("test clock = %T, want a fake clock to advance", d.clock)
	}
	clk.Advance(2 * time.Hour)
	rec.stages = nil

	if d.runTierSweep() {
		t.Fatal("runTierSweep reached a verdict with a rig deferring")
	}
	if len(rec.stages) == 0 {
		t.Errorf("gastown's red was not re-swept while other deferred every cycle, log:\n%s", logs.String())
	}
}

// tierSweepNoWaitClock is the sweep's clock in the tests that prove no wait
// runs before the integration stage. It tells time from a fake clock and
// records After and Sleep, the two primitives the daemon's waits are built
// from. The sweep takes every pause through Daemon.clk, and its code holds no
// time.Sleep or time.After of its own, so a wait put back anywhere in the cycle
// is recorded at the call that asks for it. A wait taken from the time package
// directly would not be recorded; that is outside what this check covers.
type tierSweepNoWaitClock struct {
	*clockwork.FakeClock
	waits []string
}

func (c *tierSweepNoWaitClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, "After("+d.String()+")")
	// Closed, so a cycle that does wait runs past it and the test reports the
	// wait rather than hanging on it.
	ch := make(chan time.Time)
	close(ch)
	return ch
}

func (c *tierSweepNoWaitClock) Sleep(d time.Duration) {
	c.waits = append(c.waits, "Sleep("+d.String()+")")
}

// tierSweepNoWaitRun runs one green sweep of gastown on a clock that records
// its waits, and returns the stages it ran, the clock and the log.
// beforeFirstStage, when set, runs on the sweep's clock as the first stage
// starts, so a control can put a wait exactly where the no-wait check has to
// see it.
func tierSweepNoWaitRun(t *testing.T, beforeFirstStage func(d *Daemon)) ([]string, *tierSweepNoWaitClock, *bytes.Buffer) {
	t.Helper()
	now := atHour(time.Now(), 14)
	d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
	clk := &tierSweepNoWaitClock{FakeClock: clockwork.NewFakeClockAt(now)}
	d.clock = clk
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	}
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) {
		return func() {}, nil
	}
	var ran []string
	d.tierSweepSeams.run = func(_ context.Context, _ string, st tierSweepStage) tierSweepStageResult {
		if beforeFirstStage != nil && len(ran) == 0 {
			beforeFirstStage(d)
		}
		ran = append(ran, strings.Join(st.tiers, ","))
		var b strings.Builder
		for _, tier := range st.tiers {
			fmt.Fprintf(&b, "tier-sweep: %s GREEN passed=1 failed=0 skipped=0\n", tier)
		}
		return tierSweepStageResult{output: b.String(), exitCode: 0, ran: true}
	}

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	return ran, clk, logs
}

// gt-qpeei.8: the integration stage takes the container-gate slot with no wait
// before it: it starts when the shell stage returns. A settings file that still
// carries the removed keys loads with them read as nothing. The config package
// proves that in TestToleratedTierSweepKeysDecode
// (internal/config/tolerated_keys_test.go). The daemon never holds those keys,
// so there is no legacy path to cover here: the sweep runs the town's ordinary
// config, and the clock proves no wait.
func TestTierSweepIntegrationStageStartsWithoutAWait(t *testing.T) {
	t.Parallel()
	ran, clk, logs := tierSweepNoWaitRun(t, nil)
	if got, want := strings.Join(ran, "|"), "shell|integration,race"; got != want {
		t.Fatalf("stages ran %q, want %q", got, want)
	}
	if len(clk.waits) > 0 {
		t.Errorf("the cycle waited before a stage: it asked its clock for %v, and the integration stage must take the slot with no wait; log:\n%s",
			clk.waits, logs.String())
	}
}

// The control for the no-wait check above: a wait the sweep takes on its clock
// before the first stage must be recorded, or that check passes without proving
// the absence of a wait.
func TestTierSweepNoWaitClockRecordsAWaitBeforeTheFirstStage(t *testing.T) {
	t.Parallel()
	_, clk, _ := tierSweepNoWaitRun(t, func(d *Daemon) { d.clk().Sleep(time.Second) })
	if got, want := strings.Join(clk.waits, ","), "Sleep(1s)"; got != want {
		t.Fatalf("the clock recorded %q for a wait taken before the first stage, want %q", got, want)
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
	// Each cycle below judges every rig, and that is what retires the sweep's
	// own due-ness clock — triggerTierSweep's job after a cycle returns true.
	// origin/main does not move between them, so the later cycles are the
	// cadence re-sweep of an unchanged tip (gt-zq1pb).
	sweep := func() {
		if !d.runTierSweep() {
			t.Fatal("the cycle did not judge every rig")
		}
		if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now); err != nil {
			t.Fatal(err)
		}
	}

	sweep()

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
	sweep()
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
	sweep()
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
// a green sweep closes an open bead but leaves a claimed one to its polecat
// (gt-9hzs5, gt-2h3eo).
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
			if status == "open" {
				if len(bd.closed) != 1 || !strings.HasPrefix(bd.closed[0], "gt-claimed ") {
					t.Errorf("closed = %v, want the open bead closed on green", bd.closed)
				}
			} else if len(bd.closed) != 0 {
				t.Errorf("closed = %v, want the %s bead left to its polecat on green", bd.closed, status)
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
	for _, failStatus := range []string{"open", string(beads.StatusInProgress), beads.StatusHooked} {
		t.Run(failStatus, func(t *testing.T) {
			t.Parallel()
			d, _, _, logs := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
			byStatus := map[string][]*beads.Issue{}
			for _, status := range []string{"open", string(beads.StatusInProgress), beads.StatusHooked} {
				byStatus[status] = []*beads.Issue{{ID: "gt-" + status, Title: tierSweepBeadPrefix("gastown", "integration") + "./internal/" + status}}
			}
			bd := &failingStatusBeads{byStatusBeads: byStatusBeads{byStatus: byStatus}, failStatus: failStatus}
			d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

			if _, err := tierSweepOpenBeads(bd, "gastown", "integration"); err == nil {
				t.Fatalf("tierSweepOpenBeads returned no error when the %s list failed", failStatus)
			}

			red := map[string]tierSweepTierResult{
				"integration": {Verdict: tierSweepRed, Failed: 2, FailedNames: []string{"./internal/x", "./internal/y"}},
			}
			d.tierSweepRecordBeads("gastown", "abc123", red)
			if len(bd.created) != 0 || len(bd.comments) != 0 {
				t.Errorf("created = %+v, comments = %v, want nothing filed or commented from an unknown state", bd.created, bd.comments)
			}
			wantLog := "listing " + tierSweepRedLabel + " beads for the integration tier, leaving it unrecorded"
			if !strings.Contains(logs.String(), wantLog) || !strings.Contains(logs.String(), "--status="+failStatus+": boom") {
				t.Errorf("log lacks the skip alarm %q naming the failed %s list:\n%s", wantLog, failStatus, logs.String())
			}

			green := map[string]tierSweepTierResult{"integration": {Verdict: tierSweepGreen, Passed: 1}}
			d.tierSweepRecordBeads("gastown", "def456", green)
			if len(bd.closed) != 0 {
				t.Errorf("closed = %v, want nothing closed from a partial listing", bd.closed)
			}
		})
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

// The summary line's "(logs <dir>)" group is the directory the script wrote
// each unit's own log into. The daemon replaces Log with the stage log it
// writes, so without the separate field the only path to a unit log is lost and
// a slowcheck-only red can never name its test (gt-j6vlz). A record written
// before the field existed must still decode.
func TestTierSweepResultKeepsTheUnitLogDir(t *testing.T) {
	t.Parallel()
	got := parseTierSweepSummaries("tier-sweep: integration RED passed=9 failed=1 skipped=0 failed: ./tests/host-rehearsal (logs /x/y) in 2m14s")
	res, ok := got["integration"]
	if !ok {
		t.Fatalf("summaries = %+v, want the integration verdict", got)
	}
	if res.LogDir != "/x/y" {
		t.Errorf("log dir = %q, want /x/y from the summary line", res.LogDir)
	}

	var st tierSweepState
	if err := json.Unmarshal([]byte(`{"tiers":{"integration":{"verdict":"RED","passed":9,"failed":1,"failed_names":["./tests/host-rehearsal"]}}}`), &st); err != nil {
		t.Fatalf("a record without the field does not decode: %v", err)
	}
	if dir := st.Tiers["integration"].LogDir; dir != "" {
		t.Errorf("log dir = %q, want empty for a record written before the field", dir)
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

// TestTierSweepLogsSlotWait: a stage's wall can be mostly the container-gate
// queue, so the finished line names the wait apart from the run time
// (gt-sy14g). Mutation: drop the waitNote append and the first row goes red;
// the second holds the wait under a second out of the line.
func TestTierSweepLogsSlotWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		slotWait time.Duration
		want     string
		notWant  string
	}{
		{"a wait is named apart from the run time", 22*time.Minute + 27*time.Second,
			"tier_sweep: gastown: integration finished (exit 0) in 26m53s (slot wait 22m27s);", ""},
		{"no wait adds nothing", 0,
			"tier_sweep: gastown: integration finished (exit 0) in 26m53s;", "slot wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, rec, _, logs := newTierSweepDaemon(t, now, "gastown")
			clk := clockwork.NewFakeClockAt(now)
			d.clock = clk
			rec.result = func(st tierSweepStage) tierSweepStageResult {
				clk.Advance(26*time.Minute + 53*time.Second)
				var b strings.Builder
				for _, tier := range st.tiers {
					fmt.Fprintf(&b, "tier-sweep: %s GREEN passed=3 failed=0 skipped=0 in 4m23s\n", tier)
				}
				res := tierSweepStageResult{output: b.String(), exitCode: 0, ran: true}
				if st.slot {
					res.slotWait = tc.slotWait
				}
				return res
			}
			sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
			d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

			if !d.runTierSweep() {
				t.Fatal("runTierSweep deferred; want a verdict")
			}
			out := logs.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("log missing %q; got:\n%s", tc.want, out)
			}
			if tc.notWant != "" && strings.Contains(out, tc.notWant) {
				t.Errorf("log carries %q beside no wait; got:\n%s", tc.notWant, out)
			}
		})
	}
}

// TestTierSweepStateRecordsSlotWait: the record keeps the wait beside the
// verdict, so the dashboard and the next investigation read queue and work
// apart. The tiers of the stage that held the slot carry it; the shell stage
// never holds the slot and omits it (gt-sy14g).
func TestTierSweepStateRecordsSlotWait(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	clk := clockwork.NewFakeClockAt(now)
	d.clock = clk
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		clk.Advance(time.Minute)
		var b strings.Builder
		for _, tier := range st.tiers {
			fmt.Fprintf(&b, "tier-sweep: %s GREEN passed=3 failed=0 skipped=0 in 4m23s\n", tier)
		}
		res := tierSweepStageResult{output: b.String(), exitCode: 0, ran: true}
		if st.slot {
			res.slotWait = 22*time.Minute + 27*time.Second
		}
		return res
	}
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return sha, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred; want a verdict")
	}
	st := tierSweepTestState(t, d, "gastown")
	for tier, want := range map[string]int{"integration": 1347, "race": 1347, "shell": 0} {
		if got := st.Tiers[tier].SlotWaitSeconds; got != want {
			t.Errorf("%s slot_wait_seconds = %d, want %d", tier, got, want)
		}
	}
	// The shell tier's object must omit the key, not write a zero: a reader
	// takes its presence as the stage having held the slot.
	raw, err := os.ReadFile(tierSweepStatePath(d.config.TownRoot, "gastown"))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Tiers map[string]map[string]any `json:"tiers"`
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	for tier, want := range map[string]int{"integration": 1347, "race": 1347, "shell": 0} {
		got, present := written.Tiers[tier]["slot_wait_seconds"]
		if want == 0 {
			if present {
				t.Errorf("%s record carries slot_wait_seconds %v, want the key omitted", tier, got)
			}
			continue
		}
		if !present || got != float64(want) {
			t.Errorf("%s record slot_wait_seconds = %v (present %t), want %d", tier, got, present, want)
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
	// The started line is closed, or the dashboard reads the rig as running.
	if want := TierSweepAbortedLine("gastown", "abc123", "no stage ran"); !strings.Contains(out, want) {
		t.Errorf("log missing the closing line %q; got:\n%s", want, out)
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

// gt-zq1pb: the cycle's own deadline is not a verdict about the tree. Land's
// gate marks a step TimedOut only for the step's own budget, so a stage the
// cycle's context killed comes back with exit -1 and no flag — read as RED it
// filed a bead against a stage that never finished, and held LastGreenSHA back.
// The cycle defers instead, leaving the record it had.
func TestTierSweepRigCycleParentDeadlineDefers(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "deadlinesha", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-time.Hour), LastSHA: "prior", LastGreenSHA: "green0",
		Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"TestPrior"}, SHA: "prior"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The stage is running when the cycle's deadline takes it: the gate kills
	// it and reports exit -1, with no timeout of the stage's own.
	rec.result = func(tierSweepStage) tierSweepStageResult {
		cancel()
		return tierSweepStageResult{exitCode: -1, ran: true}
	}
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()

	if d.runTierSweepRig(ctx, cycle, "gastown", now) {
		t.Error("the rig reported a verdict; want a defer: the cycle's deadline ended its stage")
	}
	if len(bd.created) != 0 {
		t.Errorf("beads = %+v, want none: no stage reached a verdict", bd.created)
	}
	if len(bd.comments) != 0 {
		t.Errorf("comments = %v, want none", bd.comments)
	}
	if strings.Contains(logs.String(), "no verdict line") {
		t.Errorf("the deadline read as a stage verdict; got:\n%s", logs.String())
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "prior" || st.LastGreenSHA != "green0" {
		t.Errorf("record = %+v, want the prior record's tips untouched", st)
	}
	if got := st.Tiers["shell"]; got.Verdict != tierSweepRed || len(got.FailedNames) != 1 || got.FailedNames[0] != "TestPrior" {
		t.Errorf("shell verdict = %+v, want the prior RED, not this cycle's", got)
	}
}

// gt-zq1pb: the deadline deferral is scoped to a stage that never reached a
// verdict. A stage that reported every tier it covers had its say, so the
// cycle's deadline expiring as it finishes is no reason to throw the verdict
// away.
func TestTierSweepRigCycleKeepsAVerdictTheDeadlineArrivedAfter(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, bd, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "greensha", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The stage has its verdict in hand when the deadline takes the cycle: it
	// returns the gate's own result, exit 0 with a summary line per tier.
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		var b strings.Builder
		for _, tier := range st.tiers {
			fmt.Fprintf(&b, "tier-sweep: %s GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n", tier)
		}
		res := tierSweepStageResult{output: b.String(), exitCode: 0, ran: true}
		// The deadline lands as the stage finishes, with the verdict in hand.
		cancel()
		return res
	}
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()

	if !d.runTierSweepRig(ctx, cycle, "gastown", now) {
		t.Error("the rig deferred; want the verdict its stages reported")
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "greensha" || st.LastGreenSHA != "greensha" {
		t.Errorf("record = %+v, want the green tip the stages reported", st)
	}
	if len(bd.created) != 0 {
		t.Errorf("beads = %+v, want none: every tier came back green", bd.created)
	}
}

// gt-qgrdy: a rig whose script has no such tier says so with "tier-sweep:
// unknown tier <name>" and exits 2 with no summary for that tier. That is a
// contract mismatch, not a failure, so the tier leaves the cycle's results and
// no verdict, bead or attention item follows. Every other missing verdict
// stays RED, and only the line naming the missing tier itself excuses it.
func TestTierSweepRigCycleTierTheScriptDoesNotOffer(t *testing.T) {
	t.Parallel()
	const (
		shellSummary       = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
		integrationSummary = "tier-sweep: integration GREEN passed=4 failed=0 skipped=0 (logs /tmp/tier-sweep.y)\n"
		declinedRace       = "tier-sweep: unknown tier race (want shell, integration or race)\n"
	)
	tests := []struct {
		name string
		// stage is the slot stage's combined output and exit status: the run
		// that carries the integration and race tiers.
		stage    string
		wantRace string // the RED reason; "" for no race verdict at all
	}{{
		name:  "the script's own unknown-tier line leaves race out of the results",
		stage: integrationSummary + declinedRace,
	}, {
		// The script exits at the first tier it does not offer, so race after
		// it never ran and no line names it (gt-bdva9).
		name:  "a tier the script declines ends its stage, so race after it leaves the results",
		stage: "tier-sweep: unknown tier integration (want shell, integration or race)\n",
	}, {
		name:     "a missing verdict without that line is still RED",
		stage:    integrationSummary,
		wantRace: "no verdict line (exit 2)",
	}, {
		name:     "an unknown-tier line for another tier excuses nothing",
		stage:    integrationSummary + "tier-sweep: unknown tier shell (want shell, integration or race)\n",
		wantRace: "no verdict line (exit 2)",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
			d.tierSweepSeams.treeHas = treeWith("scripts/tier-sweep.sh")
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tips1", nil }
			d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
			rec.result = func(st tierSweepStage) tierSweepStageResult {
				if st.slot {
					return tierSweepStageResult{output: tc.stage, exitCode: 2, ran: true}
				}
				return tierSweepStageResult{output: shellSummary, exitCode: 0, ran: true}
			}
			cycle := d.startDogCycle("tier_sweep")
			defer cycle.close()
			if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
				t.Fatal("the cycle deferred; a stage that ran and reported a verdict is a complete cycle")
			}

			st := tierSweepTestState(t, d, "gastown")
			race, hasRace := st.Tiers["race"]
			notOfferedLogs := strings.Count(logs.String(), "gastown: race not offered by the rig's script")
			offered := notOfferedLogs > 0
			if notOfferedLogs > 1 {
				t.Errorf("race logged as not offered %d times in one cycle, want once:\n%s", notOfferedLogs, logs.String())
			}
			switch {
			case tc.wantRace == "":
				if hasRace {
					t.Fatalf("race verdict = %+v, want no verdict for the tier the script does not offer", race)
				}
				if !offered {
					t.Errorf("the cycle did not log race as not offered:\n%s", logs.String())
				}
				if strings.Contains(logs.String(), "race RED") {
					t.Errorf("the not-offered tier was logged RED:\n%s", logs.String())
				}
				// The tier is not part of this rig's gate, so the offered
				// tiers' green tip advances: nothing is held back for a tier
				// the script will never run.
				if st.LastGreenSHA != "tips1" {
					t.Errorf("LastGreenSHA = %q, want the offered tiers' green tip tips1", st.LastGreenSHA)
				}
				if st.Partial {
					t.Error("the record is partial; every stage ran")
				}
			default:
				if !hasRace || race.Verdict != tierSweepRed || len(race.FailedNames) != 1 || race.FailedNames[0] != tc.wantRace {
					t.Fatalf("race verdict = %+v (present = %v), want RED %q", race, hasRace, tc.wantRace)
				}
				if offered {
					t.Errorf("the cycle logged a tier as not offered on a line that names another one:\n%s", logs.String())
				}
			}

			var raceBeads int
			for _, c := range bd.created {
				if strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", "race")) {
					raceBeads++
				}
			}
			var raceItems int
			for _, it := range tierSweepItems(t, d, now, "gastown") {
				if strings.Contains(it.Summary, "tier sweep gastown race ") {
					raceItems++
				}
			}
			want := 0
			if tc.wantRace != "" {
				want = 1
			}
			if raceBeads != want || raceItems != want {
				t.Errorf("race beads = %d, attention items = %d, want %d each", raceBeads, raceItems, want)
			}
		})
	}
}

// gastownIntegrationLog is a synthetic log shaped like a real integration stage
// log from the sweep's own directory (~/gt/.runtime/tier-sweep/logs/gastown/
// integration.log). That log is not in this sandbox, so the package names,
// counts and log directories here are invented stand-ins, not a copy of a
// recorded run. The stage log holds the step's stdout and stderr in one buffer
// (internal/land/gate.go), so it carries the sweep's progress lines (stderr)
// beside its summary lines (stdout, where cmd/tier-sweep writes one line per
// tier as the tier ends). One command runs the integration tier and then the
// race tier, so a RED integration's exit 1 arrives with the race tier's GREEN
// line after it — the input that was recorded as race "no verdict line
// (exit 1)" and filed this bead (gt-xyigz).
const gastownIntegrationLog = "" +
	"tier-sweep: integration ./internal/cmd ...\n" +
	"tier-sweep: integration ./internal/daemon ...\n" +
	"tier-sweep: integration ./internal/repoguard ...\n" +
	"tier-sweep: integration RED passed=124 failed=1 skipped=0 failed: ./internal/repoguard (logs /tmp/tier-sweep.2f9c)\n" +
	"tier-sweep: race ./internal/cmd ...\n" +
	"tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs /tmp/tier-sweep.71bd)\n"

// gt-xyigz: the Go command runs the integration and race tiers in one process
// and exits 1 when either is RED. The GREEN line of the tier that passed is
// that tier's verdict, so a red integration files no race bead and no race
// attention item beside its own.
func TestTierSweepRigCycleGreenRaceBesideRedIntegration(t *testing.T) {
	t.Parallel()
	const shellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
	now := atHour(time.Now(), 15)
	d, rec, bd, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tips1", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{output: gastownIntegrationLog, exitCode: tierSweepRedExit, ran: true}
		}
		return tierSweepStageResult{output: shellSummary, exitCode: 0, ran: true}
	}
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatal("the cycle deferred; every stage ran and reported a verdict")
	}

	st := tierSweepTestState(t, d, "gastown")
	race, ok := st.Tiers["race"]
	if !ok || race.Verdict != tierSweepGreen {
		t.Fatalf("race = %+v (present = %v), want GREEN: its own line reported it", race, ok)
	}
	if slices.Contains(race.FailedNames, "no verdict line (exit 1)") {
		t.Errorf("race failed names = %q, want none: that record is what this bead was filed for", race.FailedNames)
	}
	if in := st.Tiers["integration"]; in.Verdict != tierSweepRed || !slices.Contains(in.FailedNames, "./internal/repoguard") {
		t.Errorf("integration = %+v, want RED from the line that named ./internal/repoguard", in)
	}
	if st.LastGreenSHA == "tips1" {
		t.Error("LastGreenSHA advanced to tips1; a red integration must hold the green tip back")
	}
	var integrationBeads, raceBeads int
	for _, c := range bd.created {
		switch {
		case strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", "integration")):
			integrationBeads++
		case strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", "race")):
			raceBeads++
		}
	}
	if integrationBeads != 1 {
		t.Errorf("integration beads = %d, want 1 for ./internal/repoguard", integrationBeads)
	}
	if raceBeads != 0 {
		t.Errorf("race beads = %d, want 0: the race tier passed in that run", raceBeads)
	}
	for _, it := range tierSweepItems(t, d, now, "gastown") {
		if strings.Contains(it.Summary, "tier sweep gastown race ") {
			t.Errorf("attention item for a green race tier: %+v", it)
		}
	}
}

// The Go command offers every tier it is asked for, so an unknown-tier line in
// its output excuses nothing: the tier it names is RED, not left out of the
// results. The stage does not depend on the exit status go run reports.
func TestTierSweepRigCycleGoCommandDeclineExcusesNothing(t *testing.T) {
	t.Parallel()
	const (
		shellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
		declinedRace = "tier-sweep: unknown tier race (want shell, integration or race)\n"
		wantLog      = "tier_sweep: gastown: race RED: no verdict line (exit 1)"
	)
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tips1", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{output: declinedRace, exitCode: 1, ran: true}
		}
		return tierSweepStageResult{output: shellSummary, exitCode: 0, ran: true}
	}
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	d.runTierSweepRig(context.Background(), cycle, "gastown", now)

	race, ok := tierSweepTestState(t, d, "gastown").Tiers["race"]
	if !ok || race.Verdict != tierSweepRed || len(race.FailedNames) != 1 || race.FailedNames[0] != "no verdict line (exit 1)" {
		t.Errorf("race = %+v (present = %v), want RED %q: a Go command's unknown-tier line excuses no tier", race, ok, "no verdict line (exit 1)")
	}
	if !slices.Contains(strings.Split(logs.String(), "\n"), wantLog) {
		t.Errorf("log has no line %q:\n%s", wantLog, logs.String())
	}
	if strings.Contains(logs.String(), "not offered") {
		t.Errorf("a tier was logged as not offered, want race judged RED:\n%s", logs.String())
	}
	var raceBeads int
	for _, c := range bd.created {
		if strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", "race")) {
			raceBeads++
		}
	}
	var raceItems int
	for _, it := range tierSweepItems(t, d, now, "gastown") {
		if strings.Contains(it.Summary, "tier sweep gastown race ") {
			raceItems++
		}
	}
	if raceBeads != 1 || raceItems != 1 {
		t.Errorf("race beads = %d, attention items = %d, want 1 each: the RED race verdict files its bead and item", raceBeads, raceItems)
	}
}

// gt-zlcl6: the rig's script runs its tiers in an order of its own, and a
// decline is read as ending the stage rather than as a position in the stage's
// list. A script that declines the tier the stage lists last and exits 2 never
// reached the one listed before it, so that tier leaves the results too instead
// of filing a RED "no verdict line" bead against a script that never ran it.
func TestTierSweepRigCycleDeclinedTierEndsTheStageWhicheverOrderItRuns(t *testing.T) {
	t.Parallel()
	const (
		shellSummary = "tier-sweep: shell GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
		declinedRace = "tier-sweep: unknown tier race (want shell, integration or race)\n"
	)
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.treeHas = treeWith("scripts/tier-sweep.sh")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tips1", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		if st.slot {
			return tierSweepStageResult{output: declinedRace, exitCode: 2, ran: true}
		}
		return tierSweepStageResult{output: shellSummary, exitCode: 0, ran: true}
	}
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if !d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatal("the cycle deferred; a stage that ran and reported a verdict is a complete cycle")
	}

	st := tierSweepTestState(t, d, "gastown")
	for _, tier := range []string{"integration", "race"} {
		if ts, has := st.Tiers[tier]; has {
			t.Errorf("%s verdict = %+v, want none: the script declined race and exited at it", tier, ts)
		}
		if n := strings.Count(logs.String(), "gastown: "+tier+" not offered by the rig's script"); n != 1 {
			t.Errorf("%s logged as not offered %d times in one cycle, want once:\n%s", tier, n, logs.String())
		}
		for _, c := range bd.created {
			if strings.HasPrefix(c.Title, tierSweepBeadPrefix("gastown", tier)) {
				t.Errorf("filed a bead for %s: %+v", tier, c)
			}
		}
	}
	if st.LastGreenSHA != "tips1" {
		t.Errorf("LastGreenSHA = %q, want the offered tiers' green tip tips1", st.LastGreenSHA)
	}
}

// gt-bdva9: the script exits at the first tier it declines, so the rest of its
// stage was never reached. gt-zlcl6: which tiers those are does not follow the
// stage's list, since the rig's script runs its own; a decline ends the stage,
// so every other tier of it with no summary line is unreached. A declined tier
// outside the stage names nothing.
func TestTierSweepUnreachedAfterDeclined(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		tiers    []string
		declined []string
		want     []string
	}{
		{name: "nothing declined", tiers: []string{"integration", "race"}, want: nil},
		{name: "the last tier declined leaves the stage's other tier unreached", tiers: []string{"integration", "race"}, declined: []string{"race"}, want: []string{"integration"}},
		{name: "first tier declined leaves the rest unreached", tiers: []string{"integration", "race"}, declined: []string{"integration"}, want: []string{"race"}},
		{name: "the declined tier itself is not named", tiers: []string{"integration", "race"}, declined: []string{"integration", "race"}, want: nil},
		{name: "a tier outside the stage reaches nothing", tiers: []string{"integration", "race"}, declined: []string{"shell"}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			declined := map[string]bool{}
			for _, tier := range tc.declined {
				declined[tier] = true
			}
			got := tierSweepUnreachedAfterDeclined(tc.tiers, declined)
			if len(got) != len(tc.want) {
				t.Fatalf("unreached = %v, want %v", got, tc.want)
			}
			for _, tier := range tc.want {
				if !got[tier] {
					t.Errorf("unreached = %v, want %v", got, tc.want)
				}
			}
		})
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
		// notOffered names the tiers the rig's script declined this cycle.
		notOffered map[string]bool
		want       string
	}{{
		// gt-qgrdy: the earlier verdict for a declined tier was taken when
		// the script still offered it; carrying it would show it as current.
		name: "a partial cycle does not carry the verdict of a tier the script declined",
		prior: tierSweepState{LastSHA: "tipA", Tiers: map[string]tierSweepTierResult{
			"integration": {Verdict: tierSweepGreen, SHA: "tipA"},
			"race":        {Verdict: tierSweepRed, SHA: "tipA"},
		}},
		results:    map[string]tierSweepTierResult{"shell": {Verdict: tierSweepGreen}},
		deferred:   true,
		notOffered: map[string]bool{"race": true},
		want:       "integration=GREEN@tipA shell=GREEN@tipB",
	}, {
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
			got := tierSweepRecordedTiers(tc.results, tc.prior, stages, "tipB", tc.deferred, tc.notOffered)
			if rendered := tierSweepVerdictsAt(got); rendered != tc.want {
				t.Errorf("recorded = %s, want %s", rendered, tc.want)
			}
		})
	}
}

// gt-qgrdy: a tier the script does not offer cannot hold the green tip back,
// but a cycle in which the script offered nothing judged nothing and is not
// green.
func TestTierSweepAllGreen(t *testing.T) {
	t.Parallel()
	stages := []tierSweepStage{{tiers: []string{"shell"}}, {tiers: []string{"integration", "race"}}}
	green := tierSweepTierResult{Verdict: tierSweepGreen}
	red := tierSweepTierResult{Verdict: tierSweepRed}
	tests := []struct {
		name       string
		results    map[string]tierSweepTierResult
		notOffered map[string]bool
		want       bool
	}{
		{"every tier green", map[string]tierSweepTierResult{"shell": green, "integration": green, "race": green}, nil, true},
		{"a red tier", map[string]tierSweepTierResult{"shell": green, "integration": green, "race": red}, nil, false},
		{"a declined tier is not held against the rest", map[string]tierSweepTierResult{"shell": green, "integration": green}, map[string]bool{"race": true}, true},
		{"a red tier beside a declined one", map[string]tierSweepTierResult{"shell": red, "integration": green}, map[string]bool{"race": true}, false},
		{"a tier with no result and no decline", map[string]tierSweepTierResult{"shell": green, "integration": green}, nil, false},
		{"every tier declined judged nothing", map[string]tierSweepTierResult{}, map[string]bool{"shell": true, "integration": true, "race": true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tierSweepAllGreen(tc.results, stages, tc.notOffered); got != tc.want {
				t.Errorf("tierSweepAllGreen = %v, want %v", got, tc.want)
			}
		})
	}
}

// gt-qgrdy: when the script declines every tier of the cycle there is no
// verdict anywhere, so the cycle is not green: it aborts with the reason named,
// the last record stands (tip not advanced, nothing promoted) and no bead is
// filed.
func TestTierSweepRigCycleEveryTierDeclined(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
	// A decline is the rig's script's contract, so the tree ships the script.
	d.tierSweepSeams.treeHas = treeWith("scripts/tier-sweep.sh")
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "tips1", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastSHA: "tips0", LastGreenSHA: "tips0",
		Tiers: map[string]tierSweepTierResult{"shell": {Verdict: tierSweepGreen, SHA: "tips0"}},
	}); err != nil {
		t.Fatal(err)
	}
	rec.result = func(st tierSweepStage) tierSweepStageResult {
		var out string
		for _, tier := range st.tiers {
			out += "tier-sweep: unknown tier " + tier + " (want nothing)\n"
		}
		return tierSweepStageResult{output: out, exitCode: 2, ran: true}
	}
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()
	if d.runTierSweepRig(context.Background(), cycle, "gastown", now) {
		t.Fatalf("the cycle reported complete with no tier offered:\n%s", logs.String())
	}
	if want := tierSweepAbortedLine("gastown", "tips1", "no tier offered by the rig's script"); !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}

	st := tierSweepTestState(t, d, "gastown")
	if st.LastGreenSHA != "tips0" || st.LastSHA != "tips0" {
		t.Errorf("record = last %q, last green %q, want the earlier record left as it was", st.LastSHA, st.LastGreenSHA)
	}
	if len(bd.created) != 0 {
		t.Errorf("beads filed = %d, want none for tiers the script does not offer", len(bd.created))
	}
	if strings.Contains(logs.String(), " RED") {
		t.Errorf("a declined tier was logged RED:\n%s", logs.String())
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
	d.landingHolds.hold(landingHoldUpgradeRestart)
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
	d.landingHolds.hold(landingHoldUpgradeRestart)
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

	d.landingHolds.hold(landingHoldUpgradeRestart)
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
		d.landingHolds.hold(landingHoldUpgradeRestart)

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
		d.landingHolds.hold(landingHoldUpgradeRestart)

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

// A still-red bead filed before the shape fix (gt-uzkic, gt-1lzy1) has prose
// and no acceptance, so the dispatcher's lint refuses it and never slots it.
// The next red run rewrites it to the shape while no polecat holds it (gt-hang2).
func TestTierSweepFileRed_ReshapesAnUnshapedOpenBead(t *testing.T) {
	t.Parallel()
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }
	title := tierSweepBeadPrefix("gastown", "integration") + "./internal/x"
	bd.open = []*beads.Issue{{
		ID:          "gt-old",
		Title:       title,
		Labels:      []string{tierSweepRedLabel},
		Description: "The integration tier went red. Please look.",
	}}

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"./internal/x"}},
	}
	d.tierSweepRecordBeads("gastown", "abc123", red)

	if len(bd.created) != 0 {
		t.Errorf("created = %+v, want the open bead reshaped instead of a new one", bd.created)
	}
	if len(bd.comments) != 1 || !strings.HasPrefix(bd.comments[0], "gt-old: still red") {
		t.Errorf("comments = %v, want the still-red comment kept", bd.comments)
	}
	upd, ok := bd.updated["gt-old"]
	if !ok || upd.Description == nil || upd.Acceptance == nil {
		t.Fatalf("gt-old was not reshaped: updated = %+v", bd.updated)
	}
	v := lintTierSweepBead(beads.CreateOptions{
		Title:       title,
		Labels:      []string{tierSweepRedLabel},
		Description: *upd.Description,
		Acceptance:  *upd.Acceptance,
	})
	if !v.Clean() {
		t.Fatalf("the reshaped bead fails the lint: %s\ndescription:\n%s", v.ShapeNote(), *upd.Description)
	}
}

// A bead already in shape keeps its text, and a bead a polecat has claimed is
// left to its owner even when its text is unshaped.
func TestTierSweepFileRed_LeavesShapedAndClaimedBeadsAlone(t *testing.T) {
	t.Parallel()
	d, _, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	ts := tierSweepTierResult{Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"./internal/x"}}
	shapedDesc, shapedAcc := tierSweepBeadShape("integration", "./internal/x", "abc123", ts,
		specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
	bd := &byStatusBeads{byStatus: map[string][]*beads.Issue{
		"open": {{
			ID:                 "gt-shaped",
			Title:              tierSweepBeadPrefix("gastown", "integration") + "./internal/x",
			Labels:             []string{tierSweepRedLabel},
			Description:        shapedDesc,
			AcceptanceCriteria: shapedAcc,
		}},
		"in_progress": {{
			ID:          "gt-claimed",
			Title:       tierSweepBeadPrefix("gastown", "integration") + "./internal/y",
			Labels:      []string{tierSweepRedLabel},
			Description: "A polecat is working this.",
		}},
	}}
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Failed: 2, FailedNames: []string{"./internal/x", "./internal/y"}},
	}
	d.tierSweepRecordBeads("gastown", "abc123", red)

	if len(bd.updated) != 0 {
		t.Errorf("updated = %+v, want no rewrite of a shaped or claimed bead", bd.updated)
	}
}

// TestTierSweepStages_ResolveInPicksTheTreesCommand: each stage runs the Go
// command where the checked-out tree ships its directory, the rig's own script
// where it does not, and no command where the tree ships neither (gt-j0wki). A
// stage that always picks the Go command fails the script rows.
func TestTierSweepStages_ResolveInPicksTheTreesCommand(t *testing.T) {
	t.Parallel()
	const (
		shellDir  = "internal/shelltier/cmd/shelltier"
		tierDir   = "cmd/tier-sweep"
		script    = "scripts/tier-sweep.sh"
		goShell   = "go run ./internal/shelltier/cmd/shelltier --all --budget 10m"
		goTiers   = "go run ./cmd/tier-sweep integration race"
		scriptSh  = "bash scripts/tier-sweep.sh shell"
		scriptInt = "bash scripts/tier-sweep.sh integration race"
	)
	statErr := errors.New("stat tree: permission denied")
	tests := []struct {
		name    string
		has     func(dir, rel string) (bool, error)
		stage   int
		want    string
		wantOK  bool
		wantErr error
	}{
		{name: "go tree, shell", has: treeWith(shellDir, tierDir, script), stage: 0, want: goShell, wantOK: true},
		{name: "go tree, integration race", has: treeWith(shellDir, tierDir, script), stage: 1, want: goTiers, wantOK: true},
		{name: "script tree, shell", has: treeWith(script), stage: 0, want: scriptSh, wantOK: true},
		{name: "script tree, integration race", has: treeWith(script), stage: 1, want: scriptInt, wantOK: true},
		{name: "tree with only the shell Go dir, integration falls to the script", has: treeWith(shellDir, script), stage: 1, want: scriptInt, wantOK: true},
		{name: "tree with neither, shell", has: treeWith(), stage: 0, wantOK: false},
		{name: "tree with neither, integration race", has: treeWith(), stage: 1, wantOK: false},
		{name: "unreadable tree, shell", has: treeFailing(statErr), stage: 0, wantErr: statErr},
		{name: "unreadable tree, integration race", has: treeFailing(statErr), stage: 1, wantErr: statErr},
		{name: "no Go dir, unreadable script check", has: func(_, rel string) (bool, error) {
			if rel == script {
				return false, statErr
			}
			return false, nil
		}, stage: 1, wantErr: statErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, ok, err := tierSweepStages("devops")[tc.stage].resolveIn(func(rel string) (bool, error) { return tc.has("", rel) })
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Fatalf("runnable = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got := st.command(); got != tc.want {
				t.Errorf("command = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTierSweepRig_TreeWithNoCommandIsRed: a tree with neither the Go command
// nor the script runs no stage. Each tier is RED with the reason, files a bead,
// and the rig's tip is not recorded green (gt-j0wki).
func TestTierSweepRig_TreeWithNoCommandIsRed(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "devops")
	d.tierSweepSeams.treeHas = treeWith()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()

	d.runTierSweepRig(context.Background(), cycle, "devops", now)

	if len(rec.stages) != 0 {
		t.Errorf("stages run = %d, want none: the tree has no command to run", len(rec.stages))
	}
	st := tierSweepTestState(t, d, "devops")
	for _, tier := range []string{"shell", "integration", "race"} {
		ts, ok := st.Tiers[tier]
		if !ok || ts.Verdict != tierSweepRed || !slices.Equal(ts.FailedNames, []string{tierSweepNoCommandReason}) {
			t.Errorf("tier %s = %+v, want RED with reason %q", tier, ts, tierSweepNoCommandReason)
		}
		if ts.Command != "" {
			t.Errorf("tier %s command = %q, want none: nothing ran", tier, ts.Command)
		}
	}
	if st.LastGreenSHA == "abc123" {
		t.Errorf("LastGreenSHA = %q, want the tip not recorded green", st.LastGreenSHA)
	}
}

// TestTierSweepRig_UnreadableTreeIsRedWithTheError: a tree whose check fails
// runs no stage. Each tier is RED with the check's error, not the no-command
// reason, since the tree may well ship a command; the tip is not recorded green
// (gt-j0wki).
func TestTierSweepRig_UnreadableTreeIsRedWithTheError(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "devops")
	statErr := errors.New("stat tree: permission denied")
	d.tierSweepSeams.treeHas = treeFailing(statErr)
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	cycle := d.startDogCycle("tier_sweep")
	defer cycle.close()

	d.runTierSweepRig(context.Background(), cycle, "devops", now)

	if len(rec.stages) != 0 {
		t.Errorf("stages run = %d, want none: the tree could not be checked", len(rec.stages))
	}
	st := tierSweepTestState(t, d, "devops")
	for _, tier := range []string{"shell", "integration", "race"} {
		ts, ok := st.Tiers[tier]
		if !ok || ts.Verdict != tierSweepRed || !slices.Equal(ts.FailedNames, []string{statErr.Error()}) {
			t.Errorf("tier %s = %+v, want RED with reason %q", tier, ts, statErr.Error())
		}
		if ts.Command != "" {
			t.Errorf("tier %s command = %q, want none: nothing ran", tier, ts.Command)
		}
	}
	if st.LastGreenSHA == "abc123" {
		t.Errorf("LastGreenSHA = %q, want the tip not recorded green", st.LastGreenSHA)
	}
}

// TestTierSweepTreeHas_ReadsTheTree reads the production tree check against a
// real temporary tree: a file or a directory that is there is shipped, a path
// that is not is not shipped, and a path whose parent is a file is an error,
// not a missing command (gt-j0wki). A real stat is the one thing this test
// checks, so it is the one test that touches a tree on disk.
func TestTierSweepTreeHas_ReadsTheTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "tier-sweep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "shipped.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notadir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		rel     string
		want    bool
		wantErr bool
	}{
		{name: "file is shipped", rel: "scripts/shipped.txt", want: true},
		{name: "directory is shipped", rel: "cmd/tier-sweep", want: true},
		{name: "absent path is not shipped", rel: "internal/shelltier/cmd/shelltier", want: false},
		{name: "parent is a file, an error", rel: "notadir/tier-sweep.sh", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tierSweepTreeHas(dir, tc.rel)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("shipped = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTierSweepRig_ScriptTreeReadsTheScriptVerdict: a tree that ships only
// scripts/tier-sweep.sh runs it with the tiers, and its summary lines are the
// tiers' verdicts. The script reports a RED tier with exit 1, so a GREEN line
// stands beside that RED, and any other status withholds GREEN whatever the
// script printed (gt-xyigz).
func TestTierSweepRig_ScriptTreeReadsTheScriptVerdict(t *testing.T) {
	t.Parallel()
	const (
		green = "tier-sweep: %s GREEN passed=3 failed=0 skipped=0 (logs /tmp/tier-sweep.x)\n"
		red   = "tier-sweep: %s RED passed=2 failed=1 skipped=0 failed: ./internal/cmd (logs /tmp/tier-sweep.x)\n"
	)
	for _, tc := range []struct {
		name            string
		integrationLine string // the summary line the script printed for the integration tier
		exitCode        int    // the integration and race stage's status
		wantIntegration string
		wantRace        string
		wantLastGreen   bool // whether the tree's sha is recorded green
	}{
		{name: "exit 0", integrationLine: green, exitCode: 0, wantIntegration: tierSweepGreen, wantRace: tierSweepGreen, wantLastGreen: true},
		{name: "exit 1 over a red integration", integrationLine: red, exitCode: 1, wantIntegration: tierSweepRed, wantRace: tierSweepGreen},
		{name: "exit 1 over GREEN lines alone is withheld", integrationLine: green, exitCode: 1, wantIntegration: tierSweepRed, wantRace: tierSweepRed},
		{name: "exit 2 over a red integration is withheld", integrationLine: red, exitCode: 2, wantIntegration: tierSweepRed, wantRace: tierSweepRed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, rec, _, _ := newTierSweepDaemon(t, now, "devops")
			d.tierSweepSeams.treeHas = treeWith("scripts/tier-sweep.sh")
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "abc123", nil }
			d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
			rec.result = func(st tierSweepStage) tierSweepStageResult {
				var b strings.Builder
				for _, tier := range st.tiers {
					if tier == "integration" {
						fmt.Fprintf(&b, tc.integrationLine, tier)
						continue
					}
					fmt.Fprintf(&b, green, tier)
				}
				// The shell tier is its own stage and passed there; the exit
				// status under test is the integration and race stage's.
				code := tc.exitCode
				if !st.slot {
					code = 0
				}
				return tierSweepStageResult{output: b.String(), exitCode: code, ran: true}
			}
			cycle := d.startDogCycle("tier_sweep")
			defer cycle.close()

			d.runTierSweepRig(context.Background(), cycle, "devops", now)

			wantCmds := []string{"bash scripts/tier-sweep.sh shell", "bash scripts/tier-sweep.sh integration race"}
			var gotCmds []string
			for _, st := range rec.stages {
				gotCmds = append(gotCmds, st.command())
			}
			if !slices.Equal(gotCmds, wantCmds) {
				t.Fatalf("commands run = %q, want %q", gotCmds, wantCmds)
			}
			st := tierSweepTestState(t, d, "devops")
			if got := st.Tiers["integration"].Verdict; got != tc.wantIntegration {
				t.Errorf("integration verdict = %q, want %q", got, tc.wantIntegration)
			}
			if got := st.Tiers["race"].Verdict; got != tc.wantRace {
				t.Errorf("race verdict = %q, want %q", got, tc.wantRace)
			}
			if got := st.Tiers["shell"].Verdict; got != tierSweepGreen {
				t.Errorf("shell verdict = %q, want GREEN from its own stage", got)
			}
			if got := st.LastGreenSHA == "abc123"; got != tc.wantLastGreen {
				t.Errorf("LastGreenSHA = %q, want recorded green = %v", st.LastGreenSHA, tc.wantLastGreen)
			}
			if got := st.Tiers["shell"].Command; got != "bash scripts/tier-sweep.sh shell" {
				t.Errorf("shell command on the record = %q, want the script the tree ran", got)
			}
		})
	}
}
