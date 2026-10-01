package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/util"
)

// Patrol scan tick (gt-4k3fj.6, ADR 0005).
//
// Every interval (default 2m) the daemon scans each rig with
// internal/patrolscan: a confirmed-dead polecat holding unfinished, unheld
// work is restarted through the supervisor; the work molecule of a polecat
// that no longer exists is closed; stranded work is reported once per window.
// This file is the host: it implements patrolscan.Env over tmux, the intent
// records, the liveness function, the bd CLI (ADR 0001) and git. Off unless
// patrols.patrol_scan.enabled is true.

const (
	defaultPatrolScanInterval = 2 * time.Minute

	// patrolScanBdTimeout bounds one bd call the tick makes.
	patrolScanBdTimeout = 30 * time.Second

	// patrolScanRestartTimeout bounds one `gt session restart`: it stops a
	// session, creates one and waits for the agent to come up.
	patrolScanRestartTimeout = 3 * time.Minute
)

// patrolScanInterval returns the configured interval, or 2m.
func patrolScanInterval(config *DaemonPatrolConfig) time.Duration {
	if c := patrolScanConfig(config); c != nil && c.IntervalStr != "" {
		if d, err := time.ParseDuration(c.IntervalStr); err == nil && d > 0 {
			return d
		}
	}
	return defaultPatrolScanInterval
}

func patrolScanConfig(config *DaemonPatrolConfig) *PatrolScanConfig {
	if config == nil || config.Patrols == nil {
		return nil
	}
	return config.Patrols.PatrolScan
}

// patrolScanOptions builds the scanner options from config.
func patrolScanOptions(config *DaemonPatrolConfig, now func() time.Time) patrolscan.Options {
	o := patrolscan.Options{
		HoldReason: func(w patrolscan.Work) string {
			return convoy.DispatchHoldFields(w.Status, w.Labels, w.Assignee, w.Design, w.Notes)
		},
		IsRefusal: func(err error) bool { return errors.Is(err, supervisor.ErrRefused) },
		Now:       now,
	}
	if c := patrolScanConfig(config); c != nil {
		o.DeadSamples = c.DeadSamples
		if d, err := time.ParseDuration(c.ReportWindow); err == nil && d > 0 {
			o.ReportWindow = d
		}
	}
	return o
}

// patrolScanActiveForRig reports whether the tick owns polecat recovery in
// rigName: the patrol is on and the rig is in its rigs list (or the list is
// empty).
func (d *Daemon) patrolScanActiveForRig(rigName string) bool {
	if !d.isPatrolActive("patrol_scan") {
		return false
	}
	rigs := GetPatrolRigs(d.patrolConfig, "patrol_scan")
	if len(rigs) == 0 {
		return true
	}
	for _, r := range rigs {
		if r == rigName {
			return true
		}
	}
	return false
}

// triggerPatrolScan starts one tick on its own goroutine unless one is
// already running. It reports whether a tick started.
func (d *Daemon) triggerPatrolScan() bool {
	if !d.patrolScanRunning.CompareAndSwap(false, true) {
		d.logger.Printf("patrol_scan: previous tick still running, skipping")
		return false
	}
	d.patrolScanCycles.Add(1)
	go func() {
		defer d.patrolScanCycles.Done()
		defer d.patrolScanRunning.Store(false)
		d.runPatrolScan()
	}()
	return true
}

// runPatrolScan scans every rig the tick covers, one rig at a time: restarts
// are serialized so a mass death cannot raise a burst of sessions at once.
func (d *Daemon) runPatrolScan() {
	if !d.isPatrolActive("patrol_scan") || d.config == nil {
		return
	}
	env := &patrolScanHost{d: d}
	ledger := patrolscan.NewFileLedger(patrolscan.LedgerPath(d.config.TownRoot), 7*24*time.Hour)
	scanner := patrolscan.New(env, ledger, patrolScanOptions(d.patrolConfig, d.clk().Now))
	rigs := d.getPatrolRigs("patrol_scan")
	for _, rigName := range rigs {
		if d.ctx != nil && d.ctx.Err() != nil {
			return
		}
		report := scanner.Tick(rigName)
		for _, line := range report.Lines() {
			d.logger.Printf("patrol_scan: %s", line)
		}
	}
	d.patrolScanTimerGates(env, rigs)
	d.patrolScanRogueBD()
}

// patrolScanTimerGates resolves elapsed timer gates in the town database and
// in each scanned rig's (the deacon's gate-evaluation step and the witness's
// check-timer-gates step). `bd gate check` resolves a timer gate whose
// timeout has passed; it never escalates one.
func (d *Daemon) patrolScanTimerGates(h *patrolScanHost, rigs []string) {
	for _, rigName := range append([]string{""}, rigs...) {
		where := rigName
		if where == "" {
			where = "town"
		}
		out, err := h.bdMutating(rigName, "gate", "check", "--type=timer")
		if err != nil {
			d.logger.Printf("patrol_scan: %s: timer gate check failed: %v", where, err)
			continue
		}
		// bd prints a JSON null before its no-gates notice ("nullNo open gates of
		// type 'timer' found."); a tick that resolved nothing is not worth a line.
		line := strings.TrimPrefix(lastLine(string(out)), "null")
		if line == "" || strings.HasPrefix(line, "No open gates") {
			continue
		}
		d.logger.Printf("patrol_scan: %s: timer gates: %s", where, line)
	}
}

// rogueBDInterval is how often the rogue bd walk runs: rare event, bounded
// walk, persisted cadence so daemon restarts do not re-run it.
const rogueBDInterval = time.Hour

// patrolScanRogueBD runs the rogue bd check when it is due (the deacon's
// rogue-bd-check step). Findings are neutralized and escalated to the
// operator; an inconclusive walk is escalated too, never logged as clean.
func (d *Daemon) patrolScanRogueBD() {
	now := d.clk().Now()
	if due := evaluatePatrolDue(d.config.TownRoot, "patrol_scan_rogue_bd", time.Time{}, now, rogueBDInterval); !due.due {
		return
	}
	r := patrolscan.CheckRogueBD(patrolscan.RogueBDAggregates(d.config.TownRoot), patrolscan.RogueBDOptions{
		PathDirs:      filepath.SplitList(os.Getenv("PATH")),
		IsBuildOutput: gitIgnoredBDBuildOutput,
	})
	if err := savePatrolLastRun(d.config.TownRoot, "patrol_scan_rogue_bd", now); err != nil {
		d.logger.Printf("patrol_scan: rogue bd: recording last run: %v", err)
	}
	if r.Clean() {
		d.logger.Printf("patrol_scan: rogue bd check: clean (%d candidate(s), all build output)", len(r.Candidates))
		return
	}
	for _, f := range r.Findings() {
		line := fmt.Sprintf("rogue bd %s: %s (%s)", f.Path, f.Verdict, f.Detail)
		d.logger.Printf("patrol_scan: %s", line)
		d.escalateAlert("rogue-bd:"+f.Path, "patrol-scan", line+". A bd inside an agent worktree can migrate a production database; find who put it there.")
	}
	if len(r.WalkErrors) > 0 {
		line := fmt.Sprintf("rogue bd check INCONCLUSIVE: %d unreadable path(s), first: %s", len(r.WalkErrors), r.WalkErrors[0])
		d.logger.Printf("patrol_scan: %s", line)
		d.escalateAlert("rogue-bd:inconclusive", "patrol-scan", line)
	}
}

// gitIgnoredBDBuildOutput reports whether path is the git-ignored build
// output of a worktree that carries cmd/bd (the beads repo's own `make
// build`), which is by design (gt-5zsc). Any git failure, including a file
// outside a worktree, is an error: the candidate is then left alone and
// escalated as unclassified, never neutralized on a guess.
func gitIgnoredBDBuildOutput(path string) (bool, error) {
	dir := filepath.Dir(path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	top, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse in %s: %w", dir, err)
	}
	root := strings.TrimSpace(string(top))
	if !bdSourceTree(root) {
		return false, nil
	}
	err = exec.CommandContext(ctx, "git", "-C", dir, "check-ignore", "-q", path).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil // exit 1 is git's "not ignored" answer, not a failure
	}
	return false, fmt.Errorf("git check-ignore %s: %w", path, err)
}

// bdSourceTree reports whether root carries the bd source directory. A stat
// that fails for any reason reads as "carries it": the caller then asks git
// whether the binary is ignored build output, so an unreadable tree can only
// make a candidate by-design or unknown, never a false finding.
func bdSourceTree(root string) bool {
	fi, err := os.Stat(filepath.Join(root, "cmd", "bd"))
	return err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && fi.IsDir()
}

// patrolScanHost implements patrolscan.Env for the daemon.
type patrolScanHost struct {
	d *Daemon
}

var _ patrolscan.Env = (*patrolScanHost)(nil)

func (h *patrolScanHost) town() string { return h.d.config.TownRoot }

func (h *patrolScanHost) seat(rig, name string) supervisor.Seat {
	return supervisor.SeatIn(h.d.prefixRegistry(), rig, constants.RolePolecat, name)
}

func (h *patrolScanHost) sessionName(rig, name string) string {
	return session.PolecatSessionName(h.d.prefixRegistry().PrefixForRig(rig), name)
}

func (h *patrolScanHost) Polecats(rig string) ([]string, error) {
	names, err := listPolecatWorktrees(filepath.Join(h.town(), rig, "polecats"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // a rig with no polecats directory has no polecats
	}
	return names, err
}

func (h *patrolScanHost) Intent(rig, name string) (intent.Record, error) {
	return intent.Read(h.town(), supervisor.IntentSeat(h.seat(rig, name)))
}

func (h *patrolScanHost) Assess(rig, name string) liveness.Result {
	return h.d.assessSeat(h.seat(rig, name), liveness.Input{})
}

// bdJSON runs a read-only bd command pinned to the rig's database (or routed
// from the town root when the rig has no route) and returns stdout.
func (h *patrolScanHost) bdJSON(rig string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), patrolScanBdTimeout)
	defer cancel()
	env := bdReadOnlyRoutingEnv(h.town())
	if rig != "" {
		if rigDir := beads.GetRigDirForName(h.town(), rig); rigDir != "" {
			env = bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(rigDir))
		}
	}
	cmd := beads.CommandContextWithPath(ctx, h.d.bdPathOrDefault(), h.town(), env, args...)
	util.SetProcessGroup(cmd.Cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("bd %s: %w: %s", args[0], err, lastLine(msg))
		}
		return nil, fmt.Errorf("bd %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// bdMutating runs a bd command that may write, pinned to the rig's database
// (or routed from the town root for rig ""), and returns its output.
func (h *patrolScanHost) bdMutating(rig string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), patrolScanBdTimeout)
	defer cancel()
	env := bdMutationRoutingEnv(h.town())
	if rig != "" {
		if rigDir := beads.GetRigDirForName(h.town(), rig); rigDir != "" {
			base := agentconfig.NormalizeConfiguredDoltEnv(os.Environ(), h.town())
			env = beads.BuildMutationPinnedBDEnv(base, beads.ResolveBeadsDir(rigDir))
		}
	}
	cmd := beads.CommandContextWithPath(ctx, h.d.bdPathOrDefault(), h.town(), env, args...)
	util.SetProcessGroup(cmd.Cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, lastLine(string(out)))
	}
	return out, nil
}

// bdIssue is the slice of `bd list/show --json` the tick reads.
type bdIssue struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	Assignee    string   `json:"assignee"`
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
	Design      string   `json:"design"`
	Notes       string   `json:"notes"`
	UpdatedAt   string   `json:"updated_at"`
	AgentState  string   `json:"agent_state"`
}

func (b bdIssue) work() patrolscan.Work {
	w := patrolscan.Work{ID: b.ID, Status: b.Status, Assignee: b.Assignee, Labels: b.Labels, Design: b.Design, Notes: b.Notes}
	if t, err := time.Parse(time.RFC3339, b.UpdatedAt); err == nil {
		w.UpdatedAt = t
	}
	if f := beads.ParseAttachmentFields(&beads.Issue{Description: b.Description}); f != nil {
		w.AttachedMolecule = f.AttachedMolecule
	}
	return w
}

// listByStatus lists the rig's beads in the given statuses, optionally for
// one assignee. Any failed status makes the whole answer an error: the
// failed status may be the one holding the work.
func (h *patrolScanHost) listByStatus(rig, assignee string, statuses ...string) ([]bdIssue, error) {
	var all []bdIssue
	for _, status := range statuses {
		args := []string{"list", "--status=" + status, "--json", "--limit=0"}
		if assignee != "" {
			args = append(args, "--assignee="+assignee)
		}
		out, err := h.bdJSON(rig, beads.InjectFlatForListJSON(args)...)
		if err != nil {
			return nil, err
		}
		var batch []bdIssue
		if len(bytes.TrimSpace(out)) > 0 {
			if err := json.Unmarshal(out, &batch); err != nil {
				return nil, fmt.Errorf("parsing bd list --status=%s: %w", status, err)
			}
		}
		all = append(all, batch...)
	}
	return all, nil
}

func (h *patrolScanHost) AssignedWork(rig, name string) (*patrolscan.Work, error) {
	issues, err := h.listByStatus(rig, rig+"/polecats/"+name, "hooked", "in_progress")
	if err != nil {
		return nil, err
	}
	for _, is := range issues {
		if is.ID != "" {
			w := is.work()
			return &w, nil
		}
	}
	return nil, nil
}

func (h *patrolScanHost) AgentState(rig, name string) (string, error) {
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(h.town(), rig), rig, name)
	out, err := h.bdJSON(rig, "show", id, "--json")
	if err != nil {
		return "", err
	}
	var issues []bdIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return "", fmt.Errorf("parsing bd show %s: %w", id, err)
	}
	if len(issues) == 0 {
		return "", nil // a polecat with no agent bead has not parked itself
	}
	return beads.ResolveAgentState(issues[0].Description, issues[0].AgentState), nil
}

func (h *patrolScanHost) Heartbeat(rig, name string) *patrolscan.Heartbeat {
	hb := polecat.ReadSessionHeartbeat(h.town(), h.sessionName(rig, name))
	if hb == nil {
		return nil
	}
	return &patrolscan.Heartbeat{State: string(hb.State), At: hb.Timestamp}
}

func (h *patrolScanHost) Restart(rig, name, reason string) error {
	return h.d.sup().Restart(h.seat(rig, name), reason, patrolscan.Actor)
}

func (h *patrolScanHost) ActiveWork(rig string) ([]patrolscan.Work, error) {
	issues, err := h.listByStatus(rig, "", "hooked", "in_progress")
	if err != nil {
		return nil, err
	}
	out := make([]patrolscan.Work, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.work())
	}
	return out, nil
}

func (h *patrolScanHost) PolecatDirExists(rig, name string) (bool, error) {
	_, err := os.Stat(filepath.Join(h.town(), rig, "polecats", name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (h *patrolScanHost) SessionExists(rig, name string) (bool, error) {
	return h.d.tmux.HasSession(h.sessionName(rig, name))
}

func (h *patrolScanHost) MoleculeStatus(id string) (string, error) {
	out, err := h.bdJSON("", "show", id, "--json")
	if err != nil {
		return "", err
	}
	var issues []bdIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return "", fmt.Errorf("parsing bd show %s: %w", id, err)
	}
	if len(issues) == 0 {
		return "", nil
	}
	return issues[0].Status, nil
}

// CloseMolecule force-closes the molecule root and every step wisp under it,
// deepest first. The children are read with `bd show --children`, which sees
// the parent-child edges of a bonded ephemeral molecule (gt-43t7,
// gt-22hdp.36). A failed read closes nothing.
func (h *patrolScanHost) CloseMolecule(id, reason string) (int, error) {
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), 2*patrolScanBdTimeout)
	defer cancel()
	tree, err := beads.WispTree(ctx, h.town(), bdReadOnlyRoutingEnv(h.town()), id)
	if err != nil {
		return 0, err
	}
	return beads.CloseWispTree(ctx, h.town(), bdMutationRoutingEnv(h.town()), reason, tree)
}

func (h *patrolScanHost) SurvivingBranch(rig, beadID string) (string, error) {
	return polecat.SurvivingWorkForIssue(filepath.Join(h.town(), rig), beadID)
}

func (h *patrolScanHost) Comment(beadID, text string) error {
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), patrolScanBdTimeout)
	defer cancel()
	cmd := beads.CommandContextWithPath(ctx, h.d.bdPathOrDefault(), h.town(), bdMutationRoutingEnv(h.town()),
		"comments", "add", beadID, text, "--author", patrolscan.Actor)
	util.SetProcessGroup(cmd.Cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("bd comments add %s: %w: %s", beadID, err, lastLine(msg))
		}
		return fmt.Errorf("bd comments add %s: %w", beadID, err)
	}
	return nil
}

// restartPolecatSession is the supervisor's restart executor for polecat
// seats: `gt session restart <rig>/<name> --force`, which stops any session
// the seat has and starts a fresh one in the preserved worktree. It runs
// only after the supervisor's guards passed. The daemon never restarts a
// polecat outside the patrol_scan tick, so a restart for a rig the tick does
// not cover is declined.
func (d *Daemon) restartPolecatSession(seat supervisor.Seat) error {
	if !d.patrolScanActiveForRig(seat.Rig) {
		return fmt.Errorf("%w: patrol_scan does not cover rig %s", supervisor.ErrDeclined, seat.Rig)
	}
	if d.gtPath == "" {
		return fmt.Errorf("%w: no gt binary resolved", errNoDaemonStarter)
	}
	ctx, cancel := context.WithTimeout(d.ctxOrBackground(), patrolScanRestartTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.gtPath, "session", "restart", seat.Rig+"/"+seat.Name, //nolint:gosec // G204: gtPath resolved at daemon init
		"--force", "--requested-by", patrolscan.Actor)
	cmd.Dir = d.config.TownRoot
	cmd.Env = daemonGTEnv(os.Environ())
	util.SetProcessGroup(cmd)
	out, err := d.combinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("gt session restart %s/%s: %w: %s", seat.Rig, seat.Name, err, lastLine(string(out)))
	}
	return nil
}

// ctxOrBackground returns the daemon context, or Background for a Daemon
// built as a struct literal.
func (d *Daemon) ctxOrBackground() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// bdPathOrDefault returns the resolved bd binary, or "bd".
func (d *Daemon) bdPathOrDefault() string {
	if d.bdPath != "" {
		return d.bdPath
	}
	return "bd"
}
