package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// convoyScheduleOpts holds options for convoy schedule operations.
type convoyScheduleOpts struct {
	Formula     string
	HookRawBead bool
	Force       bool
	DryRun      bool
	NoBoot      bool
}

// convoyCandidate is one tracked bead of a convoy queued for dispatch; both
// manual dispatch paths walk the same list.
type convoyCandidate struct {
	ID      string
	Title   string
	RigName string
}

// convoyDispatchJob is a candidate paired with the agent it must be
// re-dispatched with, resolved from the convoy's sling-time record.
type convoyDispatchJob struct {
	candidate convoyCandidate
	agent     string
	// agentDesc names where the agent choice came from, for the log line. The
	// no-agent fallback is named rather than applied silently: an invisible
	// fallback is what made the original dropped-agent defect hard to see
	// (gt-yg24).
	agentDesc string
}

// planConvoyDispatch resolves the agent the convoy recorded at sling time for
// every candidate, so a dispatch path cannot omit it and let the rig default
// override the routing decision (gt-mxyk).
func planConvoyDispatch(candidates []convoyCandidate, convoyDescription, townRoot string) []convoyDispatchJob {
	jobs := make([]convoyDispatchJob, 0, len(candidates))
	for _, c := range candidates {
		agent, agentDesc := convoyRecordedAgent(convoyDescription, townRoot, c.RigName)
		jobs = append(jobs, convoyDispatchJob{candidate: c, agent: agent, agentDesc: agentDesc})
	}
	return jobs
}

// convoyRecordedAgent resolves the runtime agent a manual convoy dispatch must
// re-dispatch one tracked bead with, plus a description of that choice for
// logging. `gt sling <convoy>` re-dispatches beads that were already slung once,
// so it answers to the same invariant as the automatic feeders: the recorded
// agent is honored, or the choice is left to gt sling — never silently swapped
// for the rig default (gt-mxyk). The decision is convoy.RedispatchAgent's, so
// the manual and automatic paths cannot drift apart.
func convoyRecordedAgent(convoyDescription, townRoot, rig string) (agent, description string) {
	return convoy.RedispatchAgent(convoyDescription, townRoot, rig)
}

// convoyDescriptionByID reads a convoy's description, empty when the convoy
// cannot be read. The sling-time agent is recorded in that description (gt-yg24).
func convoyDescriptionByID(convoyID string) string {
	info, err := bdShow(convoyID)
	if err != nil {
		return ""
	}
	return info.Description
}

// convoyScheduleOptionsFor builds the deferred-dispatch options for one
// candidate. agent is the convoy's recorded agent; empty leaves the choice to
// gt sling's own resolution.
func convoyScheduleOptionsFor(opts convoyScheduleOpts, agent string) ScheduleOptions {
	return ScheduleOptions{
		Formula:     opts.Formula,
		NoConvoy:    true, // Already tracked by this convoy
		Force:       opts.Force,
		HookRawBead: opts.HookRawBead,
		Agent:       agent,
	}
}

// convoySlingParams builds the SlingParams for one candidate's immediate
// dispatch. This is the bead's first dispatch, so the content duplicate check
// runs: two beads of one convoy are the same-vantage-point pair the check exists
// to catch, and --force is the documented override. The agent carries the
// convoy's sling-time record; re-dispatching with the rig default instead would
// override the routing decision (gt-mxyk, gt-skk7).
func convoySlingParams(job convoyDispatchJob, opts convoyScheduleOpts, townRoot string) SlingParams {
	c := job.candidate
	return SlingParams{
		BeadID:        c.ID,
		RigName:       c.RigName,
		FormulaName:   opts.Formula,
		Force:         opts.Force,
		HookRawBead:   opts.HookRawBead,
		NoConvoy:      true, // Already tracked by this convoy
		NoBoot:        opts.NoBoot,
		CallerContext: "convoy-sling",
		TownRoot:      townRoot,
		BeadsDir:      filepath.Join(townRoot, ".beads"),
		Agent:         job.agent,
	}
}

// runConvoyScheduleByID schedules all open tracked issues of a convoy.
func runConvoyScheduleByID(convoyID string, opts convoyScheduleOpts) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	if err := verifyBeadExists(convoyID); err != nil {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	townBeads := filepath.Join(townRoot, ".beads")
	tracked, err := getTrackedIssues(townBeads, convoyID)
	if err != nil {
		return fmt.Errorf("getting tracked issues: %w", err)
	}

	if len(tracked) == 0 {
		fmt.Printf("Convoy %s has no tracked issues.\n", convoyID)
		return nil
	}

	var candidates []convoyCandidate
	skippedClosed := 0
	skippedAssigned := 0
	skippedScheduled := 0
	skippedNoRig := 0

	// Batch-check scheduling status for all tracked issues (single DB query).
	var beadIDs []string
	for _, t := range tracked {
		beadIDs = append(beadIDs, t.ID)
	}
	scheduledSet := areScheduledForTown(townRoot, beadIDs)

	for _, t := range tracked {
		if t.Status == "closed" || t.Status == "tombstone" {
			skippedClosed++
			continue
		}

		if t.Assignee != "" && !opts.Force {
			skippedAssigned++
			continue
		}

		if scheduledSet[t.ID] {
			skippedScheduled++
			continue
		}

		rigName := resolveRigForBead(townRoot, t.ID)
		if rigName == "" {
			skippedNoRig++
			prefix := beads.ExtractPrefix(t.ID)
			fmt.Printf("  %s %s: cannot resolve rig from prefix %q (town-root or unknown)\n",
				style.Dim.Render("○"), t.ID, prefix)
			continue
		}

		candidates = append(candidates, convoyCandidate{ID: t.ID, Title: t.Title, RigName: rigName})
	}

	if len(candidates) == 0 {
		fmt.Printf("No issues to schedule from convoy %s", convoyID)
		if skippedClosed > 0 || skippedAssigned > 0 || skippedScheduled > 0 || skippedNoRig > 0 {
			fmt.Printf(" (%d closed, %d assigned, %d already scheduled, %d no rig)",
				skippedClosed, skippedAssigned, skippedScheduled, skippedNoRig)
		}
		fmt.Println()
		return nil
	}

	formula := opts.Formula

	if opts.DryRun {
		fmt.Printf("%s Would schedule %d issue(s) from convoy %s:\n",
			style.Bold.Render("DRY-RUN"), len(candidates), convoyID)
		if formula != "" {
			fmt.Printf("  Formula: %s\n", formula)
		} else {
			fmt.Printf("  Hook raw beads (no formula)\n")
		}
		for _, c := range candidates {
			fmt.Printf("  Would schedule: %s -> %s (%s)\n", c.ID, c.RigName, c.Title)
		}
		if skippedClosed > 0 || skippedAssigned > 0 || skippedScheduled > 0 || skippedNoRig > 0 {
			fmt.Printf("\nSkipped: %d closed, %d assigned, %d already scheduled, %d no rig\n",
				skippedClosed, skippedAssigned, skippedScheduled, skippedNoRig)
		}
		return nil
	}

	fmt.Printf("%s Scheduling %d issue(s) from convoy %s...\n",
		style.Bold.Render("📋"), len(candidates), convoyID)

	// The convoy's sling-time agent record, read once for every candidate below.
	convoyDescription := convoyDescriptionByID(convoyID)

	successCount := 0
	for _, job := range planConvoyDispatch(candidates, convoyDescription, townRoot) {
		fmt.Printf("  %s %s\n", style.Dim.Render("→"), job.agentDesc)
		err := scheduleBead(job.candidate.ID, job.candidate.RigName, convoyScheduleOptionsFor(opts, job.agent))
		if err != nil {
			fmt.Printf("  %s %s: %v\n", style.Dim.Render("✗"), job.candidate.ID, err)
			continue
		}
		successCount++
	}

	fmt.Printf("\n%s Scheduled %d/%d issue(s) from convoy %s\n",
		style.Bold.Render("📊"), successCount, len(candidates), convoyID)
	if skippedClosed > 0 || skippedAssigned > 0 || skippedScheduled > 0 || skippedNoRig > 0 {
		fmt.Printf("  Skipped: %d closed, %d assigned, %d already scheduled, %d no rig\n",
			skippedClosed, skippedAssigned, skippedScheduled, skippedNoRig)
	}

	if successCount == 0 {
		return fmt.Errorf("all %d schedule attempts failed for convoy %s", len(candidates), convoyID)
	}
	return nil
}

// runConvoySlingByID immediately dispatches all open tracked issues of a convoy.
// Used when max_polecats=-1 (direct dispatch mode). Each tracked issue gets its
// own polecat via executeSling(). Sets NoConvoy=true since issues are already tracked.
func runConvoySlingByID(convoyID string, opts convoyScheduleOpts) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	if err := verifyBeadExists(convoyID); err != nil {
		return fmt.Errorf("convoy '%s' not found", convoyID)
	}

	townBeads := filepath.Join(townRoot, ".beads")
	tracked, err := getTrackedIssues(townBeads, convoyID)
	if err != nil {
		return fmt.Errorf("getting tracked issues: %w", err)
	}

	if len(tracked) == 0 {
		fmt.Printf("Convoy %s has no tracked issues.\n", convoyID)
		return nil
	}

	var candidates []convoyCandidate
	skippedClosed := 0
	skippedAssigned := 0
	skippedNoRig := 0

	for _, t := range tracked {
		if t.Status == "closed" || t.Status == "tombstone" {
			skippedClosed++
			continue
		}
		if t.Assignee != "" && !opts.Force {
			skippedAssigned++
			continue
		}
		rigName := resolveRigForBead(townRoot, t.ID)
		if rigName == "" {
			skippedNoRig++
			prefix := beads.ExtractPrefix(t.ID)
			fmt.Printf("  %s %s: cannot resolve rig from prefix %q (town-root or unknown)\n",
				style.Dim.Render("○"), t.ID, prefix)
			continue
		}
		candidates = append(candidates, convoyCandidate{ID: t.ID, Title: t.Title, RigName: rigName})
	}

	if len(candidates) == 0 {
		fmt.Printf("No issues to dispatch from convoy %s", convoyID)
		if skippedClosed > 0 || skippedAssigned > 0 || skippedNoRig > 0 {
			fmt.Printf(" (%d closed, %d assigned, %d no rig)",
				skippedClosed, skippedAssigned, skippedNoRig)
		}
		fmt.Println()
		return nil
	}

	if opts.DryRun {
		fmt.Printf("%s Would dispatch %d issue(s) from convoy %s:\n",
			style.Bold.Render("DRY-RUN"), len(candidates), convoyID)
		for _, c := range candidates {
			fmt.Printf("  Would dispatch: %s -> %s (%s)\n", c.ID, c.RigName, c.Title)
		}
		if skippedClosed > 0 || skippedAssigned > 0 || skippedNoRig > 0 {
			fmt.Printf("\nSkipped: %d closed, %d assigned, %d no rig\n",
				skippedClosed, skippedAssigned, skippedNoRig)
		}
		return nil
	}

	fmt.Printf("%s Dispatching %d issue(s) from convoy %s...\n",
		style.Bold.Render("▶"), len(candidates), convoyID)

	// The convoy's sling-time agent record, read once for every candidate below.
	convoyDescription := convoyDescriptionByID(convoyID)

	jobs := planConvoyDispatch(candidates, convoyDescription, townRoot)

	var tally feederDispatchTally
	successfulRigs := make(map[string]bool)
	for i, job := range jobs {
		if slingMaxConcurrent > 0 && i >= slingMaxConcurrent {
			fmt.Printf("  %s Reached --max-concurrent spawn batch size (%d), remaining will be scheduled next cycle\n", style.Dim.Render("○"), slingMaxConcurrent)
			break
		}

		c := job.candidate
		fmt.Printf("\n[%d/%d] Dispatching %s → %s...\n", i+1, len(jobs), c.ID, c.RigName)
		fmt.Printf("  %s %s\n", style.Dim.Render("→"), job.agentDesc)
		_, err := executeSling(convoySlingParams(job, opts, townRoot))
		if !tally.record(c.ID, err) {
			continue
		}
		successfulRigs[c.RigName] = true

		// Brief delay between spawns to avoid Dolt contention
		if i < len(jobs)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Wake rig agents for each unique rig that had successful dispatches
	if !opts.NoBoot {
		for rig := range successfulRigs {
			wakeRigAgents(rig)
		}
	}

	fmt.Printf("\n%s Dispatched %d/%d issue(s) from convoy %s\n",
		style.Bold.Render("📊"), tally.success, len(candidates), convoyID)
	if skippedClosed > 0 || skippedAssigned > 0 || skippedNoRig > 0 {
		fmt.Printf("  Skipped: %d closed, %d assigned, %d no rig\n",
			skippedClosed, skippedAssigned, skippedNoRig)
	}

	return tally.result("convoy", convoyID)
}
