package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/style"
)

type wispCreateJSON struct {
	NewEpicID string `json:"new_epic_id"`
	RootID    string `json:"root_id"`
	ResultID  string `json:"result_id"`
}

func parseWispIDFromJSON(jsonOutput []byte) (string, error) {
	var result wispCreateJSON
	if err := json.Unmarshal(jsonOutput, &result); err != nil {
		return "", fmt.Errorf("parsing wisp JSON: %w (output: %s)", err, trimJSONForError(jsonOutput))
	}

	switch {
	case result.NewEpicID != "":
		return result.NewEpicID, nil
	case result.RootID != "":
		return result.RootID, nil
	case result.ResultID != "":
		return result.ResultID, nil
	default:
		return "", fmt.Errorf("wisp JSON missing id field (expected one of new_epic_id, root_id, result_id); output: %s", trimJSONForError(jsonOutput))
	}
}

func trimJSONForError(jsonOutput []byte) string {
	s := strings.TrimSpace(string(jsonOutput))
	const maxLen = 500
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

func closeFormulaWisp(wispRootID, formulaWorkDir, reason string) error {
	if wispRootID == "" {
		return nil
	}
	bd := beads.New(formulaWorkDir)
	if _, err := forceCloseDescendants(bd, wispRootID); err != nil {
		return fmt.Errorf("force-close descendants: %w", err)
	}
	if err := bd.ForceCloseWithReason(reason, wispRootID); err != nil {
		return fmt.Errorf("force-close formula wisp: %w", err)
	}
	return nil
}

// burnSlingWispFn closes a formula wisp a failed or superseded sling owns.
var burnSlingWispFn = func(wispRootID, formulaWorkDir string) error {
	return closeFormulaWisp(wispRootID, formulaWorkDir, "burned: formula sling rolled back")
}

// formulaSlingPrompt is the start prompt for a formula slung with args.
func formulaSlingPrompt(formulaName, args string) string {
	if args != "" {
		return fmt.Sprintf("Formula %s slung. Args: %s. Run `"+cli.Name()+" hook` to see your hook, then execute using these args.", formulaName, args)
	}
	return fmt.Sprintf("Formula %s slung. Run `"+cli.Name()+" hook` to see your hook, then execute the steps.", formulaName)
}

// findHookedFormulaSingleton returns the existing hooked bead for an assignee
// when that bead already carries the same attached_formula metadata.
func findHookedFormulaSingleton(workDir, targetAgent, formulaName string) (*beads.Issue, error) {
	if workDir == "" || targetAgent == "" || formulaName == "" {
		return nil, nil
	}

	b := beads.New(workDir)
	hookedBeads, err := b.List(beads.ListOptions{
		Status:    beads.StatusHooked,
		Assignee:  targetAgent,
		Priority:  -1,
		Ephemeral: true,
		Limit:     0,
	})
	if err != nil {
		return nil, err
	}

	return newestHookedFormula(hookedBeads, formulaName), nil
}

func newestHookedFormula(hookedBeads []*beads.Issue, formulaName string) *beads.Issue {
	var newest *beads.Issue
	var newestAt time.Time
	var newestHasAt bool
	for _, bead := range hookedBeads {
		fields := beads.ParseAttachmentFields(bead)
		if fields == nil || fields.AttachedFormula != formulaName {
			continue
		}
		attachedAt, hasAttachedAt := attachmentTime(fields)
		if newerAttachment(newest == nil, attachedAt, hasAttachedAt, newestAt, newestHasAt) {
			newest = bead
			newestAt = attachedAt
			newestHasAt = hasAttachedAt
		}
	}
	return newest
}

// findHookedFormulaSingletonFn is a seam for tests of other commands.
var findHookedFormulaSingletonFn = findHookedFormulaSingleton

func attachmentTime(fields *beads.AttachmentFields) (time.Time, bool) {
	if fields == nil || fields.AttachedAt == "" {
		return time.Time{}, false
	}
	attachedAt, err := time.Parse(time.RFC3339Nano, fields.AttachedAt)
	if err != nil {
		return time.Time{}, false
	}
	return attachedAt, true
}

func newerAttachment(noCurrent bool, candidate time.Time, candidateOK bool, current time.Time, currentOK bool) bool {
	if noCurrent {
		return true
	}
	if candidateOK != currentOK {
		return candidateOK
	}
	return candidateOK && candidate.After(current)
}

func shouldReuseExistingFormula(existing *beads.Issue, force bool) bool {
	return existing != nil && !force
}

// formulaShowHasBody reports whether `bd formula show` printed a formula: not
// empty and not the JSON null a machine-mode envelope carries for no data.
func formulaShowHasBody(out []byte) bool {
	s := strings.TrimSpace(string(out))
	return s != "" && s != "null"
}

// verifyFormulaExists checks that the formula exists using bd formula show.
// Formulas are TOML files (.formula.toml).
func verifyFormulaExists(formulaName, workDir, townRoot string) error {
	return realFormulaBD().verifyFormula(formulaName, workDir, townRoot)
}

// verifyFormula is verifyFormulaExists with bd reached through f.
func (f formulaBD) verifyFormula(formulaName, workDir, townRoot string) error {
	if workDir == "" {
		workDir = townRoot
	}
	// Try bd formula show (handles all formula file formats), then with
	// the mol- prefix. bd can exit 0 for a formula it did not find, with
	// an empty body, so the body is checked too.
	eng := f.engine(formulaSite{dir: workDir, townRoot: townRoot})
	for _, name := range []string{formulaName, "mol-" + formulaName} {
		if out, err := eng.FormulaShow(name); err == nil && formulaShowHasBody(out) {
			return nil
		}
	}
	if _, err := formula.GetEmbeddedFormulaContent(formulaName); err == nil {
		return nil
	}
	if _, err := formula.GetEmbeddedFormulaContent("mol-" + formulaName); err == nil {
		return nil
	}

	return fmt.Errorf("formula '%s' not found (check 'bd formula list')", formulaName)
}

// runSlingFormula handles standalone formula slinging with the flags the
// cobra command holds, from the cwd's town.
func runSlingFormula(ctx context.Context, args []string) error {
	return newSlingRun(slingOptionsFromFlags()).runFormula(ctx, args)
}

// createFormulaWisp instantiates formulaName as an ephemeral wisp and
// returns bd's JSON answer.
func createFormulaWisp(formulaName, workDir, townRoot string, vars []string) ([]byte, error) {
	return realFormulaBD().engine(formulaSite{dir: workDir, townRoot: townRoot}).Wisp(formulaName, vars)
}

// runFormula handles standalone formula slinging.
// Flow: cook → wisp → attach to hook → nudge
func (r *slingRun) runFormula(ctx context.Context, args []string) (err error) {
	formulaName := args[0]
	out := r.out

	// Get town root early - needed for BEADS_DIR when running bd commands
	if r.townErr != nil {
		return fmt.Errorf("finding town root: %w", r.townErr)
	}
	townRoot := r.townRoot
	townBeadsDir := filepath.Join(townRoot, ".beads")

	// Resolve target using shared dispatch logic
	var target string
	if len(args) > 1 {
		target = args[1]
	}
	var admission *polecatAdmissionHandle
	if !r.opts.dryRun && target != "" {
		admissionRig := ""
		if rigName, isRig := r.isRigName(target); isRig {
			admissionRig = rigName
		}
		if admissionRig != "" {
			admission, _, err = r.admitPolecat(townRoot, admissionRig, formulaName, "formula")
			if err != nil {
				return err
			}
			defer admission.Release()
		}
	}
	resolved, err := r.resolveTarget(target, ResolveTargetOptions{
		DryRun:               r.opts.dryRun,
		Force:                r.opts.force,
		Create:               r.opts.create,
		Account:              r.opts.account,
		Agent:                r.opts.agent,
		NoBoot:               r.opts.noBoot,
		TownRoot:             townRoot,
		SkipPolecatAdmission: admission != nil,
	})
	if err != nil {
		return err
	}
	targetAgent := resolved.Agent
	targetPane := resolved.Pane
	formulaWorkDir := resolved.WorkDir
	isSelfSling := resolved.IsSelfSling

	fmt.Fprintf(out, "%s Slinging formula %s to %s...\n", style.Bold.Render("🎯"), formulaName, targetAgent)

	// Rollback guard (gt-7evi4): once resolveTarget has spawned or reused a
	// polecat, every exit that does not reach the commit point rolls it back
	// exactly once. rollbackBeadID names the wisp only once this sling is about
	// to hook it; earlier failures touch no bead. A wisp this sling created and
	// did not commit is burned, so it cannot stay hooked to a removed or idle
	// polecat.
	slingCommitted := false
	rollbackBeadID := ""
	rollbackWorkDir := formulaWorkDir
	var wispRootID string
	rollbackUnlessCommitted := func() {
		if slingCommitted {
			return
		}
		// Burn first: the rollback below may remove the sandbox the wisp's
		// bd commands run from.
		if wispRootID != "" {
			if err := r.burnWisp(wispRootID, rollbackWorkDir); err != nil {
				fmt.Fprintf(out, "  %s Could not burn wisp %s from the failed sling: %v\n", style.Dim.Render("Warning:"), wispRootID, err)
			} else {
				fmt.Fprintf(out, "  %s Burned wisp %s from the failed sling\n", style.Dim.Render("○"), wispRootID)
			}
		}
		if resolved.NewPolecatInfo != nil {
			fmt.Fprintf(out, "%s Rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), resolved.NewPolecatInfo.PolecatName)
			r.rollbackArtifacts(resolved.NewPolecatInfo, rollbackBeadID, rollbackWorkDir, "")
		}
	}
	defer rollbackUnlessCommitted()

	// Resolve working directory for bd commands (routes to correct rig beads)
	// Fall back to townRoot (HQ beads) if no specific rig directory was determined
	if formulaWorkDir == "" {
		formulaWorkDir = townRoot
	}
	if rollbackWorkDir == "" {
		rollbackWorkDir = formulaWorkDir
	}

	if r.opts.dryRun {
		existing, err := r.findHookedFormula(formulaWorkDir, targetAgent, formulaName)
		if err != nil {
			return fmt.Errorf("checking existing hooked formulas for %s: %w", targetAgent, err)
		}
		if existing != nil && !r.opts.force {
			fmt.Fprintf(out, "Would reuse existing formula %s on %s via %s\n", formulaName, targetAgent, existing.ID)
			return nil
		}

		fmt.Fprintf(out, "Would cook formula: %s\n", formulaName)
		fmt.Fprintf(out, "Would create wisp and pin to: %s\n", targetAgent)
		for _, v := range r.opts.vars {
			fmt.Fprintf(out, "  --var %s\n", v)
		}
		fmt.Fprintf(out, "Would nudge pane: %s\n", targetPane)
		return nil
	}

	// Serialize standalone formula slings per assignee so same-formula retries
	// and handoffs cannot create duplicate hooked wisps for one target.
	assigneeUnlock, assigneeLockErr := r.lockAssignee(townRoot, targetAgent)
	if assigneeLockErr != nil {
		return fmt.Errorf("serializing formula sling for %s: %w", targetAgent, assigneeLockErr)
	}
	defer assigneeUnlock()
	mode := ""
	if r.opts.ralph {
		mode = "ralph"
	}

	existing, err := r.findHookedFormula(formulaWorkDir, targetAgent, formulaName)
	if err != nil {
		return fmt.Errorf("checking existing hooked formulas for %s: %w", targetAgent, err)
	}
	// A polecat this sling just spawned or reused has no running session, so
	// a formula wisp still hooked to its identity is left over from an earlier
	// holder of the slot. Reporting "already hooked, no-op" would leave that
	// wisp hooked to a polecat nobody starts; burn it and dispatch fresh.
	if existing != nil && resolved.NewPolecatInfo != nil {
		fmt.Fprintf(out, "  %s Burning stale formula wisp %s left hooked to %s\n",
			style.Warning.Render("⚠"), existing.ID, targetAgent)
		if err := r.burnWisp(existing.ID, formulaWorkDir); err != nil {
			return fmt.Errorf("burning stale formula wisp %s on %s: %w", existing.ID, targetAgent, err)
		}
		existing = nil
	}
	if shouldReuseExistingFormula(existing, r.opts.force) {
		existingMode := ""
		if fields := beads.ParseAttachmentFields(existing); fields != nil {
			existingMode = fields.Mode
		}
		if existingMode != mode {
			if err := r.storeFields(townRoot, existing.ID, beadFieldUpdates{Mode: &mode}); err != nil {
				return fmt.Errorf("updating existing formula mode: %w", err)
			}
			if mode != "" || existingMode != "" {
				r.updateAgentMode(targetAgent, mode, "", townBeadsDir)
			}
		}
		fmt.Fprintf(out, "%s Formula %s already hooked to %s via %s, no-op\n",
			style.Dim.Render("○"), formulaName, targetAgent, existing.ID)
		return nil
	}
	if admission == nil && strings.Contains(targetAgent, "/polecats/") {
		parts := strings.Split(targetAgent, "/")
		if len(parts) >= 3 {
			admission, _, err = r.admitPolecat(townRoot, parts[0], formulaName, "formula")
			if err != nil {
				return err
			}
			defer admission.Release()
		}
	}

	// Step 1: Cook the formula (ensures proto exists)
	fmt.Fprintf(out, "  Cooking formula...\n")
	if err := r.cookFormula(formulaName, formulaWorkDir, townRoot); err != nil {
		return fmt.Errorf("cooking formula: %w", err)
	}

	// Step 2: Create wisp instance (ephemeral)
	fmt.Fprintf(out, "  Creating wisp...\n")
	wispOut, err := r.createWisp(formulaName, formulaWorkDir, townRoot, r.opts.vars)
	if err != nil {
		return fmt.Errorf("creating wisp: %w", err)
	}

	// Parse wisp output to get the root ID
	wispRootID, err = parseWispIDFromJSON(wispOut)
	if err != nil {
		return fmt.Errorf("parsing wisp output: %w", err)
	}

	fmt.Fprintf(out, "%s Wisp created: %s\n", style.Bold.Render("✓"), wispRootID)

	// Step 3: Hook the wisp bead with retry and verification.
	// See: https://github.com/steveyegge/gastown/issues/148.
	hookDir := r.hookDir(townRoot, wispRootID, "")
	rollbackBeadID = wispRootID
	if err := r.hookWisp(wispRootID, targetAgent, hookDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s Attached to hook (status=hooked)\n", style.Bold.Render("✓"))

	// Log sling event to activity feed (formula slinging)
	actor := r.actor()
	payload := events.SlingPayload(wispRootID, targetAgent)
	payload["formula"] = formulaName
	_ = r.logFeed(events.TypeSling, actor, payload)

	// Update agent bead's hook_bead field (ZFC: agents track their current work)
	// Note: formula slinging uses town root as workDir (no polecat-specific path)
	r.updateAgentHook(targetAgent, wispRootID, "", townBeadsDir)

	// Store all attachment fields in a single read-modify-write cycle.
	// NOTE: For standalone formula sling, the wisp IS the work - do NOT store
	// attached_molecule as a self-reference (the wisp's own ID pointing to itself
	// is meaningless). attached_molecule is only meaningful when a formula-on-bead
	// creates a wisp that's bonded to a separate base bead.
	fieldUpdates := beadFieldUpdates{
		Dispatcher:      actor,
		Args:            r.opts.argsText,
		Vars:            append([]string(nil), r.opts.vars...),
		AttachedFormula: formulaName,
		Mode:            &mode,
		FormulaVars:     strings.Join(r.opts.vars, "\n"),
	}
	if err := r.storeFields(townRoot, wispRootID, fieldUpdates); err != nil {
		fmt.Fprintf(out, "%s Could not store fields in bead: %v\n", style.Dim.Render("Warning:"), err)
	} else if r.opts.argsText != "" {
		fmt.Fprintf(out, "%s Args stored in bead (durable)\n", style.Bold.Render("✓"))
	}
	if mode != "" {
		r.updateAgentMode(targetAgent, mode, "", townBeadsDir)
	}

	// Start spawned polecat session now that hook is set.
	// This ensures polecat sees the wisp when gt prime runs on session start.
	if resolved.NewPolecatInfo != nil {
		pane, err := r.startSession(resolved.NewPolecatInfo)
		if err != nil {
			// The guard rolls back: releases the wisp, cleans up the polecat.
			return fmt.Errorf("starting polecat session: %w", err)
		}
		targetPane = pane
	}

	// Commit point (gt-7evi4): the wisp is hooked and any polecat this sling
	// spawned is running it. Nothing after this is rolled back.
	slingCommitted = true

	// Step 4: Nudge to start (graceful if no tmux)
	// Skip for self-sling - agent is currently processing the sling command and will see
	// the hooked work on next turn. Nudging would inject text while agent is busy.
	if isSelfSling {
		fmt.Fprintf(out, "%s Self-sling: work hooked, will process on next turn\n", style.Dim.Render("○"))
		return nil
	}

	// Skip nudge during tests to prevent agent self-interruption
	if r.getenv("GT_TEST_NO_NUDGE") != "" {
		return nil
	}

	prompt := formulaSlingPrompt(formulaName, r.opts.argsText)

	if targetPane == "" {
		fmt.Fprintf(out, "%s No pane to nudge (agent will discover work via gt prime)\n", style.Dim.Render("○"))
		return nil
	}

	if err := r.nudgePane(targetPane, prompt); err != nil {
		// Graceful fallback for no-tmux mode
		fmt.Fprintf(out, "%s Could not nudge (no tmux?): %v\n", style.Dim.Render("○"), err)
		fmt.Fprintf(out, "  Agent will discover work via gt prime / bd show\n")
	} else {
		fmt.Fprintf(out, "%s Nudged to start\n", style.Bold.Render("▶"))
	}

	return nil
}
