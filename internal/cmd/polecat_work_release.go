package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// polecatWorkReleaser is the bead surface releasePolecatWork needs. The
// production implementation shells out to bd; tests pass a fake.
type polecatWorkReleaser interface {
	// HookState reads the work bead's current status and assignee.
	HookState(beadID string) (status, assignee string, err error)
	// ReleaseBead returns the work bead to open with no assignee, atomically
	// guarded on the bead still being assigned to expectedAssignee. It reports
	// released=false with a nil error when the guard no longer held (another
	// actor re-assigned the bead between the read and the write).
	ReleaseBead(beadID, expectedAssignee string) (released bool, err error)
	// ResetSlot clears the polecat agent bead's hook_bead and marks it idle.
	ResetSlot(agentID string) error
	// Annotate appends a comment to the bead.
	Annotate(beadID, text string) error
}

// releasableHookStatuses are the statuses in which a bead is held by its
// assignee. A bead in any other status (open, closed, blocked, ...) is not
// this polecat's to give back.
var releasableHookStatuses = map[string]bool{
	beads.StatusHooked: true,
	"in_progress":      true,
}

// workReleaseOutcome reports what releasePolecatWork did, for tests and logs.
type workReleaseOutcome struct {
	Released  bool   // the work bead was returned to open
	SkipNote  string // why the bead was left alone, when it was
	SlotReset bool   // the polecat's agent bead was reset to idle
}

// releasePolecatWork is the one "give the work back" step shared by
// `gt polecat nuke` and sling rollback (gt-vm5g4, gt-7evi4).
//
//  1. Compare-and-release: the bead is returned to open only while it is still
//     hooked/in_progress to agentID. The status is read first; the write is
//     guarded atomically by bd (--if-assignee), so a bead re-slung to someone
//     else between the read and the write is left untouched. The guarded write
//     is also the sanctioned claim transfer: bd refuses a plain assignee write
//     on an in_progress bead that someone else holds.
//  2. When resetSlot is set, clear the polecat's hook_bead and mark its slot
//     idle. Callers that are about to remove the sandbox pass false: removal
//     resets the agent bead itself.
//
// Best-effort: failures are printed and reported, never returned, because
// both callers are already on a teardown path that must keep going.
func releasePolecatWork(r polecatWorkReleaser, agentID, beadID string, resetSlot bool) workReleaseOutcome {
	var out workReleaseOutcome
	if beadID != "" {
		out = releaseHeldBead(r, agentID, beadID)
	}
	if resetSlot {
		if err := r.ResetSlot(agentID); err != nil {
			fmt.Printf("  %s Could not reset slot for %s: %v\n", style.Dim.Render("Warning:"), agentID, err)
		} else {
			out.SlotReset = true
		}
	}
	return out
}

// heldBy reports whether beadID is currently held (hooked/in_progress) by
// agentID, with the reason when it is not.
func heldBy(r polecatWorkReleaser, agentID, beadID string) (bool, string) {
	status, assignee, err := r.HookState(beadID)
	switch {
	case err != nil:
		note := fmt.Sprintf("could not read %s: %v", beadID, err)
		fmt.Printf("  %s Left hooked work %s alone: %s\n", style.Dim.Render("Warning:"), beadID, note)
		return false, note
	case assignee != agentID:
		return false, fmt.Sprintf("assigned to %q, not %s", assignee, agentID)
	case !releasableHookStatuses[status]:
		return false, fmt.Sprintf("status %s is not held", status)
	}
	return true, ""
}

func releaseHeldBead(r polecatWorkReleaser, agentID, beadID string) workReleaseOutcome {
	var out workReleaseOutcome
	if held, note := heldBy(r, agentID, beadID); !held {
		out.SkipNote = note
		return out
	}
	released, err := r.ReleaseBead(beadID, agentID)
	switch {
	case err != nil:
		out.SkipNote = fmt.Sprintf("release failed: %v", err)
		fmt.Printf("  %s Could not release hooked work %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
	case !released:
		out.SkipNote = fmt.Sprintf("no longer assigned to %s at write time", agentID)
	default:
		out.Released = true
		fmt.Printf("  %s Released hooked work %s from %s\n", style.Dim.Render("○"), beadID, agentID)
	}
	return out
}

// bdPolecatWorkReleaser is the production polecatWorkReleaser.
type bdPolecatWorkReleaser struct {
	townRoot    string
	hookWorkDir string // fallback bd dir when the bead prefix has no route
}

func (r bdPolecatWorkReleaser) HookState(beadID string) (string, string, error) {
	info, err := getBeadInfoFromTownRoot(r.townRoot, beadID)
	if err != nil {
		return "", "", err
	}
	return info.Status, info.Assignee, nil
}

func (r bdPolecatWorkReleaser) ReleaseBead(beadID, expectedAssignee string) (bool, error) {
	// The same guarded write polecat removal uses (beads.ReleaseIfAssignee).
	return beads.New(beads.ResolveHookDir(r.townRoot, beadID, r.hookWorkDir)).ReleaseIfAssignee(beadID, expectedAssignee)
}

func (r bdPolecatWorkReleaser) ResetSlot(agentID string) error {
	// Same reset a --force reassignment applies to the outgoing polecat
	// (gt-skwt): hook_bead cleared, agent_state idle. Warn-only inside.
	clearReassignedPolecatState(r.townRoot, agentID)
	return nil
}

func (r bdPolecatWorkReleaser) Annotate(beadID, text string) error {
	return beads.New(beads.ResolveHookDir(r.townRoot, beadID, r.hookWorkDir)).AddComment(beadID, text)
}

// newPolecatWorkReleaserFn is a seam for tests.
var newPolecatWorkReleaserFn = func(townRoot, hookWorkDir string) polecatWorkReleaser {
	return bdPolecatWorkReleaser{townRoot: townRoot, hookWorkDir: hookWorkDir}
}
