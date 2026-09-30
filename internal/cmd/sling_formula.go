package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/telemetry"
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

func cleanupFailedDogFormulaWisp(wispRootID, formulaWorkDir string) error {
	return closeFormulaWisp(wispRootID, formulaWorkDir, "burned: dog session start failed")
}

func cleanupStaleDogFormulaWisp(wispRootID, formulaWorkDir string) error {
	return closeFormulaWisp(wispRootID, formulaWorkDir, "burned: stale dog formula hook replaced")
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

// cleanupDelayedDogFormulaFailure undoes a delayed dog's part in a failed
// formula sling: it burns the wisp the sling created and clears the dog's
// assignment, and still clears the assignment when the burn fails.
func (d *slingDeps) cleanupDelayedDogFormulaFailure(currentErr error, delayedDogInfo *DogDispatchInfo, wispRootID, formulaWorkDir string) error {
	var cleanupErr error
	if wispRootID != "" {
		if err := d.cleanupFailedDogWisp(wispRootID, formulaWorkDir); err != nil {
			cleanupErr = fmt.Errorf("cleaning failed dog formula wisp %s: %w", wispRootID, err)
		}
	}
	if err := d.clearDogWork(delayedDogInfo); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clearing failed dog assignment: %w", err))
	}
	if cleanupErr == nil {
		return currentErr
	}
	if currentErr == nil {
		return cleanupErr
	}
	return errors.Join(currentErr, cleanupErr)
}

// formulaSlingPrompt is the start prompt for a formula slung with args.
func formulaSlingPrompt(formulaName, args string) string {
	if args != "" {
		return fmt.Sprintf("Formula %s slung. Args: %s. Run `"+cli.Name()+" hook` to see your hook, then execute using these args.", formulaName, args)
	}
	return fmt.Sprintf("Formula %s slung. Run `"+cli.Name()+" hook` to see your hook, then execute the steps.", formulaName)
}

// nudgeFormulaDog sends prompt to the dog's session. A failed nudge is only
// reported: the dog discovers its work via gt prime.
func (d *slingDeps) nudgeFormulaDog(delayedDogInfo *DogDispatchInfo, prompt string) {
	dogSession := fmt.Sprintf("hq-dog-%s", delayedDogInfo.DogName)
	if err := d.nudgeSession(dogSession, prompt); err != nil {
		fmt.Fprintf(d.out, "%s Could not nudge dog %s: %v (will discover work via gt prime)\n",
			style.Dim.Render("○"), delayedDogInfo.DogName, err)
	} else {
		fmt.Fprintf(d.out, "%s Nudged dog %s\n", style.Bold.Render("▶"), delayedDogInfo.DogName)
	}
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

func findHookedFormulaForDogPool(workDir, formulaName string, reusableDog func(*beads.Issue, string) bool) (*beads.Issue, string, error) {
	if workDir == "" || formulaName == "" {
		return nil, "", nil
	}

	b := beads.New(workDir)
	hookedBeads, err := b.List(beads.ListOptions{
		Status:    beads.StatusHooked,
		Priority:  -1,
		Ephemeral: true,
		Limit:     0,
	})
	if err != nil {
		return nil, "", err
	}

	bead, dogName := reusableHookedDogFormula(hookedBeads, formulaName, reusableDog)
	return bead, dogName, nil
}

func reusableHookedDogFormula(hookedBeads []*beads.Issue, formulaName string, reusableDog func(*beads.Issue, string) bool) (*beads.Issue, string) {
	const dogAssigneePrefix = "deacon/dogs/"
	var newest *beads.Issue
	var newestDogName string
	var newestAt time.Time
	var newestHasAt bool
	for _, bead := range hookedBeads {
		if !strings.HasPrefix(bead.Assignee, dogAssigneePrefix) {
			continue
		}
		dogName := strings.TrimPrefix(bead.Assignee, dogAssigneePrefix)
		if dogName == "" || strings.Contains(dogName, "/") {
			continue
		}
		fields := beads.ParseAttachmentFields(bead)
		if fields == nil || fields.AttachedFormula != formulaName {
			continue
		}
		if reusableDog != nil && !reusableDog(bead, dogName) {
			continue
		}
		attachedAt, hasAttachedAt := attachmentTime(fields)
		if newerAttachment(newest == nil, attachedAt, hasAttachedAt, newestAt, newestHasAt) {
			newest = bead
			newestDogName = dogName
			newestAt = attachedAt
			newestHasAt = hasAttachedAt
		}
	}

	return newest, newestDogName
}

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

func shouldReuseExistingFormula(existing *beads.Issue, delayedDogInfo *DogDispatchInfo, force bool) bool {
	if existing == nil || force {
		return false
	}
	if delayedDogInfo == nil {
		return true
	}
	if delayedDogInfo.ownsWork {
		return false
	}
	return delayedDogInfo.worksOnHook(existing)
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

// runSlingFormula handles standalone formula slinging with the flags the
// cobra command holds, from the cwd's town.
func runSlingFormula(ctx context.Context, args []string) error {
	return newSlingRun(slingOptionsFromFlags()).runFormula(ctx, args)
}

// cookStandaloneFormula cooks a formula's proto for a standalone sling.
func cookStandaloneFormula(formulaName, workDir, townRoot string) error {
	return BdCmd("cook", formulaName).
		Dir(workDir).
		WithGTRoot(townRoot).
		Run()
}

// createFormulaWisp instantiates formulaName as an ephemeral wisp and
// returns bd's JSON answer.
func createFormulaWisp(formulaName, workDir, townRoot string, vars []string) ([]byte, error) {
	wispArgs := []string{"mol", "wisp", formulaName}
	for _, v := range vars {
		wispArgs = append(wispArgs, "--var", v)
	}
	wispArgs = append(wispArgs, "--json")
	return BdCmd(wispArgs...).
		Dir(workDir).
		WithAutoCommit().
		WithGTRoot(townRoot).
		Output()
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
	if !r.opts.dryRun {
		if dogName, isDog := IsDogTarget(target); isDog && dogName == "" {
			// One pool-wide lock, not one per formula: two formulas slung to
			// the pool at once must not pick the same idle dog.
			poolUnlock, poolLockErr := r.lockAssignee(townRoot, "deacon/dogs")
			if poolLockErr != nil {
				return fmt.Errorf("serializing dog-pool formula sling for %s: %w", formulaName, poolLockErr)
			}
			defer poolUnlock()
		}
	}
	resolved, err := r.resolveTarget(target, ResolveTargetOptions{
		DryRun:               r.opts.dryRun,
		Force:                r.opts.force,
		Create:               r.opts.create,
		Account:              r.opts.account,
		Agent:                r.opts.agent,
		NoBoot:               r.opts.noBoot,
		WorkDesc:             formulaName,
		TownRoot:             townRoot,
		SkipPolecatAdmission: admission != nil,
	})
	if err != nil {
		return err
	}
	targetAgent := resolved.Agent
	targetPane := resolved.Pane
	formulaWorkDir := resolved.WorkDir
	delayedDogInfo := resolved.DelayedDogInfo
	isSelfSling := resolved.IsSelfSling

	fmt.Fprintf(out, "%s Slinging formula %s to %s...\n", style.Bold.Render("🎯"), formulaName, targetAgent)

	// Rollback guard (gt-7evi4): once resolveTarget has spawned or reused a
	// polecat, every exit that does not reach the commit point rolls it back
	// exactly once. rollbackBeadID names the wisp only once this sling is about
	// to hook it; earlier failures touch no bead. A wisp this sling created and
	// did not commit is burned, so it cannot stay hooked to a removed or idle
	// polecat; a delayed dog's wisp is left to its own failure cleanup.
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
		if wispRootID != "" && delayedDogInfo == nil {
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

	delayedDogComplete := false
	// Serialize standalone formula slings per assignee so same-formula retries
	// and handoffs cannot create duplicate hooked wisps for one target.
	assigneeUnlock, assigneeLockErr := r.lockAssignee(townRoot, targetAgent)
	if assigneeLockErr != nil {
		lockErr := fmt.Errorf("serializing formula sling for %s: %w", targetAgent, assigneeLockErr)
		if delayedDogInfo == nil {
			return lockErr
		}
		if clearErr := r.clearDogWork(delayedDogInfo); clearErr != nil {
			return errors.Join(lockErr, fmt.Errorf("clearing failed dog assignment: %w", clearErr))
		}
		return lockErr
	}
	defer assigneeUnlock()
	// Deferred after the unlock, so the dog cleanup runs while the assignee
	// lock is still held.
	defer func() {
		if delayedDogInfo == nil || delayedDogComplete {
			return
		}
		if err == nil && wispRootID == "" {
			return
		}
		err = r.cleanupDelayedDogFormulaFailure(err, delayedDogInfo, wispRootID, formulaWorkDir)
	}()
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
	if shouldReuseExistingFormula(existing, delayedDogInfo, r.opts.force) {
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
		if delayedDogInfo != nil {
			// The dog was assigned this work but its session was delayed:
			// start and nudge it before reporting the no-op.
			if _, err := r.startDelayedDog(delayedDogInfo); err != nil {
				return fmt.Errorf("starting delayed dog session for existing formula: %w", err)
			}
			delayedDogComplete = true
			if r.getenv("GT_TEST_NO_NUDGE") == "" {
				r.nudgeFormulaDog(delayedDogInfo, formulaSlingPrompt(formulaName, r.opts.argsText))
			}
		}
		return nil
	}
	// A dog that does not own this work may only reuse it; it must not go on
	// to create a fresh wisp.
	if delayedDogInfo != nil && !delayedDogInfo.ownsWork {
		return fmt.Errorf("dog formula reuse became stale before hook verification; retry dispatch")
	}
	if existing != nil && !r.opts.force && delayedDogInfo != nil && delayedDogInfo.ownsWork {
		if err := r.cleanupStaleDogWisp(existing.ID, formulaWorkDir); err != nil {
			return fmt.Errorf("cleaning stale dog formula wisp %s: %w", existing.ID, err)
		}
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
		telemetry.RecordMolCook(ctx, formulaName, err)
		return fmt.Errorf("cooking formula: %w", err)
	}
	telemetry.RecordMolCook(ctx, formulaName, nil)

	// Step 2: Create wisp instance (ephemeral)
	fmt.Fprintf(out, "  Creating wisp...\n")
	wispOut, err := r.createWisp(formulaName, formulaWorkDir, townRoot, r.opts.vars)
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

	// Start delayed dog session now that hook is set
	// This ensures dog sees the hook when gt prime runs on session start
	if delayedDogInfo != nil {
		pane, err := r.startDelayedDog(delayedDogInfo)
		if err != nil {
			return fmt.Errorf("starting delayed dog session: %w", err)
		}
		delayedDogComplete = true
		targetPane = pane
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

	// Dog sessions need a nudge sent to their session (not to the bare pane ID
	// from StartDelayedSession, which is ambiguous on platforms where tmux pane
	// IDs are not globally unique). Use NudgeSession which qualifies the target
	// with the session name, before the empty-pane return below. (gt-etc)
	if delayedDogInfo != nil {
		r.nudgeFormulaDog(delayedDogInfo, prompt)
		return nil
	}

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
