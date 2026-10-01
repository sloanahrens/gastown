package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/steward"
)

// readyBead and rejectedBead are the two queue shapes the scan reads, written
// the way gt done and Land write them.
func readyBead(id, head string) *beads.Issue {
	return &beads.Issue{
		ID: id, Status: "open", Assignee: "gastown/polecats/emerald", Labels: []string{land.LabelReadyToLand},
		Notes: land.FormatReadyNote(land.Work{Branch: "polecat/emerald/" + id, Head: head, Target: "main", Worker: "emerald"}),
	}
}

func rejectedBead(id, head string) *beads.Issue {
	return &beads.Issue{
		ID: id, Status: "open", Labels: []string{land.LabelRework},
		Notes: land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: "gate", Reason: "make test exit 2", Branch: "polecat/emerald/" + id, Target: "main", MR: id, Head: head}),
	}
}

func stewardPatrolConfig(c *StewardConfig) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{Patrols: &PatrolsConfig{Steward: c}}
}

// TestStewardPatrolIsOptIn: the jobs act on submitted work, so only an
// explicit enabled:true starts the scan (gt-9bioi.1).
func TestStewardPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "steward") {
		t.Error("steward is on without a config")
	}
	if IsPatrolEnabled(stewardPatrolConfig(&StewardConfig{}), "steward") {
		t.Error("steward is on with no enabled flag")
	}
	if !IsPatrolEnabled(stewardPatrolConfig(&StewardConfig{Enabled: true}), "steward") {
		t.Error("steward is off with enabled:true")
	}
}

func TestStewardConfigDefaults(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg                   *StewardConfig
		wantInterval          time.Duration
		wantJobs              int
		wantTimeout           time.Duration
		wantRoutine, wantHard string
	}{
		"nothing configured": {nil, defaultStewardInterval, steward.DefaultMaxJobs, steward.DefaultJobTimeout, steward.DefaultRoutineAgent, steward.DefaultHardAgent},
		"configured": {&StewardConfig{IntervalStr: "5m", MaxJobs: 4, JobTimeoutStr: "10m", RoutineAgent: "deepseek-flash-x", HardAgent: "deepseek-pro-x"},
			5 * time.Minute, 4, 10 * time.Minute, "deepseek-flash-x", "deepseek-pro-x"},
		"garbage durations keep the defaults": {&StewardConfig{IntervalStr: "soon", JobTimeoutStr: "-1m"},
			defaultStewardInterval, steward.DefaultMaxJobs, steward.DefaultJobTimeout, steward.DefaultRoutineAgent, steward.DefaultHardAgent},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := stewardPatrolConfig(tc.cfg)
			if got := stewardInterval(cfg); got != tc.wantInterval {
				t.Errorf("interval = %v, want %v", got, tc.wantInterval)
			}
			if got := stewardMaxJobs(tc.cfg); got != tc.wantJobs {
				t.Errorf("max jobs = %d, want %d", got, tc.wantJobs)
			}
			if got := stewardJobTimeout(tc.cfg); got != tc.wantTimeout {
				t.Errorf("job timeout = %v, want %v", got, tc.wantTimeout)
			}
			if got := stewardRoutineAgent(tc.cfg); got != tc.wantRoutine {
				t.Errorf("routine agent = %q, want %q", got, tc.wantRoutine)
			}
			if got := stewardHardAgent(tc.cfg); got != tc.wantHard {
				t.Errorf("hard agent = %q, want %q", got, tc.wantHard)
			}
		})
	}
}

func TestStewardRigs(t *testing.T) {
	t.Parallel()
	known := []string{"gastown", "longeye"}
	if got := stewardRigs(stewardPatrolConfig(nil), known); len(got) != 2 {
		t.Errorf("no rigs configured = %v, want every known rig", got)
	}
	cfg := stewardPatrolConfig(&StewardConfig{Enabled: true, Rigs: []string{"longeye"}})
	if got := stewardRigs(cfg, known); len(got) != 1 || got[0] != "longeye" {
		t.Errorf("rigs = %v, want longeye", got)
	}
}

// TestStewardWorkRootRefusesTheTown: git refuses a worktree under the town
// root, so a configured path there fails at the scan rather than at every
// job (the rule landingWorkRoot keeps).
func TestStewardWorkRootRefusesTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	under := filepath.Join(town, "work")
	if _, err := stewardWorkRoot(under, town); err == nil || !strings.Contains(err.Error(), "under the town root") {
		t.Errorf("work root under the town = %v, want a refusal", err)
	}
	if _, err := stewardWorkRoot("work", town); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("a relative work root = %v, want a refusal", err)
	}
	outside := t.TempDir()
	got, err := stewardWorkRoot(outside, town)
	if err != nil || got != outside {
		t.Errorf("work root = %q, %v; want %q", got, err, outside)
	}
	if got, err := stewardWorkRoot("", town); err != nil || !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("default work root = %q, %v; want one under %s", got, err, os.TempDir())
	}
}

// TestStewardRunnerClosesRowsLeftByThePreviousProcess: a job is a child of
// the daemon, so a row without an end time is one whose daemon died, and the
// next process must not read it as still running forever (gt-9bioi.1).
func TestStewardRunnerClosesRowsLeftByThePreviousProcess(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ledger := steward.NewLedger(steward.LedgerPath(townRoot))
	stale := steward.Job{
		ID: "steward-1", Event: steward.KindReview, Bead: "gt-x", Rig: "gastown",
		Started: time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC),
	}
	if err := ledger.Append(stale); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		config:       &Config{TownRoot: townRoot},
		logger:       discardLogger,
		patrolConfig: stewardPatrolConfig(&StewardConfig{Enabled: true, WorkRoot: t.TempDir()}),
	}
	if d.stewardRunnerFor(townRoot) == nil {
		t.Fatal("no runner")
	}
	active, err := ledger.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("the stale row is still running: %+v", active)
	}
	// One runner per process: the cap and the one-job-per-bead rule have to
	// hold across scans.
	if d.stewardRunnerFor(townRoot) != d.stewardRunner {
		t.Error("a second scan built a second runner")
	}
}

// TestStewardScanFindsQueueEvents: the scan asks bd for every priority (the
// zero value would ask for P0 only) and turns the two labeled queues into
// the events their jobs run on.
func TestStewardScanFindsQueueEvents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var opts []string
	d := &Daemon{
		config: &Config{TownRoot: root},
		logger: discardLogger,
		stewardListFn: func(_ string, o beads.ListOptions) ([]*beads.Issue, error) {
			opts = append(opts, o.Label)
			switch o.Label {
			case land.LabelReadyToLand:
				return []*beads.Issue{readyBead("gt-x", "c0ffee")}, nil
			case land.LabelRework:
				return []*beads.Issue{rejectedBead("gt-y", "beef")}, nil
			}
			return nil, nil
		},
	}
	events := d.stewardEvents("gastown", func(string) bool { return false })
	if len(events) != 2 {
		t.Fatalf("events = %+v, want a review and a rejection", events)
	}
	if events[0].Kind != steward.KindReview || events[0].Bead != "gt-x" || events[0].Head != "c0ffee" {
		t.Errorf("review event = %+v", events[0])
	}
	if events[1].Kind != steward.KindRejection || events[1].Bead != "gt-y" {
		t.Errorf("rejection event = %+v", events[1])
	}
	if len(opts) != 2 || opts[0] != land.LabelReadyToLand || opts[1] != land.LabelRework {
		t.Errorf("the scan asked for %v", opts)
	}
}

// TestStewardListAsksEveryPriority guards the ListOptions zero value: bd
// reads Priority 0 as "only P0 beads", which would hide every P1 and P2
// submission from the scan.
func TestStewardListAsksEveryPriority(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var got []beads.ListOptions
	d := &Daemon{
		config: &Config{TownRoot: root},
		logger: discardLogger,
		stewardListFn: func(_ string, o beads.ListOptions) ([]*beads.Issue, error) {
			got = append(got, o)
			return nil, nil
		},
	}
	d.stewardEvents("gastown", func(string) bool { return false })
	if len(got) == 0 {
		t.Fatal("the scan asked bd nothing")
	}
	for _, o := range got {
		if o.Priority != -1 {
			t.Errorf("list options %+v: Priority must be -1 (no filter)", o)
		}
		if o.Status != "open" {
			t.Errorf("list options %+v: closed beads are not queue events", o)
		}
	}
}

// countingSpawner answers every job with a pass and counts them.
type countingSpawner struct {
	mu     sync.Mutex
	seen   []steward.Event
	result steward.SpawnResult
}

func (c *countingSpawner) Spawn(_ context.Context, req steward.SpawnRequest) steward.SpawnResult {
	c.mu.Lock()
	c.seen = append(c.seen, req.Event)
	c.mu.Unlock()
	return c.result
}

func (c *countingSpawner) events() []steward.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]steward.Event(nil), c.seen...)
}

// TestStewardScanStartsOneJobPerHead: a scan spawns a job for an event no job
// has handled, and the next scan leaves that head alone — the dedupe the
// whole design rests on (gt-9bioi.1).
func TestStewardScanStartsOneJobPerHead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRigsJSON(t, townRoot, []string{"gastown"})
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":2,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := &countingSpawner{result: steward.SpawnResult{Verdict: &steward.Result{Outcome: steward.OutcomePass, Summary: "ok"}}}
	ids := 0
	runner := &steward.Runner{
		Ledger:  steward.NewLedger(steward.LedgerPath(townRoot)),
		Spawn:   sp,
		WorkDir: t.TempDir(),
		MaxJobs: steward.DefaultMaxJobs,
		Timeout: time.Minute,
		Logf:    func(string, ...any) {},
		NewID:   func() string { ids++; return strconv.Itoa(ids) },
	}
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        discardLogger,
		ctx:           t.Context(),
		patrolConfig:  stewardPatrolConfig(&StewardConfig{Enabled: true, WorkRoot: t.TempDir()}),
		stewardRunner: runner,
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
		stewardListFn: func(_ string, o beads.ListOptions) ([]*beads.Issue, error) {
			if o.Label == land.LabelReadyToLand {
				return []*beads.Issue{readyBead("gt-x", "c0ffee")}, nil
			}
			return nil, nil
		},
	}
	d.runSteward()
	runner.Wait()
	if got := sp.events(); len(got) != 1 || got[0].Bead != "gt-x" {
		t.Fatalf("first scan started %+v, want one job for gt-x", got)
	}
	// The head is in the ledger now: the next scan must not run it again.
	d.runSteward()
	runner.Wait()
	if got := sp.events(); len(got) != 1 {
		t.Fatalf("second scan started %+v, want nothing", got[1:])
	}
	jobs, err := runner.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[1].Outcome != steward.OutcomePass {
		t.Fatalf("ledger = %+v, want a start row and a pass", jobs)
	}
}

// TestStewardScanRetriesAFailedRoutineJobOnHard: a routine job that fails
// earns one retry on the hard preset, and nothing after that (gt-9bioi.1).
func TestStewardScanRetriesAFailedRoutineJobOnHard(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRigsJSON(t, townRoot, []string{"gastown"})
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":2,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := &countingSpawner{result: steward.SpawnResult{Verdict: &steward.Result{Outcome: steward.OutcomeFail, Summary: "no"}}}
	ids := 0
	runner := &steward.Runner{
		Ledger:  steward.NewLedger(steward.LedgerPath(townRoot)),
		Spawn:   sp,
		WorkDir: t.TempDir(),
		MaxJobs: steward.DefaultMaxJobs,
		Timeout: time.Minute,
		Logf:    func(string, ...any) {},
		NewID:   func() string { ids++; return strconv.Itoa(ids) },
	}
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        discardLogger,
		ctx:           t.Context(),
		patrolConfig:  stewardPatrolConfig(&StewardConfig{Enabled: true, WorkRoot: t.TempDir()}),
		stewardRunner: runner,
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
		stewardListFn: func(_ string, o beads.ListOptions) ([]*beads.Issue, error) {
			if o.Label == land.LabelReadyToLand {
				return []*beads.Issue{readyBead("gt-x", "c0ffee")}, nil
			}
			return nil, nil
		},
	}
	for range 3 {
		d.runSteward()
		runner.Wait()
	}
	if got := sp.events(); len(got) != 2 {
		t.Fatalf("three scans started %d job(s), want the routine one and one hard retry", len(got))
	}
	jobs, err := runner.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, j := range jobs {
		if j.Outcome.Failed() {
			models = append(models, j.Model)
		}
	}
	if len(models) != 2 || models[0] != steward.DefaultRoutineAgent || models[1] != steward.DefaultHardAgent {
		t.Fatalf("failed jobs ran on %v, want [%s %s]", models, steward.DefaultRoutineAgent, steward.DefaultHardAgent)
	}
}

// TestStewardScanNeedsItsPatrol: the tick does nothing while the patrol is
// off, however the daemon is wired.
func TestStewardScanNeedsItsPatrol(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: root},
		logger: discardLogger,
		ctx:    t.Context(),
	}
	d.runSteward()
	if d.stewardRunner != nil {
		t.Error("a scan with the patrol off built a runner")
	}
	if !d.triggerSteward() {
		t.Error("the first trigger did not start")
	}
	d.stewardCycles.Wait()
}
