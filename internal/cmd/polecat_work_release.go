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
	// ReleaseBead returns the work bead to open with no assignee.
	ReleaseBead(beadID string) error
	// ResetSlot clears the polecat agent bead's hook_bead and marks it idle.
	ResetSlot(agentID string) error
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
//  1. Compare-and-release: re-read beadID and return it to open ONLY while it is
//     still hooked/in_progress to agentID. A bead that has since been re-slung
//     to someone else, closed, or released is left untouched.
//  2. When resetSlot is set, clear the polecat's hook_bead and mark its slot
//     idle. Callers that are about to remove the sandbox pass false: removal
//     resets the agent bead itself.
//
// Best-effort: failures are printed and reported, never returned, because
// both callers are already on a teardown path that must keep going.
func releasePolecatWork(r polecatWorkReleaser, agentID, beadID string, resetSlot bool) workReleaseOutcome {
	var out workReleaseOutcome
	if beadID != "" {
		status, assignee, err := r.HookState(beadID)
		switch {
		case err != nil:
			out.SkipNote = fmt.Sprintf("could not read %s: %v", beadID, err)
			fmt.Printf("  %s Left hooked work %s alone: %s\n", style.Dim.Render("Warning:"), beadID, out.SkipNote)
		case assignee != agentID:
			out.SkipNote = fmt.Sprintf("assigned to %q, not %s", assignee, agentID)
		case !releasableHookStatuses[status]:
			out.SkipNote = fmt.Sprintf("status %s is not held", status)
		default:
			if err := r.ReleaseBead(beadID); err != nil {
				out.SkipNote = fmt.Sprintf("release failed: %v", err)
				fmt.Printf("  %s Could not release hooked work %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
			} else {
				out.Released = true
				fmt.Printf("  %s Released hooked work %s from %s\n", style.Dim.Render("○"), beadID, agentID)
			}
		}
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

func (r bdPolecatWorkReleaser) ReleaseBead(beadID string) error {
	return BdCmd("update", beadID, "--status=open", "--assignee=").
		Dir(beads.ResolveHookDir(r.townRoot, beadID, r.hookWorkDir)).
		WithAutoCommit().
		Run()
}

func (r bdPolecatWorkReleaser) ResetSlot(agentID string) error {
	// Same reset a --force reassignment applies to the outgoing polecat
	// (gt-skwt): hook_bead cleared, agent_state idle. Warn-only inside.
	clearReassignedPolecatState(r.townRoot, agentID)
	return nil
}

// newPolecatWorkReleaserFn is a seam for tests.
var newPolecatWorkReleaserFn = func(townRoot, hookWorkDir string) polecatWorkReleaser {
	return bdPolecatWorkReleaser{townRoot: townRoot, hookWorkDir: hookWorkDir}
}
