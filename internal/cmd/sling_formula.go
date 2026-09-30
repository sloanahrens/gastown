package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
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

func formulaSlingPrompt(formulaName string) string {
	if slingArgs != "" {
		return fmt.Sprintf("Formula %s slung. Args: %s. Run `"+cli.Name()+" hook` to see your hook, then execute using these args.", formulaName, slingArgs)
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
// Requests stale-read compatibility for consistency with verifyBeadExists.
func verifyFormulaExists(formulaName, workDir, townRoot string) error {
	if workDir == "" {
		workDir = townRoot
	}
	// Try bd formula show (handles all formula file formats)
	// Use Output() instead of Run() to detect bd exit 0 bug:
	// when formula not found, bd may exit 0 but produce empty stdout
	// (an envelope with null data under machine mode).
	// Stderr discarded — first attempt may fail expectedly (retry with mol- prefix).
	if out, err := BdCmd("formula", "show", formulaName).
		AllowStale().
		Dir(workDir).
		WithGTRoot(townRoot).
		Stderr(io.Discard).Output(); err == nil && formulaShowHasBody(out) {
		return nil
	}

	// Try with mol- prefix
	if out, err := BdCmd("formula", "show", "mol-"+formulaName).
		AllowStale().
		Dir(workDir).
		WithGTRoot(townRoot).
		Stderr(io.Discard).Output(); err == nil && formulaShowHasBody(out) {
		return nil
	}
	if _, err := formula.GetEmbeddedFormulaContent(formulaName); err == nil {
		return nil
	}
	if _, err := formula.GetEmbeddedFormulaContent("mol-" + formulaName); err == nil {
		return nil
	}

	return fmt.Errorf("formula '%s' not found (check 'bd formula list')", formulaName)
}

// runSlingFormula handles standalone formula slinging.
// Flow: cook → wisp → attach to hook → nudge
func runSlingFormula(ctx context.Context, args []string) (err error) {
	formulaName := args[0]

	// Get town root early - needed for BEADS_DIR when running bd commands
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")

	// Resolve target using shared dispatch logic
	var target string
	if len(args) > 1 {
		target = args[1]
	}
	var admission *polecatAdmissionHandle
	if !slingDryRun && target != "" {
		admissionRig := ""
		if rigName, isRig := IsRigName(target); isRig {
			admissionRig = rigName
		}
		if admissionRig != "" {
			admission, _, err = acquirePolecatAdmissionFn(townRoot, admissionRig, formulaName, "formula")
			if err != nil {
				return err
			}
			defer admission.Release()
		}
	}
	resolved, err := resolveTarget(target, ResolveTargetOptions{
		DryRun:               slingDryRun,
		Force:                slingForce,
		Create:               slingCreate,
		Account:              slingAccount,
		Agent:                slingAgent,
		NoBoot:               slingNoBoot,
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

	fmt.Printf("%s Slinging formula %s to %s...\n", style.Bold.Render("🎯"), formulaName, targetAgent)

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
			if err := burnSlingWispFn(wispRootID, rollbackWorkDir); err != nil {
				fmt.Printf("  %s Could not burn wisp %s from the failed sling: %v\n", style.Dim.Render("Warning:"), wispRootID, err)
			} else {
				fmt.Printf("  %s Burned wisp %s from the failed sling\n", style.Dim.Render("○"), wispRootID)
			}
		}
		if resolved.NewPolecatInfo != nil {
			fmt.Printf("%s Rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), resolved.NewPolecatInfo.PolecatName)
			rollbackSlingArtifactsFn(resolved.NewPolecatInfo, rollbackBeadID, rollbackWorkDir, "")
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

	if slingDryRun {
		existing, err := findHookedFormulaSingletonFn(formulaWorkDir, targetAgent, formulaName)
		if err != nil {
			return fmt.Errorf("checking existing hooked formulas for %s: %w", targetAgent, err)
		}
		if existing != nil && !slingForce {
			fmt.Printf("Would reuse existing formula %s on %s via %s\n", formulaName, targetAgent, existing.ID)
			return nil
		}

		fmt.Printf("Would cook formula: %s\n", formulaName)
		fmt.Printf("Would create wisp and pin to: %s\n", targetAgent)
		for _, v := range slingVars {
			fmt.Printf("  --var %s\n", v)
		}
		fmt.Printf("Would nudge pane: %s\n", targetPane)
		return nil
	}

	// Serialize standalone formula slings per assignee so same-formula retries
	// and handoffs cannot create duplicate hooked wisps for one target.
	assigneeUnlock, assigneeLockErr := tryAcquireSlingAssigneeLock(townRoot, targetAgent)
	if assigneeLockErr != nil {
		return fmt.Errorf("serializing formula sling for %s: %w", targetAgent, assigneeLockErr)
	}
	defer assigneeUnlock()
	mode := ""
	if slingRalph {
		mode = "ralph"
	}

	existing, err := findHookedFormulaSingletonFn(formulaWorkDir, targetAgent, formulaName)
	if err != nil {
		return fmt.Errorf("checking existing hooked formulas for %s: %w", targetAgent, err)
	}
	// A polecat this sling just spawned or reused has no running session, so
	// a formula wisp still hooked to its identity is left over from an earlier
	// holder of the slot. Reporting "already hooked, no-op" would leave that
	// wisp hooked to a polecat nobody starts; burn it and dispatch fresh.
	if existing != nil && resolved.NewPolecatInfo != nil {
		fmt.Printf("  %s Burning stale formula wisp %s left hooked to %s\n",
			style.Warning.Render("⚠"), existing.ID, targetAgent)
		if err := burnSlingWispFn(existing.ID, formulaWorkDir); err != nil {
			return fmt.Errorf("burning stale formula wisp %s on %s: %w", existing.ID, targetAgent, err)
		}
		existing = nil
	}
	if shouldReuseExistingFormula(existing, slingForce) {
		existingMode := ""
		if fields := beads.ParseAttachmentFields(existing); fields != nil {
			existingMode = fields.Mode
		}
		if existingMode != mode {
			if err := storeRawSlingMetadataFn(townRoot, existing.ID, beadFieldUpdates{Mode: &mode}); err != nil {
				return fmt.Errorf("updating existing formula mode: %w", err)
			}
			if mode != "" || existingMode != "" {
				updateAgentMode(targetAgent, mode, "", townBeadsDir)
			}
		}
		fmt.Printf("%s Formula %s already hooked to %s via %s, no-op\n",
			style.Dim.Render("○"), formulaName, targetAgent, existing.ID)
		return nil
	}
	if admission == nil && strings.Contains(targetAgent, "/polecats/") {
		parts := strings.Split(targetAgent, "/")
		if len(parts) >= 3 {
			admission, _, err = acquirePolecatAdmissionFn(townRoot, parts[0], formulaName, "formula")
			if err != nil {
				return err
			}
			defer admission.Release()
		}
	}

	// Step 1: Cook the formula (ensures proto exists)
	fmt.Printf("  Cooking formula...\n")
	if err := BdCmd("cook", formulaName).
		Dir(formulaWorkDir).
		WithGTRoot(townRoot).
		Run(); err != nil {
		telemetry.RecordMolCook(ctx, formulaName, err)
		return fmt.Errorf("cooking formula: %w", err)
	}
	telemetry.RecordMolCook(ctx, formulaName, nil)

	// Step 2: Create wisp instance (ephemeral)
	fmt.Printf("  Creating wisp...\n")
	wispArgs := []string{"mol", "wisp", formulaName}
	for _, v := range slingVars {
		wispArgs = append(wispArgs, "--var", v)
	}
	wispArgs = append(wispArgs, "--json")

	wispOut, err := BdCmd(wispArgs...).
		Dir(formulaWorkDir).
		WithAutoCommit().
		WithGTRoot(townRoot).
		Output()
	if err != nil {
		return fmt.Errorf("creating wisp: %w", err)
	}

	// Parse wisp output to get the root ID
	wispRootID, err = parseWispIDFromJSON(wispOut)
	if err != nil {
		telemetry.RecordMolWisp(ctx, formulaName, "", "", err)
		return fmt.Errorf("parsing wisp output: %w", err)
	}
	telemetry.RecordMolWisp(ctx, formulaName, wispRootID, "", nil)

	fmt.Printf("%s Wisp created: %s\n", style.Bold.Render("✓"), wispRootID)

	// Step 3: Hook the wisp bead with retry and verification.
	// See: https://github.com/steveyegge/gastown/issues/148.
	hookDir := beads.ResolveHookDir(townRoot, wispRootID, "")
	rollbackBeadID = wispRootID
	if err := hookBeadWithRetryFn(wispRootID, targetAgent, hookDir); err != nil {
		return err
	}
	fmt.Printf("%s Attached to hook (status=hooked)\n", style.Bold.Render("✓"))

	// Log sling event to activity feed (formula slinging)
	actor := resolveSlingActor()
	payload := events.SlingPayload(wispRootID, targetAgent)
	payload["formula"] = formulaName
	_ = events.LogFeed(events.TypeSling, actor, payload)

	// Update agent bead's hook_bead field (ZFC: agents track their current work)
	// Note: formula slinging uses town root as workDir (no polecat-specific path)
	updateAgentHookBead(targetAgent, wispRootID, "", townBeadsDir)

	// Store all attachment fields in a single read-modify-write cycle.
	// NOTE: For standalone formula sling, the wisp IS the work - do NOT store
	// attached_molecule as a self-reference (the wisp's own ID pointing to itself
	// is meaningless). attached_molecule is only meaningful when a formula-on-bead
	// creates a wisp that's bonded to a separate base bead.
	fieldUpdates := beadFieldUpdates{
		Dispatcher:      actor,
		Args:            slingArgs,
		Vars:            append([]string(nil), slingVars...),
		AttachedFormula: formulaName,
		Mode:            &mode,
		FormulaVars:     strings.Join(slingVars, "\n"),
	}
	if err := storeFieldsInBeadFromTownRoot(townRoot, wispRootID, fieldUpdates); err != nil {
		fmt.Printf("%s Could not store fields in bead: %v\n", style.Dim.Render("Warning:"), err)
	} else if slingArgs != "" {
		fmt.Printf("%s Args stored in bead (durable)\n", style.Bold.Render("✓"))
	}
	if mode != "" {
		updateAgentMode(targetAgent, mode, "", townBeadsDir)
	}

	// Start spawned polecat session now that hook is set.
	// This ensures polecat sees the wisp when gt prime runs on session start.
	if resolved.NewPolecatInfo != nil {
		pane, err := startSpawnedPolecatSessionFn(resolved.NewPolecatInfo)
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
		fmt.Printf("%s Self-sling: work hooked, will process on next turn\n", style.Dim.Render("○"))
		return nil
	}

	// Skip nudge during tests to prevent agent self-interruption
	if os.Getenv("GT_TEST_NO_NUDGE") != "" {
		return nil
	}

	prompt := formulaSlingPrompt(formulaName)

	if targetPane == "" {
		fmt.Printf("%s No pane to nudge (agent will discover work via gt prime)\n", style.Dim.Render("○"))
		return nil
	}

	t := tmux.NewTmux()
	if err := t.NudgePane(targetPane, prompt); err != nil {
		// Graceful fallback for no-tmux mode
		fmt.Printf("%s Could not nudge (no tmux?): %v\n", style.Dim.Render("○"), err)
		fmt.Printf("  Agent will discover work via gt prime / bd show\n")
	} else {
		fmt.Printf("%s Nudged to start\n", style.Bold.Render("▶"))
	}

	return nil
}
