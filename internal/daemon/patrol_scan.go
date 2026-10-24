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
		DoneCloseGrace: polecatDoneCloseGrace,
		Now:            now,
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

// patrolScanRunOptions is the tick's options: the config-derived ones plus
// the readings that need the host.
func (d *Daemon) patrolScanRunOptions(env *patrolScanHost) patrolscan.Options {
	opts := patrolScanOptions(d.patrolConfig, d.clk().Now)
	// The pool's cap, read here rather than in patrolScanOptions: it needs the
	// host's tmux reading, and it is the same cap a sling answers to
	// (gt-02tcp).
	opts.SeatCap = env.seatCap
	// The crash detector's own read of a seat's last done, so the tick and the
	// detector agree on a turn whose bead has not closed yet (gt-iwgbd).
	opts.DoneAge = env.doneAge
	return opts
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
	scanner := patrolscan.New(env, ledger, d.patrolScanRunOptions(env))
	rigs := d.getPatrolRigs("patrol_scan")
	for _, rigName := range rigs {
		if d.ctx != nil && d.ctx.Err() != nil {
			return
		}
		report := scanner.Tick(rigName)
		for _, line := range report.Lines() {
			d.logger.Printf("patrol_scan: %s", line)
		}
		d.reapAlertsIfEnabled(rigName, report, ledger)
	}
	d.patrolScanTimerGates(env, rigs)
	d.patrolScanGHGates(env, rigs)
	d.patrolScanRogueBD()
	env.patrolScanSandboxRecords()
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
//
// Each database keeps its own last run. bd reads one database per call, so a
// database whose check failed has had none of its gates evaluated and stays due
// for the next tick, while one that answered spends its hour. A single record
// for all of them either held the failed database's gates for the interval or,
// to avoid that, re-ran every database's `gh` check on each tick for as long
// as one stayed down (gt-oyrav, gt-kp9fo, gt-96bsu).
func (d *Daemon) patrolScanGHGates(h *patrolScanHost, rigs []string) {
	now := d.clk().Now()
	for _, rigName := range append([]string{""}, rigs...) {
		patrol := ghGatesPatrol(rigName)
		mem := d.ghGatesRunMemory(patrol)
		if due := evaluatePatrolDue(d.config.TownRoot, patrol, mem.last(), now, ghGateInterval); !due.due {
			continue
		}
		if !d.patrolScanGateCheckDB(h, rigName, "gh") {
			d.logger.Printf("patrol_scan: %s: gh gates: the gate check failed; not recording the run", gateCheckWhere(rigName))
			continue
		}
		d.recordPatrolRun(mem, patrol, now)
	}
}

// ghGatesRunMemory returns the in-memory last-run time of one database's gh
// gate check, keyed as its persisted record is.
func (d *Daemon) ghGatesRunMemory(patrol string) *patrolLastRunMemory {
	mem, _ := d.ghGatesRun.LoadOrStore(patrol, &patrolLastRunMemory{})
	return mem.(*patrolLastRunMemory)
}

// ghGatesPatrol is the last-run key for one database's gh gate check ("" is the
// town's). The town keeps the key the check had when one record covered every
// database, so an upgrade does not re-run an hour already spent.
func ghGatesPatrol(rigName string) string {
	if rigName == "" {
		return "patrol_scan_gh_gates"
	}
	return "patrol_scan_gh_gates:" + rigName
}

// gateCheckWhere names a gate-check database in the log.
func gateCheckWhere(rigName string) string {
	if rigName == "" {
		return "town"
	}
	return rigName
}

// patrolScanGateCheck runs `bd gate check --type=<gateType>` over the town
// database and each scanned rig's. Every database is checked even when an
// earlier one failed.
func (d *Daemon) patrolScanGateCheck(h *patrolScanHost, rigs []string, gateType string) {
	for _, rigName := range append([]string{""}, rigs...) {
		d.patrolScanGateCheckDB(h, rigName, gateType)
	}
}

// patrolScanGateCheckDB runs the gate check over one database ("" is the
// town's), logging what bd resolved. It reports whether bd answered: a database
// whose call failed has had none of its gates evaluated.
func (d *Daemon) patrolScanGateCheckDB(h *patrolScanHost, rigName, gateType string) bool {
	where := gateCheckWhere(rigName)
	out, err := h.bdMutating(rigName, "gate", "check", "--type="+gateType)
	if err != nil {
		d.logger.Printf("patrol_scan: %s: %s gate check failed: %v", where, gateType, err)
		return false
	}
	// bd prints a JSON null before its no-gates notice ("nullNo open gates of
	// type 'timer' found."); a tick that resolved nothing is not worth a line.
	line := strings.TrimPrefix(lastLine(string(out)), "null")
	if line != "" && !strings.HasPrefix(line, "No open gates") {
		d.logger.Printf("patrol_scan: %s: %s gates: %s", where, gateType, line)
	}
	return true
}

// rogueBDInterval is how often the rogue bd walk runs: rare event, bounded
// walk, persisted cadence so daemon restarts do not re-run it.
const rogueBDInterval = time.Hour

// patrolScanRogueBD runs the rogue bd check when it is due (the deacon's
// rogue-bd-check step). Findings are neutralized and escalated to the
// operator; an inconclusive walk is escalated too, never logged as clean.
func (d *Daemon) patrolScanRogueBD() {
	now := d.clk().Now()
	if due := evaluatePatrolDue(d.config.TownRoot, "patrol_scan_rogue_bd", d.rogueBDRun.last(), now, rogueBDInterval); !due.due {
		return
	}
	r := patrolscan.CheckRogueBD(patrolscan.RogueBDAggregates(d.config.TownRoot), patrolscan.RogueBDOptions{
		PathDirs:      filepath.SplitList(os.Getenv("PATH")),
		IsBuildOutput: gitIgnoredBDBuildOutput,
	})
	d.recordPatrolRun(&d.rogueBDRun, "patrol_scan_rogue_bd", now)
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
	// cleanupSandbox is session.CleanupSandboxWith over the real deleter when
	// nil: a seam for the unit tier, which has no gateway.
	cleanupSandbox func(townRoot, sessionName string) error
	// listOriginBranches is polecat.ListOriginPolecatBranches when nil: a seam
	// for the unit tier, which cannot start git.
	listOriginBranches func(rigRoot string) ([]string, error)
	// openRecoveryBeads is the routed bd client when nil: a seam for the unit
	// tier, which cannot start bd.
	openRecoveryBeads func(env []string) beads.Client
	// gitState is polecat.ProbeLiveGitState when nil: a seam for the unit
	// tier, which cannot start git.
	gitState func(worktreePath string) polecat.LiveGitState
	// bdGateCheck is the raw `bd gate check` call when nil: a seam for the
	// unit tier, which cannot start bd.
	bdGateCheck func(rig string, args ...string) ([]byte, error)
	// reapBatches caches each rig's check-recovery-batch for the life of this
	// host, which is one tick: the reap pass asks for a verdict on every
	// session-less seat, and one bulk sweep must answer them all.
	reapBatches map[string]reapBatch
	// reapClaims caches each rig's non-terminal bead listing for this tick:
	// every seat with a branch asks whether a bead claims it, and one
	// listing answers them all.
	reapClaims map[string]reapClaim
	// agentRecord is the polecat agent bead's recorded agent when nil: a seam
	// for the unit tier, which cannot start bd.
	agentRecord func(rig, name string) (string, error)
	// sandboxExists asks the gateway whether it has a sandbox called name when
	// nil: a seam for the unit tier, which has no gateway.
	sandboxExists func(gateway, name string) (bool, error)
	// removeSandboxRecord is session.CleanupSandboxWith(nil, ...) when nil: a
	// seam for the unit tier's removal failure.
	removeSandboxRecord func(townRoot, name string) error
	// sandboxRecordBudget is the records one record pass may check, each a tmux
	// probe and, unless a live session answers, a gateway question: 20
	// (sandboxRecordsPerTick) when zero. A seam so the bound test writes few
	// records.
	sandboxRecordBudget int
	// unrecordedLogged is set once the census has listed the polecats that
	// record no agent. The host lives one tick (patrol_scan.go), so each tick
	// logs them once however many restarts ask.
	unrecordedLogged bool
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
	if h.bdGateCheck != nil {
		return h.bdGateCheck(rig, args...)
	}
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

// AssignedWork reads the seat's live work bead, most specific status first.
// `open` is an assigned status, so it is read too: an open-assigned bead is
// still the seat's live work (gt-74cbe).
func (h *patrolScanHost) AssignedWork(rig, name string) (*patrolscan.Work, error) {
	issues, err := h.listByStatus(rig, rig+"/polecats/"+name, "hooked", "in_progress", "open")
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
	rec := patrolscan.AgentRecord{
		State: beads.ResolveAgentState(issue.Description, issue.AgentState),
		// hook_bead is written to the description; a bead from before that
		// contract may still carry the column, and either one set reads as work
		// the seat may still hold.
		HookBead:  issue.HookBead,
		UpdatedAt: polecat.AgentBeadUpdatedAt(issue),
	}
	if f := beads.ParseAgentFields(issue.Description); f != nil {
		rec.ExitType = f.ExitType
		rec.CleanupStatus = f.CleanupStatus
		rec.LastSourceIssue = f.LastSourceIssue
		if f.HookBead != "" {
			rec.HookBead = f.HookBead
		}
	}
	return rec, nil
}

// PolecatEscalations reads the escalations the rig's polecat seats raised, open
// and closed, from the town database: notify.Raise files every escalation
// there (escalationBeads), whatever rig the raiser belongs to, so one read
// answers for every rig. A record another role raised is dropped here, and the
// mail carriers routed for an escalation (gt:message, which outlives it) are
// not escalations at all (IsEscalationRecord).
func (h *patrolScanHost) PolecatEscalations(rig string) ([]patrolscan.Escalation, error) {
	issues, err := h.d.escalationBeads().List(beads.ListOptions{
		Label: "gt:escalation", Status: "all", IncludeInfra: true, Priority: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("bd list escalations: %w", err)
	}
	prefix := rig + "/polecats/"
	out := make([]patrolscan.Escalation, 0, len(issues))
	for _, is := range issues {
		if !beads.IsEscalationRecord(is) {
			continue
		}
		fields := beads.ParseEscalationFields(is.Description)
		if !strings.HasPrefix(strings.TrimSpace(fields.EscalatedBy), prefix) {
			continue
		}
		out = append(out, patrolscan.Escalation{
			ID:          is.ID,
			EscalatedBy: fields.EscalatedBy,
			Title:       is.Title,
			RelatedBead: fields.RelatedBead,
			Open:        !beads.IssueStatus(is.Status).IsTerminal(),
		})
	}
	return out, nil
}

// GitState measures the seat's worktree with the live probe: the same three
// facts the reuse verdict reads, measured now, since a recorded cleanup_status
// says what the turn that ended saw, not what the worktree holds today.
//
// The probe runs in the seat's git worktree, resolved the same way the reuse
// verdict and the checkpoint dog resolve it. Probing the polecats/<name>
// container instead — which is not a git worktree — never answered, so a seat
// whose escalation had been resolved could not reach the clean verdict and
// stayed DEGRADED (gt-okk50).
//
// A sandbox seat's work lives in its sandbox, so the host worktree is a stale
// leftover whose commits read as unpushed: the gateway's answer decides instead
// (polecat.SandboxSeatGitState, gt-b4waf). The host worktree is not resolved
// for such a seat, since a nuked seat has none.
func (h *patrolScanHost) GitState(rig, name string) (patrolscan.GitState, error) {
	seat, sandboxed, err := h.openshellSeat(rig, name)
	if err != nil {
		return patrolscan.GitState{}, fmt.Errorf("telling a sandbox seat from a host seat: %w", err)
	}
	var st polecat.LiveGitState
	if sandboxed {
		sessionName := h.sessionName(rig, name)
		exists, existsErr := h.sandboxStillExists(seat.gateway, sessionName)
		st = polecat.SandboxSeatGitState("", exists, existsErr, sessionName)
	} else {
		polecatsDir := filepath.Join(h.town(), rig, "polecats")
		path := resolvePolecatWorktree(polecatsDir, name, rig)
		if path == "" {
			return patrolscan.GitState{}, fmt.Errorf("no git worktree for polecat %s: tried %s",
				name, strings.Join(polecatWorktreeCandidates(polecatsDir, name, rig), ", "))
		}
		st = h.gitProbe()(path)
	}
	if st.Source != polecat.GitStateSourceLive {
		reason := st.FailedReason
		if reason == "" {
			reason = "the probe did not answer"
		}
		return patrolscan.GitState{}, errors.New(reason)
	}
	return patrolscan.GitState{
		Branch:          st.Branch,
		Dirty:           st.Dirty,
		StashCount:      st.StashCount,
		UnpushedCommits: st.UnpushedCommits,
	}, nil
}

// gitProbe is polecat.ProbeLiveGitState unless a test substituted one: the
// live probe runs git, which the unit tier cannot start.
func (h *patrolScanHost) gitProbe() func(string) polecat.LiveGitState {
	if h.gitState != nil {
		return h.gitState
	}
	return polecat.ProbeLiveGitState
}

// ClearStuckExit resets the seat's agent bead to idle and removes the exit
// type gt done's ESCALATED or DEFERRED exit wrote beside it (updateAgentStateOnDone
// writes stuck for every exit but COMPLETED). The write is guarded on the
// record still reading as the exit the tick decided on, so a seat something else has moved on
// (a fresh sling, a human) is left to it rather than overwritten with idle.
func (h *patrolScanHost) ClearStuckExit(rig, name, wantExit string) (bool, error) {
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(h.town(), rig), rig, name)
	bd := h.recoveryBeads()
	issue, err := bd.Show(id)
	if err != nil {
		return false, fmt.Errorf("bd show %s: %w", id, err)
	}
	if issue == nil {
		return false, fmt.Errorf("bd show %s: no issue", id)
	}
	// The guard reads the state the tick decided on, resolved the same way
	// AgentRecord reads it.
	exit := ""
	if f := beads.ParseAgentFields(issue.Description); f != nil {
		exit = f.ExitType
	}
	state := beads.ResolveAgentState(issue.Description, issue.AgentState)
	if !strings.EqualFold(strings.TrimSpace(state), "stuck") ||
		!strings.EqualFold(strings.TrimSpace(exit), strings.TrimSpace(wantExit)) {
		return false, nil
	}
	idle, cleared := string(beads.AgentStateIdle), ""
	if err := beads.UpdateAgentDescriptionFields(bd, id, beads.AgentFieldUpdates{AgentState: &idle, ExitType: &cleared}); err != nil {
		return false, fmt.Errorf("clearing %s: %w", id, err)
	}
	return true, nil
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

// Kill stops a stalled seat's live session through the supervisor's
// automatic-path kill, which refuses a parked or frozen seat and an e-stop
// (gt-9p99w).
//
// A sandboxed seat's session lives in its sandbox, so once the kill succeeded the
// sandbox and its registry record are deleted too; left behind they keep the seat
// looking occupied. A host seat has no record and nothing more happens.
func (h *patrolScanHost) Kill(rig, name, reason string) error {
	if err := h.d.sup().Kill(h.seat(rig, name), reason, patrolscan.Actor); err != nil {
		return err
	}
	cleanup := h.cleanupSandbox
	if cleanup == nil {
		cleanup = func(townRoot, sessionName string) error {
			return session.CleanupSandboxWith(session.SandboxDeleter, townRoot, sessionName)
		}
	}
	if err := cleanup(h.town(), h.sessionName(rig, name)); err != nil {
		return fmt.Errorf("killed the session, but removing the sandbox of %s/%s failed: %w", rig, name, err)
	}
	return nil
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

// doneAge answers patrolscan.Options.DoneAge from the event log, through the
// read the daemon's crash detector uses.
func (h *patrolScanHost) doneAge(rig, name, workBead string, now time.Time) (time.Duration, bool) {
	return h.d.seatLastDoneAge(rig, name, workBead, now)
}

// ActiveWork lists the rig's assigned work beads for the orphan and held-wisp
// passes. The statuses it reads, and why `open` is one of them, are the
// patrolscan.Env contract's (gt-gnqmu).
func (h *patrolScanHost) ActiveWork(rig string) ([]patrolscan.Work, error) {
	issues, err := h.listByStatus(rig, "", "hooked", "in_progress", "open")
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

// MoleculeStatus reads a molecule root's status. A root bd says is not found
// was reaped, which the patrolscan.Env contract reports as "" with no error.
func (h *patrolScanHost) MoleculeStatus(id string) (string, error) {
	issue, err := h.readBeads("").Show(id)
	if errors.Is(err, beads.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if issue == nil {
		return "", nil
	}
	return issue.Status, nil
}

// leakedWispStatuses are the statuses of a wisp that is not closed. The
// work-bead reads never ask for pinned, which holds most of the leaks.
var leakedWispStatuses = []string{"open", "in_progress", "pinned", "hooked"}

// LeakedWisps lists the rig's non-closed wisps, from the wisps table (bd
// query, which also reports each wisp's parent). Any failed status makes the
// whole answer an error: the failed status may be the one holding the leak.
// A wisp whose created_at does not parse comes back with a zero CreatedAt,
// which the pass reads as an unknown age.
func (h *patrolScanHost) LeakedWisps(rig string) ([]patrolscan.Wisp, error) {
	var out []patrolscan.Wisp
	seen := map[string]bool{}
	for _, status := range leakedWispStatuses {
		batch, err := h.readBeads(rig).List(beads.ListOptions{Status: status, Ephemeral: true, Priority: -1})
		if err != nil {
			return nil, fmt.Errorf("listing %s wisps: %w", status, err)
		}
		for _, is := range batch {
			if is == nil || is.ID == "" || seen[is.ID] {
				continue
			}
			seen[is.ID] = true
			w := patrolscan.Wisp{ID: is.ID, Status: is.Status, Assignee: is.Assignee, Parent: is.Parent}
			if t, err := time.Parse(time.RFC3339, is.CreatedAt); err == nil {
				w.CreatedAt = t
			}
			out = append(out, w)
		}
	}
	return out, nil
}

// WispStatus reads the status of the wisp or bead id names. A bead bd says is
// not found is gone, not an unknown: the root a leaked step names was reaped.
func (h *patrolScanHost) WispStatus(id string) (string, error) {
	issue, err := h.readBeads("").Show(id)
	if errors.Is(err, beads.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if issue == nil {
		return "", nil
	}
	return issue.Status, nil
}

// RootWork reads the work beads a molecule root's blocks edges point at,
// from the dependencies bd show reports for the root, each with the status bd
// reports for that bead. A root bd says does not exist has none: it was
// reaped between the listing and this read.
func (h *patrolScanHost) RootWork(root string) ([]patrolscan.WorkRef, error) {
	issue, err := h.readBeads("").Show(root)
	if errors.Is(err, beads.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, nil
	}
	var out []patrolscan.WorkRef
	for _, dep := range issue.Dependencies {
		if dep.DependencyType == "blocks" {
			out = append(out, patrolscan.WorkRef{ID: dep.ID, Status: dep.Status})
		}
	}
	return out, nil
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
