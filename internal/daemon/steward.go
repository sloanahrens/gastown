package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/steward"
)

// Steward job runner (gt-9bioi.1).
//
// Every interval the daemon scans each rig's landing queue and spawns one
// headless job per event it has not handled: a bead that gained
// gt:ready-to-land with a new head (a review), and a bead the landing worker
// rejected (a rejection carrying the kind and findings). The jobs, their
// prompts and their authority are gt-9bioi.2's; this file is the trigger, the
// concurrency cap, the timeout and the ledger. Off unless
// patrols.steward.enabled is true.

const (
	defaultStewardInterval = 60 * time.Second

	// stewardBeadLimit bounds one rig's queue scan. The landing queue is
	// small (the daemon holds one submission per polecat), so a limit that
	// large is really "all of them".
	stewardBeadLimit = 200

	// stewardRowRetention is how long a job's ledger row and worktree are
	// kept. gt steward status reads the rows; 7 days matches the landing
	// logs' window.
	stewardRowRetention = 7 * 24 * time.Hour

	// stewardDrainTimeout bounds the wait for in-flight jobs at shutdown:
	// their kill grace plus the ledger write, not the whole job timeout.
	stewardDrainTimeout = 10 * time.Second
)

// drainStewardJobs waits for the runner's jobs to end, up to timeout. Their
// contexts are canceled by the time it is called, so this is how long a job
// takes to die and be recorded, not how long it may run (gt-9bioi.1).
func drainStewardJobs(r *steward.Runner, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		r.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func stewardConfig(config *DaemonPatrolConfig) *StewardConfig {
	if config == nil || config.Patrols == nil {
		return nil
	}
	return config.Patrols.Steward
}

func stewardInterval(config *DaemonPatrolConfig) time.Duration {
	if c := stewardConfig(config); c != nil && c.IntervalStr != "" {
		if d, err := time.ParseDuration(c.IntervalStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultStewardInterval
}

// stewardRigs is the configured rig allowlist within known.
func stewardRigs(config *DaemonPatrolConfig, known []string) []string {
	c := stewardConfig(config)
	if c == nil || len(c.Rigs) == 0 {
		return known
	}
	var out []string
	for _, r := range known {
		for _, want := range c.Rigs {
			if r == want {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// triggerSteward starts one scan on its own goroutine unless one is
// already running. It reports whether a scan started.
func (d *Daemon) triggerSteward() bool {
	d.triggerStewardMonitor()
	if !d.stewardRunning.CompareAndSwap(false, true) {
		d.logger.Printf("steward: previous scan still running, skipping")
		return false
	}
	d.stewardCycles.Add(1)
	go func() {
		defer d.stewardCycles.Done()
		defer d.stewardRunning.Store(false)
		d.runSteward()
	}()
	return true
}

// triggerStewardMonitor runs the monitoring pass on a goroutine of its own:
// an escalation retries for minutes when the town store is slow, and a scan
// waiting behind it would hold every review back (gt-9bioi.3).
func (d *Daemon) triggerStewardMonitor() {
	if !d.stewardMonitoring.CompareAndSwap(false, true) {
		return
	}
	d.stewardCycles.Add(1)
	go func() {
		defer d.stewardCycles.Done()
		defer d.stewardMonitoring.Store(false)
		d.monitorSteward()
	}()
}

// runSteward scans every rig the patrol covers and starts the jobs it finds
// room for. A job runs on past the scan; the runner owns it from there.
func (d *Daemon) runSteward() {
	if !d.isPatrolActive("steward") || d.config == nil || d.ctx == nil {
		return
	}
	townRoot := d.config.TownRoot
	runner := d.stewardRunnerFor(townRoot)
	if runner == nil {
		return
	}
	// A group that survived the startup reap may be gone by now.
	d.reapStewardOrphans(runner)
	ledger := steward.NewLedger(steward.LedgerPath(townRoot))
	jobs, err := ledger.Read()
	if err != nil {
		d.logger.Printf("steward: reading the ledger: %v", err)
		return
	}
	cfg := stewardConfig(d.patrolConfig)
	mode, modeErr := StewardMode(d.patrolConfig)
	if modeErr != nil {
		d.logger.Printf("steward: %v", modeErr)
	}
	// Only jobs of this mode spend an event: shadow runs on a head must not
	// stop live from acting on it once the operator switches, and a live run
	// already acted, so shadow need not repeat it.
	history := map[string][]steward.Job{}
	for _, j := range jobs {
		if j.Mode.Shadow() == mode.Shadow() {
			history[j.Key()] = append(history[j.Key()], j)
		}
	}
	// An event is spent once its history allows no further job: a failed
	// routine job still earns its one retry on the hard preset.
	seen := func(key string) bool {
		_, run := steward.ChooseModel(history[key], stewardRoutineAgent(cfg), stewardHardAgent(cfg))
		return !run
	}
	var started []string
	for _, rigName := range stewardRigs(d.patrolConfig, d.getKnownRigs()) {
		if d.ctx.Err() != nil {
			return
		}
		if ok, why := d.isRigOperational(rigName); !ok {
			d.logger.Printf("steward: %s: not scanning: %s", rigName, why)
			continue
		}
		for _, ev := range d.stewardEvents(rigName, seen) {
			if runner.RunningBead(ev.Bead) {
				continue
			}
			ev.Mode = mode
			model, run := steward.StartedModel(ev, history[ev.Key()], stewardRoutineAgent(cfg), stewardHardAgent(cfg))
			if !run {
				continue
			}
			prompt, err := steward.PromptFor(ev, model != stewardRoutineAgent(cfg))
			if err != nil {
				d.logger.Printf("steward: %s: not started: %v", ev.Bead, err)
				continue
			}
			if !runner.Start(d.ctx, ev, model, prompt) {
				d.logger.Printf("steward: %s: not started: the concurrency cap (%d) is full or the bead is already running", ev.Bead, runner.MaxJobs)
				return
			}
			started = append(started, string(ev.Kind)+":"+ev.Bead)
		}
	}
	if len(started) > 0 {
		d.logger.Printf("steward: started %d job(s): %v", len(started), started)
	}
}

// stewardEvents is the events one rig's landing queue raises that no job has
// handled. A scan that cannot read a queue is logged and skipped: the next
// scan retries it, and acting on a partial list would drop the rest.
func (d *Daemon) stewardEvents(rigName string, seen func(key string) bool) []steward.Event {
	rigPath := filepath.Join(d.config.TownRoot, rigName)
	list := d.stewardListFn
	if list == nil {
		bd := beads.NewWithBeadsDir(rigPath, beads.ResolveBeadsDir(rigPath))
		list = func(_ string, opts beads.ListOptions) ([]*beads.Issue, error) { return bd.List(opts) }
	}
	var out []steward.Event
	for _, label := range []string{land.LabelReadyToLand, land.LabelRework} {
		// Priority -1 is "no filter": the zero value asks bd for P0 beads only.
		issues, err := list(rigPath, beads.ListOptions{Status: "open", Label: label, Priority: -1, Limit: stewardBeadLimit})
		if err != nil {
			d.logger.Printf("steward: %s: listing %s beads: %v", rigName, label, err)
			continue
		}
		for _, issue := range issues {
			if ev, ok := steward.Detect(issue, rigName, seen); ok {
				out = append(out, ev)
			}
		}
	}
	return out
}

// stewardRunnerFor is the runner for this daemon process, built on first use.
// One runner per process is what makes the concurrency cap and the
// one-job-per-bead rule hold across scans, not just within one.
func (d *Daemon) stewardRunnerFor(townRoot string) *steward.Runner {
	if d.stewardRunner != nil {
		return d.stewardRunner
	}
	cfg := stewardConfig(d.patrolConfig)
	if cfg == nil {
		return nil
	}
	root, err := stewardWorkRoot(cfg.WorkRoot, townRoot)
	if err != nil {
		d.logger.Printf("steward: %v", err)
		return nil
	}
	if err := ensurePrivateDir(root); err != nil {
		d.logger.Printf("steward: creating the work root %s: %v", root, err)
		return nil
	}
	ledger := steward.NewLedger(steward.LedgerPath(townRoot))
	runner := &steward.Runner{
		Ledger:   ledger,
		WorkDir:  root,
		MaxJobs:  stewardMaxJobs(cfg),
		Timeout:  stewardJobTimeout(cfg),
		Logf:     d.logger.Printf,
		SpawnFor: d.stewardSpawnerFor(),
	}
	d.reapStewardOrphans(runner)
	d.stewardRunnerMu.Lock()
	d.stewardRunner = runner
	d.stewardRunnerMu.Unlock()
	d.logger.Printf("steward: runner started (work root %s, max jobs %d, job timeout %s)", root, runner.MaxJobs, runner.Timeout)
	d.pruneStewardWorktrees(root, townRoot)
	return d.stewardRunner
}

// reapStewardOrphans closes the ledger's running jobs this process did not
// start. A job is a child of the daemon in a group of its own, so one whose
// daemon died may be running still: the runner kills its group before it
// closes the row, and a group it cannot kill keeps its bead busy until a
// later scan manages it (gt-9bioi.5).
func (d *Daemon) reapStewardOrphans(runner *steward.Runner) {
	n, err := runner.ReapOrphans()
	if err != nil {
		d.logger.Printf("steward: jobs left running by a previous daemon, still alive: %v", err)
	}
	if n > 0 {
		d.logger.Printf("steward: closed %d job(s) left running by a previous daemon", n)
	}
}

// pruneStewardWorktrees removes job worktrees a job failed to remove, once
// per daemon process: a worktree is a full checkout, so leaving them to
// accumulate is the failure this prevents (gt-9bioi.1).
func (d *Daemon) pruneStewardWorktrees(root, townRoot string) {
	for _, rigName := range d.getKnownRigs() {
		repo := filepath.Join(townRoot, rigName, ".repo.git")
		if _, err := os.Stat(repo); err != nil {
			continue
		}
		if err := steward.PruneJobDirs(gtgit.NewGit(repo), root, stewardRowRetention, d.clk().Now()); err != nil {
			d.logger.Printf("steward: pruning %s's job worktrees: %v", rigName, err)
		}
	}
}

// stewardSpawnerFor builds the production spawner per rig: each job runs in
// its rig's own repository, at the head the event names.
func (d *Daemon) stewardSpawnerFor() func(ev steward.Event) steward.Spawner {
	townRoot := d.config.TownRoot
	return func(ev steward.Event) steward.Spawner {
		return &steward.AgentSpawner{
			TownRoot: townRoot,
			Rig:      ev.Rig,
			Repo:     filepath.Join(townRoot, ev.Rig, ".repo.git"),
			Logf:     d.logger.Printf,
		}
	}
}

// stewardWorkRoot is where job worktrees are created: the configured
// work_root, else $TMPDIR/gt-steward-<uid>. It is never under the town root,
// where git refuses a worktree (the rule landingWorkRoot keeps).
func stewardWorkRoot(configured, townRoot string) (string, error) {
	base := configured
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("gt-steward-%d", os.Getuid()))
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("patrols.steward.work_root %q must be an absolute path", base)
	}
	if landingPathWithin(base, townRoot) {
		return "", fmt.Errorf("patrols.steward.work_root %q is under the town root %s; git refuses worktrees there, so set it outside the town (or leave it empty for $TMPDIR/gt-steward-<uid>)", base, townRoot)
	}
	return base, nil
}

// StewardMode is the mode the patrol config selects: shadow unless
// patrols.steward.mode says live. A value that is neither returns shadow with
// the error, never live.
func StewardMode(config *DaemonPatrolConfig) (steward.Mode, error) {
	c := stewardConfig(config)
	if c == nil {
		return steward.ModeShadow, nil
	}
	return steward.ParseMode(c.Mode)
}

func stewardRoutineAgent(c *StewardConfig) string {
	if c != nil && c.RoutineAgent != "" {
		return c.RoutineAgent
	}
	return steward.DefaultRoutineAgent
}

func stewardHardAgent(c *StewardConfig) string {
	if c != nil && c.HardAgent != "" {
		return c.HardAgent
	}
	return steward.DefaultHardAgent
}

func stewardMaxJobs(c *StewardConfig) int {
	if c != nil && c.MaxJobs > 0 {
		return c.MaxJobs
	}
	return steward.DefaultMaxJobs
}

func stewardJobTimeout(c *StewardConfig) time.Duration {
	if c != nil && c.JobTimeoutStr != "" {
		if d, err := time.ParseDuration(c.JobTimeoutStr); err == nil && d > 0 {
			return d
		}
	}
	return steward.DefaultJobTimeout
}
