package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// runMoleculeBurn burns (destroys) the current molecule attachment.
func runMoleculeBurn(cmd *cobra.Command, args []string) error {
	return moleculeBurn(realMoleculeLifecycleEnv(), args)
}

// moleculeBurn is gt mol burn in e.
func moleculeBurn(e moleculeLifecycleEnv, args []string) (retErr error) {
	cwd, err := e.getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}

	// Find town root
	townRoot, err := e.findTown()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return fmt.Errorf("not in a Gas Town workspace")
	}

	// Determine target agent
	var target string
	if len(args) > 0 {
		target = args[0]
	} else {
		// Auto-detect using env-aware role detection
		roleInfo, err := getRoleWithContextEnv(cwd, townRoot, e.getenv)
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
		target = buildAgentIdentity(roleCtx)
		if target == "" {
			return fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
		}
	}

	// Find beads directory
	workDir, err := e.beadsWorkDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	b := e.storeAt(workDir)

	// Find agent's pinned bead (handoff bead)
	role := extractRoleFromIdentity(target)

	handoff, err := beads.FindHandoffBead(b, role)
	if err != nil {
		return fmt.Errorf("finding handoff bead: %w", err)
	}
	if handoff == nil {
		return fmt.Errorf("no handoff bead found for %s (looked for %q with pinned status)", target, beads.HandoffBeadTitle(role))
	}

	// Check for attached molecule
	attachment := beads.ParseAttachmentFields(handoff)
	if attachment == nil || attachment.AttachedMolecule == "" {
		fmt.Fprintf(e.out, "%s No molecule attached to %s - nothing to burn\n",
			style.Dim.Render("ℹ"), target)
		return nil
	}

	moleculeID := attachment.AttachedMolecule

	// Recursively close all descendant step issues before detaching
	// This prevents orphaned step issues from accumulating (gt-psj76.1)
	childrenClosed, stepsForced, descErr := discardDescendants(b, moleculeID)
	if descErr != nil {
		style.PrintWarning("closing descendants of %s: %v", moleculeID, descErr)
	}
	// Detach the molecule with audit logging (this "burns" it by removing the attachment)
	_, err = b.DetachMoleculeWithAudit(handoff.ID, beads.DetachOptions{
		Operation: "burn",
		Agent:     target,
		Reason:    "molecule burned by agent",
	})
	if err != nil {
		return fmt.Errorf("detaching molecule: %w", err)
	}
	// Close the molecule root after detach so the audit sees original status.
	// Without this, the wisp root stays in "hooked" status indefinitely,
	// causing patrol molecule leaks (issue #1828).
	rootClosed := true
	if closeErr := b.ForceCloseWithReason("burned", moleculeID); closeErr != nil {
		style.PrintWarning("could not close molecule root %s: %v", moleculeID, closeErr)
		rootClosed = false
	}

	if e.json {
		result := map[string]interface{}{
			"burned":          moleculeID,
			"from":            target,
			"handoff_id":      handoff.ID,
			"children_closed": childrenClosed,
			"children_forced": stepsForced,
			"root_closed":     rootClosed,
		}
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Fprintf(e.out, "%s Burned molecule %s from %s\n",
		style.Bold.Render("🔥"), moleculeID, target)
	if childrenClosed > 0 {
		fmt.Fprintf(e.out, "  Closed %d step issues\n", childrenClosed)
	}
	if stepsForced > 0 {
		fmt.Fprintf(e.out, "  Force-closed %d step issues bd refused to close\n", stepsForced)
	}

	return nil
}

// runMoleculeSquash squashes the current molecule into a digest.
func runMoleculeSquash(cmd *cobra.Command, args []string) error {
	return moleculeSquash(cmd, realMoleculeLifecycleEnv(), args)
}

// moleculeSquash is gt mol squash in e.
func moleculeSquash(cmd *cobra.Command, e moleculeLifecycleEnv, args []string) (retErr error) {
	// Parse jitter early so invalid flags fail fast, but defer the sleep
	// until after workspace/attachment validation so no-op invocations
	// (wrong directory, no attached molecule) don't wait unnecessarily.
	var jitterMax time.Duration
	if e.jitter != "" {
		var err error
		jitterMax, err = time.ParseDuration(e.jitter)
		if err != nil {
			return fmt.Errorf("invalid --jitter duration %q: %w", e.jitter, err)
		}
		if jitterMax < 0 {
			return fmt.Errorf("--jitter must be non-negative, got %v", jitterMax)
		}
	}

	cwd, err := e.getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}

	// Find town root
	townRoot, err := e.findTown()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return fmt.Errorf("not in a Gas Town workspace")
	}

	// Determine target agent
	var target string
	if len(args) > 0 {
		target = args[0]
	} else {
		// Auto-detect using env-aware role detection
		roleInfo, err := getRoleWithContextEnv(cwd, townRoot, e.getenv)
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
		target = buildAgentIdentity(roleCtx)
		if target == "" {
			return fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
		}
	}

	// Find beads directory
	workDir, err := e.beadsWorkDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	b := e.storeAt(workDir)

	// Find agent's pinned bead (handoff bead)
	role := extractRoleFromIdentity(target)

	handoff, err := beads.FindHandoffBead(b, role)
	if err != nil {
		return fmt.Errorf("finding handoff bead: %w", err)
	}
	if handoff == nil {
		return fmt.Errorf("no handoff bead found for %s (looked for %q with pinned status)", target, beads.HandoffBeadTitle(role))
	}

	// Check for attached molecule
	attachment := beads.ParseAttachmentFields(handoff)
	if attachment == nil || attachment.AttachedMolecule == "" {
		fmt.Fprintf(e.out, "%s No molecule attached to %s - nothing to squash\n",
			style.Dim.Render("ℹ"), target)
		return nil
	}

	moleculeID := attachment.AttachedMolecule

	// Apply jitter before acquiring any Dolt locks.
	// Multiple patrol agents (deacon, witness, refinery) squash concurrently at
	// cycle end, causing exclusive-lock contention. A random pre-sleep
	// desynchronizes them without changing semantics.
	if jitterMax > 0 {
		//nolint:gosec // weak RNG is fine for jitter
		sleep := time.Duration(rand.Int63n(int64(jitterMax)))
		fmt.Fprintf(e.errOut, "jitter: sleeping %v before squash\n", sleep)
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(sleep):
		}
	}

	// Recursively close all descendant step issues before squashing
	// This prevents orphaned step issues from accumulating (gt-psj76.1)
	childrenClosed, stepsForced, descErr := discardDescendants(b, moleculeID)
	if descErr != nil {
		style.PrintWarning("closing descendants of %s: %v", moleculeID, descErr)
	}

	// Skip digest creation if --no-digest flag is set (gt-t2bjt).
	// Patrol molecules (deacon, witness, refinery) run frequently and their
	// digests pollute the database with thousands of low-value beads.
	if !e.noDigest {
		// Get progress info for the digest
		progress, _ := getMoleculeProgressInfo(b, moleculeID)

		// Create a digest issue
		digestTitle := fmt.Sprintf("Digest: %s", moleculeID)
		digestDesc := fmt.Sprintf(`Squashed molecule execution.

molecule: %s
agent: %s
squashed_at: %s
`, moleculeID, target, time.Now().UTC().Format(time.RFC3339))

		if e.summary != "" {
			digestDesc += fmt.Sprintf("\n## Summary\n%s\n", e.summary)
		}

		if progress != nil {
			digestDesc += fmt.Sprintf(`
## Execution Summary
- Steps: %d/%d completed
- Status: %s
`, progress.DoneSteps, progress.TotalSteps, func() string {
				if progress.Complete {
					return "complete"
				}
				return "partial"
			}())
		}

		// Create the digest bead (ephemeral to avoid git pollution)
		// Per-cycle digests are aggregated daily by 'gt patrol digest'
		digestIssue, err := b.Create(beads.CreateOptions{
			Title:       digestTitle,
			Description: digestDesc,
			Labels:      []string{"gt:task"},
			Priority:    4, // P4 - backlog priority for digests
			Actor:       target,
			Ephemeral:   true, // Don't export to JSONL - daily aggregation handles permanent record
		})
		if err != nil {
			return fmt.Errorf("creating digest: %w", err)
		}

		// Add the digest label (non-fatal: digest works without label)
		_ = b.Update(digestIssue.ID, beads.UpdateOptions{
			AddLabels: []string{"digest"},
		})

		// Close the digest immediately
		closedStatus := "closed"
		err = b.Update(digestIssue.ID, beads.UpdateOptions{
			Status: &closedStatus,
		})
		if err != nil {
			style.PrintWarning("Created digest but couldn't close it: %v", err)
		}
	}

	// Detach the molecule from the handoff bead with audit logging
	detachReason := "molecule squashed (no digest)"
	if !e.noDigest {
		detachReason = "molecule squashed"
	}
	_, err = b.DetachMoleculeWithAudit(handoff.ID, beads.DetachOptions{
		Operation: "squash",
		Agent:     target,
		Reason:    detachReason,
	})
	if err != nil {
		return fmt.Errorf("detaching molecule: %w", err)
	}

	// Close the molecule root after detach so the audit sees original status.
	// Without this, the wisp root stays in "hooked" status indefinitely,
	// causing patrol molecule leaks (issue #1828).
	rootClosed := true
	if closeErr := b.ForceCloseWithReason("squashed", moleculeID); closeErr != nil {
		style.PrintWarning("could not close molecule root %s: %v", moleculeID, closeErr)
		rootClosed = false
	}

	if e.json {
		result := map[string]interface{}{
			"squashed":        moleculeID,
			"from":            target,
			"handoff_id":      handoff.ID,
			"children_closed": childrenClosed,
			"children_forced": stepsForced,
			"digest_skipped":  e.noDigest,
			"root_closed":     rootClosed,
		}
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if e.noDigest {
		fmt.Fprintf(e.out, "%s Squashed molecule %s (no digest)\n",
			style.Bold.Render("📦"), moleculeID)
	} else {
		fmt.Fprintf(e.out, "%s Squashed molecule %s\n",
			style.Bold.Render("📦"), moleculeID)
	}
	if childrenClosed > 0 {
		fmt.Fprintf(e.out, "  Closed %d step issues\n", childrenClosed)
	}
	if stepsForced > 0 {
		fmt.Fprintf(e.out, "  Force-closed %d step issues bd refused to close\n", stepsForced)
	}

	return nil
}

// closeUntilNoProgress closes ids unforced, retrying the ones bd refused
// while each pass closes at least one. bd closes a batch in argument order
// and refuses an issue whose blocker is still open at its turn, even when
// the blocker is later in the same batch; a molecule's steps are chained by
// blocks dependencies and Children lists them in ID order, so one pass can
// close only part of a chain that would close whole. Only the issues still
// refused when a pass makes no progress are reported, in a
// *beads.PartialCloseError.
func closeUntilNoProgress(b beads.Client, ids []string) error {
	var closed []string
	pending := ids
	for {
		err := b.Close(pending...)
		if err == nil {
			return nil
		}
		var pe *beads.PartialCloseError
		if !errors.As(err, &pe) || len(pe.Closed) == 0 {
			// No progress: bd refused every issue left (a single refused
			// issue, or a batch refused whole), or the close failed.
			if len(closed) == 0 {
				return err
			}
			// The previous pass reported pending as refused — but only a
			// refusal may carry that label. A later pass can also end on a
			// transient failure (the re-read that checks a batch died), and
			// closeStepsThenRoot reads errors.Is(err, beads.ErrCloseRefused)
			// as "leave the root open"; mislabeling that error strands a
			// molecule whose steps all closed (gt-22hdp.36).
			cause := err
			if errors.Is(err, beads.ErrCloseRefused) {
				cause = errors.Join(beads.ErrCloseRefused, err)
			}
			return &beads.PartialCloseError{Closed: closed, NotClosed: pending, Err: cause}
		}
		closed = append(closed, pe.Closed...)
		pending = pe.NotClosed
	}
}

// discardDescendants closes parentID's descendants for an explicit discard
// (gt mol burn and squash). It closes them unforced first, then force-closes
// whatever bd refused, so no open step is left under the root the caller
// closes next. It returns the steps closed unforced and the steps forced.
func discardDescendants(b beads.Client, parentID string) (closed, forced int, err error) {
	closed, err = closeDescendantsImpl(b, parentID, false)
	if err == nil {
		return closed, 0, nil
	}
	forced, err = closeDescendantsImpl(b, parentID, true)
	return closed, forced, err
}

// closeStepsThenRoot closes a molecule whose work is claimed complete (gt
// done, the patrol helpers): its descendants, unforced, and then the root
// through closeRoot, but only when no descendant is left open. A step bd
// refuses stays open and so does the root, so unfinished work stays visible
// (gt-7lx3); the error names each open step and why it is open. A failure
// that leaves no step known to be open (the steps could not be listed) is
// logged and the root still closes, as gt done always has. It returns the
// steps closed.
func closeStepsThenRoot(b beads.Client, molID string, closeRoot func() error) (int, error) {
	steps, err := closeDescendantsImpl(b, molID, false)
	if err != nil {
		open := openDescendants(b, molID)
		switch {
		case len(open) > 0:
			return steps, fmt.Errorf("leaving molecule %s open: %d step(s) still open: %s: %w",
				molID, len(open), strings.Join(open, "; "), err)
		case errors.Is(err, beads.ErrCloseRefused):
			return steps, fmt.Errorf("leaving molecule %s open: %w", molID, err)
		}
		style.PrintWarning("closing descendants of %s: %v", molID, err)
	}
	return steps, closeRoot()
}

// openDescendants describes parentID's descendants that are not closed, each
// as "<id> (<why>)".
func openDescendants(b beads.Client, parentID string) []string {
	children, err := b.Children(parentID)
	if err != nil {
		return nil
	}
	var out []string
	for _, child := range children {
		out = append(out, openDescendants(b, child.ID)...)
		if child.Status != string(beads.StatusClosed) {
			out = append(out, fmt.Sprintf("%s (%s)", child.ID, openStepReason(b, child)))
		}
	}
	return out
}

// openStepReason says why bd would refuse to close is: another assignee, an
// open blocker, or an open child.
func openStepReason(b beads.Client, is *beads.Issue) string {
	if full, err := b.Show(is.ID); err == nil {
		is = full
	}
	var why []string
	if is.Assignee != "" {
		why = append(why, "assigned to "+is.Assignee)
	}
	var blockers []string
	for _, d := range is.Dependencies {
		if d.DependencyType == "blocks" && d.Status != string(beads.StatusClosed) {
			blockers = append(blockers, d.ID)
		}
	}
	if len(blockers) > 0 {
		why = append(why, "blocked by "+strings.Join(blockers, ", "))
	}
	if kids, err := b.Children(is.ID); err == nil {
		n := 0
		for _, k := range kids {
			if k.Status != string(beads.StatusClosed) {
				n++
			}
		}
		if n > 0 {
			why = append(why, fmt.Sprintf("%d open child(ren)", n))
		}
	}
	if len(why) == 0 {
		return "status " + is.Status
	}
	return strings.Join(why, "; ")
}

// closeDescendants recursively closes all descendant issues of a parent.
// Returns the count of issues closed. Logs warnings on errors but doesn't fail.
func closeDescendants(b beads.Client, parentID string) int {
	count, err := closeDescendantsImpl(b, parentID, false)
	if err != nil {
		style.PrintWarning("closing descendants of %s: %v", parentID, err)
	}
	return count
}

// forceCloseDescendants is like closeDescendants but uses force-close,
// which succeeds even for beads in invalid states. Returns the count of
// issues closed and any error encountered. Callers should check the error
// to avoid closing a parent while children survive (gt-7lx3).
func forceCloseDescendants(b beads.Client, parentID string) (int, error) {
	return closeDescendantsImpl(b, parentID, true)
}

// releasePinnedSteps clears the pin and the claim on each pinned issue of
// steps, and reports how many it cleared. Unpinning leaves the step open and
// unassigned, as a bd release does; the caller closes it immediately after.
// A pinned descendant of a molecule whose owner is closing it is a hand-off
// marker, not unfinished work, and bd refuses to close it — a pin is a close
// fence, as an assignee other than the actor is — so the sweep clears both
// rather than strand the molecule and its remaining steps (gt-z5m6j).
func releasePinnedSteps(b beads.Client, steps []*beads.Issue) int {
	open, unassigned := string(beads.StatusOpen), ""
	released := 0
	for _, step := range steps {
		if beads.IssueStatus(step.Status) != beads.IssueStatusPinned {
			continue
		}
		if err := b.Update(step.ID, beads.UpdateOptions{Status: &open, Assignee: &unassigned}); err != nil {
			style.PrintWarning("releasing pinned step %s: %v", step.ID, err)
			continue
		}
		released++
	}
	return released
}

func closeDescendantsImpl(b beads.Client, parentID string, force bool) (int, error) {
	// Uses Children (bd show --children), not List(ListOptions{Parent:
	// parentID}) (bd list --parent): the latter only checks the persistent
	// dependencies table and silently misses ephemeral wisp children,
	// whose parent-child edges live in a separate wisp_dependencies table.
	// That gap is why mol-polecat-work step wisps leaked without bound —
	// gt done never found them here to close. See gt-43t7.
	children, err := b.Children(parentID)
	if err != nil {
		return 0, fmt.Errorf("listing children of %s: %w", parentID, err)
	}

	if len(children) == 0 {
		return 0, nil
	}

	// First, recursively close grandchildren
	totalClosed := 0
	var errs []error
	for _, child := range children {
		closed, childErr := closeDescendantsImpl(b, child.ID, force)
		totalClosed += closed
		if childErr != nil {
			errs = append(errs, childErr)
		}
	}

	// Then close direct children
	var idsToClose []string
	for _, child := range children {
		if child.Status != "closed" {
			idsToClose = append(idsToClose, child.ID)
		}
	}

	if len(idsToClose) > 0 {
		var closeErr error
		if force {
			closeErr = b.ForceCloseWithReason("burned: force-close descendants", idsToClose...)
		} else {
			// A pinned child is a hand-off marker, not unfinished work, and
			// bd refuses to close it: clear the mark before the sweep
			// (gt-z5m6j). Forced closes skip bd's fences, the pin among them.
			if n := releasePinnedSteps(b, children); n > 0 {
				fmt.Fprintf(os.Stderr, "Released %d pinned step(s) under %s\n", n, parentID)
			}
			closeErr = closeUntilNoProgress(b, idsToClose)
		}
		// A batch close can close some children and have bd refuse others
		// (a *beads.PartialCloseError): count only the ones that closed, and
		// report the rest so no caller takes a stranded step for a closed
		// one (gt-7lx3).
		totalClosed += len(beads.ClosedIDs(idsToClose, closeErr))
		if closeErr != nil {
			errs = append(errs, fmt.Errorf("closing children of %s: %w", parentID, closeErr))
		}
	}

	if len(errs) > 0 {
		return totalClosed, errors.Join(errs...)
	}
	return totalClosed, nil
}

// moleculeLifecycleEnv is what gt mol burn and squash read from the process
// and their flags: the cwd, the town, the environment role detection reads,
// the local beads workspace, how bd is reached, and where output goes.
// realMoleculeLifecycleEnv is the running gt's; tests give a town and a
// beadsfake store, so they need no stub on PATH, chdir, Setenv or flag
// globals.
type moleculeLifecycleEnv struct {
	getwd        func() (string, error)
	findTown     func() (string, error)
	getenv       func(string) string
	beadsWorkDir func() (string, error)
	// store opens the bead store at the beads workspace; nil is bd, routing
	// by prefix.
	store       func(workDir string) moleculeStore
	out, errOut io.Writer
	json        bool
	jitter      string
	noDigest    bool
	summary     string
}

// moleculeStore is the store gt mol burn and squash work in: the shared
// Client and the audited molecule detach. *beads.Beads implements it.
type moleculeStore interface {
	beads.Client
	DetachMoleculeWithAudit(id string, opts beads.DetachOptions) (*beads.Issue, error)
}

func (e moleculeLifecycleEnv) storeAt(workDir string) moleculeStore {
	if e.store == nil {
		return beads.NewWithBeadsDir(workDir, "")
	}
	return e.store(workDir)
}

func realMoleculeLifecycleEnv() moleculeLifecycleEnv {
	return moleculeLifecycleEnv{
		getwd:        os.Getwd,
		findTown:     workspace.FindFromCwd,
		getenv:       os.Getenv,
		beadsWorkDir: findLocalBeadsDir,
		out:          os.Stdout,
		errOut:       os.Stderr,
		json:         moleculeJSON,
		jitter:       moleculeJitter,
		noDigest:     moleculeNoDigest,
		summary:      moleculeSummary,
	}
}
