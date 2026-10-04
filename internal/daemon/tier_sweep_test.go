package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// at builds a fake-clock time at the given local hour so the even-hour
// selection is decided the same way production decides it.
func atHour(t time.Time, hour int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), hour, 0, 0, 0, time.Local)
}

func TestRunTierSweep_RunsShellEveryCycleAndIntegrationOnEvenHours(t *testing.T) {
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
	if got := len(rec.stages); got != 1 || strings.Join(rec.stages[0].tiers, " ") != "shell" {
		t.Fatalf("odd-hour stages = %+v, want one shell stage", rec.stages)
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != sha || st.Tiers["shell"].Verdict != tierSweepGreen {
		t.Errorf("state = %+v, want a green shell at %s", st, sha)
	}
	if st.LastGreenSHA != "" {
		t.Errorf("LastGreenSHA = %q after a shell-only run, want empty: integration was not covered", st.LastGreenSHA)
	}

	// The same sha on an even hour: the integration stage is due and runs,
	// because no complete sweep has been green at this sha yet.
	rec.stages = nil
	d.clock = clockwork.NewFakeClockAt(atHour(now, 16))
	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred on the even hour")
	}
	if got := len(rec.stages); got != 2 {
		t.Fatalf("even-hour stages = %+v, want shell then integration", rec.stages)
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
	st = tierSweepTestState(t, d, "gastown")
	if st.LastGreenSHA != sha {
		t.Errorf("LastGreenSHA = %q, want %s after a complete green sweep", st.LastGreenSHA, sha)
	}

	// With a complete green sweep at this sha, the next cycle is silent.
	rec.stages = nil
	d.clock = clockwork.NewFakeClockAt(atHour(now, 17))
	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred on an unchanged sha")
	}
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none: origin/main is unchanged since the last green sweep", rec.stages)
	}

	// A moved sha runs again, shell only on an odd hour.
	moved := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return moved, nil }
	if !d.runTierSweep() {
		t.Fatal("runTierSweep deferred after a moved sha")
	}
	if len(rec.stages) != 1 || strings.Join(rec.stages[0].tiers, " ") != "shell" {
		t.Fatalf("stages after a moved sha = %+v, want one shell stage", rec.stages)
	}
	st = tierSweepTestState(t, d, "gastown")
	if st.LastSHA != moved || st.LastGreenSHA != sha {
		t.Errorf("state = %+v, want LastSHA %s and LastGreenSHA %s", st, moved, sha)
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

	if len(bd.created) != 2 {
		t.Fatalf("created = %+v, want one bead per failing package", bd.created)
	}
	if got := bd.created[0].Title; got != "tier sweep (gastown): shell: ./internal/cmd" {
		t.Errorf("title = %q", got)
	}
	for _, c := range bd.created {
		if len(c.Labels) != 1 || c.Labels[0] != tierSweepRedLabel {
			t.Errorf("labels = %v, want %s", c.Labels, tierSweepRedLabel)
		}
	}

	// A second red run comments on the open beads instead of refiling them.
	bd.created = nil
	bd.open = []*beads.Issue{
		{ID: "gt-1", Title: "tier sweep (gastown): shell: ./internal/cmd"},
		{ID: "gt-2", Title: "tier sweep (gastown): shell: ./internal/daemon"},
	}
	d.runTierSweep()
	if len(bd.created) != 0 {
		t.Errorf("created = %+v, want comments on the open beads instead", bd.created)
	}
	if len(bd.comments) != 2 {
		t.Errorf("comments = %v, want one per open bead", bd.comments)
	}

	// Green closes the tier's open beads.
	rec.green()
	bd.comments, bd.open = nil, []*beads.Issue{
		{ID: "gt-1", Title: "tier sweep (gastown): shell: ./internal/cmd"},
		{ID: "gt-9", Title: "tier sweep (gastown): integration: ./internal/cmd"},
	}
	d.runTierSweep()
	if len(bd.closed) != 1 || !strings.HasPrefix(bd.closed[0], "gt-1 ") {
		t.Errorf("closed = %v, want only the shell tier's bead", bd.closed)
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
	stages, _ := tierSweepStages("gastown", atHour(time.Now(), 16))
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
		"tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs /tmp/tier-sweep.aB12)",
		"tier-sweep: integration GREEN passed=29 failed=0 skipped=2",
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
