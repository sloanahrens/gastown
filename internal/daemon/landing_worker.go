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
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/supervisor"
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
	skipLogged := ""
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
			d.landingBeads.Store(rigName, "")
			rep := w.Pass(d.ctx)
			d.landingBeads.Delete(rigName)
			d.landingPasses.Add(-1)
			if rep != (landworker.Report{}) {
				d.logger.Printf("landing_worker: %s: pass: %s", rigName, rep)
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
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
	gtPath := d.gtPath
	if gtPath == "" {
		gtPath = "gt"
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
			townRoot: townRoot, rig: rigName, gtPath: gtPath, logRoot: d.landingLogRoot(rigName),
		},
		Rerun:            landRerun(townRoot, rigName, gtPath, d.landingLogRoot(rigName)),
		GateBeads:        landworker.GateBeads{Rig: rigName, Beads: bd},
		Reviewer:         reviewer,
		Beads:            bd,
		Landings:         landings,
		Out:              out,
		RangeChecks:      []land.RangeCheck{land.AttributionCheck},
		ReviewErrorLands: true,
	}
	run := postLandRun(repo, workRoot, d.landingLogRoot(rigName), gtPath, rigName, landingWorkerDuration(cfg.PostLandTimeoutStr, defaultPostLandTimeout))
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
		WatchTarget: rigDefaultBranch(rigPath),
		MainState:   mainState,
		LandTimeout: landingWorkerLandTimeout(d.patrolConfig),
		Logf:        d.logger.Printf,
		Draining:    d.upgradeRestartPending.Load,
		Active:      func(id string) { d.landingBeads.Store(rigName, id) },
		ClearIntent: func(w land.Work) error {
			seat := supervisor.IntentSeat(supervisor.SeatFor(rigName, constants.RolePolecat, w.Worker))
			_, err := intent.ClearLanded(townRoot, seat, w.BeadID, "landing worker", time.Now())
			return err
		},
	}, nil
}

// rigLandGate is the rig's merged-tree gate, resolved per landing so a
// settings change takes effect on the next one: merge_queue.gate (gastown:
// `make gate`), else `make gate`, else `make test` under the container slot.
// The verdict is the exit code; nothing reads the output for it.
type rigLandGate struct {
	townRoot, rig, gtPath, logRoot string
}

func (g rigLandGate) Run(ctx context.Context, dir string) land.GateResult {
	mq := rig.ResolveMergeQueueConfig(g.townRoot, g.rig)
	cg := land.WithSlot(land.LandGate(dir, mq), g.gtPath, g.rig+"/landing")
	// dir is <work root>/land-XXXX/wt: one log directory per landing.
	cg.LogDir = filepath.Join(g.logRoot, filepath.Base(filepath.Dir(dir)))
	return cg.Run(ctx, dir)
}

// landRerun is Land's flake-policy rerun: only the failed packages, once,
// in the merged tree, in the gate's tier. `make gate` is the unit tier
// (containers off, no slot). Any other gate may have run containers, so its
// packages rerun in the full tier under the container slot: a superset of
// what the gate ran, never less.
func landRerun(townRoot, rigName, gtPath, logRoot string) func(context.Context, string, []string) land.GateResult {
	return func(ctx context.Context, dir string, pkgs []string) land.GateResult {
		mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
		gate := land.LandGate(dir, mq)
		cmd, err := rerunCommand(gate.Steps[0].Command != "make gate", pkgs...)
		if err != nil {
			return land.GateResult{Err: err}
		}
		// Named "test" so WithSlot holds the container slot around it; the
		// unit tier needs none.
		cg := land.CommandGate{Steps: []land.Step{{Name: "test", Command: cmd}}}
		if strings.Contains(cmd, "GT_TEST_DOCKER=1") {
			cg = land.WithSlot(cg, gtPath, rigName+"/landing")
		}
		// Beside the gate's own log for this landing, as test.log.
		cg.LogDir = filepath.Join(logRoot, filepath.Base(filepath.Dir(dir)), "rerun")
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
func postLandRun(repo, workRoot, logRoot, gtPath, rigName string, timeout time.Duration) func(context.Context, string, landworker.PostLand) landworker.PostLandResult {
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
		cg := land.WithSlot(land.CommandGate{Steps: []land.Step{{Name: "test", Command: cmd}}}, gtPath, rigName+"/post-land")
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
		return landworker.PostLandResult{ExitCode: step.ExitCode, Tail: step.Tail, Packages: step.Packages, LogPath: logPath}
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
// pushes: the rig's configured default branch, else main.
func rigDefaultBranch(rigPath string) string {
	if cfg, err := rig.LoadRigConfig(rigPath); err == nil && cfg.DefaultBranch != "" {
		return cfg.DefaultBranch
	}
	return "main"
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
