// Package schedulerrun dispatches queued (scheduled) work: it reads the open
// sling contexts, decides which of their work beads are ready, and runs one
// capacity-controlled dispatch cycle.
//
// `gt scheduler run` and the daemon heartbeat's dispatch tick call Run, so the
// operator's manual trigger and the automatic one run the same code, each in
// its own process. Package cmd keeps what this package cannot own: cobra flag
// parsing, the sling itself, and the polecat-capacity probe. Those arrive
// through Deps.
package schedulerrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/style"
)

// MaxDispatchFailures is the maximum number of consecutive dispatch failures
// before a sling context is closed as circuit-broken.
const MaxDispatchFailures = 3

// crossRigEscalationDebounce is the minimum interval between cross-rig prefix
// escalations for the same (rig, prefix) pair. Prevents alert spam when a
// stuck context keeps re-appearing on every dispatch tick.
const crossRigEscalationDebounce = time.Hour

// Options is one run's inputs.
type Options struct {
	// TownRoot is the workspace root the run reads and writes.
	TownRoot string
	// Actor identifies the caller in the scheduler feed events: the operator
	// for `gt scheduler run`, "daemon" for the heartbeat tick.
	Actor string
	// BatchOverride caps how many beads this run may dispatch (0 = the
	// configured batch size).
	BatchOverride int
	// DryRun prints what would dispatch without dispatching anything.
	DryRun bool
	// Daemon marks a run the daemon heartbeat started. Operator narration is
	// suppressed and a dispatch another process already holds is not an error
	// to report, only a reason to come back next tick.
	Daemon bool
	// Out and ErrOut receive narration and warnings. nil means os.Stdout and
	// os.Stderr.
	Out, ErrOut io.Writer
}

// Deps are the collaborators package cmd still owns. Sling and Seats are
// required for a run that dispatches; a dry run needs only Seats.
type Deps struct {
	// Sling dispatches one ready bead: it hooks the work, spawns the polecat
	// and starts its session (package cmd's executeSling). A refusal that
	// should leave the context queued rather than fail it is returned as a
	// *Deferral.
	Sling func(ctx context.Context, townRoot string, b capacity.PendingBead) (SlingOutcome, error)
	// Seats reports the polecat-capacity picture the plan is built against
	// (package cmd's polecatCapacitySnapshotForTown). clean asks it to sweep
	// stale seat reservations first; a dry run passes false and stays
	// read-only.
	Seats func(townRoot string, clean bool) (Seats, error)
	// Escalate reports a cross-rig dispatch refusal to the operator. Optional;
	// nil drops the escalation and keeps the dispatch refusal.
	Escalate func(rig, prefix, beadID string)
	// BeadInTargetRig reports whether a work bead is visible in its target
	// rig's database, the guard a dry run uses to preview a dispatch the sling
	// would refuse. Optional; nil skips the probe.
	BeadInTargetRig func(beadID, targetRig, townRoot string) error
}

// SlingOutcome is what one successful dispatch reports back.
type SlingOutcome struct {
	// PolecatName is the seat the sling took, when the implementation names
	// one. It is carried into the scheduler_dispatch feed event.
	PolecatName string
}

// Deferral is a dispatch refusal that leaves the sling context queued and
// does not count against its circuit breaker: capacity is full, or the work
// of a dead holder survives. Anything else returned by Sling is a failure.
type Deferral struct {
	// Why names the refusal in operator terms ("Capacity full").
	Why string
	// Err is the refusal being classified.
	Err error
}

// Error is the refusal's own words; Why is how the dispatcher names it in
// the log line.
func (d *Deferral) Error() string {
	if d.Err != nil {
		return d.Err.Error()
	}
	return d.Why
}

func (d *Deferral) Unwrap() error { return d.Err }

// Seats is the polecat seat picture: how many seats the town's polecat pool
// has, how many are taken, and by what. Free is what the plan admits.
type Seats struct {
	Max             int
	Working         int
	RecoveryBlocked int
	ReusableIdle    int
	Parked          int
	PendingMR       int
	Reservations    int
	Free            int
}

// Report is what one run did.
type Report struct {
	Dispatched int
	Failed     int
	Skipped    int
	Reason     string
	// Rigs are the target rigs that took a dispatch this run, each once.
	Rigs []string
}

// runner is one Run's options and collaborators.
type runner struct {
	opts Options
	deps Deps
	// actor is opts.Actor with the empty-string default applied.
	actor string
}

func (r *runner) out() io.Writer {
	if r.opts.Out != nil {
		return r.opts.Out
	}
	return os.Stdout
}

func (r *runner) errOut() io.Writer {
	if r.opts.ErrOut != nil {
		return r.opts.ErrOut
	}
	return os.Stderr
}

func (r *runner) printf(format string, args ...interface{}) {
	fmt.Fprintf(r.out(), format, args...)
}

func (r *runner) eprintf(format string, args ...interface{}) {
	fmt.Fprintf(r.errOut(), format, args...)
}

// escalationDebouncer tracks last-escalation timestamps per (rig, prefix).
// Process-local — debounce resets on daemon restart, which is fine: a new
// process should be allowed to surface the issue once.
type escalationDebouncer struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// crossRigEscalations is the dispatch path's debounce state. Tests build
// their own escalationDebouncer rather than sharing this one.
var crossRigEscalations = &escalationDebouncer{}

// crossRigEscalationKey returns the debounce key for a (rig, prefix) pair.
func crossRigEscalationKey(rig, prefix string) string {
	return rig + "/" + prefix
}

// shouldFire reports whether enough time has elapsed since the last
// escalation for this (rig, prefix) pair to fire a new one. Updates the
// timestamp on a positive answer.
func (d *escalationDebouncer) shouldFire(rig, prefix string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := crossRigEscalationKey(rig, prefix)
	if last, ok := d.last[key]; ok && now.Sub(last) < crossRigEscalationDebounce {
		return false
	}
	if d.last == nil {
		d.last = map[string]time.Time{}
	}
	d.last[key] = now
	return true
}

// schedulerDispatchPlan is everything one dispatch run decided before it
// starts slinging.
type schedulerDispatchPlan struct {
	State       *capacity.SchedulerState
	MaxPolecats int
	BatchSize   int
	SpawnDelay  time.Duration
	Seats       Seats
	Scheduled   []ContextInfo
	Ready       []capacity.PendingBead
	Plan        capacity.DispatchPlan
}

// Run performs one scheduler dispatch pass and reports what it did.
//
// A dry run only reads: it prints the plan and dispatches nothing. A real run
// stands down under the operator's town-wide dispatch hold, takes the
// scheduler-dispatch lock so two dispatchers cannot double-sling a context,
// and then runs one capacity.DispatchCycle.
func Run(ctx context.Context, opts Options, deps Deps) (Report, error) {
	r := &runner{opts: opts, deps: deps, actor: opts.Actor}
	if r.actor == "" {
		r.actor = "scheduler"
	}

	if opts.DryRun {
		dispatchPlan, err := r.buildPlan(opts.TownRoot, opts.BatchOverride, false)
		if err != nil {
			return Report{}, fmt.Errorf("planning dispatch: %w", err)
		}
		dispatchPlan.Plan = r.validateDryRun(opts.TownRoot, dispatchPlan.Plan)
		r.printDryRunPlan(dispatchPlan)
		return Report{}, nil
	}

	// The operator's town-wide dispatch hold parks the scheduler (gt-ifijm).
	// Every caller — the daemon heartbeat, the witness on SLOT_OPEN, and a
	// hand-typed `gt scheduler run` — passes through here, so this is the one
	// gate for all of them. Queued contexts stay open and dispatch after the
	// hold lifts. A dry run above is read-only and stays available.
	if reason := dispatch.OperatorHold(opts.TownRoot); reason != "" {
		if !opts.Daemon {
			r.printf("%s Scheduler dispatch held: %s\n", style.Dim.Render("⏸"), reason)
		}
		return Report{}, nil
	}

	// Acquire exclusive lock to prevent concurrent dispatch
	runtimeDir := filepath.Join(opts.TownRoot, ".runtime")
	_ = os.MkdirAll(runtimeDir, 0755)
	lockFile := filepath.Join(runtimeDir, "scheduler-dispatch.lock")
	fileLock := flock.New(lockFile)
	locked, err := fileLock.TryLock()
	if err != nil {
		return Report{}, fmt.Errorf("acquiring dispatch lock: %w", err)
	}
	if !locked {
		if opts.Daemon {
			return Report{}, nil
		}
		return Report{}, fmt.Errorf("scheduler dispatch already in progress (lock held: %s)", lockFile)
	}
	defer func() { _ = fileLock.Unlock() }()

	dispatchPlan, err := r.buildPlan(opts.TownRoot, opts.BatchOverride, true)
	if err != nil {
		return Report{}, fmt.Errorf("planning dispatch: %w", err)
	}

	if dispatchPlan.State.Paused {
		if !opts.Daemon {
			r.printf("%s Scheduler is paused (by %s), skipping %d ready bead(s)\n",
				style.Dim.Render("⏸"), dispatchPlan.State.PausedBy, len(dispatchPlan.Ready))
		}
		return Report{}, nil
	}

	// Nothing to dispatch when scheduler is in direct dispatch or disabled mode.
	if dispatchPlan.MaxPolecats <= 0 {
		if !opts.Daemon {
			if len(dispatchPlan.Scheduled) > 0 {
				r.printf("%s %d context bead(s) still open from a previous deferred mode\n",
					style.Warning.Render("⚠"), len(dispatchPlan.Scheduled))
				r.printf("  Use: gt scheduler clear  (close all sling context beads)\n")
				r.printf("  Or:  gt config set scheduler.max_polecats N  (re-enable deferred dispatch)\n")
			} else {
				r.printf("No ready beads scheduled for dispatch\n")
			}
		}
		return Report{}, nil
	}

	// Wire up the DispatchCycle
	successfulRigs := make(map[string]bool)
	// Track polecat names from dispatch results, keyed by context bead ID.
	polecatNames := make(map[string]string)
	cycle := &capacity.DispatchCycle{
		Validate: func(b capacity.PendingBead) error {
			return r.validateForDispatch(opts.TownRoot, b, true)
		},
		Execute: func(b capacity.PendingBead) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			result, err := r.deps.Sling(ctx, opts.TownRoot, b)
			if err != nil {
				return err
			}
			// Track side effects here (Execute runs exactly once, never retried).
			if result.PolecatName != "" {
				polecatNames[b.ID] = result.PolecatName
			}
			if b.TargetRig != "" {
				successfulRigs[b.TargetRig] = true
			}
			_ = events.LogFeed(events.TypeSchedulerDispatch, r.actor,
				events.SchedulerDispatchPayload(b.WorkBeadID, b.TargetRig, polecatNames[b.ID]))
			return nil
		},
		OnSuccess: func(b capacity.PendingBead) error {
			// OnSuccess may be retried — only do the close here, no side effects.
			// Route to the correct rig's beads dir (GH#3468).
			return beads.CloseSlingContext(beadsForPendingContext(opts.TownRoot, b), b.ID, "dispatched")
		},
		OnFailure: func(b capacity.PendingBead, err error) {
			var onSuccessErr *capacity.ErrOnSuccessFailed
			if errors.As(err, &onSuccessErr) {
				// Polecat launched but context close failed — not a true dispatch failure.
				// Log a distinct warning so operators can distinguish from "polecat never launched".
				r.eprintf("%s Dispatch of %s succeeded but context close failed: %v\n",
					style.Warning.Render("⚠"), b.WorkBeadID, err)
				// Last-resort close attempt to prevent double-dispatch on next cycle.
				// OnSuccess already retried 2x; this is a final attempt before circuit-breaking.
				ctxBeads := beadsForPendingContext(opts.TownRoot, b)
				if closeErr := beads.CloseSlingContext(ctxBeads, b.ID, "dispatch-close-failed"); closeErr != nil {
					r.eprintf("%s CRITICAL: last-resort close of %s failed — risk of double-dispatch for %s: %v\n",
						style.Warning.Render("⚠"), b.ID, b.WorkBeadID, closeErr)
				} else {
					// Last-resort close succeeded — context is now closed.
					// Log a feed event so the degradation is visible.
					_ = events.LogFeed(events.TypeSchedulerCloseRetry, r.actor,
						events.SchedulerDispatchPayload(b.WorkBeadID, b.TargetRig, polecatNames[b.ID]))
					// Skip recordDispatchFailure to avoid writing to a closed context.
					return
				}
			} else if deferral, deferred := asDeferral(err); deferred {
				r.eprintf("%s %s while dispatching %s; leaving context queued: %v\n",
					style.Dim.Render("○"), deferral.Why, b.WorkBeadID, err)
				return
			} else {
				_ = events.LogFeed(events.TypeSchedulerDispatchFailed, r.actor,
					events.SchedulerDispatchFailedPayload(b.WorkBeadID, b.TargetRig, err.Error()))
			}
			r.recordFailure(beadsForPendingContext(opts.TownRoot, b), b, err)
		},
		SpawnDelay: dispatchPlan.SpawnDelay,
	}

	dispatchPlan.Plan = r.dropRigHeldBeads(opts.TownRoot, dispatchPlan.Plan)
	cycleReport, err := cycle.RunPlan(dispatchPlan.Plan)
	if err != nil {
		return Report{}, fmt.Errorf("dispatch cycle failed: %w", err)
	}
	if len(dispatchPlan.Plan.ToDispatch) > 0 && cycleReport.Dispatched == 0 && cycleReport.Failed == 0 {
		return Report{}, fmt.Errorf("scheduler dispatch invariant violation: plan had %d dispatchable bead(s) but no dispatch result", len(dispatchPlan.Plan.ToDispatch))
	}

	// Update runtime state with fresh read to avoid clobbering concurrent pause.
	if cycleReport.Dispatched > 0 {
		freshState, err := capacity.LoadState(opts.TownRoot)
		if err != nil {
			r.printf("%s Could not reload scheduler state: %v\n", style.Dim.Render("Warning:"), err)
		} else {
			freshState.RecordDispatch(cycleReport.Dispatched)
			if err := capacity.SaveState(opts.TownRoot, freshState); err != nil {
				r.printf("%s Could not save scheduler state: %v\n", style.Dim.Render("Warning:"), err)
			}
		}
	}

	if cycleReport.Dispatched > 0 || cycleReport.Failed > 0 {
		r.printf("\n%s Dispatched %d, failed %d (reason: %s)\n",
			style.Bold.Render("✓"), cycleReport.Dispatched, cycleReport.Failed, cycleReport.Reason)
	} else if cycleReport.Skipped > 0 {
		r.printDispatchNoOp(cycleReport, dispatchPlan.Seats)
	} else if !opts.Daemon {
		r.printDispatchNoOp(cycleReport, dispatchPlan.Seats)
	}

	report := Report{
		Dispatched: cycleReport.Dispatched,
		Failed:     cycleReport.Failed,
		Skipped:    cycleReport.Skipped,
		Reason:     cycleReport.Reason,
	}
	for rig := range successfulRigs {
		report.Rigs = append(report.Rigs, rig)
	}
	sort.Strings(report.Rigs)
	return report, nil
}

// asDeferral reports whether err is a dispatch deferral, and names it.
func asDeferral(err error) (*Deferral, bool) {
	var deferral *Deferral
	if errors.As(err, &deferral) {
		return deferral, true
	}
	return nil, false
}

// buildPlan assembles one dispatch plan: load scheduler state and settings,
// optionally clean stale contexts, assess the open sling contexts, and ask
// internal/scheduler/capacity what to do with the ready ones.
func (r *runner) buildPlan(townRoot string, batchOverride int, cleanup bool) (*schedulerDispatchPlan, error) {
	state, err := capacity.LoadState(townRoot)
	if err != nil {
		return nil, fmt.Errorf("loading scheduler state: %w", err)
	}

	settingsPath := config.TownSettingsPath(townRoot)
	settings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("loading town settings: %w", err)
	}
	schedulerCfg := settings.Scheduler
	if schedulerCfg == nil {
		schedulerCfg = capacity.DefaultSchedulerConfig()
	}

	maxPolecats := schedulerCfg.GetMaxPolecats()
	batchSize := schedulerCfg.GetBatchSize()
	if batchOverride > 0 {
		batchSize = batchOverride
	}
	spawnDelay := schedulerCfg.GetSpawnDelay()

	if cleanup && !state.Paused && maxPolecats > 0 {
		if err := cleanupStaleContexts(townRoot); err != nil {
			return nil, fmt.Errorf("cleaning stale scheduler contexts: %w", err)
		}
	}

	assessments, blockedErr := assessScheduledContexts(townRoot, r.errOut())
	if blockedErr != nil {
		return nil, blockedErr
	}

	seats, err := r.deps.Seats(townRoot, cleanup)
	if err != nil {
		return nil, fmt.Errorf("loading polecat capacity: %w", err)
	}

	ready := readySlingContextsFromAssessments(r.errOut(), assessments)
	dispatchPlan := capacity.PlanDispatch(seats.Free, batchSize, ready)
	if len(ready) > 0 {
		switch {
		case state.Paused:
			dispatchPlan = capacity.DispatchPlan{Skipped: len(ready), Reason: "paused"}
		case maxPolecats <= 0:
			dispatchPlan = capacity.DispatchPlan{Skipped: len(ready), Reason: "direct-mode"}
		}
	}

	return &schedulerDispatchPlan{
		State:       state,
		MaxPolecats: maxPolecats,
		BatchSize:   batchSize,
		SpawnDelay:  spawnDelay,
		Seats:       seats,
		Scheduled:   contextInfosFromAssessments(assessments),
		Ready:       ready,
		Plan:        dispatchPlan,
	}, nil
}

func (r *runner) printDryRunPlan(dispatchPlan *schedulerDispatchPlan) {
	if dispatchPlan.State.Paused {
		r.printf("Scheduler is paused (by %s); %d ready bead(s) not dispatched\n",
			dispatchPlan.State.PausedBy, len(dispatchPlan.Ready))
		return
	}
	if dispatchPlan.MaxPolecats <= 0 {
		if len(dispatchPlan.Scheduled) == 0 {
			r.printf("No ready beads scheduled for dispatch\n")
			return
		}
		r.printf("Scheduler is in direct dispatch mode (scheduler.max_polecats=%d); %d open context bead(s) will not dispatch\n",
			dispatchPlan.MaxPolecats, len(dispatchPlan.Scheduled))
		return
	}
	r.printDryRunPlanFor(dispatchPlan.Plan, dispatchPlan.Seats, dispatchPlan.BatchSize)
}

func (r *runner) printDispatchNoOp(report capacity.DispatchReport, seats Seats) {
	w := r.out()
	switch report.Reason {
	case "none":
		fmt.Fprintln(w, "No ready beads scheduled for dispatch")
	case "capacity":
		fmt.Fprintf(w, "\n%s No capacity: %d ready bead(s) waiting (working: %d recovery_blocked: %d reservations: %d reusable_idle: %d parked: %d pending_mr: %d)\n",
			style.Dim.Render("○"), report.Skipped, seats.Working, seats.RecoveryBlocked, seats.Reservations, seats.ReusableIdle, seats.Parked, seats.PendingMR)
	default:
		fmt.Fprintf(w, "\n%s No dispatchable beads (reason: %s, skipped: %d)\n",
			style.Dim.Render("○"), report.Reason, report.Skipped)
	}
}

// printDryRunPlanFor displays a dry-run dispatch plan.
func (r *runner) printDryRunPlanFor(plan capacity.DispatchPlan, seats Seats, batchSize int) {
	w := r.out()
	if plan.Reason == "none" {
		fmt.Fprintln(w, "No ready beads scheduled for dispatch")
		return
	}

	capStr := "unlimited"
	if seats.Max > 0 {
		capStr = fmt.Sprintf("%d free of %d (working: %d, recovery_blocked: %d, reservations: %d, reusable_idle: %d, parked: %d, pending_mr: %d)",
			seats.Free, seats.Max, seats.Working, seats.RecoveryBlocked, seats.Reservations, seats.ReusableIdle, seats.Parked, seats.PendingMR)
	}

	totalReady := len(plan.ToDispatch) + plan.Skipped
	if len(plan.ToDispatch) == 0 {
		switch plan.Reason {
		case "capacity":
			fmt.Fprintf(w, "No capacity: %s, %d ready bead(s) waiting\n", capStr, totalReady)
		case "validation":
			fmt.Fprintf(w, "No dispatchable beads: validation failed for %d candidate(s)\n", totalReady)
		default:
			fmt.Fprintf(w, "No dispatchable beads: reason=%s, %d candidate(s) skipped\n", plan.Reason, totalReady)
		}
		return
	}

	fmt.Fprintf(w, "%s Would dispatch %d bead(s) (capacity: %s, batch: %d, ready: %d, reason: %s)\n",
		style.Bold.Render("📋"), len(plan.ToDispatch), capStr, batchSize, totalReady, plan.Reason)
	for _, b := range plan.ToDispatch {
		fmt.Fprintf(w, "  Would dispatch: %s → %s\n", b.WorkBeadID, b.TargetRig)
	}
}

// beadsForContext returns a Beads instance that can operate on a sling context
// bead. Sling contexts live in the target rig's beads dir (GH#3468), so we
// resolve the dir from the context's TargetRig field. Falls back to HQ if
// the target rig is unknown (e.g., invalid context with nil fields).
func beadsForContext(townRoot string, fields *capacity.SlingContextFields) beads.Client {
	if fields != nil && fields.TargetRig != "" {
		rigBeadsDir := doltserver.FindRigBeadsDir(townRoot, fields.TargetRig)
		if rigBeadsDir != "" {
			return beads.NewWithBeadsDir(townRoot, rigBeadsDir)
		}
	}
	// Fallback to HQ for contexts without a valid TargetRig
	return beads.NewWithBeadsDir(townRoot, filepath.Join(townRoot, ".beads"))
}

func beadsForPendingContext(townRoot string, b capacity.PendingBead) beads.Client {
	if b.ContextBeadsDir != "" {
		workDir := b.ContextWorkDir
		if workDir == "" {
			workDir = filepath.Dir(b.ContextBeadsDir)
		}
		return beads.NewWithBeadsDir(workDir, b.ContextBeadsDir)
	}
	return beadsForContext(townRoot, b.Context)
}
