package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/schedulerrun"
	"github.com/steveyegge/gastown/internal/style"
)

// The scheduler's queue logic — what is queued, what is ready, and one
// capacity-controlled dispatch cycle — lives in internal/schedulerrun, which
// `gt scheduler run` and the daemon heartbeat both call in process (gt-638go.9).
// What stays here is what that package cannot own: the sling itself
// (executeSling, sling_dispatch.go) and the polecat-capacity probe
// (polecatCapacitySnapshotForTown, polecat_capacity.go).

// schedulerDeps binds the two cmd-owned collaborators plus the operator
// notices for one dispatch run.
func schedulerDeps() schedulerrun.Deps {
	return schedulerrun.Deps{
		Sling:           schedulerSling,
		Seats:           schedulerSeats,
		Escalate:        fireCrossRigEscalation,
		BeadInTargetRig: verifyBeadExistsInTargetRigDatabase,
	}
}

// schedulerSling dispatches one planned bead through executeSling, the same
// call a direct `gt sling` makes.
//
// A refusal that must leave the context queued — capacity admission, or the
// surviving work of a dead holder — crosses the package boundary as a
// schedulerrun.Deferral so the dispatch cycle neither counts it against the
// context's circuit breaker nor closes it.
func schedulerSling(_ context.Context, townRoot string, b capacity.PendingBead) (schedulerrun.SlingOutcome, error) {
	result, err := dispatchSingleBead(b, townRoot, "")
	if err != nil {
		if why, deferred := capacityDispatchDeferral(err); deferred {
			return schedulerrun.SlingOutcome{}, &schedulerrun.Deferral{Why: why, Err: err}
		}
		return schedulerrun.SlingOutcome{}, err
	}
	outcome := schedulerrun.SlingOutcome{}
	if result != nil {
		outcome.PolecatName = result.PolecatName
	}
	return outcome, nil
}

// schedulerSeats reports the polecat-capacity picture the plan is built
// against, in the shape schedulerrun owns. clean sweeps stale seat
// reservations first; a dry run does not, so previewing dispatch never
// changes the town.
func schedulerSeats(townRoot string, clean bool) (schedulerrun.Seats, error) {
	var snapshot polecatCapacitySnapshot
	var err error
	if clean {
		snapshot, err = polecatCapacitySnapshotForTown(townRoot)
	} else {
		snapshot, err = polecatCapacitySnapshotForTownNoCleanup(townRoot)
	}
	if err != nil {
		return schedulerrun.Seats{}, err
	}
	return schedulerrun.Seats{
		Max:             snapshot.Max,
		Working:         snapshot.Working,
		RecoveryBlocked: snapshot.RecoveryBlocked,
		ReusableIdle:    snapshot.ReusableIdle,
		Parked:          snapshot.Parked,
		PendingMR:       snapshot.PendingMR,
		Reservations:    snapshot.Reservations,
		Free:            snapshot.Free,
		ActiveSessions:  snapshot.ActiveSessions,
	}, nil
}

// fireCrossRigEscalation invokes `gt escalate` with a MEDIUM severity. Best
// effort — escalation failure is logged but does not block the dispatch path.
func fireCrossRigEscalation(rig, prefix, beadID string) {
	msg := fmt.Sprintf("cross-rig dispatch refused: rig=%s prefix=%s bead=%s — see gt-el4", rig, prefix, beadID)
	cmd := exec.Command("gt", "escalate", "--severity", "medium", "--reason", "cross-rig-prefix", msg)
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s cross-rig escalation failed: %v\n", style.Warning.Render("⚠"), err)
	}
}

// dispatchSingleBead dispatches one scheduled bead via executeSling.
// A queued bead is a first dispatch: runSling returns into scheduleBead before
// its content duplicate check, so the check runs here (gt-eisp2).
// Context fields are already parsed (from PendingBead.Context).
// Returns the SlingResult (including PolecatName) on success.
func dispatchSingleBead(b capacity.PendingBead, townRoot, _ string) (*SlingResult, error) {
	params, err := schedulerSlingParams(b, townRoot)
	if err != nil {
		return nil, err
	}

	fmt.Printf("  Dispatching %s → %s...\n", b.WorkBeadID, b.TargetRig)
	result, err := executeSling(params)
	if err != nil {
		return nil, fmt.Errorf("sling failed: %w", err)
	}

	return result, nil
}

// schedulerSlingParams rebuilds the sling a scheduled bead was queued with,
// aimed at the target rig's beads database.
func schedulerSlingParams(b capacity.PendingBead, townRoot string) (SlingParams, error) {
	if b.Context == nil {
		return SlingParams{}, fmt.Errorf("missing sling context for %s", b.ID)
	}

	dp := capacity.ReconstructFromContext(b.Context)
	targetBeadsDir := filepath.Join(townRoot, ".beads")
	if dp.RigName != "" {
		resolved, ok := beads.ResolveRepoAliasBeadsDir(townRoot, dp.RigName)
		if !ok {
			return SlingParams{}, fmt.Errorf("cannot resolve target rig %q beads database for %s", dp.RigName, b.WorkBeadID)
		}
		targetBeadsDir = resolved
	}
	return SlingParams{
		BeadID:           dp.BeadID,
		RigName:          dp.RigName,
		FormulaName:      dp.FormulaName,
		Args:             dp.Args,
		Vars:             dp.Vars,
		Merge:            dp.Merge,
		BaseBranch:       dp.BaseBranch,
		ResumeBranch:     dp.ResumeBranch,
		NoMerge:          dp.NoMerge,
		ReviewOnly:       dp.ReviewOnly,
		Account:          dp.Account,
		Agent:            dp.Agent,
		HookRawBead:      dp.HookRawBead,
		Mode:             dp.Mode,
		FormulaFailFatal: true,
		CallerContext:    "scheduler-dispatch",
		NoConvoy:         true,
		NoBoot:           true,
		TownRoot:         townRoot,
		BeadsDir:         targetBeadsDir,
	}, nil
}

// capacityDispatchDeferral reports whether a failed scheduled dispatch is a
// deferral: the context stays queued and no dispatch failure is recorded, so
// it never counts toward the circuit breaker. Capacity admission refusals and
// sling's surviving-work refusal (a dead holder's work survives or cannot be
// verified, gt-vm5g4) are deferrals; everything else is a failure.
//
// A content-duplicate refusal (gt-mcq) is deliberately a failure. It does not
// clear until the overlapping bead closes, so deferring it would leave the
// context queued and silent for the 72h lookback. As a failure it is recorded
// with the refusal text, and after maxDispatchFailures the context closes as
// circuit-broken: the operator's signal to close the overlapping bead or
// re-sling with --force (gt-eisp2). The breaker is per context, so one refused
// bead does not stop the dispatcher.
func capacityDispatchDeferral(err error) (why string, deferred bool) {
	var admissionErr *polecatCapacityAdmissionError
	switch {
	case errors.As(err, &admissionErr):
		return "Capacity full", true
	case errors.Is(err, errReslingRefused):
		return "Surviving work of a dead holder", true
	}
	return "", false
}
