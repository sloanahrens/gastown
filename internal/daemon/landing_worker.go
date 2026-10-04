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

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
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

func landingWorkerLandTimeout(config *DaemonPatrolConfig) time.Duration {
	if c := landingWorkerConfig(config); c != nil && c.LandTimeoutStr != "" {
		if d, err := time.ParseDuration(c.LandTimeoutStr); err == nil && d > 0 {
			return d
		}
	}
	return landworker.DefaultLandTimeout
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
				d.logger.Printf("landing_worker: enabled (pass interval %v, land timeout %v)", interval, landingWorkerLandTimeout(d.patrolConfig))
				announced = true
			}
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

// newRigLandingWorker wires the production collaborators for one rig.
func (d *Daemon) newRigLandingWorker(rigName string) (*landworker.Worker, error) {
	townRoot := d.config.TownRoot
	rigPath := filepath.Join(townRoot, rigName)
	repo := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(repo); err != nil {
		return nil, fmt.Errorf("rig repository %s: %w", repo, err)
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
	if landingReviewEnabled(d.patrolConfig) {
		home, _ := os.UserHomeDir()
		omPath := resolveOMPath(cfg.OMPath, exec.LookPath, home)
		if _, err := os.Stat(omPath); err != nil && filepath.IsAbs(omPath) {
			d.logger.Printf("landing_worker: %s: WARNING om not found at %s (%v); landings will record om_verdict error:<reason> until it is", rigName, omPath, err)
		}
		d.logger.Printf("landing_worker: %s: om review on (%s)", rigName, omPath)
		reviewer = land.OMReviewer{Path: omPath, OutDir: filepath.Join(townRoot, ".runtime", "landing-om", rigName),
			Timeout: landingWorkerDuration(cfg.OMTimeoutStr, land.DefaultOMTimeout)}
	} else {
		d.logger.Printf("landing_worker: %s: om review OFF (patrols.landing_worker.review=false); landings record om_verdict skipped", rigName)
	}
	lander := &land.Lander{
		Repo:     repo,
		WorkRoot: workRoot,
		Route:    "daemon",
		Gate: rigLandGate{
			townRoot: townRoot, rig: rigName, logRoot: d.landingLogRoot(rigName),
			lintTimeout: landingWorkerDuration(cfg.LintTimeoutStr, defaultLandLintTimeout),
			testTimeout: landingWorkerDuration(cfg.TestTimeoutStr, defaultLandTestTimeout),
		},
		Rerun:              landRerun(townRoot, rigName, d.landingLogRoot(rigName)),
		GateBeads:          landworker.GateBeads{Rig: rigName, Beads: bd},
		Reviewer:           reviewer,
		Beads:              bd,
		Landings:           landings,
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
	}
	// A rig with a merge_queue.forgejo block lands through its Forgejo CI: the
	// candidate gate replaces the local gate built above, which stays for
	// shadow mode (slice 8).
	if forgejoCfg := rig.ResolveForgejoConfig(townRoot, rigName); forgejoCfg != nil {
		candidate, err := d.newForgejoCandidate(rigName, forgejoCfg, repo, landings, cfg)
		if err != nil {
			return nil, err
		}
		lander.Candidate = candidate
	}
	run := postLandRun(repo, workRoot, d.landingLogRoot(rigName), townRoot, rigName, landingWorkerDuration(cfg.PostLandTimeoutStr, defaultPostLandTimeout))
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
		Revert:   postLandRevert(repo, workRoot),
		// The landing's own commit range: the record's base is what it merged
		// onto, so name-only is the landing's change (gt-40so9).
		Diff: func(_ context.Context, rec land.LandingRecord) ([]string, error) {
			return git.NewGit(repo).DiffNameOnly(rec.Base, rec.LandedCommit)
		},
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
	return &landworker.Worker{
		Rig:         rigName,
		Beads:       bd,
		Remote:      gitRemote{g: git.NewGit(repo), remote: "origin"},
		Lander:      lander,
		Landings:    landings,
		PostLand:    postLand,
		Reverts:     redMain,
		WatchTarget: watchBranch,
		MainState:   mainState,
		LandTimeout: landingWorkerLandTimeout(d.patrolConfig),
		Logf:        d.logger.Printf,
		Escalate: func(beadID, message string) {
			d.escalateAlert("landing-needs-human:"+beadID, "landing_worker", message)
		},
		Draining: d.upgradeRestartPending.Load,
		Active: func(id string) {
			// The Active callback is the landing-stuck item's clock: a bead
			// entering flight is when its in-flight time starts, and leaving
			// it is activity (gt-vsct7.3).
			d.landingStates.setBead(rigName, id, d.clk().Now())
		},
		ClearIntent: func(w land.Work) error {
			seat := supervisor.IntentSeat(supervisor.SeatFor(rigName, constants.RolePolecat, w.Worker))
			_, err := intent.ClearLanded(townRoot, seat, w.BeadID, "landing worker", time.Now())
			return err
		},
	}, nil
}

// forgejoVerifyWindow is how many of the landings file's last records the
// startup context check reads to find a commit the gate workflow tested.
const forgejoVerifyWindow = 20

// newForgejoCandidate builds the Forgejo gate a cut-over rig lands through and
// checks its required context against the rig's own history. A rig whose
// merge_queue.forgejo block is unusable (no readable remote, no landing bot
// token) fails construction: CI is that rig's only landing path, so running
// the local gate instead would bypass the cutover.
func (d *Daemon) newForgejoCandidate(rigName string, fj *config.ForgejoConfig, repo string, landings *land.LandingsFile, cfg *LandingWorkerConfig) (*land.CandidateGate, error) {
	owner, repoName, err := land.RepoFromRemoteURL(fj.RemoteURL)
	if err != nil {
		return nil, err
	}
	apiBase, err := land.APIBaseFromRemoteURL(fj.RemoteURL)
	if err != nil {
		return nil, err
	}
	tokenDir, err := cfg.ForgejoTokenDir()
	if err != nil {
		return nil, err
	}
	// The token is read from the role's file, never from config: the key names
	// the role, and the file is what holds the secret (design, "Bots and
	// tokens").
	client, err := forgejo.NewClient(config.ForgejoRoleLanding,
		forgejo.WithBaseURL(apiBase),
		forgejo.WithTokenFile(filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleLanding+".env")))
	if err != nil {
		return nil, fmt.Errorf("rig %s lands through Forgejo CI and its %s bot token is unusable: %w", rigName, config.ForgejoRoleLanding, err)
	}
	gate := &land.CandidateGate{
		Client:   client,
		Owner:    owner,
		RepoName: repoName,
		Workflow: fj.GateWorkflowName(),
		Out:      landingLogWriter{logf: d.logger.Printf},
	}
	d.verifyForgejoGate(rigName, gate, repo, fj.GateWorkflowName(), landings)
	return gate, nil
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

// lastLandedCandidate is the newest commit the worker itself landed, whose
// pushed candidate the gate workflow tested: what the context check reads.
func lastLandedCandidate(landings *land.LandingsFile, logf func(string, ...any), rigName string) string {
	recs, err := landings.Recent(forgejoVerifyWindow)
	if err != nil {
		logf("landing_worker: %s: reading the landings file for the Forgejo gate check: %v", rigName, err)
		return ""
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Route == "" || recs[i].Route == "daemon" {
			return recs[i].LandedCommit
		}
	}
	return ""
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

// landingGateBudget is the wall the merged-tree gate's stage may legitimately
// take: its lint, test and shell steps' timeouts summed, the same numbers
// rigLandGate arms them with. Summing the shell step even when a submission
// does not move its inputs is the safe bound — the alarm only asks whether
// the stage has outlived every deadline it could still be running under
// (gt-84gcp).
func landingGateBudget(cfg *LandingWorkerConfig) time.Duration {
	lint, test := defaultLandLintTimeout, defaultLandTestTimeout
	if cfg != nil {
		lint = landingWorkerDuration(cfg.LintTimeoutStr, defaultLandLintTimeout)
		test = landingWorkerDuration(cfg.TestTimeoutStr, defaultLandTestTimeout)
	}
	return lint + test + defaultLandShellTimeout
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

// rigLandGate is the rig's merged-tree gate, resolved per landing so a
// settings change takes effect on the next one: merge_queue.gate (gastown:
// `make gate`), else `make gate`, else `make test` under the container slot.
// The verdict is the exit code; nothing reads the output for it.
type rigLandGate struct {
	townRoot, rig, logRoot string
	// lintTimeout and testTimeout bound the gate's stages (gt-b5ugw).
	lintTimeout, testTimeout time.Duration
}

// Stage defaults for patrols.landing_worker lint_timeout and test_timeout:
// several times the measured walls (lint ~20s, build and unit tier 75-100s on
// 2026-10-01), so load does not trip them and a hang is cut short (gt-b5ugw).
const (
	defaultLandLintTimeout = 2 * time.Minute
	defaultLandTestTimeout = 6 * time.Minute
	// defaultLandShellTimeout bounds the gate's shell tier. It is compiled in
	// rather than resolved from a rig setting: the tier is a fixed extra stage
	// for the submissions that move its inputs, and a landing's wall should
	// not depend on how an operator tuned the other two (gt-vsct7.8).
	defaultLandShellTimeout = 3 * time.Minute
)

func (g rigLandGate) Run(ctx context.Context, dir string) land.GateResult {
	mq := rig.ResolveMergeQueueConfig(g.townRoot, g.rig)
	cg := land.WithTimeouts(land.LandGate(dir, mq), g.lintTimeout, g.testTimeout, defaultLandShellTimeout)
	cg = land.WithSlot(cg, g.townRoot, g.rig+"/landing")
	cg.LogDir = landingLogDir(ctx, g.logRoot, dir)
	return cg.Run(ctx, dir)
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

// landRerun is Land's flake-policy rerun: only the failed packages, once,
// in the merged tree, in the gate's tier. `make gate` is the unit tier
// (containers off, no slot). Any other gate may have run containers, so its
// packages rerun in the full tier under the container slot: a superset of
// what the gate ran, never less.
func landRerun(townRoot, rigName, logRoot string) func(context.Context, string, []string) land.GateResult {
	return func(ctx context.Context, dir string, pkgs []string) land.GateResult {
		mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
		gate := land.LandGate(dir, mq)
		cmd, err := rerunCommand(!gate.UnitTier(), pkgs...)
		if err != nil {
			return land.GateResult{Err: err}
		}
		// Named "test" so WithSlot holds the container slot around it; the
		// unit tier needs none.
		cg := land.CommandGate{Steps: []land.Step{{Name: "test", Command: cmd}}}
		if strings.Contains(cmd, "GT_TEST_DOCKER=1") {
			cg = land.WithSlot(cg, townRoot, rigName+"/landing")
		}
		// Beside the gate's own log for this landing, as test.log.
		cg.LogDir = filepath.Join(landingLogDir(ctx, logRoot, dir), "rerun")
		return cg.Run(ctx, dir)
	}
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

// postLandRun runs the post-landing command in a throwaway worktree of repo
// at the landed commit, under the container slot, bounded by timeout.
func postLandRun(repo, workRoot, logRoot, townRoot, rigName string, timeout time.Duration) func(context.Context, string, landworker.PostLand) landworker.PostLandResult {
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
		if err := postLandFetch(g, "origin", pl); err != nil {
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
		// Named "test" so WithSlot holds the container slot around it. The
		// role is gate-class (slot.IsGateRole): this run is the red-main
		// detector, so it takes reserved slots and crew yield to it.
		cg := land.WithSlot(land.CommandGate{Steps: []land.Step{{Name: "test", Command: cmd}}}, townRoot, rigName+"/post-land")
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
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil { //nolint:gosec // G306: two commit ids, read by humans
		return err
	}
	return os.Rename(tmp, f.path)
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
// repo at the landed commit and pushes it to origin as branch. A landing is
// a --no-ff merge (reverted against its first parent, the target) or a
// squash (one parent).
func postLandRevert(repo, workRoot string) landworker.RevertBuilder {
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
		wt := git.NewGit(dir)
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
		if err := wt.Push("origin", "HEAD:refs/heads/"+branch, false); err != nil {
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
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
}

var _ landingRemoteGit = (*git.Git)(nil)

// gitRemote answers the worker's questions about origin from the rig's
// bare repository.
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

func (r gitRemote) DeleteRemoteBranchIfAt(branch, expectedHash string) error {
	return r.g.DeleteRemoteBranchIfAt(r.remote, branch, expectedHash)
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
