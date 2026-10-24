package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// moleculeStepDoneCmd is the "gt mol step done" command.
var moleculeStepDoneCmd = &cobra.Command{
	Use:   "done <step-id>",
	Short: "Complete step and auto-continue to next",
	Long: `Complete a molecule step and automatically continue to the next ready step.

This command handles the step-to-step transition for polecats:

1. Closes the completed step (gt bead close <step-id>)
2. Extracts the molecule ID from the step
3. Finds the next ready step (dependency-aware)
4. If next step exists:
   - Assigns the next step to this session
   - Says so, since the step is worked here
5. If molecule complete:
   - Clears the hook
   - Runs gt done --status DEFERRED
   - Exits the session

IMPORTANT: This is the canonical way to complete molecule steps. Do NOT manually
close steps with 'gt bead close' or 'bd close' - they skip the auto-continuation logic.

Example:
  gt mol step done gt-abc.1    # Complete step 1 of molecule gt-abc`,
	Args: cobra.ExactArgs(1),
	RunE: runMoleculeStepDone,
}

var (
	moleculeStepDryRun bool
)

func init() {
	moleculeStepDoneCmd.Flags().BoolVarP(&moleculeStepDryRun, "dry-run", "n", false, "Show what would be done without executing")
	moleculeStepDoneCmd.Flags().BoolVar(&moleculeJSON, "json", false, "Output as JSON")
}

// StepDoneResult is the result of a step done operation.
type StepDoneResult struct {
	StepID        string   `json:"step_id"`
	MoleculeID    string   `json:"molecule_id"`
	StepClosed    bool     `json:"step_closed"`
	NextStepID    string   `json:"next_step_id,omitempty"`
	NextStepTitle string   `json:"next_step_title,omitempty"`
	ParallelSteps []string `json:"parallel_steps,omitempty"` // Multiple ready steps for fan-out
	Complete      bool     `json:"complete"`
	Action        string   `json:"action"` // "continue", "parallel", "done", "no_more_ready"
}

func runMoleculeStepDone(cmd *cobra.Command, args []string) error {
	stepID := args[0]

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}

	// Find town root
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return fmt.Errorf("not in a Gas Town workspace")
	}

	// Find beads directory
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	b := beads.New(workDir)

	// Step 1: Verify the step exists
	step, err := b.Show(stepID)
	if err != nil {
		return fmt.Errorf("step not found: %w", err)
	}

	// Step 2: Extract molecule ID from step ID (gt-xxx.1 -> gt-xxx)
	// Also handle wisp format (go-wisp-xxx) by using the step's Parent field
	moleculeID := extractMoleculeIDFromStep(stepID)
	if moleculeID == "" {
		// Fallback: use the step's Parent field (for wisps)
		if step.Parent != "" {
			moleculeID = step.Parent
		} else {
			return fmt.Errorf("cannot extract molecule ID from step %s (expected format: prefix.N or wisp with parent)", stepID)
		}
	}

	result, readySteps, err := completeStep(os.Stdout, os.Stderr, b, step, moleculeID, moleculeStepDryRun, moleculeJSON)
	if err != nil {
		return err
	}

	// --json reports what happened and stops: the next-step assignment and the
	// gt done run below act on the session, which a caller reading JSON did not
	// ask for.
	if moleculeJSON {
		return nil
	}

	// Step 5: Handle next action
	switch result.Action {
	case "continue":
		return handleStepContinue(cwd, townRoot, readySteps[0], moleculeStepDryRun)

	case "parallel":
		return handleParallelSteps(cwd, townRoot, workDir, readySteps, moleculeStepDryRun)

	case "done":
		return handleMoleculeComplete(cwd, townRoot, moleculeID, moleculeStepDryRun)

	case "no_more_ready":
		fmt.Printf("\n%s All remaining steps are blocked - waiting on dependencies\n",
			style.Dim.Render("ℹ"))
		fmt.Printf("Run 'gt mol progress %s' to see blocked steps\n", moleculeID)
		return nil
	}

	return nil
}

// completeStep closes step (or says it would, on a dry run), then reads what
// comes next. The human lines go to out, or to errOut under asJSON so stdout
// holds only the JSON object, which asJSON also writes to out.
func completeStep(out, errOut io.Writer, b beads.Client, step *beads.Issue, moleculeID string, dryRun, asJSON bool) (StepDoneResult, []*beads.Issue, error) {
	say := out
	if asJSON {
		say = errOut
	}

	result := StepDoneResult{
		StepID:     step.ID,
		MoleculeID: moleculeID,
	}

	// Step 3: Close the step
	if dryRun {
		if isPinnedStep(step) {
			fmt.Fprintf(say, "[dry-run] Would release pinned step: %s\n", step.ID)
		}
		fmt.Fprintf(say, "[dry-run] Would close step: %s\n", step.ID)
		result.StepClosed = true
	} else {
		if err := closeStep(b, step); err != nil {
			return result, nil, fmt.Errorf("closing step: %w", err)
		}
		result.StepClosed = true
		fmt.Fprintf(say, "%s Closed step %s: %s\n", style.Bold.Render("✓"), step.ID, step.Title)
	}

	// Step 4: Find all ready steps (supports fan-out pattern)
	readySteps, allComplete, err := findAllReadySteps(b, moleculeID)
	if err != nil {
		return result, nil, fmt.Errorf("finding next steps: %w", err)
	}

	if allComplete {
		result.Complete = true
		result.Action = "done"
	} else if len(readySteps) > 1 {
		// Multiple ready steps - fan-out pattern
		result.Action = "parallel"
		result.ParallelSteps = make([]string, len(readySteps))
		for i, s := range readySteps {
			result.ParallelSteps[i] = s.ID
		}
	} else if len(readySteps) == 1 {
		result.NextStepID = readySteps[0].ID
		result.NextStepTitle = readySteps[0].Title
		result.Action = "continue"
	} else {
		// There are more steps but none are ready (blocked on dependencies)
		result.Action = "no_more_ready"
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return result, readySteps, fmt.Errorf("writing step result: %w", err)
		}
	}
	return result, readySteps, nil
}

// isPinnedStep reports whether a step still carries the pin a hand-off left on it.
func isPinnedStep(step *beads.Issue) bool {
	return beads.IssueStatus(step.Status) == beads.IssueStatusPinned
}

// stepWriter is the beads write surface completing a step needs.
type stepWriter interface {
	Update(id string, opts beads.UpdateOptions) error
	Close(ids ...string) error
}

// closeStep closes a completed step, releasing a hand-off pin first: bd refuses
// to close a pinned issue, so the session the step was handed to could not
// complete it without claiming the step by hand (gt-tiv2x).
func closeStep(w stepWriter, step *beads.Issue) error {
	if isPinnedStep(step) {
		inProgress := string(beads.StatusInProgress)
		if err := w.Update(step.ID, beads.UpdateOptions{Status: &inProgress}); err != nil {
			return fmt.Errorf("releasing pinned step: %w", err)
		}
	}
	return w.Close(step.ID)
}

// extractMoleculeIDFromStep extracts the molecule ID from a step ID.
// Step IDs have format: mol-id.N where N is the step number.
// Examples:
//
//	gt-abc.1 -> gt-abc
//	gt-xyz.3 -> gt-xyz
//	bd-mol-abc.2 -> bd-mol-abc
func extractMoleculeIDFromStep(stepID string) string {
	// Find the last dot
	lastDot := strings.LastIndex(stepID, ".")
	if lastDot == -1 {
		return "" // No dot - not a step ID format
	}

	// Check if what's after the dot is a number (step suffix)
	suffix := stepID[lastDot+1:]
	if len(suffix) == 0 {
		return "" // Trailing dot - no suffix
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return "" // Not a numeric suffix
		}
	}

	return stepID[:lastDot]
}

// findAllReadySteps finds all ready steps in a molecule.
// Returns (readySteps, allComplete, error).
// If all steps are complete, returns (nil, true, nil).
// If no steps are ready but some are blocked/in_progress, returns (nil, false, nil).
//
// Delegates readiness calculation to beads' canonical bd ready --mol command,
// which uses the materialized blocked_issues_cache for correct handling of all
// blocking types (blocks, conditional-blocks, waits-for), transitive propagation,
// and cross-rig dependencies. See #1420.
func findAllReadySteps(b beads.Client, moleculeID string) ([]*beads.Issue, bool, error) {
	// Check completion: list all children to detect "all closed" state.
	// bd ready --mol only returns open+unblocked steps, so it can't distinguish
	// "all complete" from "all blocked".
	children, err := b.List(beads.ListOptions{
		Parent:   moleculeID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing molecule steps: %w", err)
	}

	if len(children) == 0 {
		return nil, true, nil // No steps = complete
	}

	// Check if all steps are closed
	allClosed := true
	for _, child := range children {
		if child.Status != "closed" {
			allClosed = false
			break
		}
	}
	if allClosed {
		return nil, true, nil
	}

	// Beads' blocked_issues_cache is the ready-work authority here: all
	// blocking types, transitive propagation, conditional-blocks resolution.
	readySteps, err := beads.ReadyForMol(b, moleculeID)
	if err != nil {
		return nil, false, fmt.Errorf("finding ready steps: %w", err)
	}

	// Sort ready steps by sequence number so step 1 comes before step 2, etc.
	sortStepsBySequence(readySteps)

	return readySteps, false, nil
}

// handleStepContinue hands the next step to the session running this command,
// which assigns it and tells that session to work it here (gt-qgq55).
func handleStepContinue(cwd, townRoot string, nextStep *beads.Issue, dryRun bool) error {
	fmt.Printf("\n%s Next step: %s\n", style.Bold.Render("→"), nextStep.ID)
	fmt.Printf("  %s\n", nextStep.Title)

	// Detect agent identity
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return fmt.Errorf("detecting role: %w", err)
	}

	roleCtx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentID := buildAgentIdentity(roleCtx)
	if agentID == "" {
		return fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
	}

	// Get git root for hook files
	gitRoot, err := getGitRoot()
	if err != nil {
		return fmt.Errorf("finding git root: %w", err)
	}

	if dryRun {
		fmt.Printf("\n[dry-run] Would assign next step: %s\n", nextStep.ID)
		fmt.Printf("[dry-run] Would hand the next step to this session\n")
		return nil
	}

	return handNextStep(os.Stdout, beads.NewPlain(gitRoot, nil), nextStep, agentID)
}

// stepAssigner is the beads write surface handing the next step over needs.
type stepAssigner interface {
	Update(id string, opts beads.UpdateOptions) error
}

// handNextStep assigns nextStep to agent, then tells the session reading w
// where that step is worked: here. The command runs inside the pane it would
// have to replace, and killing that pane's processes ends this process before
// it can respawn anything, so the step it announces stays with a session that
// is still running (gt-qgq55).
func handNextStep(w io.Writer, assigner stepAssigner, nextStep *beads.Issue, agentID string) error {
	// in_progress, assigned to this agent, is the status a claim leaves.
	// Pinned is the status for beads that never get closed, and bd refuses to
	// close a pinned issue, so pinning here stranded the successor on the step
	// it was handed (gt-tiv2x).
	assigned := string(beads.StatusInProgress)
	if err := assigner.Update(nextStep.ID, beads.UpdateOptions{Status: &assigned, Assignee: &agentID}); err != nil {
		return fmt.Errorf("assigning next step: %w", err)
	}

	fmt.Fprintf(w, "%s Next step assigned: %s\n", style.Bold.Render("→"), nextStep.ID)
	announceStepContinuation(w, nextStep)
	return nil
}

// announceStepContinuation says where the next step is worked, since a session
// told a successor is coming waits for one instead of working the step
// (gt-qgq55).
func announceStepContinuation(w io.Writer, nextStep *beads.Issue) {
	fmt.Fprintf(w, "\n%s Continue in the same session: %s is yours now.\n",
		style.Bold.Render("→"), nextStep.ID)
	fmt.Fprintf(w, "  This session keeps running - work the step here.\n")
	fmt.Fprintf(w, "  Finish it with 'gt mol step done %s'.\n", nextStep.ID)
}

// handleParallelSteps handles executing multiple steps concurrently (fan-out pattern).
// This function spawns goroutines to execute each step in parallel and waits for all to complete.
func handleParallelSteps(cwd, townRoot, _ string, steps []*beads.Issue, dryRun bool) error {
	fmt.Printf("\n%s Fan-out: %d parallel steps ready\n", style.Bold.Render("⚡"), len(steps))
	for i, step := range steps {
		fmt.Printf("  %d. %s: %s\n", i+1, step.ID, step.Title)
	}

	if dryRun {
		fmt.Printf("\n[dry-run] Would execute %d steps in parallel\n", len(steps))
		return nil
	}

	// For parallel execution, we use goroutines with a WaitGroup
	// Each step is executed by running its commands in sequence
	// For now, we execute them sequentially but mark them all as in_progress first
	// TODO: True parallel execution requires spawning subagents or separate tmux panes

	fmt.Printf("\n%s Executing parallel steps...\n", style.Bold.Render("🔄"))

	// Mark all steps as in_progress
	gitRoot, err := getGitRoot()
	if err != nil {
		return fmt.Errorf("finding git root: %w", err)
	}

	rootBd := beads.NewPlain(gitRoot, nil)
	inProgress := string(beads.StatusInProgress)
	for _, step := range steps {
		if err := rootBd.Update(step.ID, beads.UpdateOptions{Status: &inProgress}); err != nil {
			style.PrintWarning("could not mark step %s as in_progress: %v", step.ID, err)
		}
	}

	// Execute steps concurrently using goroutines
	// Note: This is simplified - each step's "execution" just marks it complete
	// In practice, the agent (witness/deacon) needs to actually do the work described in step.Description
	// For true parallel execution, this would spawn separate tmux panes or Task subagents

	var wg sync.WaitGroup
	errChan := make(chan error, len(steps))

	for _, step := range steps {
		wg.Add(1)
		go func(s *beads.Issue) {
			defer wg.Done()

			// Execute the step by closing it
			// In a real implementation, the agent would process the step description
			// For now, we just mark it as requiring manual execution
			fmt.Printf("  %s Step %s ready for parallel execution\n", style.Dim.Render("→"), s.ID)
		}(step)
	}

	wg.Wait()
	close(errChan)

	// Check for errors
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	fmt.Printf("\n%s All parallel steps marked as in_progress\n", style.Bold.Render("✓"))
	fmt.Printf("%s Execute each step and close with: gt mol step done <step-id>\n", style.Dim.Render("ℹ"))
	fmt.Printf("%s Once all parallel steps are closed, the gather step will become ready\n", style.Dim.Render("ℹ"))

	// For the current agent, pick the first step to continue with
	// Other steps can be picked up by other agents or run manually
	if len(steps) > 0 {
		fmt.Printf("\n%s Continuing with first parallel step: %s\n", style.Bold.Render("→"), steps[0].ID)
		return handleStepContinue(cwd, townRoot, steps[0], dryRun)
	}

	return nil
}

// handleMoleculeComplete handles when a molecule is complete.
func handleMoleculeComplete(cwd, townRoot, moleculeID string, dryRun bool) error {
	fmt.Printf("\n%s Molecule complete!\n", style.Bold.Render("🎉"))

	// Detect agent identity
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return fmt.Errorf("detecting role: %w", err)
	}

	roleCtx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentID := buildAgentIdentity(roleCtx)

	// Get git root for hook files
	gitRoot, err := getGitRoot()
	if err != nil {
		return fmt.Errorf("finding git root: %w", err)
	}

	if dryRun {
		fmt.Printf("[dry-run] Would unpin work for %s\n", agentID)
		fmt.Printf("[dry-run] Would run gt done --status DEFERRED\n")
		return nil
	}

	// Unpin the molecule bead (set status to open, will be closed by gt done or manually)
	workDir, err := findLocalBeadsDir()
	if err == nil {
		unpinWork(os.Stdout, beads.New(workDir), beads.NewPlain(gitRoot, nil), agentID)
	}

	// For polecats, use gt done to signal completion
	if roleCtx.Role == RolePolecat {
		fmt.Printf("%s Running gt done --status DEFERRED...\n", style.Bold.Render("📤"))

		doneCmd := exec.Command("gt", "done", "--status", "DEFERRED")
		doneCmd.Stdout = os.Stdout
		doneCmd.Stderr = os.Stderr
		return doneCmd.Run()
	}

	// For other roles, just print completion message
	fmt.Printf("\nMolecule %s is complete. Ready for next assignment.\n", moleculeID)
	return nil
}

// unpinWork sets agentID's pinned bead back to open. A role with no agent
// identity unpins nothing: an empty assignee filter lists every pinned bead,
// and the first of them can be another agent's handoff bead.
func unpinWork(w io.Writer, lister interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
}, updater stepAssigner, agentID string) {
	if agentID == "" {
		return
	}
	pinnedBeads, err := lister.List(beads.ListOptions{
		Status:   beads.StatusPinned,
		Assignee: agentID,
		Priority: -1,
	})
	if err != nil || len(pinnedBeads) == 0 {
		return
	}
	// Unpin by setting status to open
	open := string(beads.StatusOpen)
	if err := updater.Update(pinnedBeads[0].ID, beads.UpdateOptions{Status: &open}); err != nil {
		style.PrintWarning("could not unpin bead: %v", err)
		return
	}
	fmt.Fprintf(w, "%s Work unpinned\n", style.Bold.Render("✓"))
}

// getGitRoot is defined in prime.go
