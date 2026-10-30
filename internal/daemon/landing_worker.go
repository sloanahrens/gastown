package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/polecat/spans"
	"github.com/steveyegge/gastown/internal/promote"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/version"
)

// The landing worker (ADR 0004, gt-v4ssj.2): one goroutine per rig, each
// landing that rig's gt:ready-to-land beads one at a time, rigs in parallel.
// It is opt-in (patrols.landing_worker.enabled) because it pushes main.

const (
	defaultLandingWorkerInterval = 60 * time.Second
	landingLogRetention          = 7 * 24 * time.Hour
	landingRemoteFetchTimeout    = 2 * time.Minute
)

// beadReader reads one bead's record; landworker.RetryBeads is one.
type beadReader interface {
	Show(id string) (*beads.Issue, error)
}

// landingPersonKey is the escalation key for a landing left for a person, named
// by the person label the bead carries, so an overseer hold does not upsert
// onto an old label's escalation (gt-slmaz). A bead carrying two labels reads
// as the overseer's, the first step; a bead with no person label keeps the
// landing-needs-human key, where a person hold was raised before the new labels
// existed. A bead whose labels cannot be read gets a key that names no label:
// the hold is still raised, and it cannot land on another label's escalation.
func landingPersonKey(reader beadReader, beadID string) string {
	const old = "landing-needs-human:"
	is, err := reader.Show(beadID)
	if err != nil || is == nil {
		return "landing-needs-person:" + beadID
	}
	for _, p := range []struct{ label, key string }{
		{land.LabelNeedsOverseer, "landing-needs-overseer:"},
		{land.LabelNeedsOperator, "landing-needs-operator:"},
		{land.LabelNeedsHuman, old},
	} {
		if beads.HasLabel(is, p.label) {
			return p.key + beadID
		}
	}
	return old + beadID
}

func landingWorkerConfig(config *DaemonPatrolConfig) *LandingWorkerConfig {
	if config == nil || config.Patrols == nil {
		return nil
	}
	return config.Patrols.LandingWorker
}

func landingWorkerInterval(config *DaemonPatrolConfig) time.Duration {
	if c := landingWorkerConfig(config); c != nil && c.IntervalStr != "" {
		if d, err := time.ParseDuration(c.IntervalStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultLandingWorkerInterval
}

// landingForgejoMergeSlack is the wall a cut-over rig's landing reserves after
// its CI wait and its om review, for the merge itself: the PR merge call, the
// read-back that confirms it, and the record's own writes (gt-fn9e6.26).
const landingForgejoMergeSlack = 5 * time.Minute

// landingRigLandTimeout is one rig's landing deadline. A landing spends its
// candidate gate's whole CI wait before om even starts and then has to merge,
// so the deadline is the CI wait plus the om timeout plus the merge slack —
// 30 minutes at the defaults — rather than a flat value that would cut off the
// wait the gate is still legitimately running (gt-fn9e6.26).
func landingRigLandTimeout(config *DaemonPatrolConfig) time.Duration {
	return land.DefaultCandidateWaitTimeout + landingOMBudget(landingWorkerConfig(config)) + landingForgejoMergeSlack
}

const defaultPostLandTimeout = 60 * time.Minute

func landingWorkerDuration(s string, def time.Duration) time.Duration {
	if s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// landingReviewEnabled is true unless the config explicitly sets
// review:false: the om review runs on every landing by default.
func landingReviewEnabled(config *DaemonPatrolConfig) bool {
	c := landingWorkerConfig(config)
	return c == nil || c.Review == nil || *c.Review
}

// resolveOMPath is the configured om, else om on PATH, else
// $HOME/go/bin/om (where `go install` puts it; the daemon's launchd PATH
// often lacks it).
func resolveOMPath(configured string, lookPath func(string) (string, error), home string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	if p, err := lookPath("om"); err == nil {
		return p
	}
	if home != "" {
		return filepath.Join(home, "go", "bin", "om")
	}
	return "om"
}

// landingWorkRoot is the private base directory for one rig's throwaway
// worktrees: the configured work_root, else $TMPDIR/gt-landing-<uid> (the
// uid-scoped pattern of the gate's GATE_DIR, gt-22hdp.51), plus the rig. It
// is never under the town root: internal/git refuses a worktree target there
// (ErrUnsafeTownRootGitMutation), which failed every landing.
func landingWorkRoot(configured, townRoot, rigName string) (string, error) {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("gt-landing-%d", os.Getuid()))
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("patrols.landing_worker.work_root %q must be an absolute path", base)
	}
	root := filepath.Join(base, rigName)
	if landingPathWithin(root, townRoot) {
		return "", fmt.Errorf("patrols.landing_worker.work_root %q is under the town root %s; git refuses worktrees there, so set it outside the town (or leave it empty for $TMPDIR/gt-landing-<uid>)", base, townRoot)
	}
	return root, nil
}

// landingPathWithin reports whether path is dir or below it, comparing both the
// cleaned paths and their symlink-resolved forms (macOS /var -> /private/var).
func landingPathWithin(path, dir string) bool {
	within := func(p, d string) bool {
		rel, err := filepath.Rel(d, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	p, d := filepath.Clean(path), filepath.Clean(dir)
	if within(p, d) {
		return true
	}
	return within(resolveExisting(p), resolveExisting(d))
}

// resolveExisting resolves symlinks in the longest existing prefix of p.
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		if parent := filepath.Dir(cur); parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}

// ensurePrivateDir creates dir 0700 and tightens an existing one.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// landingWorkerRigs is the configured rig allowlist within known.
func landingWorkerRigs(config *DaemonPatrolConfig, known []string) []string {
	c := landingWorkerConfig(config)
	if c == nil || len(c.Rigs) == 0 {
		return known
	}
	var out []string
	for _, r := range known {
		if slices.Contains(c.Rigs, r) {
			out = append(out, r)
		}
	}
	return out
}

// landingWorkers tracks which rigs have a running worker goroutine.
type landingWorkers struct {
	mu      sync.Mutex
	running map[string]bool
}

// landingQueueState is the daemon's one landing-queue view, built on its first
// use: the workers publish into it and the burst sampler's Env reads it.
type landingQueueState struct {
	once sync.Once
	view *landworker.LandingQueueView
}

// landingQueueView is the view where each rig's worker publishes its count and
// the burst sampler reads the queue from (gt-hw6ih). It takes the daemon's
// clock, so one clock decides when a published count goes stale.
func (d *Daemon) landingQueueView() *landworker.LandingQueueView {
	d.landingQueue.once.Do(func() {
		d.landingQueue.view = landworker.NewLandingQueueView(func() time.Time { return d.clk().Now() })
	})
	return d.landingQueue.view
}

// startLandingWorkers starts the manager that keeps one worker goroutine per
// rig. It returns at once; everything stops with d.ctx.
func (d *Daemon) startLandingWorkers() {
	if d.config == nil || d.ctx == nil {
		return
	}
	lw := &landingWorkers{running: map[string]bool{}}
	go d.runLandingWorkerManager(lw)
}

func (d *Daemon) runLandingWorkerManager(lw *landingWorkers) {
	interval := landingWorkerInterval(d.patrolConfig)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	announced := false
	for {
		if d.isPatrolActive("landing_worker") {
			if !announced {
				d.logger.Printf("landing_worker: enabled (pass interval %v, land timeout %v)", interval, landingRigLandTimeout(d.patrolConfig))
				announced = true
			}
			// The om the workers review with is a per-daemon reading, not a
			// per-rig one, and it lives off this loop's goroutine (om_skew.go).
			d.startOMSkewMonitor()
			// So is the watch over the deploys that run after these landings
			// (deploy_watch.go): it polls the same rigs' repositories.
			d.startDeployWatch()
			for _, rigName := range landingWorkerRigs(d.patrolConfig, d.getKnownRigs()) {
				lw.mu.Lock()
				if !lw.running[rigName] {
					lw.running[rigName] = true
					go d.runRigLandingWorker(lw, rigName, interval)
				}
				lw.mu.Unlock()
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRigLandingWorker is one rig's worker: a pass, then the interval, until
// the daemon stops or the patrol is disabled.
func (d *Daemon) runRigLandingWorker(lw *landingWorkers, rigName string, interval time.Duration) {
	defer func() {
		lw.mu.Lock()
		delete(lw.running, rigName)
		lw.mu.Unlock()
	}()
	w, err := d.newRigLandingWorker(rigName)
	if err != nil {
		d.logger.Printf("landing_worker: %s: not starting: %v", rigName, err)
		return
	}
	d.logger.Printf("landing_worker: %s: worker started", rigName)
	d.landingWorkerLoop(rigName, interval, w.Pass)
}

// landingWorkerLoop passes, then waits the interval, until the daemon stops
// or the patrol is disabled. The first pass runs as the loop starts, so a
// daemon restarted for an upgrade resumes landing without waiting out an
// interval (gt-fzwcd).
func (d *Daemon) landingWorkerLoop(rigName string, interval time.Duration, pass func(context.Context) landworker.Report) {
	skipLogged := ""
	// landedSinceQuiet makes an idle pass readable as the drain: main moved
	// under the installed binary and the queue has now emptied, the quiet
	// point the install follows. A pass that did not land leaves it standing —
	// a rejection sends its bead to rework rather than back in the queue — and
	// the idle pass clears it, so one landing draws one install (gt-3qmv4.4).
	landedSinceQuiet := false
	for {
		if !d.isPatrolActive("landing_worker") {
			d.logger.Printf("landing_worker: %s: patrol disabled; worker stopping", rigName)
			return
		}
		if ok, why := d.isRigOperational(rigName); !ok {
			if skipLogged != why {
				d.logger.Printf("landing_worker: %s: not landing: %s", rigName, why)
				skipLogged = why
			}
		} else if d.upgradeRestartPending.Load() {
			// Drain: no new pass while an upgrade restart is pending.
			skipLogged = ""
		} else {
			skipLogged = ""
			pruneLandingLogs(d.landingLogRoot(rigName), time.Now())
			d.landingPasses.Add(1)
			d.landingStates.beginPass(rigName)
			rep := pass(d.ctx)
			d.landingStates.endPass(rigName)
			d.landingPasses.Add(-1)
			if rep != (landworker.Report{}) {
				d.logger.Printf("landing_worker: %s: pass: %s", rigName, rep)
			}
			// A gt:install-now bead asks for the install in the pass that
			// landed it, while beads are still queued behind it: the drain
			// below is the ordinary quiet point, and this label is the
			// operator saying not to wait for one (gt-3qmv4.2).
			if rep.InstallRequested {
				d.noteLandingInstallNow(rigName)
			}
			if rep == (landworker.Report{}) && landedSinceQuiet {
				d.noteLandingDrained(rigName)
				landedSinceQuiet = false
			}
			if rep.Landed > 0 {
				landedSinceQuiet = true
			}
			// This pass was the last thing a pending restart waited for, so
			// its end is the idle moment: wake the run loop now rather than
			// up to a heartbeat (3 min) later (gt-fzwcd).
			if d.upgradeRestartPending.Load() {
				d.signalLandingDrained()
			}
			// The pass's report has been applied, so the pass is over: tell a
			// waiting test now, before the next pass starts (gt-v0t6k).
			d.noteLandingPassDone()
			// A pass that did work may have left beads behind it: anything
			// submitted while it ran. Pass again at once rather than idle them
			// for a whole interval; only an idle pass waits. Skipped or
			// failed-only passes wait too, so a broken queue cannot spin.
			if rep.Landed+rep.Rejected+rep.Repaired > 0 && !d.upgradeRestartPending.Load() {
				if d.ctx.Err() != nil {
					return
				}
				continue
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// landingDrained is the run loop's wake channel for drained landing passes,
// created once so every signaller shares the loop's channel.
func (d *Daemon) landingDrained() chan struct{} {
	d.landingDrainedOnce.Do(func() { d.landingDrainedCh = make(chan struct{}, 1) })
	return d.landingDrainedCh
}

// signalLandingDrained tells the run loop that a landing pass ended with an
// upgrade restart pending. It never blocks: one queued wake is all the run
// loop needs, and it may be busy in a heartbeat.
func (d *Daemon) signalLandingDrained() {
	select {
	case d.landingDrained() <- struct{}{}:
	default:
	}
}

// noteLandingPassDone tells a test its wait for a landing pass is over (see
// landingPassFn). It runs on the landing loop's goroutine, so the hook must not
// block: a test that stopped waiting cannot stall landing (gt-v0t6k).
func (d *Daemon) noteLandingPassDone() {
	if d.landingPassFn != nil {
		d.landingPassFn()
	}
}

// noteLandingInstallNow asks rebuild_gt to install at the next heartbeat when
// the pass that landed a gt:install-now bead belongs to the rig whose checkout
// the binary is built from (gt-3qmv4.2).
func (d *Daemon) noteLandingInstallNow(rigName string) {
	if !d.landingOwnsGTSource(rigName) {
		return
	}
	d.requestRebuildGTInstallNow()
}

// noteLandingDrained asks rebuild_gt to install at the next heartbeat when the
// drained rig is the one whose checkout the binary is built from (gt-3qmv4.1).
func (d *Daemon) noteLandingDrained(rigName string) {
	if !d.landingOwnsGTSource(rigName) {
		return
	}
	d.requestRebuildGTInstall()
}

// landingOwnsGTSource reports whether rigName's checkout is the town's gt
// source. A landing on any other rig moves that rig's main, not the main the
// installed binary comes from.
func (d *Daemon) landingOwnsGTSource(rigName string) bool {
	repoRoot, err := version.GetRepoRootForTown(d.config.TownRoot)
	if err != nil {
		return false
	}
	return landingPathWithin(repoRoot, filepath.Join(d.config.TownRoot, rigName))
}

func (d *Daemon) landingLogRoot(rigName string) string {
	return filepath.Join(d.config.TownRoot, ".runtime", "landing-logs", rigName)
}

// refreshAuthorSeat refreshes one landed bead author's seat worktree: its
// remote-tracking default branch, and nothing else. A Forgejo landing merges
// on the remote and deletes the author's branch there, so the seat's
// origin/<default> keeps the pre-landing commit and every reader that does
// not run a live check — gt polecat list, the dashboard — reads the landed
// work as local-only (gt-fn9e6.55). Best effort: the error is the caller's to
// log, once, and it never fails, delays or reorders the landing.
func (d *Daemon) refreshAuthorSeat(rigName, polecat string) error {
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	workDir := resolvePolecatWorktree(polecatsDir, polecat, rigName)
	if workDir == "" {
		return fmt.Errorf("no git worktree at %s", filepath.Join(polecatsDir, polecat))
	}
	return d.gitAt(workDir).RefreshRemoteDefaultBranch("origin")
}

// newRigLandingWorker wires the production collaborators for one rig.
func (d *Daemon) newRigLandingWorker(rigName string) (*landworker.Worker, error) {
	townRoot := d.config.TownRoot
	// A rejected gate keeps its whole job log under the town runtime (gt-wwqx8),
	// and GateLogDir resolves that directory from the town root and rig name. An
	// empty resolution would leave every rejection with no log and nothing
	// saying so, so the rig gets no worker until the town root names one
	// (gt-a8fg9).
	gateLogDir := land.GateLogDir(townRoot, rigName)
	if gateLogDir == "" {
		return nil, fmt.Errorf("rig %q: the gate log directory is empty because the town root %q is unset or the rig name is not one path element, so a rejected gate would keep no job log", rigName, townRoot)
	}
	rigPath := filepath.Join(townRoot, rigName)
	repo := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(repo); err != nil {
		return nil, fmt.Errorf("rig repository %s: %w", repo, err)
	}
	// The rig's configured landing remote (gt-fn9e6.9), read through the
	// daemon's git seam so the unit tier starts no git. A Forgejo block whose
	// remote_url matches no remote fails construction, like an unusable bot
	// token: CI is that rig's only landing path, so a fallback to origin would
	// push candidates to GitHub and wait on a verdict that never comes
	// (gt-fn9e6.18).
	landingRemote, err := rig.ResolveLandingRemoteWith(d.repoFileOpener(), d.gitAt(repo), townRoot, rigName)
	if err != nil {
		return nil, err
	}
	// Fail closed on a config.json that exists but does not decode: the file
	// names no branch, so the rig gets no worker and no landing until it is
	// fixed. Construction is the seam because the manager retries it every
	// interval, which is what lets the rig resume on its own (gt-v4r0x).
	watchBranch, cfgErr := rigDefaultBranch(rigPath)
	if cfgErr != nil {
		configPath := filepath.Join(rigPath, "config.json")
		// The notify layer keys an alert by fingerprint, so the manager's
		// retries upsert onto one escalation rather than minting one apiece.
		// The error the caller logs is what names the file.
		d.escalateAlert("landing-rig-config:"+rigName, "landing_worker",
			fmt.Sprintf("%s does not decode, so the landing worker for rig %s is not running and nothing is landing: %v",
				configPath, rigName, cfgErr))
		return nil, fmt.Errorf("rig config %s: %w", configPath, cfgErr)
	}
	landings, err := land.RigLandingsFile(townRoot, rigName)
	if err != nil {
		return nil, err
	}
	// This file lives as long as the rig's landing worker, so a torn line it
	// skips is worth warning about once. A reader that builds its own file per
	// poll (the attention queue) leaves Logf nil rather than repeat it.
	landings.Logf = func(format string, args ...any) {
		d.logger.Printf("landing_worker: %s: %s", rigName, fmt.Sprintf(format, args...))
	}
	// The polecat time each record carries is the seats' own span log, read
	// once per landing; a read that fails costs the time, not the landing
	// (gt-yxl22).
	landings.Spans = spans.New(spans.DefaultDir(townRoot), nil)
	// The rig's failing landings, the snapshot the dashboard's Landings pane
	// and the town health field read (gt-fn9e6.44).
	backoff, err := land.RigBackoffFile(townRoot, rigName)
	if err != nil {
		return nil, err
	}
	bd := landworker.RetryBeads{Inner: beads.NewWithBeadsDir(rigPath, beads.ResolveBeadsDir(rigPath))}
	out := landingLogWriter{logf: d.logger.Printf}
	cfg := landingWorkerConfig(d.patrolConfig)
	if cfg == nil {
		cfg = &LandingWorkerConfig{}
	}
	workRoot, err := landingWorkRoot(cfg.WorkRoot, townRoot, rigName)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(filepath.Dir(workRoot)); err != nil {
		return nil, fmt.Errorf("landing work root: %w", err)
	}
	if err := ensurePrivateDir(workRoot); err != nil {
		return nil, fmt.Errorf("landing work root: %w", err)
	}
	var reviewer land.Reviewer = land.SkipReviewer{}
	// The rig's om directory holds the transient verdict files and the rounds
	// ledgers that outlive them, both named after the work bead (gt-yphcl).
	omDir := filepath.Join(townRoot, ".runtime", "landing-om", rigName)
	if landingReviewEnabled(d.patrolConfig) {
		home, _ := os.UserHomeDir()
		omPath := resolveOMPath(cfg.OMPath, exec.LookPath, home)
		if _, err := os.Stat(omPath); err != nil && filepath.IsAbs(omPath) {
			d.logger.Printf("landing_worker: %s: WARNING om not found at %s (%v); landings will record om_verdict error:<reason> until it is", rigName, omPath, err)
		}
		d.logger.Printf("landing_worker: %s: om review on (%s)", rigName, omPath)
		reviewer = land.OMReviewer{Path: omPath, OutDir: omDir,
			Timeout:  landingWorkerDuration(cfg.OMTimeoutStr, land.DefaultOMTimeout),
			Verdicts: land.TownVerdicts(townRoot, rigName)}
	} else {
		d.logger.Printf("landing_worker: %s: om review OFF (patrols.landing_worker.review=false); landings record om_verdict skipped", rigName)
	}
	lander := &land.Lander{
		Repo:               repo,
		Remote:             landingRemote,
		WorkRoot:           workRoot,
		Route:              "daemon",
		Reviewer:           reviewer,
		Beads:              bd,
		Landings:           landings,
		Reviews:            land.TownReviewsFile(townRoot),
		RoundsDir:          roundsDirFor(reviewer, omDir),
		Out:                out,
		RangeChecks:        []land.RangeCheck{land.AttributionCheck},
		ReviewErrorRejects: true,
		Slow:               d.landingSlowAlarm(rigName, cfg),
		// The Stage callback lets the landing-stuck item judge the pass by the
		// stage it is running rather than by the whole pipeline, so a long but
		// healthy gate is not an item (gt-84gcp).
		Stage: func(_ string, stage string) {
			d.landingStates.setStage(rigName, stage, d.clk().Now())
		},
		// A revert of a red main lands without om rather than wait on it.
		ReviewErrorLandsLabels: []string{landworker.LabelRevert},
		// A bird's rejection keeps the bead with the bird and reaches them as
		// an attention item, where the town puts what its owners must read
		// (gt-h4cvi).
		BirdOwned:      birdOwnedAssignee,
		RejectionAlert: d.rejectionAlert(),
	}
	// A rig lands through its Forgejo CI: the candidate gate runs the merged
	// tree and the PR merger writes the target. merge_queue.forgejo is
	// mandatory — the local gate and the force-push were removed once every
	// rig was cut over (gt-fn9e6.32), so a rig without the block no longer has
	// a landing path and fails closed here rather than falling back silently.
	// The same block decides the landing's deadline below, so it is read once,
	// here (gt-fn9e6.26).
	forgejoCfg := rig.ResolveForgejoConfigIn(d.repoFileOpener(), townRoot, rigName)
	if forgejoCfg == nil {
		// Keyed per rig so the manager's retries upsert onto one escalation.
		d.escalateAlert("landing-rig-config:"+rigName, "landing_worker", fmt.Sprintf(
			"rig %s has no merge_queue.forgejo block, so it has no landing path and nothing is landing for it: add the block to %s",
			rigName, filepath.Join(rigPath, "settings", "config.json")))
		return nil, fmt.Errorf("rig %s has no merge_queue.forgejo block: Forgejo is the only landing path, so the landing worker for this rig is not running until the block is added", rigName)
	}
	candidate, merger, err := d.newForgejoLanding(rigName, landingRemote, forgejoCfg, repo, landings, gateLogDir, cfg)
	if err != nil {
		return nil, err
	}
	lander.Candidate = d.jevFailcauseCandidate(rigName, d.jevTestselCandidate(rigName, candidate))
	lander.Merger = merger
	// The follow-up feed files beads nobody is waiting on, so it runs only for
	// a rig whose own settings turn it on (gt-s01pn).
	if rigFollowupsEnabled(rigPath) {
		lander.Followups = followupBeads{client: bd}
		lander.FollowupBackpressure = rigFollowupBackpressure(rigPath)
		// The backpressure holds findings back silently otherwise, so the day
		// it does, the loss reaches a human as a low attention item (gt-4ty22).
		lander.FollowupAlert = d.followupBackpressureAlert(rigName)
		d.logger.Printf("landing_worker: %s: om follow-up feed on (landing.followups; %d open %s beads stop it)",
			rigName, land.FollowupBackpressureOrDefault(lander.FollowupBackpressure), land.FollowUpLabel)
	}
	// Every use of an accepted risk reaches the overseer, whether or not the
	// follow-up feed is on (gt-tvhg8).
	lander.AcceptedRiskAlert = d.acceptedRiskAlert()
	// A test that keeps failing for beads that did not touch it, and on main too,
	// is the daemon's to escalate and file a repair for (gt-xvw20); the watch does
	// that off the landing's goroutine, where a retrying alert cannot hold up a
	// rejection.
	ciWatch := &ciTestWatch{
		Rig:    rigName,
		Ledger: ciFailureLedgerPath(townRoot),
		Beads:  bd,
		Escalate: func(key, message string) {
			d.escalateAlert(key, "landing_worker", message)
		},
		Logf:       d.logger.Printf,
		Now:        func() time.Time { return d.clk().Now() },
		MainStatus: mainStatusFromStatus(townRoot),
		Async: func(call func()) {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						d.logger.Printf("landing_worker: %s: ci-failure watch panicked: %v", rigName, r)
					}
				}()
				call()
			}()
		},
	}
	lander.CIFailure = ciWatch.Record
	// A cut-over rig can be stuck in the CI wait, so it gets the watch. An
	// alert keyed per rig must have exactly one owner (gt-fn9e6.27).
	stuck := d.newLandingStuckWatch(rigName)
	// stuckEnd ends the in-flight bead's watch; nil between beads. Only the
	// Active callback below touches it.
	var stuckEnd func()
	run := postLandRun(repo, workRoot, d.landingLogRoot(rigName), townRoot, rigName, landingRemote, landingWorkerDuration(cfg.PostLandTimeoutStr, defaultPostLandTimeout))
	// Every rig that lands here does so through Forgejo, so the merge happens
	// on the remote and the author seat's remote-tracking default branch is
	// stale until something fetches it (gt-fn9e6.55).
	refreshAuthorSeat := func(polecat string) error { return d.refreshAuthorSeat(rigName, polecat) }
	mainState := fileMainState{path: RedMainStatePath(townRoot, rigName)}
	redMain := &landworker.RedMain{
		Rig:   rigName,
		Beads: bd,
		Rerun: func(ctx context.Context, cmd, pkg string, pl landworker.PostLand) landworker.PostLandResult {
			rerun, err := postLandRerunCommand(cmd, pkg)
			if err != nil {
				return landworker.PostLandResult{ExitCode: -1, Err: err}
			}
			return run(ctx, rerun, pl)
		},
		Status: func(line string) {
			if err := writeRedMainStatus(townRoot, rigName, line, time.Now()); err != nil {
				d.logger.Printf("landing_worker: %s: red-main: writing status: %v", rigName, err)
			}
		},
		Logf:     d.logger.Printf,
		State:    mainState,
		Landings: landings,
		Revert:   postLandRevert(repo, workRoot, landingRemote),
		// The landing's own commit range: the record's base is what it merged
		// onto, so name-only is the landing's change (gt-40so9).
		Diff: func(_ context.Context, rec land.LandingRecord) ([]string, error) {
			return git.NewGit(repo).DiffNameOnly(rec.Base, rec.LandedCommit)
		},
	}
	// A rig whose forgejo block names a promote_target advances GitHub main
	// from its green verdicts (gt-fn9e6.37), instead of a push mirror that
	// pushes every commit before the slow check runs. A rig the tier sweep
	// covers takes its candidates from the sweep alone (gt-fn9e6.38): a green
	// post-land verdict is the tiers `make gate` already ran, and promoting on
	// it would publish a commit before the sweep that covers the rest has run.
	if d.tierSweepCoversRig(rigName) {
		d.logger.Printf("landing_worker: %s: the tier sweep owns GitHub promotion for this rig, so a green post-land verdict does not promote", rigName)
	} else {
		redMain.Promote = d.newPromoter(rigName, repo, forgejoCfg)
	}
	postLand := &landworker.PostLandRunner{
		Rig:     rigName,
		Command: func() string { return rigPostLandCommand(rigPath) },
		Run:     run,
		Beads:   bd,
		Logf:    d.logger.Printf,
		OnRed:   redMain.Red,
		OnGreen: redMain.Green,
		Busy: func(busy bool) {
			if busy {
				d.postLandRuns.Add(1)
			} else {
				d.postLandRuns.Add(-1)
			}
		},
	}
	// The human's recorded identity is what the worker refuses a submission
	// under, so a town whose overseer identity cannot be read gets no worker:
	// without the name no submission can be checked, and landing one would
	// pass the check by default (gt-2ge3z). Construction is the seam because
	// the manager retries it every interval, which is what lets a rig resume
	// on its own once the identity is readable.
	human, err := config.LoadOverseerConfig(config.OverseerConfigPath(townRoot))
	if err != nil {
		d.escalateAlert("landing-human-identity:"+rigName, "landing_worker",
			fmt.Sprintf("the town's overseer identity cannot be read, so the landing worker for rig %s is not running and nothing is landing: %v", rigName, err))
		return nil, fmt.Errorf("reading the town's overseer identity: %w", err)
	}
	return &landworker.Worker{
		Rig:        rigName,
		HumanActor: human.Name,
		Beads:      bd,
		Remote:     gitRemote{g: git.NewGit(repo), remote: landingRemote},
		Lander:     lander,
		Landings:   landings,
		Backoff:    backoff,
		Queue:      d.landingQueueView(),
		PostLand:   postLand,
		// The landing merges on the remote, which moves main there and leaves
		// the author seat's remote-tracking ref for it stale (gt-fn9e6.55).
		RefreshAuthorSeat: refreshAuthorSeat,
		Reverts:           redMain,
		WatchTarget:       watchBranch,
		MainState:         mainState,
		LandTimeout:       landingRigLandTimeout(d.patrolConfig),
		Logf:              d.logger.Printf,
		Escalate: func(beadID, message string) {
			d.escalateAlert(landingPersonKey(bd, beadID), "landing_worker", message)
		},
		// A landing failing at any stage is its own alert, keyed per rig and
		// bead so the retries upsert onto one escalation instead of minting
		// one apiece, and cleared when the bead lands or is rejected
		// (gt-fn9e6.44).
		FailingEscalate: func(beadID, message string) {
			d.escalateAlert(failingLandingKey(rigName, beadID), "landing_worker", message)
		},
		FailingClear: func(beadID string) {
			d.clearAlerts(fmt.Sprintf("the landing of %s on %s is off the failing list", beadID, rigName),
				failingLandingKey(rigName, beadID))
		},
		Draining: d.upgradeRestartPending.Load,
		Active: func(id string) {
			// The Active callback is the landing-stuck item's clock: a bead
			// entering flight is when its in-flight time starts, and leaving
			// it is activity (gt-vsct7.3). The same window arms the
			// stuck-landing alert, so the two never disagree about what is in
			// flight (gt-fn9e6.27).
			d.landingStates.setBead(rigName, id, d.clk().Now())
			// The landworker calls Active from its own goroutine, one bead at
			// a time, so the pending end belongs to a plain local.
			// A bead leaving flight ends the watch, and one entering it
			// replaces whatever was armed, so a bead whose end never came
			// cannot alert on a landing that has already moved on.
			if stuckEnd != nil {
				stuckEnd()
				stuckEnd = nil
			}
			if id != "" {
				stuckEnd = stuck.begin(id)
			}
		},
		ClearIntent: func(w land.Work) error {
			seat := supervisor.IntentSeat(supervisor.SeatFor(rigName, constants.RolePolecat, w.Worker))
			_, err := intent.ClearLanded(townRoot, seat, w.BeadID, "landing worker", time.Now())
			return err
		},
		// A sandbox seat is single-use: once its work has landed there is
		// nothing on the host left to read, so the seat's host directories go
		// (gt-sefzt). A kept seat is logged with its reason by the step.
		AfterLanded: func(w land.Work) {
			d.startSandboxSeatEnd(rigName, landings, w)
		},
		// The agent a landing ran on is the one its seat's agent bead names:
		// the sling writes it there and the session's end leaves it, so the
		// landing worker reads the seat's own record rather than a role
		// default (gt-mukgk.6).
		AgentBeadID: func(polecat string) string {
			return beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, rigName), rigName, polecat)
		},
		// The first-sight record the submission battery asks from, read from the
		// clone this worker lands through (gt-l7ciu).
		FirstSight: &land.SubmissionRecorder{
			Path: land.SubmissionsSeenPath(townRoot), Git: git.NewGit(repo), Remote: landingRemote,
			Warn: func(format string, args ...any) {
				d.logger.Printf("landing_worker: %s: %s", rigName, fmt.Sprintf(format, args...))
			},
		},
	}, nil
}

// failingLandingKey is the fingerprint of the alert for a bead whose landing
// keeps failing on rig: per rig and bead, so a bead re-dispatched inside the
// same rig lands on its own escalation, and the retries of one failure run
// upsert onto one bead (gt-fn9e6.44).
func failingLandingKey(rig, beadID string) string {
	return "landing-failing:" + rig + ":" + beadID
}

// forgejoVerifyWindow is how many of the landings file's last records the
// startup context check reads to find a commit the gate workflow tested.
const forgejoVerifyWindow = 20

// newForgejoLanding builds the Forgejo gate and PR merger a cut-over rig lands
// through and checks the gate's required context against the rig's own
// history. A rig whose merge_queue.forgejo block is unusable (no readable
// remote, no landing bot token) fails construction: Forgejo CI is that rig's
// only landing path (gt-fn9e6.32).
//
// One client serves both halves — the same landing bot pushes the candidate,
// posts om's verdict and merges the PR (design, "Bots and tokens").
//
// remote is the rig's configured landing remote (gt-fn9e6.9): the candidate
// has to reach the Forgejo instance whose CI gates it, so an assumed origin
// would push it to GitHub and the gate would wait on a verdict that never
// comes (design, "`gt done` pushes to Forgejo"). gateLogDir is where the rig's
// rejected gate job logs are kept, and the caller resolves it from the town
// root it already holds: a gate that read the town root itself would keep no
// log wherever that field is unset.
func (d *Daemon) newForgejoLanding(rigName, remote string, fj *config.ForgejoConfig, repo string, landings *land.LandingsFile, gateLogDir string, cfg *LandingWorkerConfig) (*land.CandidateGate, *land.ForgejoMerger, error) {
	owner, repoName, err := land.RepoFromRemoteURL(fj.RemoteURL)
	if err != nil {
		return nil, nil, err
	}
	apiBase, err := land.APIBaseFromRemoteURL(fj.RemoteURL)
	if err != nil {
		return nil, nil, err
	}
	// The landing bot's login is the only account the merge trusts to have
	// posted om / review, so a rig that does not name it has a creator check
	// that can never pass: fail construction rather than escalate every
	// landing (gt-fn9e6.7).
	botLogin := fj.BotLogin(config.ForgejoRoleLanding)
	if botLogin == "" {
		return nil, nil, fmt.Errorf("rig %s lands through Forgejo CI but merge_queue.forgejo.bots names no %s login; the merge's creator check cannot trust a review status without it", rigName, config.ForgejoRoleLanding)
	}
	tokenDir, err := cfg.ForgejoTokenDir()
	if err != nil {
		return nil, nil, err
	}
	// The token is read from the role's file, never from config: the key names
	// the role, and the file is what holds the secret (design, "Bots and
	// tokens").
	client, err := forgejo.NewClient(config.ForgejoRoleLanding,
		forgejo.WithBaseURL(apiBase),
		forgejo.WithTokenFile(filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleLanding+".env")))
	if err != nil {
		return nil, nil, fmt.Errorf("rig %s lands through Forgejo CI and its %s bot token is unusable: %w", rigName, config.ForgejoRoleLanding, err)
	}
	out := landingLogWriter{logf: d.logger.Printf}
	gate := &land.CandidateGate{
		Client:     client,
		Owner:      owner,
		RepoName:   repoName,
		Workflow:   fj.GateWorkflowName(),
		Remote:     remote,
		Out:        out,
		GateLogDir: gateLogDir,
	}
	d.verifyForgejoGate(rigName, gate, repo, fj.GateWorkflowName(), landings)
	merger := &land.ForgejoMerger{Client: client, Owner: owner, RepoName: repoName, BotLogin: botLogin, Out: out}
	return gate, merger, nil
}

// verifyForgejoGate checks the required context the workflow file derives
// against a commit this rig landed: that commit went up as a candidate, so the
// gate workflow tested it, and a context absent there means the workflow or
// its job was renamed and every landing would wait on a status that never
// arrives (design open question 1). It reports and never fails — the check is
// evidence, and failing construction would take the rig's landings down with
// it.
func (d *Daemon) verifyForgejoGate(rigName string, gate *land.CandidateGate, repo, workflow string, landings *land.LandingsFile) {
	commit := lastLandedCandidate(landings, d.logger.Printf, rigName)
	if commit == "" {
		d.logger.Printf("landing_worker: %s: Forgejo gate on %s; no landed candidate yet to check its required context against", rigName, land.GateWorkflowPath(workflow))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), land.DefaultCandidateCallTimeout)
	defer cancel()
	data, err := git.NewGit(repo).ShowFileAtRev(commit, land.GateWorkflowPath(workflow))
	if err != nil {
		d.forgejoGateAlert(rigName, fmt.Sprintf("the gate workflow %s is not in the tree of the landed candidate %s, so the required status context cannot be derived: %v", land.GateWorkflowPath(workflow), shortForgejoSHA(commit), err))
		return
	}
	wf, err := land.ParseGateWorkflow([]byte(data))
	if err != nil {
		d.forgejoGateAlert(rigName, fmt.Sprintf("the gate workflow %s at %s: %v", land.GateWorkflowPath(workflow), shortForgejoSHA(commit), err))
		return
	}
	if err := gate.VerifyReported(ctx, commit, wf.Context()); err != nil {
		d.forgejoGateAlert(rigName, fmt.Sprintf("rig %s requires the %s status to merge; %v. A renamed workflow or job orphans the branch protection, and a landing would wait on it forever.", rigName, wf.Context(), err))
	}
}

// lastLandedCandidate is the newest commit the worker itself landed through
// the candidate gate, whose pushed candidate the gate workflow tested: what
// the context check reads. A landing the local gate cleared is not evidence
// for the check — its commit never went up as a candidate, so the workflow
// test says nothing about it. A rig that has just cut over has only such
// records, and the check must read that as "no landed candidate yet" rather
// than flag the workflow as missing from a pre-cutover landing (gt-fn9e6.24).
func lastLandedCandidate(landings *land.LandingsFile, logf func(string, ...any), rigName string) string {
	recs, err := landings.Recent(forgejoVerifyWindow)
	if err != nil {
		logf("landing_worker: %s: reading the landings file for the Forgejo gate check: %v", rigName, err)
		return ""
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Route != "" && recs[i].Route != "daemon" {
			continue
		}
		if !landedThroughCandidateGate(recs[i].GateResult) {
			continue
		}
		return recs[i].LandedCommit
	}
	return ""
}

// landedThroughCandidateGate reports whether a landing record's gate result
// shows the candidate gate ran. The Forgejo path records exactly one gate
// step, named ci ("pass (ci exit 0 1m30s)"), while a landing the local gate
// cleared names lint, gate, test or shell. The record keeps only the rendered
// summary (land.GateResult.Summary), so the step list is read back out of it.
func landedThroughCandidateGate(gateResult string) bool {
	open := strings.Index(gateResult, "(")
	if open < 0 {
		return false
	}
	end := strings.Index(gateResult[open:], ")")
	if end < 0 {
		return false
	}
	for _, step := range strings.Split(gateResult[open+1:open+end], ", ") {
		if strings.HasPrefix(step, land.StageCI+" exit ") {
			return true
		}
	}
	return false
}

// forgejoGateAlert raises the startup context check's finding: the operator
// has to fix the workflow or the config before a landing can merge.
func (d *Daemon) forgejoGateAlert(rigName, message string) {
	d.logger.Printf("landing_worker: %s: forgejo gate check: %s", rigName, message)
	d.escalateAlert("landing-forgejo-context:"+rigName, "landing_worker", message)
}

// shortForgejoSHA is a commit's first 8 characters, for a message.
func shortForgejoSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// landingCIBudget is the wall the candidate gate's stage may legitimately
// take: the wait window for a verdict, plus one API call's deadline. The
// landings of a cut-over rig wait on a Forgejo runner, so a stage judged by
// the local gate's budget would read a healthy queue as a wedged pass.
func landingCIBudget() time.Duration {
	return land.DefaultCandidateWaitTimeout + land.DefaultCandidateCallTimeout
}

// landingStuckAfter is how long a landing on a Forgejo rig may stay in flight
// before the daemon alerts: the candidate gate's CI wait, the one legitimate
// stall of that length. A gate here takes 30 seconds to 3 minutes, so a
// landing still in flight past the wait is waiting on a runner the operator
// has to look at (gt-fn9e6.27).
func landingStuckAfter() time.Duration {
	return land.DefaultCandidateWaitTimeout
}

// landingOMBudget is the wall the review stage may legitimately take: the om
// timeout the rig's lander arms the reviewer with (gt-84gcp).
func landingOMBudget(cfg *LandingWorkerConfig) time.Duration {
	if cfg == nil {
		return land.DefaultOMTimeout
	}
	return landingWorkerDuration(cfg.OMTimeoutStr, land.DefaultOMTimeout)
}

// landingSlowAlarm is the rig's slow-landing alarm: evidence beside the
// landing's gate logs, a low-severity escalation per slow stage (gt-lcu5p).
func (d *Daemon) landingSlowAlarm(rigName string, cfg *LandingWorkerConfig) *land.SlowAlarm {
	return &land.SlowAlarm{
		After: landingWorkerDuration(cfg.AlarmAfterStr, land.DefaultSlowAfter),
		Escalate: func(beadID, stage, message string) {
			_ = d.escalateAlertSeverity("low", "landing-slow:"+beadID+":"+stage, "landing_worker", message)
		},
		EvidenceDir: func(ctx context.Context, dir string) string {
			return landingLogDir(ctx, d.landingLogRoot(rigName), dir)
		},
	}
}

// landingStuckWatch is one rig's stuck-landing watchdog: it raises
// landing-stuck:<rig> while a landing has been in flight longer than the CI
// wait, and clears it when the landing leaves flight. The alert names the
// bead, the rig and the candidate branch the gate pushed, so the operator
// knows which run to read (gt-fn9e6.27).
//
// It is a second goroutine because the landing pass is blocked for the whole
// of a Forgejo landing — inside the CI wait, then om, then the merge — and so
// cannot notice its own stall. Its clock is the bead's flight, not the pass:
// a pass idling between beads is not stuck.
type landingStuckWatch struct {
	// rig is the rig whose landings this watch guards, for log lines.
	rig string
	// clk times the watch; nil means the real clock.
	clk clockwork.Clock
	// after is how long the landing may be in flight before the alert; <= 0
	// turns the watch off.
	after time.Duration
	// raise files the alert for the bead in flight.
	raise func(beadID string)
	// clear closes the alert raise filed, because the landing ended.
	clear func()
	// logf records a panic in the alert path; nil drops the record.
	logf func(string, ...any)
}

// newLandingStuckWatch wires one rig's watchdog to the daemon's alert path:
// raise and clear share the landing-stuck:<rig> fingerprint, so a landing
// stuck across a restart records an occurrence on the open escalation rather
// than minting another bead.
func (d *Daemon) newLandingStuckWatch(rigName string) *landingStuckWatch {
	key := "landing-stuck:" + rigName
	return &landingStuckWatch{
		rig:   rigName,
		clk:   d.clk(),
		after: landingStuckAfter(),
		raise: func(beadID string) {
			// The candidate branch is land/<bead> unless the submission named
			// its own (land.Work.CandidateRef); the alert names the default so
			// the operator has something to look for in the Forgejo UI.
			d.escalateAlert(key, "landing_worker", fmt.Sprintf(
				"the landing of %s on rig %s has been in flight longer than the %s CI wait without completing: candidate branch land/%s. Read the Forgejo CI run for that branch and the landing worker's pass log for it.",
				beadID, rigName, landingStuckAfter(), beadID))
		},
		clear: func() {
			d.clearAlerts("the landing on "+rigName+" left flight", key)
		},
		logf: d.logger.Printf,
	}
}

// begin arms the watch for the landing of beadID; the returned func ends it,
// stopping the timer and clearing an alert the watch raised. end waits for a
// raise already in flight, so the clear that closes it can never overtake it.
// A nil watch or a disabled one returns no end func at all.
func (w *landingStuckWatch) begin(beadID string) func() {
	if w == nil || w.after <= 0 {
		return nil
	}
	clk := w.clk
	if clk == nil {
		clk = clockwork.NewRealClock()
	}
	timer := clk.NewTimer(w.after)
	done := make(chan struct{})
	raised := make(chan struct{})
	go func() {
		defer close(raised)
		select {
		case <-timer.Chan():
			// A panic in the alert path would take the whole daemon down, so
			// it is caught here; raised still closes, so end() cannot block on
			// an alert that never finished.
			defer func() {
				if r := recover(); r != nil && w.logf != nil {
					w.logf("landing_worker: %s: stuck-landing alert for %s panicked: %v", w.rig, beadID, r)
				}
			}()
			w.raise(beadID)
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			// Stop reports whether it beat the timer: a false means the watch
			// fired, and <-raised that its alert landed, so clearing here
			// cannot leave the alert open behind a raise still running.
			if !timer.Stop() {
				<-raised
				w.clear()
			}
			close(done)
		})
	}
}

// landingLogDir is one landing's log directory under logRoot: named by its
// land.LandingID. Every landing checks out at the same worktree path now
// (gt-2ycne.2), so the path no longer tells two landings apart; a context
// without an ID (not from Land) falls back to the worktree's parent name.
func landingLogDir(ctx context.Context, logRoot, dir string) string {
	if id := land.LandingID(ctx); id != "" {
		return filepath.Join(logRoot, id)
	}
	return filepath.Join(logRoot, filepath.Base(filepath.Dir(dir)))
}

// rigPostLandCommand is merge_queue.post_land_command from the rig's own
// settings/config.json only: the repo-committed tier is merged content and
// must not choose a command the daemon runs.
func rigPostLandCommand(rigPath string) string {
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil || settings == nil || settings.MergeQueue == nil {
		return ""
	}
	return strings.TrimSpace(settings.MergeQueue.PostLandCommand)
}

// rigFollowupsEnabled is landing.followups from the rig's own
// settings/config.json only, for the reason rigPostLandCommand reads its file:
// the feed files beads, so which rig feeds is the operator's call and never
// merged repo content's (gt-s01pn).
func rigFollowupsEnabled(rigPath string) bool {
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil || settings == nil || settings.Landing == nil {
		return false
	}
	return settings.Landing.Followups
}

// rigFollowupBackpressure is merge_queue.followup_backpressure from the rig's
// own settings/config.json, and 0 when it names none — the count the feed
// reads as its own default. It is read from there for the reason
// rigFollowupsEnabled reads its file: it decides when the feed stops filing
// beads nobody is waiting on (gt-4ty22).
func rigFollowupBackpressure(rigPath string) int {
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil || settings == nil || settings.MergeQueue == nil {
		return 0
	}
	return settings.MergeQueue.FollowupBackpressure
}

// followupBackpressureAlertEvery is how often the feed's backpressure item may
// reach a human for one rig: the bound holds findings back on every landing
// until the open count falls, and a landing rate is not a news rate (gt-4ty22).
const followupBackpressureAlertEvery = 24 * time.Hour

// followupBackpressureAlert is the feed's low attention item for one rig: the
// findings merge_queue.followup_backpressure held back, escalated at most once
// a day. The record lives here rather than on the lander, which holds no state
// between landings, and the mutex covers the feed's goroutine outliving the
// timeout that abandoned it (gt-4ty22).
func (d *Daemon) followupBackpressureAlert(rigName string) func(message string) {
	var mu sync.Mutex
	var last time.Time
	key := "followup-backpressure:" + rigName
	return func(message string) {
		now := d.clk().Now()
		mu.Lock()
		raised := !last.IsZero() && now.Sub(last) < followupBackpressureAlertEvery
		if !raised {
			last = now
		}
		mu.Unlock()
		if raised {
			return
		}
		// The item is the only record of what the bound held back, so a
		// delivery that failed keeps the day's slot and is logged rather than
		// dropped (gt-4ty22).
		if err := d.escalateAlertSeverity("low", key, "landing_worker", message); err != nil {
			d.logger.Printf("landing_worker: %s: the follow-up backpressure item was not raised: %v", rigName, err)
		}
	}
}

// acceptedRiskAlert is the attention item the overseer sees for a review that
// was given an accepted risk (gt-tvhg8). It is a MEDIUM escalation: the risk
// was accepted on purpose, so the overseer should look, not be paged. The key
// names the bead and the head reviewed, so the escalation keeps one item per
// head. A failed delivery goes back to the lander, which logs it and lands.
func (d *Daemon) acceptedRiskAlert() func(key, message string) error {
	return func(key, message string) error {
		return d.escalateAlertSeverity("MEDIUM", key, "landing_worker", message)
	}
}

// birdOwnedAssignee reports whether an assignee is a bird — a crew or
// operator identity such as gastown/crew/sloan, town/crew/name, or a bare
// town handle — rather than a polecat seat. Any assignee carrying a "polecats"
// path segment counts as a seat, whatever the surrounding shape: a shape this
// function does not expect must release the bead to dispatch, not leave it
// with an owner nobody will act for. An unassigned bead has no owner to keep
// it for, so it counts as a polecat's (gt-h4cvi).
func birdOwnedAssignee(assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	for _, seg := range strings.Split(assignee, "/") {
		if seg == "polecats" {
			return false
		}
	}
	return true
}

// rejectionAlert is the attention item a bird reads when the landing of its
// bead is rejected: MEDIUM, like an accepted risk, because the owner must act
// but nobody is paged (gt-h4cvi).
func (d *Daemon) rejectionAlert() func(key, message string) error {
	return func(key, message string) error {
		return d.escalateAlertSeverity("MEDIUM", key, "landing_worker", message)
	}
}

// followupBeads is the daemon's land.FollowupBeads over the beads client the
// landing worker already holds: the feed's read is one bounded list across
// open and closed beads, and its write is one create. The client is the
// retrying wrapper, so a Dolt serialization failure retries like every other
// bead write the worker makes (gt-s01pn).
type followupBeads struct {
	client followupClient
}

// followupClient is the beads surface the follow-up feed is made of.
type followupClient interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Create(opts beads.CreateOptions) (*beads.Issue, error)
}

var _ land.FollowupBeads = followupBeads{}

func (b followupBeads) CreateFollowup(spec land.FollowupSpec) (string, error) {
	issue, err := b.client.Create(beads.CreateOptions{
		Title:       spec.Title,
		Description: spec.Body,
		Labels:      spec.Labels,
		Priority:    spec.Priority,
	})
	if err != nil {
		return "", err
	}
	return issue.ID, nil
}

func (b followupBeads) ListFollowups(label string) ([]land.ExistingFollowup, error) {
	issues, err := b.client.List(beads.ListOptions{Status: "all", Label: label, Limit: land.FollowUpListLimit})
	if err != nil {
		return nil, err
	}
	out := make([]land.ExistingFollowup, 0, len(issues))
	for _, issue := range issues {
		out = append(out, land.ExistingFollowup{
			ID: issue.ID, Title: issue.Title, Description: issue.Description,
			Status: issue.Status, Labels: issue.Labels,
		})
	}
	return out, nil
}

// postLandRun runs the post-landing command in a throwaway worktree of repo
// at the landed commit, under the container slot unless the command declares
// GT_TEST_DOCKER=0, bounded by timeout.
func postLandRun(repo, workRoot, logRoot, townRoot, rigName, remote string, timeout time.Duration) func(context.Context, string, landworker.PostLand) landworker.PostLandResult {
	return func(ctx context.Context, cmd string, pl landworker.PostLand) landworker.PostLandResult {
		if err := os.MkdirAll(workRoot, 0o700); err != nil {
			return landworker.PostLandResult{ExitCode: -1, Err: err}
		}
		parent, err := os.MkdirTemp(workRoot, "post-*")
		if err != nil {
			return landworker.PostLandResult{ExitCode: -1, Err: err}
		}
		defer func() { _ = os.RemoveAll(parent) }()
		g := git.NewGit(repo)
		if err := postLandFetch(g, remote, pl); err != nil {
			return landworker.PostLandResult{ExitCode: -1, Err: err}
		}
		dir := filepath.Join(parent, "wt")
		if err := g.WorktreeAddDetached(dir, pl.Commit); err != nil {
			return landworker.PostLandResult{ExitCode: -1, Err: fmt.Errorf("worktree at %s: %w", pl.Commit, err)}
		}
		defer func() {
			_ = g.WorktreeRemove(dir, true)
			_ = g.WorktreePrune()
		}()
		// Named "test" so the slot, when taken, is held around it. A command
		// that leads every segment with GT_TEST_DOCKER=0 starts no containers
		// and takes no slot, so an install is not blocked by the shell tier
		// (gt-7k0hz). Any other command takes the slot under a gate-class role
		// (slot.IsGateRole): that run is the red-main detector, so it takes
		// reserved slots and crew yield to it.
		cg := land.WithSlotForCommand(land.CommandGate{Steps: []land.Step{{Name: "test", Command: cmd}}}, townRoot, rigName+"/post-land", cmd)
		cg.LogDir = filepath.Join(logRoot, filepath.Base(parent))
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res := cg.Run(rctx, dir)
		// The full output outlives the worktree, pruned with the landing
		// logs; red-main beads cite it (gt-f2voh).
		logPath := filepath.Join(cg.LogDir, "test.log")
		if res.Err != nil || len(res.Steps) == 0 {
			if res.Err == nil {
				res.Err = fmt.Errorf("post-land command produced no result")
			}
			return landworker.PostLandResult{ExitCode: -1, Err: res.Err, LogPath: logPath}
		}
		step := res.Steps[len(res.Steps)-1]
		return landworker.PostLandResult{ExitCode: step.ExitCode, Tail: step.Tail, Packages: step.Packages, ShellFailures: step.ShellFailures, LogPath: logPath}
	}
}

// postLandFetch makes pl.Commit present in the rig repository before a
// worktree is added at it. A landing's commit is already there (Land built
// it in that repository), but a direct push is only seen on origin, so the
// target is fetched when the commit is missing (gt-p2rs0).
func postLandFetch(g landingRemoteGit, remote string, pl landworker.PostLand) error {
	if have, err := g.RefExists(pl.Commit + "^{commit}"); err != nil {
		return fmt.Errorf("reading %s in the rig repository: %w", pl.Commit, err)
	} else if have {
		return nil
	}
	if pl.Target == "" {
		return fmt.Errorf("%s is not in the rig repository and no target names where to fetch it from", pl.Commit)
	}
	if err := g.FetchRefspecWithTimeout(remote, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", pl.Target, remote, pl.Target), landingRemoteFetchTimeout); err != nil {
		return fmt.Errorf("fetching %s/%s for %s: %w", remote, pl.Target, pl.Commit, err)
	}
	if have, err := g.RefExists(pl.Commit + "^{commit}"); err != nil {
		return fmt.Errorf("reading %s in the rig repository: %w", pl.Commit, err)
	} else if !have {
		return fmt.Errorf("%s is not on %s/%s after fetching it", pl.Commit, remote, pl.Target)
	}
	return nil
}

// postLandPackageRE is what a package path from go test output must look like
// before it goes into a shell command.
var postLandPackageRE = regexp.MustCompile(`^[A-Za-z0-9._/~-]+$`)

// postLandRerunCommand reruns one package of the tier cmd ran: the
// integration tier (cmd names test-integration) with its build tag and
// containers on, any other tier with containers off.
func postLandRerunCommand(cmd, pkg string) (string, error) {
	return rerunCommand(strings.Contains(cmd, "test-integration"), pkg)
}

// rerunCommand is `go test` of pkgs, once and uncached: the integration tier
// with its build tag and containers on, or the unit tier with containers off.
func rerunCommand(integration bool, pkgs ...string) (string, error) {
	if len(pkgs) == 0 {
		return "", fmt.Errorf("no package to rerun")
	}
	for _, pkg := range pkgs {
		if !postLandPackageRE.MatchString(pkg) || strings.HasPrefix(pkg, "-") {
			return "", fmt.Errorf("refusing to rerun package %q: not a Go package path", pkg)
		}
	}
	if integration {
		return "GT_TEST_DOCKER=1 go test -count=1 -tags integration -timeout 20m " + strings.Join(pkgs, " "), nil
	}
	return "GT_TEST_DOCKER=0 go test -count=1 -timeout 20m " + strings.Join(pkgs, " "), nil
}

// RedMainStatusPath is the file holding a rig's red-main status line.
func RedMainStatusPath(townRoot, rigName string) string {
	return filepath.Join(townRoot, ".runtime", "red-main", rigName+".status")
}

// writeRedMainStatus replaces the rig's status line, stamped with now.
func writeRedMainStatus(townRoot, rigName, line string, now time.Time) error {
	path := RedMainStatusPath(townRoot, rigName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(now.UTC().Format(time.RFC3339)+" "+line+"\n"), 0o644); err != nil { //nolint:gosec // G306: a status line, read by humans
		return err
	}
	return os.Rename(tmp, path)
}

// newPromoter is a rig's one GitHub promotion owner (gt-fn9e6.37): the git
// surface reads the target's main and pushes to it from the rig's own
// repository, the lock is the rig's, and every failure is recorded in the
// state the caller already keeps. Like NewRigPromoter, it returns nil for a
// rig that does not promote, so the caller has nothing to wire and a rig that
// has not cut over is unchanged.
func (d *Daemon) newPromoter(rigName, repo string, fj *config.ForgejoConfig) *promote.Promoter {
	return NewRigPromoter(d.config.TownRoot, rigName, fj, git.NewGit(repo), func(message string) {
		d.escalateAlert(PromoteDivergedAlertKey(rigName), "landing_worker", message)
	}, d.logger.Printf)
}

// tierSweepPromoter is the sweep's promotion owner for a rig, built from the
// rig's own forgejo block rather than a landing worker's captured one. It is
// the same newPromoter the post-land verdict calls, so whichever path holds a
// rig's green verdict, one push path, one lock and one record promote it
// (gt-fn9e6.38).
func (d *Daemon) tierSweepPromoter(rigName, repo string) *promote.Promoter {
	return d.newPromoter(rigName, repo, rig.ResolveForgejoConfigIn(d.repoFileOpener(), d.config.TownRoot, rigName))
}

// RedMainStatePath is the file holding a rig's last green and last tested
// main commits (landworker.MainState).
func RedMainStatePath(townRoot, rigName string) string {
	return filepath.Join(townRoot, ".runtime", "red-main", rigName+".json")
}

// fileMainState stores a rig's landworker.MainState as JSON; a missing file
// is the zero state.
type fileMainState struct {
	path string
}

var _ landworker.MainStateStore = fileMainState{}

func (f fileMainState) Load() (landworker.MainState, error) {
	var st landworker.MainState
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("reading %s: %w", f.path, err)
	}
	return st, nil
}

func (f fileMainState) Save(st landworker.MainState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A unique temp name: the landing worker's verdict, the tier sweep and
	// gt promote all write this record, and one shared temp path lets two
	// writers clobber each other's bytes before either renames (gt-1ohu4).
	tmp, err := os.CreateTemp(dir, filepath.Base(f.path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp opens 0600; the record is two commit ids, read by humans.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, f.path)
}

// rigDefaultBranch is the branch the landing worker watches for direct
// pushes: the rig's configured default branch, else main. A file that exists
// but does not decode yields an error, never the main fallback, so a landing
// cannot run against a branch the rig's own file did not name (gt-v4r0x).
func rigDefaultBranch(rigPath string) (string, error) {
	cfg, err := rig.LoadRigConfigIfPresent(rigPath)
	if err != nil {
		return "", err
	}
	if cfg != nil && cfg.DefaultBranch != "" {
		return cfg.DefaultBranch, nil
	}
	return "main", nil
}

// postLandRevert builds the revert of a landing in a throwaway worktree of
// repo at the landed commit and pushes it to remote as branch, the rig's
// configured landing remote (gt-fn9e6.9). A landing is a --no-ff merge
// (reverted against its first parent, the target) or a squash (one parent).
func postLandRevert(repo, workRoot, remote string) landworker.RevertBuilder {
	return func(_ context.Context, rec land.LandingRecord, branch string) (string, error) {
		if err := os.MkdirAll(workRoot, 0o700); err != nil {
			return "", err
		}
		parent, err := os.MkdirTemp(workRoot, "revert-*")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.RemoveAll(parent) }()
		g := git.NewGit(repo)
		dir := filepath.Join(parent, "wt")
		if err := g.WorktreeAddDetached(dir, rec.LandedCommit); err != nil {
			return "", fmt.Errorf("worktree at %s: %w", rec.LandedCommit, err)
		}
		defer func() {
			_ = g.WorktreeRemove(dir, true)
			_ = g.WorktreePrune()
		}()
		// The revert is a landing commit made by automation: land.RevertGit sets
		// the landing identity as both author and committer (gt-dc4y3, gt-vuv2n).
		wt := land.RevertGit(dir)
		parents, err := wt.Parents(rec.LandedCommit)
		if err != nil {
			return "", err
		}
		mainline := 0
		if len(parents) > 1 {
			mainline = 1
		}
		if err := wt.RevertNoEdit(rec.LandedCommit, mainline); err != nil {
			return "", fmt.Errorf("reverting %s: %w", rec.LandedCommit, err)
		}
		head, err := wt.Rev("HEAD")
		if err != nil {
			return "", err
		}
		if err := wt.Push(remote, "HEAD:refs/heads/"+branch, false); err != nil {
			return "", fmt.Errorf("pushing %s: %w", branch, err)
		}
		return head, nil
	}
}

// landingRemoteGit is the git surface gitRemote reads: *git.Git over the
// rig's bare repository in production, gitfake in tests.
type landingRemoteGit interface {
	PushRemoteBranchTip(remote, branch string) (string, error)
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	RefExists(ref string) (bool, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	CommitTime(rev string) (time.Time, error)
	CommitLandedOnTarget(remote, target, commit string) bool
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
	// The repository's own branches, for the local half of the reap
	// (gt-kne58).
	CurrentBranch() (string, error)
	WorktreeList() ([]git.Worktree, error)
	Rev(ref string) (string, error)
	DeleteLocalBranchIfAt(branch, expectedHash string) error
}

var _ landingRemoteGit = (*git.Git)(nil)

// gitRemote answers the worker's questions about the rig's landing remote
// (gt-fn9e6.9) in the rig's bare repository.
type gitRemote struct {
	g      landingRemoteGit
	remote string
}

func (r gitRemote) BranchTip(branch string) (string, error) {
	return r.g.PushRemoteBranchTip(r.remote, branch)
}

func (r gitRemote) Contains(target, commit string) (bool, error) {
	if err := r.g.FetchRefspecWithTimeout(r.remote, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", target, r.remote, target), landingRemoteFetchTimeout); err != nil {
		return false, err
	}
	// A commit the repository lacks after fetching the target is not on it;
	// any other failure to read it is an error, never a "no".
	exists, err := r.g.RefExists(commit + "^{commit}")
	if err != nil || !exists {
		return false, err
	}
	return r.g.IsAncestor(commit, r.remote+"/"+target)
}

func (r gitRemote) ListRemoteRefs(prefix string) ([]landworker.RemoteRef, error) {
	refs, err := r.g.ListRemoteRefsWithHashes(r.remote, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]landworker.RemoteRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, landworker.RemoteRef{Name: ref.Name, Hash: ref.Hash})
	}
	return out, nil
}

// BranchLandedOn is the leftover sweep's merge test: it brings the branch
// and the target into the rig repository, then asks whether the branch's own
// commit is on the target by reachability or by patch-id.
func (r gitRemote) BranchLandedOn(target, branch, commit string) (bool, error) {
	for _, b := range []string{branch, target} {
		if err := r.g.FetchRefspecWithTimeout(r.remote, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", b, r.remote, b), landingRemoteFetchTimeout); err != nil {
			return false, fmt.Errorf("fetching %s: %w", b, err)
		}
	}
	// A commit the repository still lacks after the fetch was never pushed
	// under this name, or the branch moved since it was listed: not proven
	// landed, and not an error either, as Contains reads it.
	exists, err := r.g.RefExists(commit + "^{commit}")
	if err != nil || !exists {
		return false, err
	}
	return r.g.CommitLandedOnTarget(r.remote, target, commit), nil
}

func (r gitRemote) CommitTime(commit string) (time.Time, error) {
	return r.g.CommitTime(commit)
}

func (r gitRemote) DeleteRemoteBranchIfAt(branch, expectedHash string) error {
	return r.g.DeleteRemoteBranchIfAt(r.remote, branch, expectedHash)
}

// LocalBranchTip reads the rig repository's own branch of that name.
func (r gitRemote) LocalBranchTip(branch string) (string, error) {
	ref := "refs/heads/" + branch
	exists, err := r.g.RefExists(ref)
	if err != nil || !exists {
		return "", err
	}
	return r.g.Rev(ref)
}

// DeleteLocalBranch deletes the rig repository's own branch of that name,
// which update-ref does only while the branch is still at expectedHash. The
// two branches a reap must never take are guarded here rather than left to
// git: the one HEAD names, which is the default branch this repository lands
// onto, and any branch a worktree has checked out, which is a polecat still
// working in it.
func (r gitRemote) DeleteLocalBranch(branch, expectedHash string) (bool, error) {
	if branch == "" {
		return false, errors.New("no branch name to delete")
	}
	exists, err := r.g.RefExists("refs/heads/" + branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	head, err := r.g.CurrentBranch()
	if err != nil {
		return false, err
	}
	if head == branch {
		return false, fmt.Errorf("%s is the branch this repository has checked out", branch)
	}
	trees, err := r.g.WorktreeList()
	if err != nil {
		return false, err
	}
	for _, wt := range trees {
		if wt.Branch == branch {
			return false, fmt.Errorf("%s is checked out in the worktree at %s", branch, wt.Path)
		}
	}
	if err := r.g.DeleteLocalBranchIfAt(branch, expectedHash); err != nil {
		return false, err
	}
	return true, nil
}

// landingLogWriter forwards Land's progress lines to the daemon log.
type landingLogWriter struct {
	logf func(format string, args ...interface{})
}

var _ io.Writer = landingLogWriter{}

func (w landingLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.logf("landing_worker: %s", line)
		}
	}
	return len(p), nil
}

// pruneLandingLogs removes per-landing gate log directories older than the
// retention window.
func pruneLandingLogs(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || (!strings.HasPrefix(e.Name(), "land-") && !strings.HasPrefix(e.Name(), "post-")) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < landingLogRetention {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// roundsDirFor is the directory the rounds ledger is written to: only when om
// reviews the landing, since the ledger is the history om is handed and a
// landing with review off has no om to hand it (gt-yphcl).
func roundsDirFor(reviewer land.Reviewer, dir string) string {
	if _, on := reviewer.(land.OMReviewer); !on {
		return ""
	}
	return dir
}
