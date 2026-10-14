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
// could not be slung.
type scheduledBead struct {
	ID          string
	Status      string
	CloseReason string
	CreatedAt   time.Time
}

// scheduledSlingFailureMarker prefixes the close reason the patrol writes on a
// run bead whose `gt sling` failed. It marks the bead as "no run happened", so
// decideScheduledSling ignores it and the next tick retries instead of reading
// it as a completed run (which would suppress dispatch for a whole interval).
const scheduledSlingFailureMarker = "scheduled_slings-sling-failed"

// scheduledSlingFailureReason is what the patrol records on such a bead.
const scheduledSlingFailureReason = scheduledSlingFailureMarker + ": gt sling failed; the patrol retries on the next tick"

// isFailedScheduledRun reports whether a closed run bead records a sling that
// never started. The reason is matched by prefix because bd may normalize or
// truncate close reasons.
func isFailedScheduledRun(b scheduledBead) bool {
	return b.Status == "closed" && strings.HasPrefix(b.CloseReason, scheduledSlingFailureMarker)
}

type scheduledAction int

const (
	scheduledSkipOpen   scheduledAction = iota // a run is in flight or its MR is queued
	scheduledSkipRecent                        // the newest run started less than an interval ago
	scheduledDispatch
)

func (a scheduledAction) String() string {
	switch a {
	case scheduledSkipOpen:
		return "skip-open"
	case scheduledSkipRecent:
		return "skip-recent"
	default:
		return "dispatch"
	}
}

// decideScheduledSling is pure: the beads carrying the entry's label, the
// entry's interval, and now. Any open bead wins over recency so a run whose
// MR is still in the queue is never doubled. A run that failed to sling is
// neither: it never started, so it must not block the retry.
func decideScheduledSling(beads []scheduledBead, interval time.Duration, now time.Time) scheduledAction {
	var newest time.Time
	for _, b := range beads {
		if isFailedScheduledRun(b) {
			continue
		}
		if b.Status != "closed" {
			return scheduledSkipOpen
		}
		if b.CreatedAt.After(newest) {
			newest = b.CreatedAt
		}
	}
	if !newest.IsZero() && now.Sub(newest) < interval {
		return scheduledSkipRecent
	}
	return scheduledDispatch
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
			ID:          is.ID,
			Status:      is.Status,
			CloseReason: is.CloseReason,
			CreatedAt:   beads.ParseIssueTime(is.CreatedAt),
		})
	}
	return out
}

// scheduledSlingRunner is the side-effect boundary: bd list, bd create, gt
// sling, and the close that retires a run whose sling failed.
type scheduledSlingRunner interface {
	listBeads(ctx context.Context, rig, label string) ([]scheduledBead, error)
	createBead(ctx context.Context, rig, title, label, description string, priority int) (string, error)
	sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error
	closeBead(ctx context.Context, rig, beadID, reason string) error
	// runGate runs the entry's gate_command under ctx's deadline and returns
	// its combined output and run error (see scheduledGateAllows).
	runGate(ctx context.Context, e ScheduledSlingEntry) ([]byte, error)
}

const (
	scheduledSlingsTickInterval  = 15 * time.Minute
	scheduledSlingCommandTimeout = 5 * time.Minute
	// scheduledSlingLookupTimeout bounds the bead read after a sling timed out.
	scheduledSlingLookupTimeout = 30 * time.Second
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

func (r *execScheduledSlingRunner) createBead(_ context.Context, rig, title, label, description string, priority int) (string, error) {
	issue, err := r.store(rig).Create(beads.CreateOptions{Title: title, Labels: []string{label}, Priority: priority, Description: description})
	if err != nil {
		return "", err
	}
	return issue.ID, nil
}

func (r *execScheduledSlingRunner) slingArgs(beadID string, e ScheduledSlingEntry) []string {
	args := []string{"sling", beadID, e.Rig, "--formula=" + e.Formula}
	if e.Agent != "" {
		args = append(args, "--agent="+e.Agent)
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

func (r *execScheduledSlingRunner) sling(ctx context.Context, beadID string, e ScheduledSlingEntry) error {
	cmd := exec.CommandContext(ctx, r.gtPath, r.slingArgs(beadID, e)...)
	cmd.Dir = r.townRoot
	cmd.Env = daemonGTEnv(bdMutationRoutingEnv(r.townRoot))
	util.SetProcessGroup(cmd)
	if _, stderr, err := runWith(r.execCmd, cmd); err != nil {
		return fmt.Errorf("gt sling %s: %w: %s", beadID, err, slingErrorLine(string(stderr)))
	}
	return nil
}

// slingErrorLine is the one-line summary of a failed sling's output: the
// first line that is not a step timing line (sling.StepPrefix, internal/sling/
// timer.go), falling back to the first line so a failure is never logged as an
// empty string.
func slingErrorLine(output string) string {
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

func (d *Daemon) scheduledRunner() scheduledSlingRunner {
	if d.scheduledSlingRunner == nil {
		d.scheduledSlingRunner = &execScheduledSlingRunner{townRoot: d.config.TownRoot, bdPath: d.bdPath, gtPath: d.gtPath, execCmd: d.execCmd}
	}
	return d.scheduledSlingRunner
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
			d.logger.Printf("scheduled_slings: Dolt server unavailable: %v", err)
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
// a stalled one that the failure count surfaces.
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
	action := decideScheduledSling(beadsForLabel, scheduledSlingInterval(e), now)
	if action != scheduledDispatch {
		d.logger.Printf("scheduled_slings: %s: %s", e.Name, action)
		return nil
	}
	// A gated or failed gate is not a failed run: no bead exists, the sling
	// failure counter is untouched, and the next tick asks again.
	if !d.scheduledGateAllows(ctx, r, e) {
		return nil
	}
	// The gate ran on its own clock; create and sling start a full budget, so a
	// slow gate does not cut gt sling short.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.scheduledSlingBudget())
	defer cancel()
	title := fmt.Sprintf("%s %s", e.Name, now.UTC().Format("2006-01-02"))
	desc := fmt.Sprintf("Scheduled run of formula %s on rig %s, created by the daemon's scheduled_slings patrol. Label %s is the run's identity; while this bead is open no second run is dispatched.", e.Formula, e.Rig, scheduledSlingLabel(e))
	id, err := r.createBead(ctx, e.Rig, title, scheduledSlingLabel(e), desc, scheduledSlingPriority(e))
	if err != nil {
		return err
	}
	d.logger.Printf("scheduled_slings: %s: created %s, slinging %s to %s (agent %q)", e.Name, id, e.Formula, e.Rig, e.Agent)
	if err := r.sling(ctx, id, e); err != nil {
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
		return err
	}
	return nil
}

// scheduledSlingBudget is the time one entry's create and sling may take.
func (d *Daemon) scheduledSlingBudget() time.Duration {
	if d.scheduledSlingTimeout > 0 {
		return d.scheduledSlingTimeout
	}
	return scheduledSlingCommandTimeout
}
