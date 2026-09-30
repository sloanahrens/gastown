package cmd

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// DispatchResult records the outcome of dispatching a single task.
type DispatchResult struct {
	BeadID  string
	Rig     string
	Success bool
	Error   error
}

// dispatchTaskDirect dispatches a single task to its rig.
// In production, this delegates to gt sling. Tests override this variable
// with a stub to avoid spawning real processes.
var dispatchTaskDirect = func(townRoot, beadID, rig string) error {
	cmd := exec.Command("gt", "sling", beadID, rig)
	cmd.Dir = townRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gt sling %s %s: %w\nstderr: %s", beadID, rig, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// transitionConvoyToOpen transitions a staged convoy to open status.
// If the convoy is staged_ready, it transitions unconditionally.
// If the convoy is staged_warnings and force is true, it transitions.
// If the convoy is staged_warnings and force is false, it returns an error.
// If the convoy is already open or closed, it returns an error.
func transitionConvoyToOpen(convoyID string, force bool) error {
	result, err := bdShow(convoyID)
	if err != nil {
		return fmt.Errorf("cannot resolve convoy %s: %w", convoyID, err)
	}

	status := normalizeConvoyStatus(result.Status)

	switch status {
	case convoyStatusStagedReady:
		// Transition directly to open.
		return bdUpdateStatus(convoyID, convoyStatusOpen)

	case convoyStatusStagedWarnings:
		if !force {
			return fmt.Errorf("convoy %s has warnings, use --force to launch", convoyID)
		}
		return bdUpdateStatus(convoyID, convoyStatusOpen)

	case convoyStatusOpen:
		return fmt.Errorf("convoy %s is already launched", convoyID)

	case convoyStatusClosed:
		return fmt.Errorf("convoy %s is closed", convoyID)

	default:
		return fmt.Errorf("convoy %s has unexpected status %q", convoyID, result.Status)
	}
}

// bdUpdateStatus runs `bd update <id> --status=<status>` against the town beads
// database, since convoys live at the HQ level.
func bdUpdateStatus(beadID, status string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}
	cmd := beads.CommandWithEnv(townBeads, nil, "update", beadID, "--status="+status)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bd update %s --status=%s: %w\noutput: %s", beadID, status, err, out)
	}
	return nil
}

// collectBlockedRigsInDAG returns a map of parked/docked rig names to the
// bead IDs that target them. Only considers slingable nodes. (gt-4owfd.1)
func collectBlockedRigsInDAG(dag *ConvoyDAG, townRoot string) map[string][]string {
	blockedRigBeads := make(map[string][]string)
	for _, node := range dag.Nodes {
		if !isSlingableType(node.Type) {
			continue
		}
		if node.Rig == "" {
			continue
		}
		if blocked, _ := IsRigParkedOrDocked(townRoot, node.Rig); blocked {
			blockedRigBeads[node.Rig] = append(blockedRigBeads[node.Rig], node.ID)
		}
	}
	return blockedRigBeads
}

// checkBlockedRigsForLaunch checks if any target rigs are parked or docked.
// Returns an error listing all blocked rigs if any are found and force is false.
// (gt-4owfd.1)
func checkBlockedRigsForLaunch(dag *ConvoyDAG, townRoot string, force bool) error {
	blockedRigBeads := collectBlockedRigsInDAG(dag, townRoot)
	if len(blockedRigBeads) == 0 {
		return nil
	}

	// Build sorted list of blocked rigs for deterministic output
	var rigs []string
	for rig := range blockedRigBeads {
		rigs = append(rigs, rig)
	}
	sort.Strings(rigs)

	if force {
		// Warn but proceed
		fmt.Printf("Warning: %d non-operational rig(s) in convoy: %s\n", len(rigs), strings.Join(rigs, ", "))
		fmt.Printf("  Proceeding with --force (tasks may fail)\n")
		return nil
	}

	// Build detailed error message
	var details []string
	for _, rig := range rigs {
		beadIDs := blockedRigBeads[rig]
		sort.Strings(beadIDs)
		details = append(details, fmt.Sprintf("  %s: %s", rig, strings.Join(beadIDs, ", ")))
	}

	return fmt.Errorf("cannot launch: %d target rig(s) are parked or docked:\n%s\n\nUse 'gt rig unpark' or 'gt rig undock' to restore, or --force to proceed anyway",
		len(rigs), strings.Join(details, "\n"))
}

// dispatchWave1 dispatches all tasks in Wave 1 of the computed waves.
// Individual task failures do not abort remaining dispatches (I-14).
// Returns a result for every Wave 1 task and a non-nil error only if waves
// are empty or contain no Wave 1.
func dispatchWave1(convoyID string, dag *ConvoyDAG, waves []Wave, townRoot string) ([]DispatchResult, error) {
	if len(waves) == 0 {
		return nil, fmt.Errorf("convoy %s: no waves to dispatch", convoyID)
	}

	wave1 := waves[0]
	if wave1.Number != 1 {
		return nil, fmt.Errorf("convoy %s: first wave has unexpected number %d", convoyID, wave1.Number)
	}

	var results []DispatchResult
	for _, taskID := range wave1.Tasks {
		node := dag.Nodes[taskID]
		rig := ""
		if node != nil {
			rig = node.Rig
		}

		err := dispatchTaskDirect(townRoot, taskID, rig)
		results = append(results, DispatchResult{
			BeadID:  taskID,
			Rig:     rig,
			Success: err == nil,
			Error:   err,
		})
	}

	return results, nil
}
