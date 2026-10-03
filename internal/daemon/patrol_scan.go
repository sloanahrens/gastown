package daemon

import (
	"context"
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
	"github.com/steveyegge/gastown/internal/dispatch"
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
			return dispatch.DispatchHoldFields(w.Status, w.Labels, w.Assignee, w.Design, w.Notes)
		},
		IsRefusal: func(err error) bool {
			return errors.Is(err, supervisor.ErrRefused) || errors.Is(err, errReapRefused)
		},
		Now: now,
	}
	if c := patrolScanConfig(config); c != nil {
		o.DeadSamples = c.DeadSamples
		if d, err := time.ParseDuration(c.ReportWindow); err == nil && d > 0 {
			o.ReportWindow = d
		}
		if wc := c.WorktreeCleanup; wc.IsEnabled() {
			o.Reap = &patrolscan.ReapOptions{
				DryRun:      wc.IsDryRun(),
				Grace:       wc.Grace(),
				ParkedGrace: wc.ParkedGrace(),
				MaxPerTick:  wc.Cap(),
			}
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
		d.reapAlertsIfEnabled(rigName, report)
	}
	d.patrolScanTimerGates(env, rigs)
	d.patrolScanGHGates(env, rigs)
	d.patrolScanRogueBD()
}

// patrolScanTimerGates resolves elapsed timer gates in the town database and
// in each scanned rig's (the deacon's gate-evaluation step and the witness's
// check-timer-gates step). `bd gate check` resolves a timer gate whose
// timeout has passed; it never escalates one.
func (d *Daemon) patrolScanTimerGates(h *patrolScanHost, rigs []string) {
	d.patrolScanGateCheck(h, rigs, "timer")
}

// ghGateInterval is how often the tick evaluates GitHub gates. A check with a
// gh gate open shells out to `gh` over the network, so it runs on its own slow
// cadence rather than on the two-minute tick; with none open it is one query
// per database.
const ghGateInterval = time.Hour

// patrolScanGHGates evaluates the GitHub gates (gh:run, gh:pr). The deacon's
// gate-evaluation step deferred them to a separate step that never existed, so
// before this no step in any patrol evaluated one (gt-4k3fj.6.2). It resolves
// a gate whose run succeeded and leaves a failed one open; the tick sends no
// mail, so escalation stays the operator's.
func (d *Daemon) patrolScanGHGates(h *patrolScanHost, rigs []string) {
	now := d.clk().Now()
	if due := evaluatePatrolDue(d.config.TownRoot, "patrol_scan_gh_gates", time.Time{}, now, ghGateInterval); !due.due {
		return
	}
	d.patrolScanGateCheck(h, rigs, "gh")
	if err := savePatrolLastRun(d.config.TownRoot, "patrol_scan_gh_gates", now); err != nil {
		d.logger.Printf("patrol_scan: gh gates: recording last run: %v", err)
	}
}

// patrolScanGateCheck runs `bd gate check --type=<gateType>` over the town
// database and each scanned rig's, logging what bd resolved.
func (d *Daemon) patrolScanGateCheck(h *patrolScanHost, rigs []string, gateType string) {
	for _, rigName := range append([]string{""}, rigs...) {
		where := rigName
		if where == "" {
			where = "town"
		}
		out, err := h.bdMutating(rigName, "gate", "check", "--type="+gateType)
		if err != nil {
			d.logger.Printf("patrol_scan: %s: %s gate check failed: %v", where, gateType, err)
			continue
		}
		// bd prints a JSON null before its no-gates notice ("nullNo open gates of
		// type 'timer' found."); a tick that resolved nothing is not worth a line.
		line := strings.TrimPrefix(lastLine(string(out)), "null")
		if line == "" || strings.HasPrefix(line, "No open gates") {
			continue
		}
		d.logger.Printf("patrol_scan: %s: %s gates: %s", where, gateType, line)
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
	// listOriginBranches is polecat.ListOriginPolecatBranches when nil: a seam
	// for the unit tier, which cannot start git.
	listOriginBranches func(rigRoot string) ([]string, error)
	// openRecoveryBeads is the routed bd client when nil: a seam for the unit
	// tier, which cannot start bd.
	openRecoveryBeads func(env []string) beads.Client
	// reapBatches caches each rig's check-recovery-batch for the life of this
	// host, which is one tick: the reap pass asks for a verdict on every
	// session-less seat, and one bulk sweep must answer them all.
	reapBatches map[string]reapBatch
	// reapClaims caches each rig's non-terminal bead listing for this tick:
	// every seat with a branch asks whether a bead claims it, and one
	// listing answers them all.
	reapClaims map[string]reapClaim
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

// readBeads is the read-only client for rig's database (pinned), or routed
// from the town root for rig "" or a rig with no directory.
func (h *patrolScanHost) readBeads(rig string) workBeadReader {
	env := bdReadOnlyRoutingEnv(h.town())
	if rig != "" {
		env = h.d.workBeadsEnv(rig)
	}
	return h.d.workBeads(env, patrolScanBdTimeout)
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
	// Kept raw (gt-7iwy0.4.1): its one caller is bd gate check, which has no
	// machine output to type and nothing a fake database could model; the
	// patrol logs its last prose line.
	cmd := beads.CommandContextWithPath(ctx, h.d.bdPathOrDefault(), h.town(), env, args...)
	util.SetProcessGroup(cmd.Cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, lastLine(string(out)))
	}
	return out, nil
}

// issueWork is the slice of a work bead the tick reads.
func issueWork(is *beads.Issue) patrolscan.Work {
	w := patrolscan.Work{ID: is.ID, Status: is.Status, Assignee: is.Assignee, Labels: is.Labels, Design: is.Design, Notes: is.Notes}
	if t, err := time.Parse(time.RFC3339, is.UpdatedAt); err == nil {
		w.UpdatedAt = t
	}
	if f := beads.ParseAttachmentFields(&beads.Issue{Description: is.Description}); f != nil {
		w.AttachedMolecule = f.AttachedMolecule
	}
	return w
}

// listByStatus lists the rig's beads in the given statuses, optionally for
// one assignee. Any failed status makes the whole answer an error: the
// failed status may be the one holding the work.
func (h *patrolScanHost) listByStatus(rig, assignee string, statuses ...string) ([]*beads.Issue, error) {
	var all []*beads.Issue
	for _, status := range statuses {
		batch, err := h.readBeads(rig).List(beads.ListOptions{Status: status, Assignee: assignee, Priority: -1})
		if err != nil {
			return nil, err
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
			w := issueWork(is)
			return &w, nil
		}
	}
	return nil, nil
}

// WorkBead reads one work bead by ID. A bead bd says is not found is gone,
// not an unknown: the seat's record outlived it, and nothing is waiting to
// land (gt-xs1ni).
func (h *patrolScanHost) WorkBead(rig, id string) (*patrolscan.Work, error) {
	issue, err := h.readBeads(rig).Show(id)
	if errors.Is(err, beads.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, nil
	}
	w := issueWork(issue)
	return &w, nil
}

// ClearSubmission ends the seat's submitted wait once its work bead is no
// longer submitted (the landing was pulled for rework, or a human took it):
// the record goes to stop and the ordinary path decides what the seat needs.
func (h *patrolScanHost) ClearSubmission(rig, name, workBead string) (bool, error) {
	return intent.ClearLanded(h.town(), supervisor.IntentSeat(h.seat(rig, name)), workBead, patrolscan.Actor, h.d.clk().Now())
}

// AgentRecord reads the polecat's agent bead: the state, the gt done exit type
// and the cleanup status gt done wrote beside it. One read answers all three,
// since they are one record of how the last turn ended.
func (h *patrolScanHost) AgentRecord(rig, name string) (patrolscan.AgentRecord, error) {
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(h.town(), rig), rig, name)
	issue, err := h.readBeads(rig).Show(id)
	if err != nil {
		return patrolscan.AgentRecord{}, err
	}
	if issue == nil {
		// Fail closed: a read that answered nothing is not "no state", which
		// the tick would read as a free seat.
		return patrolscan.AgentRecord{}, fmt.Errorf("bd show %s: no issue", id)
	}
	rec := patrolscan.AgentRecord{State: beads.ResolveAgentState(issue.Description, issue.AgentState)}
	if f := beads.ParseAgentFields(issue.Description); f != nil {
		rec.ExitType = f.ExitType
		rec.CleanupStatus = f.CleanupStatus
	}
	return rec, nil
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

// MarkIdle retires a polecat seat's record to stop once its session is gone
// and it holds no work, so the record stops asking for a session that nothing
// will start (gt-613vw).
func (h *patrolScanHost) MarkIdle(rig, name string) (bool, error) {
	return intent.MarkIdle(h.town(), supervisor.IntentSeat(h.seat(rig, name)), patrolscan.Actor, h.d.clk().Now())
}

// MarkSubmitted brings a seat's record up to the gt:ready-to-land label its
// work bead carries, for a gt done whose record write was lost: the seat is
// mid-landing, and a record that still says run reports it dead for the whole
// of the landing (gt-2z8k1).
func (h *patrolScanHost) MarkSubmitted(rig, name, workBead string) error {
	return intent.MarkSubmitted(h.town(), supervisor.IntentSeat(h.seat(rig, name)), workBead, patrolscan.Actor, h.d.clk().Now())
}

func (h *patrolScanHost) ActiveWork(rig string) ([]patrolscan.Work, error) {
	issues, err := h.listByStatus(rig, "", "hooked", "in_progress")
	if err != nil {
		return nil, err
	}
	out := make([]patrolscan.Work, 0, len(issues))
	for _, is := range issues {
		out = append(out, issueWork(is))
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
	issue, err := h.readBeads("").Show(id)
	if err != nil {
		return "", err
	}
	return issue.Status, nil
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

// Comment appends a comment written by the tick to a bead.
func (h *patrolScanHost) Comment(beadID, text string) error {
	if err := h.recoveryBeads().AddCommentAs(beadID, patrolscan.Actor, text); err != nil {
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
