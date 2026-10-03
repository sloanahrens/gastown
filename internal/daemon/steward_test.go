package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
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

// TestStewardKindsDefaultsToRejection: the scan covers rejections unless the
// operator opts into review jobs, and an unknown kind is refused with the key
// named but read as the default, never wider (gt-9bioi.7).
func TestStewardKindsDefaultsToRejection(t *testing.T) {
	t.Parallel()
	names := func(kinds []steward.Kind) []string {
		out := make([]string, len(kinds))
		for i, k := range kinds {
			out[i] = string(k)
		}
		return out
	}
	for name, tc := range map[string]struct {
		cfg     *DaemonPatrolConfig
		want    []string
		wantErr bool
	}{
		"no config":     {nil, []string{"rejection"}, false},
		"unset":         {stewardPatrolConfig(&StewardConfig{Enabled: true}), []string{"rejection"}, false},
		"empty list":    {stewardPatrolConfig(&StewardConfig{Kinds: []string{}}), []string{"rejection"}, false},
		"rejection":     {stewardPatrolConfig(&StewardConfig{Kinds: []string{"rejection"}}), []string{"rejection"}, false},
		"review":        {stewardPatrolConfig(&StewardConfig{Kinds: []string{"review"}}), []string{"review"}, false},
		"both":          {stewardPatrolConfig(&StewardConfig{Kinds: []string{"rejection", "review"}}), []string{"rejection", "review"}, false},
		"review, duped": {stewardPatrolConfig(&StewardConfig{Kinds: []string{"review", "review"}}), []string{"review"}, false},
		"typo":          {stewardPatrolConfig(&StewardConfig{Kinds: []string{"reviw"}}), []string{"rejection"}, true},
		"empty string":  {stewardPatrolConfig(&StewardConfig{Kinds: []string{""}}), []string{"rejection"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := StewardKinds(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("StewardKinds error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "patrols.steward.kinds") {
				t.Errorf("refusal %q does not name the key", err)
			}
			if diff := names(got); !slices.Equal(diff, tc.want) {
				t.Errorf("StewardKinds = %v, want %v", diff, tc.want)
			}
		})
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

// TestStewardRunnerCompactsTheLedger: the ledger stops growing — a job that
// ended before the retention is dropped when the runner starts, and one from
// within it stays (gt-9bioi.6).
func TestStewardRunnerCompactsTheLedger(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ledger := steward.NewLedger(steward.LedgerPath(townRoot))
	now := time.Now()
	for _, j := range []steward.Job{
		{ID: "steward-old", Event: steward.KindReview, Bead: "gt-x", Rig: "gastown",
			Started: now.Add(-30 * 24 * time.Hour), Ended: now.Add(-30*24*time.Hour + time.Minute), Outcome: steward.OutcomePass},
		{ID: "steward-young", Event: steward.KindReview, Bead: "gt-y", Rig: "gastown",
			Started: now.Add(-time.Hour), Ended: now, Outcome: steward.OutcomePass},
	} {
		if err := ledger.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{
		config:       &Config{TownRoot: townRoot},
		logger:       discardLogger,
		patrolConfig: stewardPatrolConfig(&StewardConfig{Enabled: true, WorkRoot: t.TempDir()}),
	}
	if d.stewardRunnerFor(townRoot) == nil {
		t.Fatal("no runner")
	}
	jobs, err := ledger.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "steward-young" {
		t.Fatalf("ledger holds %+v, want only the job from within the retention", jobs)
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
	events := d.stewardEvents("gastown", []steward.Kind{steward.KindReview, steward.KindRejection}, func(string) bool { return false })
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
	d.stewardEvents("gastown", []steward.Kind{steward.KindReview, steward.KindRejection}, func(string) bool { return false })
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
		patrolConfig:  stewardPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"review"}, WorkRoot: t.TempDir()}),
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

// TestStewardScanHonorsKinds: the scan runs only the listed kinds, and an
// unlisted kind is not marked seen: a review event under the default
// [rejection] spawns nothing and still runs once review is enabled
// (gt-9bioi.7).
func TestStewardScanHonorsKinds(t *testing.T) {
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
			switch o.Label {
			case land.LabelReadyToLand:
				return []*beads.Issue{readyBead("gt-x", "c0ffee")}, nil
			case land.LabelRework:
				return []*beads.Issue{rejectedBead("gt-y", "beef")}, nil
			}
			return nil, nil
		},
	}
	d.runSteward()
	runner.Wait()
	got := sp.events()
	if len(got) != 1 || got[0].Bead != "gt-y" || got[0].Kind != steward.KindRejection {
		t.Fatalf("scan with kinds=[rejection] started %+v, want only the rejection job", got)
	}
	jobs, err := runner.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Event == steward.KindReview {
			t.Fatalf("a review job ran under kinds=[rejection]: %+v", j)
		}
	}
	// The head was not spent: enabling review still sees the queued head.
	d.patrolConfig = stewardPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"rejection", "review"}, WorkRoot: t.TempDir()})
	d.runSteward()
	runner.Wait()
	got = sp.events()
	if len(got) != 2 || got[1].Bead != "gt-x" || got[1].Kind != steward.KindReview {
		t.Fatalf("after enabling review the scan started %+v, want the queued review", got)
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
		patrolConfig:  stewardPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"review"}, WorkRoot: t.TempDir()}),
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

// TestStewardModeDefaultsToShadow: enabling the steward does not let it act,
// and a mode that is not one of the two never reads as live (gt-9bioi.4).
func TestStewardModeDefaultsToShadow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg     *DaemonPatrolConfig
		want    steward.Mode
		wantErr bool
	}{
		"no config":   {nil, steward.ModeShadow, false},
		"unset":       {stewardPatrolConfig(&StewardConfig{Enabled: true}), steward.ModeShadow, false},
		"shadow":      {stewardPatrolConfig(&StewardConfig{Mode: "shadow"}), steward.ModeShadow, false},
		"live":        {stewardPatrolConfig(&StewardConfig{Mode: "live"}), steward.ModeLive, false},
		"typo":        {stewardPatrolConfig(&StewardConfig{Mode: "Live"}), steward.ModeShadow, true},
		"unsupported": {stewardPatrolConfig(&StewardConfig{Mode: "dry-run"}), steward.ModeShadow, true},
	} {
		got, err := StewardMode(tc.cfg)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: StewardMode = %q, %v; want %q, error=%v", name, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestStewardShadowRunsDoNotSpendLiveEvents: a head the shadow steward
// reviewed is still an event once the operator switches to live, and each
// job's ledger row says which mode it ran in (gt-9bioi.4).
func TestStewardShadowRunsDoNotSpendLiveEvents(t *testing.T) {
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
		patrolConfig:  stewardPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"review"}, WorkRoot: t.TempDir()}),
		stewardRunner: runner,
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
		stewardListFn: func(_ string, o beads.ListOptions) ([]*beads.Issue, error) {
			if o.Label == land.LabelReadyToLand {
				return []*beads.Issue{readyBead("gt-x", "c0ffee")}, nil
			}
			return nil, nil
		},
	}
	scan := func() {
		d.runSteward()
		runner.Wait()
	}
	scan()
	scan()
	if got := sp.events(); len(got) != 1 || got[0].Mode != steward.ModeShadow {
		t.Fatalf("two shadow scans started %+v, want one shadow job", got)
	}
	d.patrolConfig = stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live", Kinds: []string{"review"}, WorkRoot: t.TempDir()})
	scan()
	scan()
	got := sp.events()
	if len(got) != 2 || got[1].Mode != steward.ModeLive {
		t.Fatalf("after the switch the scans started %+v, want one more job, live", got)
	}
	jobs, err := runner.Ledger.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || !jobs[0].Mode.Shadow() || jobs[1].Mode != steward.ModeLive {
		t.Fatalf("ledger rows = %+v, want a shadow job then a live one", jobs)
	}
}

// TestStewardOwnsReworkOnlyWhenLive is gt-28ibg: a rework bead belongs to the
// steward only while its patrol is enabled, live and covering the rig. A
// shadow job changes nothing outside its own comments, so it owns nothing a
// dispatcher would collide with.
func TestStewardOwnsReworkOnlyWhenLive(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg *DaemonPatrolConfig
		rig string
	}{
		"no config":                     {nil, "gastown"},
		"disabled":                      {stewardPatrolConfig(&StewardConfig{Enabled: false}), "gastown"},
		"enabled in shadow":             {stewardPatrolConfig(&StewardConfig{Enabled: true}), "gastown"},
		"shadow spelled out":            {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "shadow"}), "gastown"},
		"a mode typo is shadow":         {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "Live"}), "gastown"},
		"live on another rig":           {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live", Rigs: []string{"longeye"}}), "gastown"},
		"live on another rig, unlisted": {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live", Rigs: []string{"longeye"}}), ""},
	} {
		if got := stewardReworkOwner(tc.cfg, tc.rig); got != "" {
			t.Errorf("%s: stewardReworkOwner = %q, want no owner", name, got)
		}
	}
	for name, tc := range map[string]struct {
		cfg *DaemonPatrolConfig
		rig string
	}{
		"live":                  {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live"}), "gastown"},
		"live, rigs empty":      {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live", Rigs: nil}), "longeye"},
		"live, rig listed":      {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live", Rigs: []string{"longeye", "gastown"}}), "gastown"},
		"live, one of two rigs": {stewardPatrolConfig(&StewardConfig{Enabled: true, Mode: "live", Rigs: []string{"gastown"}}), "gastown"},
	} {
		got := stewardReworkOwner(tc.cfg, tc.rig)
		if got == "" {
			t.Fatalf("%s: stewardReworkOwner = no owner, want the steward named", name)
		}
		if !strings.Contains(got, tc.rig) {
			t.Errorf("%s: owner reason %q does not name rig %s", name, got, tc.rig)
		}
	}
}

// TestStewardReworkOwnerReadsTheTownConfig: the guard answers from the files
// the daemon reads, and a town-level disabled_patrols entry wins over an
// enabled:true in the patrol config (gt-28ibg).
func TestStewardReworkOwnerReadsTheTownConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"type":"daemon-patrol-config","version":1,"patrols":{"steward":{"enabled":true,"mode":"live"}}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "daemon.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := StewardReworkOwner(townRoot, "gastown"); got == "" {
		t.Fatal("a live steward in the town config owns nothing")
	}

	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(`{"disabled_patrols":["steward"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := StewardReworkOwner(townRoot, "gastown"); got != "" {
		t.Errorf("a disabled steward still owns rejections: %q", got)
	}
}

// planningBead is a spec the dispatcher routed to the planner: open, labeled
// needs-planning, with the notes given (a PLAN PROPOSAL block means the plan
// job already ran).
func planningBead(id, notes string) *beads.Issue {
	return &beads.Issue{ID: id, Status: "open", Labels: []string{specdispatch.NeedsPlanningLabel}, Notes: notes}
}

// planPatrolConfig is a patrol config with the plan key set, and the steward
// key as given (nil leaves it absent).
func planPatrolConfig(stewardCfg *StewardConfig, plan *StewardPlanConfig) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{Patrols: &PatrolsConfig{Steward: stewardCfg, StewardPlan: plan}}
}

// newPlanTestDaemon is a daemon whose plan scan reads one rig, whose bd list
// answers with issues, and whose git is a gitfake world with origin/main at a
// base commit — the branch a plan job checks out.
func newPlanTestDaemon(t *testing.T, cfg *DaemonPatrolConfig, sp steward.Spawner, issues []*beads.Issue) (*Daemon, *steward.Runner, *gitfake.Fake) {
	t.Helper()
	townRoot := t.TempDir()
	writeRigsJSON(t, townRoot, []string{"gastown"})
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":2,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ids := 0
	runner := &steward.Runner{
		Ledger:  steward.NewLedger(steward.LedgerPath(townRoot)),
		Spawn:   sp,
		WorkDir: t.TempDir(),
		MaxJobs: stewardRosterCap(cfg),
		Timeout: time.Minute,
		Logf:    func(string, ...any) {},
		NewID:   func() string { ids++; return strconv.Itoa(ids) },
	}
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        discardLogger,
		ctx:           t.Context(),
		patrolConfig:  cfg,
		stewardRunner: runner,
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) { return &beads.Issue{ID: id}, nil },
		stewardListFn: func(_ string, _ beads.ListOptions) ([]*beads.Issue, error) { return issues, nil },
	}
	f := useGitfake(t, d)
	repo := filepath.Join(townRoot, "gastown", ".repo.git")
	f.InitBare(t, repo)
	f.SetRef(t, repo, "refs/remotes/origin/main", f.Commit(t, repo, "main", "base", map[string]string{"README.md": "hi\n"}))
	return d, runner, f
}

// blockingSpawner holds every job until release, so a scan's cap is observed
// with jobs actually in flight. spawned signals each Spawn, which the runner
// hands to a goroutine of its own: a test waits on it instead of sleeping.
type blockingSpawner struct {
	mu      sync.Mutex
	seen    []steward.Event
	release chan struct{}
	spawned chan struct{}
}

func (b *blockingSpawner) Spawn(ctx context.Context, req steward.SpawnRequest) steward.SpawnResult {
	b.mu.Lock()
	b.seen = append(b.seen, req.Event)
	b.mu.Unlock()
	b.spawned <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return steward.SpawnResult{Verdict: &steward.Result{Outcome: steward.OutcomePass, Summary: "ok"}}
}

func (b *blockingSpawner) events() []steward.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]steward.Event(nil), b.seen...)
}

// TestStewardPlanPatrolIsOptIn: a plan job spends an LLM session, so planning
// is its own opt-in key and never rides the steward flag (gt-4k3fj.14).
func TestStewardPlanPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "steward_plan") {
		t.Error("plan jobs are on without a config")
	}
	if IsPatrolEnabled(planPatrolConfig(nil, &StewardPlanConfig{}), "steward_plan") {
		t.Error("plan jobs are on with no enabled flag")
	}
	if !IsPatrolEnabled(planPatrolConfig(nil, &StewardPlanConfig{Enabled: true}), "steward_plan") {
		t.Error("plan jobs are off with enabled:true")
	}
	if IsPatrolEnabled(planPatrolConfig(nil, &StewardPlanConfig{Enabled: true}), "steward") {
		t.Error("the plan key turned the steward patrol on")
	}
}

// TestStewardPlanScanDoesNothingByDefault: with patrols.steward_plan absent or
// false no plan job spawns, however many specs wait — and the landing-queue
// scan does not raise plan events either (gt-4k3fj.14).
func TestStewardPlanScanDoesNothingByDefault(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*DaemonPatrolConfig{
		"absent":   stewardPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"rejection"}}),
		"disabled": planPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"rejection"}}, &StewardPlanConfig{Enabled: false}),
	} {
		t.Run(name, func(t *testing.T) {
			sp := &countingSpawner{result: steward.SpawnResult{Verdict: &steward.Result{Outcome: steward.OutcomePass, Summary: "ok"}}}
			d, runner, _ := newPlanTestDaemon(t, cfg, sp, []*beads.Issue{planningBead("gt-pl", "")})
			d.runStewardPlan()
			runner.Wait()
			if got := sp.events(); len(got) != 0 {
				t.Fatalf("plan jobs spawned with the plan patrol off: %+v", got)
			}
			// The landing-queue scan covers its own labels, and a kind it does
			// not cover is filtered even when the list hands the bead back
			// (gt-9bioi.7).
			d.runSteward()
			runner.Wait()
			for _, ev := range sp.events() {
				if ev.Kind == steward.KindPlan {
					t.Fatalf("the landing-queue scan raised a plan event: %+v", ev)
				}
			}
		})
	}
}

// TestStewardPlanScanPlansOneJobPerSpecWithinTheRosterCap: with
// patrols.steward_plan on, one plan job runs per needs-planning spec, at most
// one at a time, and it takes a seat of the steward roster — 2 steward seats
// plus 1 plan seat by default (gt-4k3fj.14).
func TestStewardPlanScanPlansOneJobPerSpecWithinTheRosterCap(t *testing.T) {
	t.Parallel()
	cfg := planPatrolConfig(&StewardConfig{Enabled: true, Kinds: []string{"rejection"}}, &StewardPlanConfig{Enabled: true})
	if got := stewardRosterCap(cfg); got != 3 {
		t.Fatalf("stewardRosterCap = %d, want 3 (2 steward seats + 1 plan seat)", got)
	}
	sp := &blockingSpawner{release: make(chan struct{}), spawned: make(chan struct{}, 8)}
	d, runner, f := newPlanTestDaemon(t, cfg, sp, []*beads.Issue{planningBead("gt-p1", ""), planningBead("gt-p2", "")})
	if runner.MaxJobs != 3 {
		t.Fatalf("runner.MaxJobs = %d, want the 3-seat roster", runner.MaxJobs)
	}

	d.runStewardPlan()
	<-sp.spawned
	// The plan seat's cap is one: the second spec waits, and so does a second
	// scan while the first job is still in flight.
	beadsStarted := func() []string {
		var out []string
		for _, ev := range sp.events() {
			out = append(out, ev.Bead)
		}
		return out
	}
	if got := beadsStarted(); !slices.Equal(got, []string{"gt-p1"}) {
		t.Fatalf("started %v, want one plan job for gt-p1", got)
	}
	// A plan event names no head, and the spawner refuses one: the scan hands
	// the job the rig's default branch tip to check out (gt-4k3fj.14).
	if ev := sp.events()[0]; ev.Head == "" || ev.Branch != "main" {
		t.Fatalf("plan event %+v names no head to run at", ev)
	}
	d.runStewardPlan()
	if got := beadsStarted(); !slices.Equal(got, []string{"gt-p1"}) {
		t.Fatalf("a second scan started %v, want nothing while the plan seat is full", got)
	}
	if n := planJobsRunning(runner); n != 1 {
		t.Fatalf("plan jobs in flight = %d, want 1", n)
	}

	close(sp.release)
	runner.Wait()

	// Main moves on. A plan event names no head, so the spec already planned
	// must not be planned again because the tip it ran at is stale.
	repo := filepath.Join(d.config.TownRoot, "gastown", ".repo.git")
	f.SetRef(t, repo, "refs/remotes/origin/main", f.Commit(t, repo, "main", "advance", nil))

	d.runStewardPlan()
	runner.Wait()
	if got := beadsStarted(); !slices.Equal(got, []string{"gt-p1", "gt-p2"}) {
		t.Fatalf("started %v, want one plan job per spec over both scans", got)
	}
	jobs, err := runner.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 {
		t.Fatalf("ledger rows = %d, want a start and an end row for each of the 2 plan jobs: %+v", len(jobs), jobs)
	}
	for _, j := range jobs {
		if j.Event != steward.KindPlan {
			t.Errorf("ledger row %+v is not a plan job", j)
		}
	}
}
