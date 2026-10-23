package schedulerrun

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/style"
)

// ReasonRigHeld is the plan reason when every candidate's target rig is held
// by its own ESTOP.<rig>.
const ReasonRigHeld = "rig-held"

// ReasonStartupBackoff is the plan reason when every candidate is still
// resting after a failure at session start.
const ReasonStartupBackoff = "startup-backoff"

// validateForDispatch refuses a planned bead before the sling runs.
//
// Cross-rig prefix guard (gt-el4): a bead whose ID prefix does not match the
// target rig's registered prefix must not be dispatched — the polecat would
// land in a rig DB that cannot resolve the bead and hang in prime. The refusal
// is escalated (debounced per rig/prefix) so a stuck context surfaces to the
// operator instead of looping silently.
func (r *runner) validateForDispatch(townRoot string, b capacity.PendingBead, escalate bool) error {
	if b.TargetRig == "" {
		return nil
	}
	rigPath := filepath.Join(townRoot, b.TargetRig)
	rigPrefix := rigBeadsPrefix(townRoot, rigPath, b.TargetRig)
	if capacity.AcceptsPrefix(rigPrefix, b.WorkBeadID) {
		return nil
	}
	gotPrefix := capacity.BeadIDPrefix(b.WorkBeadID)
	r.eprintf("%s dispatch_refused reason=cross_rig_prefix bead=%s target_rig=%s rig_prefix=%s bead_prefix=%s\n",
		style.Warning.Render("⚠"), b.WorkBeadID, b.TargetRig, rigPrefix, gotPrefix)
	if escalate && crossRigEscalations.shouldFire(b.TargetRig, gotPrefix, time.Now()) {
		r.escalateCrossRig(b.TargetRig, gotPrefix, b.WorkBeadID)
	}
	return capacity.ErrCrossRigPrefix
}

// escalateCrossRig reports a cross-rig dispatch refusal to the operator.
// The escalation is best effort: a missing hook keeps the refusal, which is
// the part that stops the bad dispatch.
func (r *runner) escalateCrossRig(rig, prefix, beadID string) {
	if r.deps.Escalate == nil {
		return
	}
	r.deps.Escalate(rig, prefix, beadID)
}

// validateDryRun rewrites a dry-run plan, dropping every candidate a real run
// would refuse: the cross-rig guard, a work bead the town cannot read, and a
// bead missing from its target rig's database. Dropped candidates count as
// skipped, so the preview matches what a real run would do.
func (r *runner) validateDryRun(townRoot string, plan capacity.DispatchPlan) capacity.DispatchPlan {
	plan = r.dropRigHeldBeads(townRoot, plan)
	plan = r.dropStartupBackoffBeads(townRoot, plan)
	if len(plan.ToDispatch) == 0 {
		return plan
	}
	validated := make([]capacity.PendingBead, 0, len(plan.ToDispatch))
	for _, b := range plan.ToDispatch {
		if err := r.validateForDispatch(townRoot, b, false); err != nil {
			r.eprintf("%s dry-run_skip reason=validation bead=%s target_rig=%s: %v\n",
				style.Dim.Render("○"), b.WorkBeadID, b.TargetRig, err)
			plan.Skipped++
			continue
		}
		if _, err := fetchBeadStatusOne(beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), b.WorkBeadID), b.WorkBeadID); err != nil {
			r.eprintf("%s dry-run_skip reason=bead_lookup bead=%s target_rig=%s: %v\n",
				style.Dim.Render("○"), b.WorkBeadID, b.TargetRig, err)
			plan.Skipped++
			continue
		}
		if b.TargetRig != "" && r.deps.BeadInTargetRig != nil {
			if err := r.deps.BeadInTargetRig(b.WorkBeadID, b.TargetRig, townRoot); err != nil {
				r.eprintf("%s dry-run_skip reason=target_db bead=%s target_rig=%s: %v\n",
					style.Dim.Render("○"), b.WorkBeadID, b.TargetRig, err)
				plan.Skipped++
				continue
			}
		}
		validated = append(validated, b)
	}
	plan.ToDispatch = validated
	if len(plan.ToDispatch) == 0 && plan.Reason != "none" {
		plan.Reason = "validation"
	}
	return plan
}

// planDispatch batches the ready beads a run can actually dispatch. The rig
// hold and the startup backoff are applied before PlanDispatch: a held bead at
// the head of the queue must not take the batch slot, or every bead behind it
// starves (gt-duisv).
func (r *runner) planDispatch(townRoot string, freeSeats, batchSize int, ready []capacity.PendingBead) capacity.DispatchPlan {
	dispatchable := r.dropStartupBackoffBeads(townRoot, r.dropRigHeldBeads(townRoot, capacity.DispatchPlan{ToDispatch: ready}))
	if len(dispatchable.ToDispatch) == 0 && dispatchable.Skipped > 0 {
		// Every ready bead is held. PlanDispatch on an empty list would say
		// "none", which tells the operator nothing is queued.
		return dispatchable
	}
	plan := capacity.PlanDispatch(freeSeats, batchSize, dispatchable.ToDispatch)
	plan.Skipped += dispatchable.Skipped
	return plan
}

// dropRigHeldBeads removes from the plan every bead whose target rig is held
// by its own ESTOP.<rig> (dispatch.RigHold), counting each as skipped rather
// than failed: the bead's sling context stays open and queued, so the first
// run after the rig is released dispatches it (gt-ifijm). When the hold leaves
// nothing to dispatch the plan's Reason becomes ReasonRigHeld.
func (r *runner) dropRigHeldBeads(townRoot string, plan capacity.DispatchPlan) capacity.DispatchPlan {
	kept := plan.ToDispatch[:0:0]
	held := 0
	for _, b := range plan.ToDispatch {
		if reason := dispatch.RigHold(townRoot, b.TargetRig); reason != "" {
			r.eprintf("%s Not dispatching %s → %s: %s\n",
				style.Dim.Render("○"), b.WorkBeadID, b.TargetRig, reason)
			plan.Skipped++
			held++
			continue
		}
		kept = append(kept, b)
	}
	plan.ToDispatch = kept
	if held > 0 && len(kept) == 0 {
		plan.Reason = ReasonRigHeld
	}
	return plan
}

// dropStartupBackoffBeads removes from the plan every bead that is still
// resting after a failure at session start (dispatch.StartupBackoff), counting
// each as skipped rather than failed: the bead is open and unassigned, so the
// first run after its window dispatches it. Without the check the scheduler
// re-slings it on every tick, and each attempt fails the same way (gt-811f6).
// A bead with no record, or one whose record cannot be read, is dispatched as
// before, and an operator's own sling never comes through here.
func (r *runner) dropStartupBackoffBeads(townRoot string, plan capacity.DispatchPlan) capacity.DispatchPlan {
	kept := plan.ToDispatch[:0:0]
	resting := 0
	for _, b := range plan.ToDispatch {
		if reason := dispatch.StartupBackoff(townRoot, b.WorkBeadID); reason != "" {
			r.eprintf("%s Not dispatching %s → %s: startup backoff: %s\n",
				style.Dim.Render("○"), b.WorkBeadID, b.TargetRig, reason)
			plan.Skipped++
			resting++
			continue
		}
		kept = append(kept, b)
	}
	plan.ToDispatch = kept
	if resting > 0 && len(kept) == 0 {
		plan.Reason = ReasonStartupBackoff
	}
	return plan
}

// recordFailure increments the dispatch failure counter on the sling context
// bead, closing it as circuit-broken once it has failed MaxDispatchFailures
// times.
func (r *runner) recordFailure(townBeads beads.Client, b capacity.PendingBead, dispatchErr error) {
	if b.Context == nil {
		return
	}

	b.Context.DispatchFailures++
	b.Context.LastFailure = dispatchErr.Error()

	if err := beads.UpdateSlingContextFields(townBeads, b.ID, b.Context); err != nil {
		r.printf("  %s Failed to record dispatch failure for %s: %v\n",
			style.Warning.Render("⚠"), b.ID, err)
	}

	if b.Context.DispatchFailures >= MaxDispatchFailures {
		if err := beads.CloseSlingContext(townBeads, b.ID, "circuit-broken"); err != nil {
			r.printf("  %s Failed to close circuit-broken context %s: %v\n",
				style.Warning.Render("⚠"), b.ID, err)
		}
		r.printf("  %s Context %s (work: %s) failed %d times, circuit-broken\n",
			style.Warning.Render("⚠"), b.ID, b.WorkBeadID, b.Context.DispatchFailures)
	}
}

// rigBeadsPrefix reads a rig's registered beads prefix: the rigs config first,
// then the rig's own config.json. An empty answer means the rig declares no
// prefix, which capacity.AcceptsPrefix reports as a refusal. A config.json
// that does not decode is reported once rather than read as declaring no
// prefix (gt-8xk9k).
func rigBeadsPrefix(townRoot, rigPath, rigName string) string {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	if rigsConfig, err := config.LoadRigsConfig(rigsConfigPath); err == nil {
		if entry, ok := rigsConfig.Rigs[rigName]; ok && entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			return entry.BeadsConfig.Prefix
		}
	}

	rigCfg, cfgErr := rig.LoadRigConfigIfPresent(rigPath)
	if cfgErr != nil {
		rig.WarnRigConfigOnce(rigPath, cfgErr)
	}
	if rigCfg != nil && rigCfg.Beads != nil && rigCfg.Beads.Prefix != "" {
		return rigCfg.Beads.Prefix
	}

	return ""
}
