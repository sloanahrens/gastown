package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/util"
)

const defaultScheduledSlingPriority = 3

var scheduledSlingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func scheduledSlingValidate(e ScheduledSlingEntry) error {
	if !scheduledSlingNameRe.MatchString(e.Name) {
		return fmt.Errorf("scheduled_slings: name %q must match %s", e.Name, scheduledSlingNameRe)
	}
	if e.Rig == "" || e.Formula == "" {
		return fmt.Errorf("scheduled_slings[%s]: rig and formula are required", e.Name)
	}
	d, err := time.ParseDuration(e.IntervalStr)
	if err != nil || d <= 0 {
		return fmt.Errorf("scheduled_slings[%s]: interval %q must be a positive Go duration", e.Name, e.IntervalStr)
	}
	if err := scheduledSlingValidateDispatch(e); err != nil {
		return err
	}
	if e.GateTimeoutStr != "" {
		if gt, err := time.ParseDuration(e.GateTimeoutStr); err != nil || gt <= 0 {
			return fmt.Errorf("scheduled_slings[%s]: gate_timeout %q must be a positive Go duration", e.Name, e.GateTimeoutStr)
		}
	}
	return nil
}

func scheduledSlingInterval(e ScheduledSlingEntry) time.Duration {
	d, _ := time.ParseDuration(e.IntervalStr)
	return d
}

// scheduledSlingLabelPrefix starts every scheduled sling's identity label.
const scheduledSlingLabelPrefix = "scheduled:"

func scheduledSlingLabel(e ScheduledSlingEntry) string { return scheduledSlingLabelPrefix + e.Name }

func scheduledSlingPriority(e ScheduledSlingEntry) int {
	if e.Priority == 0 {
		return defaultScheduledSlingPriority
	}
	return e.Priority
}

// scheduledBead is the slice of a run bead the decision needs. CreatedAt is
// parsed from bd's string timestamp (see parseScheduledBeads); CloseReason
// distinguishes a run that completed from one the patrol closed because it
// could not be slung. ResumeBranch is the branch the dead-holder recovery
// recorded when it returned the run to open ("" for a bead it never reopened).
type scheduledBead struct {
	ID           string
	Status       string
	CloseReason  string
	CreatedAt    time.Time
	ResumeBranch string
}

// scheduledSlingFailureMarker prefixes the close reason the patrol writes on a
// run bead whose `gt sling` failed. It marks the bead as "no run happened", so
// decideScheduledSling ignores it and the next tick retries instead of reading
// it as a completed run (which would suppress dispatch for a whole interval).
const scheduledSlingFailureMarker = "scheduled_slings-sling-failed"

// scheduledSlingFailureReason is what the patrol records on such a bead.
const scheduledSlingFailureReason = scheduledSlingFailureMarker + ": gt sling failed; the patrol retries on the next tick"

// scheduledSlingStalledReason is what the patrol records on a run bead it
// created but no sling ever hooked, once the bead outlived the sling budget
// (gt-g16z2). The failure marker keeps it read as "no run happened": any other
// reason would gate the interval as a completed run and suppress the entry.
const scheduledSlingStalledReason = scheduledSlingFailureMarker + ": the run bead stayed open and unhooked past the sling budget; the patrol retries on the next tick"

// isFailedScheduledRun reports whether a closed run bead records a sling that
// never started. The reason is matched by prefix because bd may normalize or
// truncate close reasons.
func isFailedScheduledRun(b scheduledBead) bool {
	return b.Status == "closed" && strings.HasPrefix(b.CloseReason, scheduledSlingFailureMarker)
}

type scheduledAction int

const (
	scheduledSkipOpen     scheduledAction = iota // a run is in flight or its MR is queued
	scheduledSkipRecent                          // the newest run started less than an interval ago
	scheduledRecoverStale                        // an unhooked run bead outlived the sling budget
	scheduledResume                              // a run the dead-holder recovery reopened is slung again on its branch
	scheduledDispatch
)

func (a scheduledAction) String() string {
	switch a {
	case scheduledSkipOpen:
		return "skip-open"
	case scheduledSkipRecent:
		return "skip-recent"
	case scheduledRecoverStale:
		return "recover-stale"
	case scheduledResume:
		return "resume"
	default:
		return "dispatch"
	}
}

// scheduledDecision is one tick's verdict for an entry: the action, and the
// run beads a stale-action has to retire.
type scheduledDecision struct {
	action scheduledAction
	// stale holds every open, unhooked run bead past staleAfter. Only
	// scheduledRecoverStale sets it.
	stale []scheduledBead
	// resume is the reopened run bead scheduledResume slings again.
	resume scheduledBead
}

// isOpenUnhooked reports whether the bead is open with no sling holding it.
// gt sling moves the bead to hooked before it spawns the polecat, so a run that
// never started is open. So is a run that did start and lost its polecat: the
// dead-holder recovery returns it to open (isReopenedRun tells the two apart).
func isOpenUnhooked(b scheduledBead) bool { return b.Status == string(beads.StatusOpen) }

// isReopenedRun reports whether the bead is a run that started and was handed
// back: the dead-holder recovery records the branch the work survives on before
// it reopens the bead, and leaves created_at alone, so age cannot tell this
// bead from one no sling ever hooked. A recovery that found no surviving branch
// leaves no note, and none is needed: there is no work to resume, so retiring
// the bead and running again from main loses nothing.
func isReopenedRun(b scheduledBead) bool { return isOpenUnhooked(b) && b.ResumeBranch != "" }

// decideScheduledSling is pure: the beads carrying the entry's label, the
// entry's interval, the age past which an open run bead is a leftover rather
// than a sling in progress (staleAfter), and now. A zero staleAfter marks an entry
// the spec dispatcher feeds: it recovers and resumes nothing, because the
// dispatcher resumes a reopened bead itself. Any bead that could still be
// a live run wins over recency, so a run whose MR is still in the queue is
// never doubled. A run that failed to sling is neither: it never started, so
// it must not block the retry. Neither is an unhooked bead past staleAfter,
// which nothing is left to hook (see retireStalledScheduledRuns); reading that
// one as skip-open would reset the failure counter that exists to escalate it.
func decideScheduledSling(beads []scheduledBead, interval, staleAfter time.Duration, now time.Time) scheduledDecision {
	var (
		newest time.Time
		stale  []scheduledBead
		resume *scheduledBead
	)
	for _, b := range beads {
		if isFailedScheduledRun(b) {
			continue
		}
		if b.Status != "closed" {
			// A reopened run is not stale at any age: its polecat died, and
			// the branch it left is work to continue, not a leftover to close.
			if isReopenedRun(b) && staleAfter > 0 {
				if resume == nil {
					resume = &b
				}
				continue
			}
			// A bead with no readable created_at has no age to judge, and a
			// duplicate run costs more than a stalled one, so it counts as a
			// run in flight. A zero staleAfter is the same answer, so a caller
			// that passes none recovers nothing rather than closing live runs.
			if isOpenUnhooked(b) && !b.CreatedAt.IsZero() && staleAfter > 0 && now.Sub(b.CreatedAt) > staleAfter {
				stale = append(stale, b)
				continue
			}
			return scheduledDecision{action: scheduledSkipOpen}
		}
		if b.CreatedAt.After(newest) {
			newest = b.CreatedAt
		}
	}
	if resume != nil {
		return scheduledDecision{action: scheduledResume, resume: *resume}
	}
	if len(stale) > 0 {
		// The beads must be retired before the entry can run again, and they
		// cannot be dispatched around: dispatching now would put a second run
		// beside a bead that is still open.
		return scheduledDecision{action: scheduledRecoverStale, stale: stale}
	}
	if !newest.IsZero() && now.Sub(newest) < interval {
		return scheduledDecision{action: scheduledSkipRecent}
	}
	return scheduledDecision{action: scheduledDispatch}
}

// parseScheduledBeads reads `bd list --json` output. created_at stays a string
// and goes through beads.ParseIssueTime: bd/Dolt timestamps can carry
// fractional seconds, which encoding/json's strict RFC3339 time.Time
// unmarshaller rejects, and one bad row would fail the whole list.
// scheduledBeadsOf is the run beads as the patrol reads them.
func scheduledBeadsOf(issues []*beads.Issue) []scheduledBead {
	out := make([]scheduledBead, 0, len(issues))
	for _, is := range issues {
		out = append(out, scheduledBead{
			ID:           is.ID,
			Status:       is.Status,
			CloseReason:  is.CloseReason,
			CreatedAt:    beads.ParseIssueTime(is.CreatedAt),
			ResumeBranch: patrolscan.ResumeBranchFromNotes(is.Notes),
		})
	}
	return out
}

// scheduledSlingRunner is the side-effect boundary: bd list, bd create, gt
// sling, and the close that retires a run whose sling failed.
type scheduledSlingRunner interface {
	listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error)
	createBead(ctx context.Context, rig string, run scheduledRunBead) (string, error)
	// sling hooks beadID and spawns the polecat. A non-empty branch resumes the
	// work on that existing branch (gt sling --branch) instead of starting
	// from main.
	sling(ctx context.Context, beadID, branch string, e ScheduledSlingEntry) error
	closeBead(ctx context.Context, rig, beadID, reason string) error
	// originHasBranch reports whether branch is on the rig repo's origin, so a
	// resume is not slung on a branch deleted out-of-band (gt-84o0a).
	originHasBranch(ctx context.Context, rig, branch string) (bool, error)
	// runGate runs the entry's gate_command under ctx's deadline and returns
	// its combined output and run error (see scheduledGateAllows).
	runGate(ctx context.Context, e ScheduledSlingEntry) ([]byte, error)
	// admit reports the town's reason for refusing this entry's sling right
	// now — the pool at its cap, the merge queue over its limit — or "" when
	// the sling has a seat (dispatch.SlingRefusalMarker is the shared contract).
	admit(ctx context.Context, e ScheduledSlingEntry) (string, error)
}

const (
	scheduledSlingsTickInterval  = 15 * time.Minute
	scheduledSlingCommandTimeout = 5 * time.Minute
	// scheduledSlingLookupTimeout bounds the bead read after a sling timed out.
	scheduledSlingLookupTimeout = 30 * time.Second
	// scheduledSlingAdmitTimeout bounds the capacity pre-flight, which runs on
	// its own clock so a slow probe does not leave the sling below less budget.
	scheduledSlingAdmitTimeout  = 30 * time.Second
	scheduledSlingEscalateAfter = 3
)

type execScheduledSlingRunner struct {
	townRoot, bdPath, gtPath string
	// execCmd runs the gt subprocesses; nil runs them for real.
	execCmd cmdRunFunc
	// open opens a rig's database; nil is bdPath pinned to the rig's .beads.
	open func(rig string) beads.Client
}

// store is the rig's own database: run beads are created, listed and closed
// there, never routed elsewhere.
func (r *execScheduledSlingRunner) store(rig string) beads.Client {
	if r.open != nil {
		return r.open(rig)
	}
	return beads.NewPinned(filepath.Join(r.townRoot, rig, ".beads"), beads.WithBin(r.bdPath))
}

// listBeads lists every run bead with label, closed ones included: the run
// bead is closed when the run finishes, and a list without the closed ones
// never sees the newest run, so every tick would re-dispatch regardless of
// the configured interval.
func (r *execScheduledSlingRunner) listBeads(_ context.Context, rig, label string) ([]scheduledBead, error) {
	issues, err := r.store(rig).List(beads.ListOptions{Label: label, Status: "all", Priority: -1})
	if err != nil {
		return nil, err
	}
	return scheduledBeadsOf(issues), nil
}

// closeBead closes a run bead with a reason.
func (r *execScheduledSlingRunner) closeBead(_ context.Context, rig, beadID, reason string) error {
	return r.store(rig).CloseWithReason(reason, beadID)
}

// originHasBranch asks the rig's bare repo whether origin carries branch.
func (r *execScheduledSlingRunner) originHasBranch(_ context.Context, rig, branch string) (bool, error) {
	return git.NewGit(filepath.Join(r.townRoot, rig, ".repo.git")).RemoteBranchExists("origin", branch)
}

func (r *execScheduledSlingRunner) createBead(_ context.Context, rig string, run scheduledRunBead) (string, error) {
	issue, err := r.store(rig).Create(beads.CreateOptions{Title: run.Title, Labels: run.Labels, Priority: run.Priority, Description: run.Description, Acceptance: run.Acceptance})
	if err != nil {
		return "", err
	}
	return issue.ID, nil
}

func (r *execScheduledSlingRunner) slingArgs(beadID, branch string, e ScheduledSlingEntry) []string {
	args := []string{"sling", beadID, e.Rig, "--formula=" + e.Formula}
	if e.Agent != "" {
		args = append(args, "--agent="+e.Agent)
	}
	if branch != "" {
		args = append(args, "--branch="+branch)
	}
	args = append(args, "--actor=daemon/scheduled:"+e.Name, "--no-boot")
	keys := make([]string, 0, len(e.Vars))
	for k := range e.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--var", k+"="+e.Vars[k])
	}
	return args
}

func (r *execScheduledSlingRunner) sling(ctx context.Context, beadID, branch string, e ScheduledSlingEntry) error {
	cmd := exec.CommandContext(ctx, r.gtPath, r.slingArgs(beadID, branch, e)...)
	cmd.Dir = r.townRoot
	cmd.Env = daemonGTEnv(bdMutationRoutingEnv(r.townRoot))
	util.SetProcessGroup(cmd)
	if _, stderr, err := runWith(r.execCmd, cmd); err != nil {
		return fmt.Errorf("gt sling %s: %w: %s", beadID, err, slingErrorLine(string(stderr)))
	}
	return nil
}

// admit asks `gt sling --dry-run` — the route the live sling would take, a
// refusal included — so the answer is read before the run bead that refusal
// would cost exists. The preview prints the refusal on stdout where the live
// sling prints it on stderr, so both streams are searched for the marker
// (dispatch.SlingRefusalLine).
func (r *execScheduledSlingRunner) admit(ctx context.Context, e ScheduledSlingEntry) (string, error) {
	args := []string{"sling", e.Formula, e.Rig, "--dry-run", "--no-boot"}
	if e.Agent != "" {
		args = append(args, "--agent="+e.Agent)
	}
	args = append(args, "--actor=daemon/scheduled:"+e.Name)
	cmd := exec.CommandContext(ctx, r.gtPath, args...)
	cmd.Dir = r.townRoot
	cmd.Env = daemonGTEnv(bdMutationRoutingEnv(r.townRoot))
	util.SetProcessGroup(cmd)
	stdout, stderr, err := runWith(r.execCmd, cmd)
	if reason, ok := dispatch.SlingRefusalLine(string(stdout) + "\n" + string(stderr)); ok {
		return reason, nil
	}
	if err != nil {
		return "", fmt.Errorf("gt sling --dry-run: %w: %s", err, slingErrorLine(string(stderr)))
	}
	return "", nil
}

// slingErrorLine is the one-line summary of a failed sling's output: the
// refusal line when sling refused the bead (so isScheduledSlingRefusal sees the
// marker whatever precedes it), else the first line that is not a step timing
// line (sling.StepPrefix, internal/sling/timer.go), falling back to the first
// line so a failure is never logged as an empty string.
func slingErrorLine(output string) string {
	if reason, ok := dispatch.SlingRefusalLine(output); ok {
		return reason
	}
	first := ""
	for i, l := range strings.Split(output, "\n") {
		if i == 0 {
			first = l
		}
		if l != "" && !strings.HasPrefix(l, sling.StepPrefix) {
			return l
		}
	}
	return first
}

// scheduledSlingDeferred is the error runScheduledSlingEntry returns for a
// sling that refused the run bead because the town is at capacity: the seat
// pool or the merge queue said not now. It is not a failed run, so the patrol
// logs it as a deferral and leaves the failure counter alone.
type scheduledSlingDeferred struct{ cause error }

func (e *scheduledSlingDeferred) Error() string { return e.cause.Error() }
func (e *scheduledSlingDeferred) Unwrap() error { return e.cause }

// isScheduledSlingRefusal reports whether a failed sling was a refusal rather
// than a break. The patrol runs `gt sling` as a subprocess, so the refusal
// marker in its output is the contract (dispatch.SlingRefusalLine, the predicate
// internal/cmd's isSlingRefusal shares), so a parked or docked rig defers the
// entry instead of counting toward its escalation.
func isScheduledSlingRefusal(err error) bool {
	if err == nil {
		return false
	}
	_, ok := dispatch.SlingRefusalLine(err.Error())
	return ok
}

func (d *Daemon) scheduledRunner() scheduledSlingRunner {
	if d.scheduledSlingRunner == nil {
		d.scheduledSlingRunner = &execScheduledSlingRunner{townRoot: d.config.TownRoot, bdPath: d.bdPath, gtPath: d.gtPath, execCmd: d.execCmd}
	}
	return d.scheduledSlingRunner
}

// catchUpScheduledSlings is the startup evaluation Run makes before the first
// tick. It stands down while shutdown.lock is held, as the tick path does, so a
// daemon started during `gt down` does not dispatch into a town being torn
// down (gt-84o0a).
func (d *Daemon) catchUpScheduledSlings() {
	if d.isShutdownInProgress() {
		d.logger.Printf("scheduled_slings: startup catch-up skipped: shutdown in progress")
		return
	}
	d.triggerScheduledSlings()
}

// triggerScheduledSlings starts a cycle on its own goroutine; an overlapping
// tick is skipped.
func (d *Daemon) triggerScheduledSlings() {
	if !d.scheduledSlingsRunning.CompareAndSwap(false, true) {
		d.logger.Printf("scheduled_slings: previous cycle still running, skipping this tick")
		return
	}
	go func() {
		defer d.scheduledSlingsRunning.Store(false)
		// The startup catch-up dispatches before the first heartbeat has
		// started Dolt, and every entry reads bead history through it, so the
		// cycle brings the server up itself rather than counting a cold start
		// as a failed tick (gt-ox6c).
		if err := d.ensureDoltServerUp(); err != nil {
			if !errors.Is(err, errDoltBringUpSkipped) {
				d.logger.Printf("scheduled_slings: Dolt server unavailable: %v", err)
			}
			return
		}
		d.runScheduledSlings()
	}()
}

func (d *Daemon) runScheduledSlings() {
	if !d.isPatrolActive("scheduled_slings") {
		return
	}
	// The operator's town-wide hold parks this patrol like every other
	// automatic dispatcher (gt-ifijm). It is not a failure: no run bead is
	// created, the failure counters are untouched, and the first tick after
	// the hold lifts dispatches whatever is due.
	holdReason := dispatch.OperatorHold(d.config.TownRoot)
	if d.scheduledSlingsHold.Changed(holdReason) {
		if holdReason != "" {
			d.logger.Printf("scheduled_slings: not dispatching: %s", holdReason)
		} else {
			d.logger.Printf("scheduled_slings: operator dispatch hold lifted; resuming")
		}
	}
	if holdReason != "" {
		return
	}
	cfg := d.patrolConfig.Patrols.ScheduledSlings
	if d.scheduledSlingFailures == nil {
		d.scheduledSlingFailures = map[string]int{}
	}
	if d.scheduledSlingEscalate == nil {
		d.scheduledSlingEscalate = d.escalate
	}
	now := time.Now()
	names := make(map[string]bool, len(cfg.Entries))
	for _, e := range cfg.Entries {
		names[e.Name] = true
	}
	d.scheduledGateFailures.retain(names)
	for _, e := range cfg.Entries {
		if err := scheduledSlingValidate(e); err != nil {
			d.logger.Printf("scheduled_slings: %v (entry skipped)", err)
			continue
		}
		// A per-rig ESTOP holds this entry's rig only; like the town hold it
		// is not a failure and does not count toward escalation.
		if reason := dispatch.RigHold(d.config.TownRoot, e.Rig); reason != "" {
			d.logger.Printf("scheduled_slings: %s: not dispatching: %s", e.Name, reason)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), scheduledSlingCommandTimeout)
		err := d.runScheduledSlingEntry(ctx, e, now)
		cancel()
		if err == nil {
			d.scheduledSlingFailures[e.Name] = 0
			continue
		}
		var deferred *scheduledSlingDeferred
		if errors.As(err, &deferred) {
			// Capacity, not failure: the entry is retried next tick. The
			// counter is neither advanced nor reset, so failures on either
			// side of a deferral still add up.
			d.logger.Printf("scheduled_slings: %s: deferred: %v", e.Name, deferred)
			continue
		}
		d.scheduledSlingFailures[e.Name]++
		n := d.scheduledSlingFailures[e.Name]
		d.logger.Printf("scheduled_slings: %s: failure %d: %v", e.Name, n, err)
		if n == scheduledSlingEscalateAfter {
			d.scheduledSlingEscalate("scheduled_slings",
				fmt.Sprintf("scheduled sling %s (%s on %s) failed %d ticks in a row; last error: %v",
					e.Name, e.Formula, e.Rig, n, err))
		}
	}
}

// scheduledRunMayHaveStarted reports whether a run bead whose sling timed out
// is past "open": gt sling hooks the bead before it spawns the polecat. A bead
// that cannot be read counts as started, since a duplicate run costs more than
// a stalled one — and a stalled one is not silent either: left open and
// unhooked past scheduledStaleRunAfter it is retired as a failed run on a later
// tick (gt-g16z2), so erring toward "started" costs the entry a tick.
func (d *Daemon) scheduledRunMayHaveStarted(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry, id string) bool {
	// The sling's own context is spent; look again on a fresh one.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledSlingLookupTimeout)
	defer cancel()
	runs, err := r.listBeads(lctx, e.Rig, scheduledSlingLabel(e))
	if err != nil {
		d.logger.Printf("scheduled_slings: %s: cannot read run %s after the sling timed out: %v", e.Name, id, err)
		return true
	}
	for _, b := range runs {
		if b.ID == id {
			return b.Status != "open"
		}
	}
	return true
}

// runScheduledSlingEntry evaluates one entry and dispatches when due.
func (d *Daemon) runScheduledSlingEntry(ctx context.Context, e ScheduledSlingEntry, now time.Time) error {
	r := d.scheduledRunner()
	beadsForLabel, err := r.listBeads(ctx, e.Rig, scheduledSlingLabel(e))
	if err != nil {
		return err
	}
	// A spec-dispatched run bead is open and unhooked for as long as the
	// dispatcher queues it, so the sling budget says nothing about its age: a
	// zero staleAfter recovers nothing and a queued run stays skip-open, until
	// reportUntakenSpecRuns reads one the dispatcher never takes as a failure.
	staleAfter := d.scheduledStaleRunAfter()
	if e.Dispatch == scheduledDispatchSpec {
		staleAfter = 0
	}
	plan := decideScheduledSling(beadsForLabel, scheduledSlingInterval(e), staleAfter, now)
	if plan.action == scheduledRecoverStale {
		return d.retireStalledScheduledRuns(ctx, r, e, plan.stale, now)
	}
	if plan.action == scheduledResume {
		return d.resumeReopenedScheduledRun(ctx, r, e, plan.resume)
	}
	if plan.action != scheduledDispatch {
		d.logger.Printf("scheduled_slings: %s: %s", e.Name, plan.action)
		if plan.action == scheduledSkipOpen && e.Dispatch == scheduledDispatchSpec {
			return reportUntakenSpecRuns(beadsForLabel, e, now)
		}
		return nil
	}
	// A gated or failed gate is not a failed run: no bead exists, the sling
	// failure counter is untouched, and the next tick asks again.
	if !d.scheduledGateAllows(ctx, r, e) {
		return nil
	}
	// Asking before the run bead exists costs one probe; the sling's own refusal
	// costs a bead created and failed-closed on every tick the town stays full
	// (gt-ttqq8). A spec-dispatched run is not slung by this patrol, so it has
	// no capacity to pre-check.
	if e.Dispatch != scheduledDispatchSpec {
		reason, err := d.scheduledSlingAdmits(ctx, r, e)
		if err != nil {
			// A probe that cannot answer is not the town at capacity (the pool's
			// own posture on an uncountable seat read): dispatch as before, and
			// let the sling's refusal be the backstop.
			d.logger.Printf("scheduled_slings: %s: capacity pre-flight could not answer (%v); dispatching as before", e.Name, err)
		} else if reason != "" {
			d.logger.Printf("scheduled_slings: %s: deferred before creating a run bead: %s", e.Name, reason)
			return &scheduledSlingDeferred{cause: errors.New(reason)}
		}
	}
	// The gate ran on its own clock; create and sling start a full budget, so a
	// slow gate does not cut gt sling short.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.scheduledSlingBudget())
	defer cancel()
	title := fmt.Sprintf("%s %s", e.Name, now.UTC().Format("2006-01-02"))
	if e.Dispatch == scheduledDispatchSpec {
		return d.createSpecDispatchedRun(ctx, r, e, title)
	}
	desc := fmt.Sprintf("Scheduled run of formula %s on rig %s, created by the daemon's scheduled_slings patrol. Label %s is the run's identity; while this bead is open no second run is dispatched.", e.Formula, e.Rig, scheduledSlingLabel(e))
	id, err := r.createBead(ctx, e.Rig, scheduledRunBead{Title: title, Labels: []string{scheduledSlingLabel(e)}, Description: desc, Priority: scheduledSlingPriority(e)})
	if err != nil {
		return err
	}
	d.logger.Printf("scheduled_slings: %s: created %s, slinging %s to %s (agent %q)", e.Name, id, e.Formula, e.Rig, e.Agent)
	if err := r.sling(ctx, id, "", e); err != nil {
		// A sling killed by its deadline may already have hooked the bead and
		// spawned the polecat; failure-closing it would dispatch the same run
		// again next tick (gt-0frud).
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && d.scheduledRunMayHaveStarted(ctx, r, e, id) {
			return fmt.Errorf("%w (run bead %s kept open: the polecat may have started)", err, id)
		}
		// The run never started, so its bead must not stay open: every later
		// tick would read it as skip-open, report success, reset the failure
		// counter, and leave the entry silently dead. Close it with the failure
		// marker — decideScheduledSling ignores such beads — and return the
		// error so this tick counts toward the escalation.
		if cerr := r.closeBead(ctx, e.Rig, id, scheduledSlingFailureReason); cerr != nil {
			d.logger.Printf("scheduled_slings: %s: closing failed run %s: %v", e.Name, id, cerr)
		}
		// A refusal retires the bead the same way but is not a failed tick.
		if isScheduledSlingRefusal(err) {
			return &scheduledSlingDeferred{cause: err}
		}
		return err
	}
	return nil
}

// scheduledSlingAdmits runs the runner's capacity pre-flight on its own budget:
// gt sling's route decision does not spend the entry's sling budget.
func (d *Daemon) scheduledSlingAdmits(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledSlingAdmitTimeout)
	defer cancel()
	return r.admit(ctx, e)
}

// resumeReopenedScheduledRun slings a reopened run bead again on the branch the
// dead-holder recovery preserved, so the formula continues where it stopped
// instead of running twice from main beside an orphaned branch. A failed sling
// leaves the bead open and is returned, so the next tick retries it and the
// failures count toward the escalation: closing it with the failure marker
// would send the next tick to a fresh run and strand the branch again.
//
// A branch that is gone from origin (deleted out-of-band) holds no work to
// continue and would fail every tick, so the run is slung from main instead.
// A probe that cannot answer is not "gone": the resume goes ahead as before
// (gt-84o0a).
func (d *Daemon) resumeReopenedScheduledRun(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry, b scheduledBead) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.scheduledSlingBudget())
	defer cancel()
	branch := b.ResumeBranch
	if onOrigin, err := r.originHasBranch(ctx, e.Rig, branch); err != nil {
		d.logger.Printf("scheduled_slings: %s: cannot check branch %s on origin: %v", e.Name, branch, err)
	} else if !onOrigin {
		d.logger.Printf("scheduled_slings: %s: run %s was reopened but branch %s is gone from origin; slinging it from main", e.Name, b.ID, branch)
		branch = ""
	}
	// A run whose last sling failed at session start rests out its backoff
	// before this patrol slings it again (gt-811f6): the reopen leaves the bead
	// open, so without the check every tick resumes it into the same failure.
	// The check sits after the branch probe so the tick still records where a
	// resumed run would go from while it rests. A deferral leaves the failure
	// counter alone and the next tick asks again.
	if reason := dispatch.StartupBackoff(d.config.TownRoot, b.ID); reason != "" {
		d.logger.Printf("scheduled_slings: %s: run %s: not resuming: startup backoff: %s", e.Name, b.ID, reason)
		return &scheduledSlingDeferred{cause: errors.New("startup backoff: " + reason)}
	}
	if branch == "" {
		d.logger.Printf("scheduled_slings: %s: slinging run %s: %s to %s from main", e.Name, b.ID, e.Formula, e.Rig)
	} else {
		d.logger.Printf("scheduled_slings: %s: run %s was reopened after its polecat died; slinging %s to %s again on branch %s",
			e.Name, b.ID, e.Formula, e.Rig, branch)
	}
	if err := r.sling(ctx, b.ID, branch, e); err != nil {
		if isScheduledSlingRefusal(err) {
			return &scheduledSlingDeferred{cause: err}
		}
		return fmt.Errorf("resuming reopened run %s %s: %w", b.ID, resumeTarget(branch), err)
	}
	return nil
}

// resumeTarget names where a resumed run was slung, for the failure message:
// the branch it continued on, or main when the branch was gone from origin.
func resumeTarget(branch string) string {
	if branch == "" {
		return "from main"
	}
	return "on branch " + branch
}

// scheduledSlingBudget is the time one entry's create and sling may take.
func (d *Daemon) scheduledSlingBudget() time.Duration {
	if d.scheduledSlingTimeout > 0 {
		return d.scheduledSlingTimeout
	}
	return scheduledSlingCommandTimeout
}

// scheduledStaleRunAfter is how long a run bead may stay open and unhooked
// before the patrol reads it as a run that never started: one sling budget,
// the longest a `gt sling` that created it can still be running, plus one tick,
// the patrol's next chance to see how that sling ended.
func (d *Daemon) scheduledStaleRunAfter() time.Duration {
	return d.scheduledSlingBudget() + scheduledSlingsTickInterval
}

// retireStalledScheduledRuns failure-closes run beads that were created but
// never hooked and outlived the sling budget, and reports the recovery as an
// error so this tick counts toward the escalation. Nothing else closes them,
// so without this a leftover from a daemon that died between creating a bead
// and slinging it reads as skip-open on every later tick: the entry reports
// success, its failure counter resets, and it stays dead until an operator
// closes the bead by hand (gt-g16z2).
func (d *Daemon) retireStalledScheduledRuns(ctx context.Context, r scheduledSlingRunner, e ScheduledSlingEntry, stale []scheduledBead, now time.Time) error {
	for _, b := range stale {
		age := now.Sub(b.CreatedAt).Round(time.Second)
		d.logger.Printf("scheduled_slings: %s: run %s was created but no sling hooked it within %s (budget %s); retiring it as a failed run so the next tick retries",
			e.Name, b.ID, age, d.scheduledStaleRunAfter())
		if err := r.closeBead(ctx, e.Rig, b.ID, scheduledSlingStalledReason); err != nil {
			return fmt.Errorf("run bead %s stayed open and unhooked for %s and could not be retired: %w", b.ID, age, err)
		}
	}
	return fmt.Errorf("run bead(s) %s left open and unhooked past the %s sling budget; retired as failed runs so the next tick retries",
		staleRunIDs(stale), d.scheduledStaleRunAfter())
}

// staleRunIDs is the retired beads' ids, for the failure message.
func staleRunIDs(stale []scheduledBead) string {
	ids := make([]string, 0, len(stale))
	for _, b := range stale {
		ids = append(ids, b.ID)
	}
	return strings.Join(ids, ", ")
}
