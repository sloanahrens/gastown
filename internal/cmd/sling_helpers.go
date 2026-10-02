package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecat"
	rigpkg "github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// resolveBeadDir returns the directory to run bd commands for a given bead ID.
// Uses prefix-based routing (routes.jsonl) to resolve the correct rig's .beads
// directory and returns its parent as the working directory for bd.
//
// Background: beads v0.62 removed built-in multi-rig routing from bd — all bd
// commands now operate on the local database only. Cross-rig resolution must
// happen in gt before invoking bd, by setting the correct working directory
// (and stripping BEADS_DIR). This function reads routes.jsonl from the town-level
// .beads directory and resolves the bead's prefix to the owning rig.
//
// PR #3166 (steveyegge/gastown) will replace bd shell-outs with the Go module
// Storage API, making this function unnecessary. Until then, this is the
// routing bridge between gt and the routing-free bd CLI.
func resolveBeadDir(beadID string) string {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "."
	}
	return resolveBeadDirFromTownRoot(townRoot, beadID)
}

func resolveBeadDirFromTownRoot(townRoot, beadID string) string {
	if townRoot == "" {
		return "."
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	resolved := beads.ResolveBeadsDirForID(townBeadsDir, beadID)
	// Return the parent of the .beads directory so bd discovers it naturally.
	// For town-level beads this returns townRoot; for rig beads it returns
	// the rig's mayor/rig directory (e.g., gastown/mayor/rig).
	return filepath.Dir(resolved)
}

// resolveBeadDirFromRigsJSON looks up the rig directory from rigs.json using prefix.
func resolveBeadDirFromRigsJSON(townRoot, prefix string) string {
	rigsFile, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return ""
	}
	// prefix includes trailing hyphen (e.g., "bd-"), rigs.json stores without (e.g., "bd")
	trimmedPrefix := strings.TrimSuffix(prefix, "-")
	for rigName, rigConfig := range rigsFile.Rigs {
		if rigConfig.BeadsConfig != nil && rigConfig.BeadsConfig.Prefix == trimmedPrefix {
			// Return mayor/rig path within the rig (where .beads/ lives)
			return townRoot + "/" + rigName + "/mayor/rig"
		}
	}
	return ""
}

// beadInfo is the dispatch engine's view of a bead, owned by internal/sling so
// the daemon's convoy feeders read and write the same shape the cobra command
// does.
type beadInfo = sling.Bead

// isDeferredBead checks whether a bead should be rejected from slinging because
// it has been deferred.
func isDeferredBead(info *beadInfo) bool {
	return sling.IsDeferredBead(info)
}

func applyWorkflowStepTargetOverride(args []string) ([]string, error) {
	if len(args) != 2 {
		return args, nil
	}
	rigName, isRig := IsRigName(args[1])
	if !isRig {
		return args, nil
	}
	info, err := getBeadInfo(args[0])
	if err != nil {
		return args, nil
	}
	target := workflowStepTargetFromDescription(info.Description, rigName)
	if target == "" || target == args[1] {
		return args, nil
	}
	if err := ValidateTarget(target); err != nil {
		return args, fmt.Errorf("invalid %s for %s: %w", workflowTargetField, args[0], err)
	}
	redirected := append([]string(nil), args...)
	redirected[1] = target
	fmt.Printf("%s Workflow step target: %s\n", style.Dim.Render("→"), target)
	return redirected, nil
}

func workflowStepTargetFromDescription(description, targetRig string) string {
	for _, line := range strings.Split(description, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), workflowTargetField) {
			continue
		}
		target := strings.TrimSpace(value)
		if target == "" || target == "rig" {
			return targetRig
		}
		return target
	}
	return ""
}

// isOrphanMolecule reports whether a bead's existing attached molecule(s)
// can be safely burned at sling time without operator confirmation. Used
// to gate the auto-burn path that lets sling self-heal from stale state.
//
// A molecule is treated as orphaned when:
//   - the bead has no assignee but is in an active status (open/in_progress)
//     or stuck in `hooked` with no assignee — the latter covers gh-3697,
//     where one orphan wisp would otherwise wedge every subsequent sling
//     to the rig with "bead already has N attached molecule(s)"; or
//   - the bead has an assignee but that assignee's tmux session is dead.
//
// `closed` and `blocked` deliberately fall through to the refuse path:
// burning molecules off a closed bead would mask completed work, and
// burning off a blocked bead can mask a real dependency.
func isOrphanMolecule(info *beadInfo) bool {
	return isOrphanMoleculeWith(info, isHookedAgentDeadFn)
}

// isOrphanMoleculeWith is isOrphanMolecule judging the assignee with dead.
func isOrphanMoleculeWith(info *beadInfo, dead func(assignee string) bool) bool {
	if info == nil {
		return false
	}
	if info.Assignee == "" {
		switch info.Status {
		case "open", "in_progress", "hooked":
			return true
		}
		return false
	}
	return dead(info.Assignee)
}

// collectExistingMolecules returns all molecule wisp IDs attached to a bead.
// Checks both dependency bonds (ground truth from bd mol bond) and the
// description's attached_molecule field (metadata pointer). Wisp IDs are
// identified by containing "-wisp-" in their ID.
// Uses Dependencies (structured []IssueDep from bd show --json) rather than
// DependsOn (raw ID list, which is unreliable — see molecule_status.go comments).
func collectExistingMolecules(info *beadInfo) []string {
	seen := make(map[string]bool)
	var molecules []string

	// Check dependency bonds (ground truth - bd mol bond creates these)
	for _, dep := range info.Dependencies {
		if strings.Contains(dep.ID, "-wisp-") && !seen[dep.ID] {
			// Skip molecules already closed/burned — bond is stale
			if dep.Status == "closed" || dep.Status == "tombstone" {
				continue
			}
			seen[dep.ID] = true
			molecules = append(molecules, dep.ID)
		}
	}

	// Also check description's attached_molecule (may differ from bonds)
	issue := &beads.Issue{Description: info.Description}
	fields := beads.ParseAttachmentFields(issue)
	if fields != nil && fields.AttachedMolecule != "" && !seen[fields.AttachedMolecule] {
		seen[fields.AttachedMolecule] = true
		molecules = append(molecules, fields.AttachedMolecule)
	}

	return molecules
}

func appendUniqueMolecules(molecules []string, extras ...string) []string {
	seen := make(map[string]bool, len(molecules)+len(extras))
	for _, molecule := range molecules {
		seen[molecule] = true
	}
	for _, molecule := range extras {
		if molecule == "" || seen[molecule] {
			continue
		}
		seen[molecule] = true
		molecules = append(molecules, molecule)
	}
	return molecules
}

func collectExistingMoleculesForBead(info *beadInfo, beadID, townRoot string) ([]string, error) {
	molecules := collectExistingMolecules(info)
	deps, err := collectExistingMoleculeDeps(beadID, townRoot)
	if err != nil {
		return molecules, err
	}
	return appendUniqueMolecules(molecules, deps...), nil
}

func collectExistingMoleculeDeps(beadID, townRoot string) ([]string, error) {
	return slingStores{}.moleculeDeps(beadID, townRoot)
}

// moleculeDeps returns the molecules bonded to beadID, read by one sql
// query over the wisp dependency edges in the bead's database.
func (s slingStores) moleculeDeps(beadID, townRoot string) ([]string, error) {
	if beadID == "" {
		return nil, nil
	}
	if !beads.IsValidBeadID(beadID) {
		return nil, fmt.Errorf("invalid bead ID: %q", beadID)
	}

	rows, err := s.pinnedAt(resolveBeadDirFromTownRoot(townRoot, beadID)).SQLCSV(beadsql.MoleculesAttachedTo(beadID))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	col := -1
	for i, name := range rows[0] {
		if name == "issue_id" {
			col = i
		}
	}
	if col < 0 {
		return nil, fmt.Errorf("parsing canonical molecule deps for %s: no issue_id column in %q", beadID, rows[0])
	}

	seen := make(map[string]bool, len(rows))
	var molecules []string
	for _, row := range rows[1:] {
		if col >= len(row) {
			continue
		}
		moleculeID := row[col]
		if moleculeID == "" || seen[moleculeID] {
			continue
		}
		seen[moleculeID] = true
		molecules = append(molecules, moleculeID)
	}
	return molecules, nil
}

// burnExistingMolecules burns all molecule wisps attached to a bead.
// Order: force-close descendants → detach from bead → remove dep bonds → force-close roots.
// Matches nukeCleanupMolecules pattern. Returns an error if detach fails, since
// proceeding with a stale attached_molecule reference creates harder-to-debug orphans.
func burnExistingMolecules(molecules []string, beadID, townRoot string) error {
	return slingStores{}.burnMolecules(molecules, beadID, townRoot)
}

// burnMolecules is burnExistingMolecules in s.
func (s slingStores) burnMolecules(molecules []string, beadID, townRoot string) error {
	if len(molecules) == 0 {
		return nil
	}
	burnDir := beads.ResolveHookDir(townRoot, beadID, "")

	// Follows the same order as nukeCleanupMolecules, plus dep bond removal:
	//   1. Force-close descendants (children before parents)
	//   2. Detach molecule from bead (clears attached_molecule in description)
	//   3. Remove dependency bonds (prevents "existing molecule(s)" on re-sling)
	//   4. Force-close molecule roots
	// Closing descendants first ensures that if detach succeeds but a later step
	// crashes, we don't leave a detached root with live children.
	bd := s.routedFrom(burnDir)

	// Step 1: Force-close descendant steps before detaching. Uses force variant
	// since burn is a destructive recovery path where prior state may be inconsistent.
	// Best-effort — log but proceed in destructive path.
	for _, molID := range molecules {
		if _, err := forceCloseDescendants(bd, molID); err != nil {
			style.PrintWarning("burn: could not close descendants of %s: %v", molID, err)
		}
	}

	// Step 2: Detach molecule from the base bead using the Go API (with audit logging
	// and advisory locking). This clears attached_molecule/attached_at from the description.
	// Without this, storeFieldsInBead preserves the stale reference because it only
	// overwrites when updates.AttachedMolecule is non-empty.
	if _, err := bd.DetachMoleculeWithAudit(beadID, beads.DetachOptions{
		Operation: "burn",
		Reason:    "force re-sling: burning stale molecules",
	}); err != nil {
		return fmt.Errorf("detaching molecule from %s: %w", beadID, err)
	}

	// Step 3: Remove dependency bonds between the bead and each molecule.
	// DetachMoleculeWithAudit (step 2) only clears the description metadata
	// (attached_molecule/attached_at). The dependency bond from bd mol bond
	// is a separate link that collectExistingMolecules reads via info.Dependencies.
	// Without this, the next sling attempt finds the closed molecule via the
	// bond and refuses with "bead has existing molecule(s)".
	for _, molID := range molecules {
		removeMoleculeBonds(bd, beadID, molID)
	}

	// Step 4: Close descendants, then force-close the orphaned wisp roots.
	// Best-effort — log but proceed in destructive path.
	for _, molID := range molecules {
		if _, err := forceCloseDescendants(bd, molID); err != nil {
			style.PrintWarning("burn: could not close descendants of %s: %v", molID, err)
		}
	}
	if err := bd.ForceCloseWithReason("burned: force re-sling", molecules...); err != nil {
		fmt.Printf("  %s Could not close molecule wisp(s): %v\n",
			style.Dim.Render("Warning:"), err)
		// Close failure is non-fatal — the detach already succeeded, so the bead
		// is clean. Orphaned wisps will be caught by reactive DetectOrphanedMolecules.
	}

	return nil
}

func removeMoleculeBonds(bd beads.Client, beadID, molID string) {
	for _, bond := range []struct {
		from string
		to   string
	}{
		{from: molID, to: beadID}, // canonical bd mol bond direction
		{from: beadID, to: molID}, // legacy reverse direction
	} {
		if err := bd.RemoveDependency(bond.from, bond.to); err != nil && !dependencyRemovalMissing(err) {
			fmt.Printf("  %s Could not remove dep bond %s → %s: %v\n",
				style.Dim.Render("Warning:"), bond.from, bond.to, err)
		}
	}
}

func dependencyRemovalMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no dependency") ||
		strings.Contains(msg, "not present")
}

// verifyBeadExists checks that the bead exists using bd show.
// Resolves the rig directory from the bead's prefix for correct dolt access.
// StripBeadsDir prevents inherited BEADS_DIR from overriding the resolved
// directory, which caused rig-prefixed beads to fail (GH#2126).
func verifyBeadExists(beadID string) error {
	if _, err := showBead("", beadID); err != nil {
		return fmt.Errorf("bead '%s' not found (bd show failed: %w)", beadID, err)
	}
	return nil
}

// verifyBeadExistsInTargetRigDatabase checks the target rig's beads database
// directly instead of following prefix routing. This prevents gt sling from
// spawning polecats or creating molecule/hook side effects for beads that only
// resolve from HQ or another rig database.
func verifyBeadExistsInTargetRigDatabase(beadID, targetRig, townRoot string) error {
	return slingStores{}.verifyInTargetRig(beadID, targetRig, townRoot)
}

// verifyInTargetRig is verifyBeadExistsInTargetRigDatabase in s.
func (s slingStores) verifyInTargetRig(beadID, targetRig, townRoot string) error {
	if beadID == "" {
		return nil
	}
	if targetRig == "" {
		return fmt.Errorf("cannot verify bead %s in target rig: target rig is empty; refusing to sling before creating hooks or molecule side effects", beadID)
	}
	if townRoot == "" {
		return fmt.Errorf("cannot verify bead %s in target rig %q: town root is unavailable; refusing to sling before creating hooks or molecule side effects", beadID, targetRig)
	}

	targetBeadsDir, ok := beads.ResolveRepoAliasBeadsDir(townRoot, targetRig)
	if !ok {
		return fmt.Errorf("cannot resolve target rig %q beads database for bead %s; refusing to sling before creating hooks or molecule side effects", targetRig, beadID)
	}
	if _, err := s.pinnedDB(targetBeadsDir).Show(beadID); err == nil {
		return nil
	}
	if s.routedBeadExistsForTargetRig(beadID, targetRig, townRoot) {
		return nil
	}
	return fmt.Errorf("bead %s is not present in target rig %q beads database; refusing to sling before creating hooks or molecule side effects", beadID, targetRig)
}

func (s slingStores) routedBeadExistsForTargetRig(beadID, targetRig, townRoot string) bool {
	prefixRig := beads.GetRigNameForPrefix(townRoot, beads.ExtractPrefix(beadID))
	if prefixRig != targetRig {
		return false
	}
	_, err := s.routedFrom(townRoot).Show(beadID)
	return err == nil
}

// getBeadInfo returns status and assignee for a bead.
// Resolves the rig directory from the bead's prefix for correct dolt access.
func getBeadInfo(beadID string) (*beadInfo, error) {
	return slingStores{}.beadInfo("", beadID)
}

func getBeadInfoFromTownRoot(townRoot, beadID string) (*beadInfo, error) {
	return slingStores{}.beadInfo(townRoot, beadID)
}

// beadFieldUpdates holds all the fields that need to be stored in a bead's
// description. This enables a single read-modify-write cycle instead of
// sequential independent updates, eliminating the race condition where
// concurrent writers could overwrite each other's fields. internal/sling owns
// the shape, because the dispatch engine writes them.
type beadFieldUpdates = sling.FieldUpdates

func buildSlingFieldUpdates(
	dispatcher string,
	args string,
	vars []string,
	attachedMolecule string,
	attachedFormula string,
	noMerge bool,
	reviewOnly bool,
	mode string,
	formulaVars string,
	convoyID string,
	mergeStrategy string,
	convoyOwned bool,
) beadFieldUpdates {
	updates := beadFieldUpdates{
		Dispatcher:       dispatcher,
		Args:             args,
		Vars:             vars,
		AttachedMolecule: attachedMolecule,
		AttachedFormula:  attachedFormula,
		NoMerge:          noMerge,
		ReviewOnly:       reviewOnly,
		Mode:             &mode,
		ConvoyID:         convoyID,
		MergeStrategy:    mergeStrategy,
		ConvoyOwned:      convoyOwned,
		FormulaVars:      formulaVars,
	}
	if attachedMolecule != "" || attachedFormula != "" || noMerge || reviewOnly {
		updates.AttachedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return updates
}

// storeFieldsInBead performs a single read-modify-write to update all attachment fields
// in a bead's description atomically. This replaces the sequential storeDispatcherInBead,
// storeArgsInBead, storeAttachedMoleculeInBead, and storeNoMergeInBead calls that each
// independently read-modify-write and could race under concurrent access.
func storeFieldsInBead(beadID string, updates beadFieldUpdates) error {
	return storeFieldsInBeadFromTownRoot("", beadID, updates)
}

func storeFieldsInBeadFromTownRoot(townRoot, beadID string, updates beadFieldUpdates) error {
	// Read the bead once, so the update is a single read-modify-write.
	issue, err := showBead(townRoot, beadID)
	if err != nil {
		return fmt.Errorf("fetching bead: %w", err)
	}

	newDesc := applyBeadFieldUpdates(issue, updates)

	updateDir := resolveBeadDir(beadID)
	if townRoot != "" {
		updateDir = resolveBeadDirFromTownRoot(townRoot, beadID)
	}
	if err := pinnedBd(updateDir).Update(beadID, beads.UpdateOptions{Description: &newDesc}); err != nil {
		return fmt.Errorf("updating bead description: %w", err)
	}

	return nil
}

// applyBeadFieldUpdates returns issue's description with updates applied to
// its attachment fields: the one write storeFieldsInBead makes. Workflow
// metadata (a molecule, a formula, no_merge or review_only) always carries a
// fresh attached_at unless updates names one.
func applyBeadFieldUpdates(issue *beads.Issue, updates beadFieldUpdates) string {
	// Get or create attachment fields
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		fields = &beads.AttachmentFields{}
	}

	// Apply all updates in one pass
	if updates.ClearAttachment {
		fields.AttachedMolecule = ""
		fields.AttachedFormula = ""
		fields.AttachedAt = ""
		fields.AttachedVars = nil
		fields.FormulaVars = ""
	}
	if updates.Dispatcher != "" {
		fields.DispatchedBy = updates.Dispatcher
	}
	if updates.Args != "" {
		fields.AttachedArgs = updates.Args
	}
	if len(updates.Vars) > 0 {
		fields.AttachedVars = append([]string(nil), updates.Vars...)
	}
	if updates.AttachedMolecule != "" {
		fields.AttachedMolecule = updates.AttachedMolecule
	}
	if updates.AttachedFormula != "" {
		fields.AttachedFormula = updates.AttachedFormula
	}
	if updates.AttachedAt != "" {
		fields.AttachedAt = updates.AttachedAt
	} else if updates.AttachedMolecule != "" || updates.AttachedFormula != "" || updates.NoMerge || updates.ReviewOnly {
		fields.AttachedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if updates.NoMerge {
		fields.NoMerge = true
	}
	if updates.ReviewOnly {
		fields.ReviewOnly = true
	}
	if updates.Mode != nil {
		fields.Mode = *updates.Mode
	}
	if updates.ConvoyID != "" {
		fields.ConvoyID = updates.ConvoyID
	}
	if updates.MergeStrategy != "" {
		fields.MergeStrategy = updates.MergeStrategy
	}
	if updates.ConvoyOwned {
		fields.ConvoyOwned = true
	}
	if updates.FormulaVars != "" {
		fields.FormulaVars = updates.FormulaVars
	}

	return beads.SetAttachmentFields(issue, fields)
}

// nudgePaneFn delivers a "start now" nudge to a target pane. It is a seam:
// tests replace it so a sling run inside the test process never types into a
// live tmux pane.
var nudgePaneFn = func(pane, message string) error {
	return tmux.NewTmux().NudgePane(pane, message)
}

// injectStartPrompt sends a prompt to the target pane to start working.
// Uses the reliable nudge pattern: literal mode + 500ms debounce + separate Enter.
func injectStartPrompt(pane, beadID, subject, args string) error {
	if pane == "" {
		return fmt.Errorf("no target pane")
	}

	// Build the prompt to inject
	var prompt string
	if args != "" {
		// Args provided - include them prominently in the prompt
		if subject != "" {
			prompt = fmt.Sprintf("Work slung: %s (%s). Args: %s. Start working now - use these args to guide your execution.", beadID, subject, args)
		} else {
			prompt = fmt.Sprintf("Work slung: %s. Args: %s. Start working now - use these args to guide your execution.", beadID, args)
		}
	} else if subject != "" {
		prompt = fmt.Sprintf("Work slung: %s (%s). Start working on it now - no questions, just begin.", beadID, subject)
	} else {
		prompt = fmt.Sprintf("Work slung: %s. Start working on it now - run `"+cli.Name()+" hook` to see the hook, then begin.", beadID)
	}

	// Use the reliable nudge pattern (same as gt nudge / tmux.NudgeSession)
	return nudgePaneFn(pane, prompt)
}

// getSessionFromPane extracts session name from a pane target.
// Pane targets can be:
// - "%9" (pane ID) - need to query tmux for session
// - "gt-rig-name:0.0" (session:window.pane) - extract session name
func getSessionFromPane(pane string) string {
	if strings.HasPrefix(pane, "%") {
		// Pane ID format - query tmux for the session
		cmd := tmux.BuildCommand("display-message", "-t", pane, "-p", "#{session_name}")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	// Session:window.pane format - extract session name
	if idx := strings.Index(pane, ":"); idx > 0 {
		return pane[:idx]
	}
	return pane
}

// ensureAgentReady waits for an agent to be ready before nudging an existing session.
// Uses a pragmatic approach: wait for the pane to leave a shell, then (Claude-only)
// accept the bypass permissions warning and give it a moment to finish initializing.
func ensureAgentReady(sessionName string) error {
	t := tmux.NewTmux()

	if t.IsAgentRunning(sessionName) {
		// Agent process is detected, but it may have just started (fresh spawn).
		// Check session age — if < 15s old, the agent likely isn't ready for input yet.
		if !isSessionYoung(sessionName, 15*time.Second) {
			return nil
		}
		// Fall through to apply startup delay for young sessions.
	} else {
		// Agent not running yet - wait for it to start (shell → program transition)
		if err := t.WaitForCommand(sessionName, constants.SupportedShells, constants.ClaudeStartTimeout); err != nil {
			return fmt.Errorf("waiting for agent to start: %w", err)
		}
	}

	// Accept startup dialogs (workspace trust + bypass permissions) if they appear
	_ = t.AcceptWorkspaceTrustDialog(sessionName)
	agentName, preset, ok := t.SessionAgentPreset(sessionName, "")
	if shouldAcceptPermissionWarning(agentName, preset, ok) {
		_ = t.AcceptBypassPermissionsWarning(sessionName)
	}

	// Use prompt-detection polling instead of fixed sleep.
	// For known presets: uses ReadyPromptPrefix (e.g. "❯ " for Claude) polled every 200ms.
	// For unknown/custom agents: falls back to a 1s fixed delay (mirrors old behavior).
	// Note: uses preset-only resolution (not ResolveRoleAgentConfig) because
	// ensureAgentReady only has the session name; the registry is the one of
	// the session's own town and rig (GT_TOWN_ROOT/GT_RIG).
	effectiveName := agentName
	if effectiveName == "" {
		effectiveName = "claude" // Default sessions without GT_AGENT are Claude
	}
	registry := t.SessionAgentRegistry(sessionName, "")
	var rc *config.RuntimeConfig
	if preset := registry.Preset(effectiveName); preset != nil {
		rc = registry.RuntimeConfigFromPreset(config.AgentPreset(effectiveName))
	} else {
		// Unknown agent — use minimal config: no prompt detection, short fixed delay.
		rc = &config.RuntimeConfig{
			Tmux: &config.RuntimeTmuxConfig{
				ReadyDelayMs: 1000,
			},
		}
	}
	// Ensure a minimum 1s readiness delay for presets without prompt detection.
	// Without this, agents with ReadyPromptPrefix="" and ReadyDelayMs=0
	// (e.g. gemini, cursor) would skip the readiness guard entirely,
	// reintroducing early-input races that this function exists to prevent.
	if rc.Tmux != nil && rc.Tmux.ReadyPromptPrefix == "" && rc.Tmux.ReadyDelayMs < 1000 {
		rc.Tmux.ReadyDelayMs = 1000
	}
	if err := t.WaitForRuntimeReady(sessionName, rc, constants.ClaudeStartTimeout); err != nil {
		// Graceful degradation: warn but proceed (matches original behavior of always continuing)
		fmt.Fprintf(os.Stderr, "Warning: agent readiness detection timed out for %s: %v\n", sessionName, err)
	}

	return nil
}

// isSessionYoung returns true if the tmux session was created less than maxAge ago.
func isSessionYoung(sessionName string, maxAge time.Duration) bool {
	out, err := tmux.BuildCommand("display-message", "-t", sessionName, "-p", "#{session_created}").Output()
	if err != nil {
		return false
	}
	createdUnix, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(createdUnix, 0)) < maxAge
}

// detectCloneRoot finds the root of the current git clone.
func detectCloneRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

// detectActor returns the current agent's actor string for event logging.
func detectActor() string {
	roleInfo, err := GetRole()
	if err != nil {
		return "unknown"
	}
	return roleInfo.ActorString()
}

// resolveSlingActor returns the --actor override for this sling invocation
// when one was passed, otherwise falls back to detectActor(). System callers
// that shell out to `gt sling` without a live agent role of their own (e.g.
// the daemon's convoy auto-dispatch) would otherwise record actor "unknown";
// they should pass --actor explicitly instead.
func resolveSlingActor() string {
	if slingActor != "" {
		return slingActor
	}
	return detectActor()
}

// agentIDToBeadID converts an agent ID to its corresponding agent bead ID.
// Uses canonical naming: prefix-rig-role-name
// Town-level agents (Mayor, Deacon) use hq- prefix and are stored in town beads.
// Rig-level agents use the rig's configured prefix (default "gt-").
// townRoot is needed to look up the rig's configured prefix.
func agentIDToBeadID(agentID, townRoot string) string {
	// Normalize: strip trailing slash (resolveSelfTarget returns "mayor/" not "mayor")
	agentID = strings.TrimSuffix(agentID, "/")

	// Handle simple cases (town-level agents with hq- prefix)
	if agentID == "mayor" {
		return beads.MayorBeadIDTown()
	}

	// Parse path-style agent IDs
	parts := strings.Split(agentID, "/")
	if len(parts) < 2 {
		return ""
	}

	rig := parts[0]
	prefix := beads.GetPrefixForRig(townRoot, rig)

	switch {
	case len(parts) == 3 && parts[1] == "crew":
		return beads.CrewBeadIDWithPrefix(prefix, rig, parts[2])
	case len(parts) == 3 && parts[1] == "polecats":
		return beads.PolecatBeadIDWithPrefix(prefix, rig, parts[2])
	default:
		return ""
	}
}

// updateAgentHookBead is a no-op. Previously set the hook_bead slot on agent beads
// when work was slung, but this was redundant: the work bead itself tracks
// status=hooked and assignee=<agent>. Agent bead slot writes caused warnings
// in cross-database scenarios and added unnecessary Dolt load.
// Removed per hq-l6mm5: replace bd slot hooks with direct bead tracking.
func updateAgentHookBead(agentID, beadID, workDir, townBeadsDir string) {
	// No-op: work bead status=hooked + assignee is the authoritative source.
	// Agent bead hook_bead slot is no longer maintained.
}

// wakeRigAgents runs after polecat dispatch. Nothing needs waking: the
// daemon's patrol_scan tick watches the new polecat (gt-4k3fj.6). It warns
// when the daemon is not running, since then nothing restarts a polecat whose
// session dies (gt-9wv0).
func wakeRigAgents(rigName string) {
	townRoot, _ := workspace.FindFromCwd()
	if townRoot != "" {
		if running, _, _ := daemon.IsRunning(townRoot); !running {
			fmt.Fprintf(os.Stderr, "Warning: daemon is not running. Nothing restarts %s polecats that die.\n", rigName)
			fmt.Fprintf(os.Stderr, "  Start with: gt daemon start\n")
		}
	}
}

// isPolecatTarget checks if the target string refers to a polecat.
// Returns true if the target format is "rig/polecats/name".
// This is used to determine if we should respawn a dead polecat
// instead of failing when slinging work.
func isPolecatTarget(target string) bool {
	parts := strings.Split(target, "/")
	return len(parts) >= 3 && parts[1] == "polecats"
}

// FormulaOnBeadResult contains the result of instantiating a formula on a bead.
type FormulaOnBeadResult = sling.FormulaResult

// formulaBD is how formula instantiation reaches bd: open is the formula
// engine at a site (nil: the bd on PATH) and sleep is the pause between
// contention retries of the bond. InstantiateFormulaOnBead, CookFormula and
// bondFormulaDirect are its methods on realFormulaBD.
type formulaBD struct {
	open  func(formulaSite) formulaEngine
	sleep func(time.Duration)
}

// realFormulaBD is the bd on PATH with the real retry backoff.
func realFormulaBD() formulaBD {
	return formulaBD{sleep: bdContentionSleep}
}

// engine is the formula engine at site.
func (f formulaBD) engine(site formulaSite) formulaEngine {
	return formulaCooker{open: f.open}.engine(site)
}

// beadEngine is the engine in formulaWorkDir, writing to the database
// beadID's prefix routes to, so a polecat worktree's own .beads cannot
// capture the wisp.
func (f formulaBD) beadEngine(beadID, formulaWorkDir, townRoot string) formulaEngine {
	return f.engine(formulaSite{
		dir:      formulaWorkDir,
		beadsDir: beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), beadID),
		townRoot: townRoot,
	})
}

// InstantiateFormulaOnBead bonds a formula directly to a bead.
// This is the formula-on-bead pattern used by issue #288 for auto-applying mol-polecat-work.
//
// Parameters:
//   - formulaName: the formula to instantiate (e.g., "mol-polecat-work")
//   - beadID: the base bead to bond the wisp to
//   - title: the bead title (used for --var feature=<title>)
//   - hookWorkDir: working directory for bd commands (polecat's worktree)
//   - townRoot: the town root directory
//   - extraVars: additional --var values supplied by the user
//
// Returns the spawned molecule root ID while leaving the base bead as the hook target.
func InstantiateFormulaOnBead(_ context.Context, formulaName, beadID, title, hookWorkDir, townRoot string, extraVars []string) (*FormulaOnBeadResult, error) {
	return realFormulaBD().instantiate(formulaName, beadID, title, hookWorkDir, townRoot, extraVars)
}

// instantiate is InstantiateFormulaOnBead with bd reached through f.
func (f formulaBD) instantiate(formulaName, beadID, title, hookWorkDir, townRoot string, extraVars []string) (*FormulaOnBeadResult, error) {
	// Route bd mutations to the correct beads context for the target bead.
	formulaWorkDir := beads.ResolveHookDir(townRoot, beadID, hookWorkDir)

	// The cook happens once, here: bd cooks the formula with the bond's vars, and a
	// cook failure fails the pour with one line.
	formulaVars, err := f.varsForBead(formulaName, beadID, title, formulaWorkDir, townRoot, extraVars)
	if err != nil {
		return nil, err
	}
	wispRootID, err := f.bond(formulaName, formulaName, beadID, formulaWorkDir, townRoot, formulaVars)
	if err != nil {
		return nil, fmt.Errorf("bonding formula %s to bead %s: %w", formulaName, beadID, err)
	}

	return &FormulaOnBeadResult{
		WispRootID:  wispRootID,
		BeadToHook:  beadID, // Hook the BASE bead (lifecycle fix: wisp is attached_molecule)
		FormulaVars: append([]string(nil), formulaVars...),
	}, nil
}

// varsForBead assembles the --var list for bonding formulaName to beadID: the
// standard feature/issue pair, the caller's extraVars, and every variable the
// formula declares a default for, as bd cooks it in the bead's beads context.
func (f formulaBD) varsForBead(formulaName, beadID, title, formulaWorkDir, townRoot string, extraVars []string) ([]string, error) {
	formulaVars := []string{
		fmt.Sprintf("feature=%s", title),
		fmt.Sprintf("issue=%s", beadID),
	}
	formulaVars = append(formulaVars, extraVars...)
	cooked, err := cookFormula(formulaName, f.beadEngine(beadID, formulaWorkDir, townRoot), formulaVars)
	if err != nil {
		return nil, err
	}
	return backfillFormulaDefaultVars(cooked, formulaVars)
}

// bondFormulaDirect attaches a formula to a bead through bd's canonical bond path.
//
// The bond is the write that spawns the wisp and attaches it, and it is the one
// step of a sling an unrelated writer can take from under it: under load the
// convoy's bulk wisp closes commit against it often enough that Dolt aborts it
// (gt-4ckuf). An abort there is a dead end for the operator — the sling exits
// non-zero with the bead left open and unassigned — so the attempt is repeated
// while bd reports contention. bd's storage layer owns the broad fix across its
// write paths; this is the retry on top that keeps one contended commit from
// failing a dispatch.
func bondFormulaDirect(bondTarget, formulaName, beadID, formulaWorkDir, townRoot string, vars []string) (string, error) {
	return realFormulaBD().bond(bondTarget, formulaName, beadID, formulaWorkDir, townRoot, vars)
}

// bond is bondFormulaDirect with bd reached through f.
func (f formulaBD) bond(bondTarget, formulaName, beadID, formulaWorkDir, townRoot string, vars []string) (string, error) {
	eng := f.beadEngine(beadID, formulaWorkDir, townRoot)
	var lastErr error
	for attempt := 1; attempt <= bdContentionAttempts; attempt++ {
		bondOut, err := eng.Bond(bondTarget, beadID, vars)
		if err == nil {
			rootID := parseBondSpawnRootID(bondOut, formulaName, beadID, "")
			if rootID == "" {
				return "", fmt.Errorf("direct bond output missing spawned root id (output: %s)", trimJSONForError(bondOut))
			}
			return rootID, nil
		}

		// The engine's error reads as bd's own message (the --json failure
		// payload), not just "exit status 1".
		lastErr = fmt.Errorf("bd mol bond %s %s: %w", bondTarget, beadID, err)
		if !bdContentionRetryable(err, err.Error()) || attempt == bdContentionAttempts {
			break
		}

		wait := slingBackoff(attempt, bdContentionBackoffMin, bdContentionBackoffMax)
		fmt.Printf("  %s Bond attempt %d lost to Dolt contention, retrying in %v...\n", style.Warning.Render("⚠"), attempt, wait)
		f.sleep(wait)
	}
	return "", lastErr
}

// parseBondSpawnRootID extracts the spawned molecule root from bd mol bond JSON.
// Handles both legacy output (root_id) and polymorphic output (result_id + id_mapping).
func parseBondSpawnRootID(bondOut []byte, formulaName, beadID, fallbackID string) string {
	rootID, _ := parseBondSpawnRootIDWithStatus(bondOut, formulaName, beadID, fallbackID)
	return rootID
}

func parseBondSpawnRootIDWithStatus(bondOut []byte, formulaName, beadID, fallbackID string) (string, bool) {
	var bondResult struct {
		RootID    string            `json:"root_id"`
		ResultID  string            `json:"result_id"`
		NewEpicID string            `json:"new_epic_id"`
		IDMapping map[string]string `json:"id_mapping"`
	}
	if err := json.Unmarshal(bondOut, &bondResult); err != nil {
		return fallbackID, false
	}

	if len(bondResult.IDMapping) > 0 {
		if mappedID := bondResult.IDMapping[formulaName]; mappedID != "" {
			return mappedID, true
		}
		if !strings.HasPrefix(formulaName, "mol-") {
			if mappedID := bondResult.IDMapping["mol-"+formulaName]; mappedID != "" {
				return mappedID, true
			}
		}
		var onlySpawned string
		for _, mappedID := range bondResult.IDMapping {
			if mappedID == "" || mappedID == beadID {
				continue
			}
			if onlySpawned != "" && onlySpawned != mappedID {
				onlySpawned = ""
				break
			}
			onlySpawned = mappedID
		}
		if onlySpawned != "" {
			return onlySpawned, true
		}
	}

	for _, candidate := range []string{bondResult.RootID, bondResult.ResultID, bondResult.NewEpicID} {
		if candidate != "" && candidate != beadID {
			return candidate, true
		}
	}
	return fallbackID, true
}

// backfillFormulaDefaultVars returns vars plus a --var entry for every variable
// the cooked formula declares with a default, so bd's bond stops demanding values
// the formula already supplies.
//
// bd's bond requires a value for every {{placeholder}} in the cooked proto and
// ignores the formula's own [vars] defaults (beads cmd/bd/mol_bond.go
// buildAttachCloneOpts). gt therefore has to hand it those defaults. Reading them
// from the formula's declarations — rather than a list hard-coded to
// mol-polecat-work — is what lets any formula bond without --var flags (gt-25wi).
// An optional variable without a default is bonded empty.
//
// A required variable with no default and no value that bd left as a
// placeholder (unresolved_vars) is an error naming the formula and the
// variables: bd rejects that bond anyway, and its own message does not say which
// formula wanted them. A required variable the formula never interpolates is not
// an error. Values already in vars are never overwritten.
func backfillFormulaDefaultVars(f *cookedFormula, vars []string) ([]string, error) {
	supplied := make(map[string]bool, len(vars))
	for _, variable := range vars {
		if eq := strings.Index(variable, "="); eq > 0 {
			supplied[variable[:eq]] = true
		}
	}
	unresolved := make(map[string]bool, len(f.UnresolvedVars))
	for _, name := range f.UnresolvedVars {
		unresolved[name] = true
	}

	var missing []string
	for _, v := range f.Vars { // bd sorts them by name
		switch {
		case supplied[v.Name]:
		case v.Default != nil:
			vars = append(vars, v.Name+"="+*v.Default)
		case !v.Required:
			vars = append(vars, v.Name+"=")
		case unresolved[v.Name]:
			missing = append(missing, v.Name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("formula %s: required variable(s) %s have no default; pass --var <name>=<value>",
			f.Formula, strings.Join(missing, ", "))
	}
	return vars, nil
}

// CookFormula cooks a formula once before a batch of pours, so a formula bd
// cannot cook fails the batch before any bead is touched.
func CookFormula(formulaName, workDir, townRoot string) error {
	return realFormulaBD().cook(formulaName, workDir, townRoot)
}

// cook is CookFormula with bd reached through f.
func (f formulaBD) cook(formulaName, workDir, townRoot string) error {
	_, err := cookFormula(formulaName, f.engine(formulaSite{dir: workDir, townRoot: townRoot}), nil)
	return err
}

// isHookedAgentDeadFn is a seam for tests. Production uses isHookedAgentDead.
var isHookedAgentDeadFn = isHookedAgentDead

// isHookedAgentDead checks if the tmux session for a hooked assignee is dead.
// Used by sling to auto-force re-sling when the previous agent has no active session (gt-pqf9x).
// Returns true if the session is confirmed dead. Returns false if alive or if we
// can't determine liveness (conservative: don't auto-force on uncertainty).
func isHookedAgentDead(assignee string) bool {
	sessionName, _ := townRegistry().AssigneeSessionName(assignee)
	if sessionName == "" {
		return false // Unknown format, can't determine
	}
	t := tmux.NewTmux()
	alive, err := t.HasSession(sessionName)
	if err != nil {
		return false // tmux not available or error, be conservative
	}
	return !alive
}

// rigRootForBead resolves the rig directory owning beadID through the prefix
// routes, or "" when the bead's prefix routes nowhere.
func rigRootForBead(townRoot, beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	rigName := beads.GetRigNameForPrefix(townRoot, prefix)
	if rigName == "" {
		return ""
	}
	return filepath.Join(townRoot, rigName)
}

// errBeadRoutesToNoRig means a bead's prefix maps to no rig, so there is no
// rig repo that could hold its polecat branches.
var errBeadRoutesToNoRig = errors.New("bead routes to no rig")

// survivingWorkForBead is the shared surviving-work predicate
// (polecat.WorkSurvival) for beadID: the newest polecat branch — local in the
// rig repo or on origin — carrying a patch that is on neither the rig's
// default branch nor an integration branch, or "" when none does. A non-nil
// error means the answer is unknown, except for polecat.ErrNoRigRepo and
// errBeadRoutesToNoRig, which mean there is no repo, so no branch to protect.
func survivingWorkForBead(townRoot, beadID string) (string, error) {
	rigRoot := rigRootForBead(townRoot, beadID)
	if rigRoot == "" {
		return "", fmt.Errorf("%s: %w", beadID, errBeadRoutesToNoRig)
	}
	return polecat.SurvivingWorkForIssue(rigRoot, beadID)
}

// survivingWorkForBeadFn is a seam for tests.
var survivingWorkForBeadFn = survivingWorkForBead

// noRepoToProtect reports whether a survival error only says there is no repo
// (and therefore no branch) for the bead.
func noRepoToProtect(err error) bool {
	return errors.Is(err, polecat.ErrNoRigRepo) || errors.Is(err, errBeadRoutesToNoRig)
}

// reslingSurvivingWorkGuard is sling's re-sling guard for a bead whose
// holder is dead (gt-3qfp): it refuses when the work survives on a branch, and
// also when survival cannot be verified — an outage must not turn into a
// second polecat started from main over preserved work. It returns nil only
// when there is verifiably nothing to protect. --force bypasses it (caller).
func reslingSurvivingWorkGuard(townRoot, beadID, holder string) error {
	return reslingSurvivingWorkGuardWith(survivingWorkForBeadFn, townRoot, beadID, holder)
}

// reslingSurvivingWorkGuardWith is reslingSurvivingWorkGuard asking
// survivingWork for the bead's surviving branch.
func reslingSurvivingWorkGuardWith(survivingWork func(townRoot, beadID string) (string, error), townRoot, beadID, holder string) error {
	branch, err := survivingWork(townRoot, beadID)
	switch {
	case err != nil && !noRepoToProtect(err):
		return &reslingRefusal{msg: fmt.Sprintf("%s %s: previous holder %s has no active session, and sling cannot verify surviving work (%v); resume with --branch or override with --force",
			dispatch.ReslingRefusalMarker, beadID, holder, err)}
	case branch != "":
		return &reslingRefusal{msg: fmt.Sprintf("%s %s: previous holder %s has no active session, but its branch still carries work that is not on main:\n  %s\nRe-slinging would start a second polecat from main on work that is already preserved.\n  Resume the preserved work:  gt sling %s <target> --branch %s\n  Start fresh anyway:         gt sling %s <target> --force",
			dispatch.ReslingRefusalMarker, beadID, holder, branch, beadID, branch, beadID)}
	}
	return nil
}

// errReslingRefused matches every reslingSurvivingWorkGuard refusal
// (errors.Is). Automated dispatchers (scheduler, convoy and epic feeders)
// treat it as a deferral, not a failure: the bead waits for an operator to
// resume (--branch) or discard (--force) the work, or for survival to become
// verifiable again, and must not burn a dispatch attempt meanwhile.
var errReslingRefused = errors.New(dispatch.ReslingRefusalMarker)

// reslingRefusal is the guard's refusal. Its text starts with
// dispatch.ReslingRefusalMarker, which the daemon's convoy feeder (running gt
// sling as a subprocess) matches on stderr.
type reslingRefusal struct{ msg string }

func (e *reslingRefusal) Error() string { return e.msg }

func (e *reslingRefusal) Is(target error) bool { return target == errReslingRefused }

// feederDispatchTally counts one convoy or epic feeder run's executeSling
// outcomes. A resling refusal is a deferral: it is neither a success nor a
// failed attempt, and a run whose every attempt was deferred is not an error.
type feederDispatchTally struct {
	success, deferred, failed int
	// out receives the per-bead and summary lines; nil is stdout.
	out io.Writer
}

func (t *feederDispatchTally) writer() io.Writer {
	if t.out == nil {
		return os.Stdout
	}
	return t.out
}

// record counts err (nil = dispatched), prints a line for a deferral or a
// failure, and reports whether the dispatch succeeded.
func (t *feederDispatchTally) record(beadID string, err error) bool {
	switch {
	case err == nil:
		t.success++
		return true
	case errors.Is(err, errReslingRefused):
		t.deferred++
		fmt.Fprintf(t.writer(), "  %s %s deferred: %v\n", style.Dim.Render("○"), beadID, err)
	default:
		t.failed++
		fmt.Fprintf(t.writer(), "  %s %s: %v\n", style.Dim.Render("✗"), beadID, err)
	}
	return false
}

// result prints the deferral count and returns an error only when nothing
// was dispatched and at least one attempt really failed.
func (t *feederDispatchTally) result(kind, id string) error {
	if t.deferred > 0 {
		fmt.Fprintf(t.writer(), "  Deferred: %d (a dead holder's work survives or cannot be verified)\n", t.deferred)
	}
	if t.success == 0 && t.failed > 0 {
		return fmt.Errorf("all %d dispatch attempts failed for %s %s", t.failed, kind, id)
	}
	return nil
}

// orphanEpisodeLabels are the witness's cross-cycle memory for an orphaned
// bead (mol-witness-patrol survey-workers step 5): the mayor was told its work
// survives, the last survival answer was unknown, and that unknown run was
// escalated. Each suppresses a repeat notice while present.
var orphanEpisodeLabels = []string{"gt:preserved-orphan", "gt:survival-unknown", "gt:survival-escalated"}

// clearOrphanEpisodeLabels removes the orphan-episode labels from a bead that
// sling just hooked to a new holder. A successful sling ends the episode —
// including one an operator ended with --branch or --force — so a later
// orphaning of the same bead must mail and escalate afresh (gt-vm5g4).
// Best-effort: every bd call is bounded by the beads subprocess timeout, a
// failure only warns, and a bead carrying none of the labels costs one read
// and no write. workDir is the hook write's work dir, so an unrouted bead is
// read from the same database the hook just wrote.
func clearOrphanEpisodeLabels(townRoot, beadID, workDir string) {
	slingStores{}.clearOrphanEpisodeLabels(os.Stdout, townRoot, beadID, workDir)
}

// clearOrphanEpisodeLabels is clearOrphanEpisodeLabels in s's stores, with
// its warnings written to w.
func (s slingStores) clearOrphanEpisodeLabels(w io.Writer, townRoot, beadID, workDir string) {
	if beadID == "" {
		return
	}
	b := s.routedFrom(beads.ResolveHookDir(townRoot, beadID, workDir))
	issue, err := b.Show(beadID)
	if err != nil {
		fmt.Fprintf(w, "  %s Could not read %s to clear orphan labels: %v\n", style.Dim.Render("Warning:"), beadID, err)
		return
	}
	var present []string
	for _, want := range orphanEpisodeLabels {
		for _, have := range issue.Labels {
			if have == want {
				present = append(present, want)
				break
			}
		}
	}
	if len(present) == 0 {
		return
	}
	if err := b.Update(beadID, beads.UpdateOptions{RemoveLabels: present}); err != nil {
		fmt.Fprintf(w, "  %s Could not clear orphan labels %v on %s: %v\n", style.Dim.Render("Warning:"), present, beadID, err)
	}
}

// clearOrphanEpisodeLabelsFn is a seam for tests.
var clearOrphanEpisodeLabelsFn = clearOrphanEpisodeLabels

// survivingBranchesForBead lists every polecat branch on the rig's origin
// remote that encodes beadID, newest first. Unlike survivingBranchForBead it
// does not distinguish "no branch" from "cannot tell": the reassignment record
// is written either way, and an unreachable remote must not block a dispatch.
func survivingBranchesForBead(townRoot, beadID string) []string {
	rigRoot := rigRootForBead(townRoot, beadID)
	if rigRoot == "" {
		return nil
	}
	branches, err := polecat.FindSurvivingBranchesForIssue(rigRoot, beadID)
	if err != nil {
		return nil
	}
	return branches
}

// recordReassignment appends the durable reassignment record to a bead. Call it
// before the write that overwrites the old assignee, or the only surviving copy
// of the old value is the Dolt events table. Best-effort: a beads failure warns
// and returns, because a missing audit line must not abort a dispatch.
func recordReassignment(townRoot, beadID, from, to, requester string) {
	slingStores{}.recordReassignment(survivingBranchesForBead, os.Stdout, townRoot, beadID, from, to, requester)
}

// recordReassignment is recordReassignment in s's stores, with the old
// holder's branches listed by branches and its report written to w.
func (s slingStores) recordReassignment(branches func(townRoot, beadID string) []string, w io.Writer, townRoot, beadID, from, to, requester string) {
	if beadID == "" || from == "" || from == to {
		return
	}
	b := s.routedFrom(beads.ResolveHookDir(townRoot, beadID, ""))
	if err := beads.RecordReassignmentIn(b, beadID, from, to, requester, branches(townRoot, beadID)); err != nil {
		fmt.Fprintf(w, "  %s Could not record reassignment of %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
		return
	}
	fmt.Fprintf(w, "  %s Recorded reassignment of %s: %s -> %s\n", style.Dim.Render("○"), beadID, from, to)
}

// reassignRequester names the actor behind a reassignment for the durable
// record: the slinging polecat when one is doing the sling, else the operator.
func reassignRequester() string {
	if p := os.Getenv("GT_POLECAT"); p != "" {
		return p
	}
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "gt-sling"
}

// hookBeadWithRetry hooks a bead to a target agent with exponential backoff retry
// and post-hook verification. This ensures the hook sticks even under Dolt concurrency.
// Fails fast on configuration/initialization errors (gt-2ra).
// See: https://github.com/steveyegge/gastown/issues/148
func hookBeadWithRetry(beadID, targetAgent, hookDir string) error {
	return hookBeadWithRetryWithTownRoot(beadID, targetAgent, hookDir, "")
}

// hookVerifyFn builds the read-back verifier hookBeadWithRetryWithTownRoot
// confirms each landed hook write with. It is a seam: a test driving a stub bd,
// which does not track hook state, replaces it with one returning nil so the
// write is not read back.
var hookVerifyFn = func(townRoot string) func(beadID string) (*beadInfo, error) {
	return func(id string) (*beadInfo, error) { return getBeadInfoFromTownRoot(townRoot, id) }
}

func hookBeadWithRetryWithTownRoot(beadID, targetAgent, hookDir, townRoot string) error {
	return slingStores{}.hookWithRetry(hookVerifyFn(townRoot), beadID, targetAgent, hookDir)
}

// hookWithRetry is hookBeadWithRetryWithTownRoot with the hook written to
// hookDir's database in s's stores (pinned, so the write auto-commits) and
// each landed write read back by verify (nil: no read-back).
func (s slingStores) hookWithRetry(verify func(beadID string) (*beadInfo, error), beadID, targetAgent, hookDir string) error {
	const maxRetries = 10
	const baseBackoff = 500 * time.Millisecond
	const maxBackoff = 30 * time.Second

	hooked := "hooked"
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := s.pinnedAt(hookDir).Update(beadID, beads.UpdateOptions{Status: &hooked, Assignee: &targetAgent})
		if err != nil {
			lastErr = err
			// Fail fast on config/init errors — retrying won't help (gt-2ra)
			if isSlingConfigError(err) {
				return fmt.Errorf("hooking bead failed (non-retryable Dolt/beads failure — not retrying): %w\nSafe next action: run `gt dolt status` and `bd show %s` to verify whether a durable hook exists before re-slinging", err, beadID)
			}
			if attempt < maxRetries {
				backoff := slingBackoff(attempt, baseBackoff, maxBackoff)
				fmt.Printf("%s Hook attempt %d failed, retrying in %v...\n", style.Warning.Render("⚠"), attempt, backoff)
				clockwork.NewRealClock().Sleep(backoff)
				continue
			}
			return fmt.Errorf("hooking bead after %d attempts: %w", maxRetries, err)
		}

		if verify == nil {
			break
		}

		verifyInfo, verifyErr := verify(beadID)
		if verifyErr != nil {
			lastErr = fmt.Errorf("verifying hook: %w", verifyErr)
			if attempt < maxRetries {
				backoff := slingBackoff(attempt, baseBackoff, maxBackoff)
				fmt.Printf("%s Hook verification failed, retrying in %v...\n", style.Warning.Render("⚠"), backoff)
				clockwork.NewRealClock().Sleep(backoff)
				continue
			}
			return fmt.Errorf("verifying hook after %d attempts: %w", maxRetries, lastErr)
		}

		if verifyInfo.Status != "hooked" || verifyInfo.Assignee != targetAgent {
			lastErr = fmt.Errorf("hook did not stick: status=%s, assignee=%s (expected hooked, %s)",
				verifyInfo.Status, verifyInfo.Assignee, targetAgent)
			if attempt < maxRetries {
				backoff := slingBackoff(attempt, baseBackoff, maxBackoff)
				fmt.Printf("%s %v, retrying in %v...\n", style.Warning.Render("⚠"), lastErr, backoff)
				clockwork.NewRealClock().Sleep(backoff)
				continue
			}
			return fmt.Errorf("hook failed after %d attempts: %w", maxRetries, lastErr)
		}

		break
	}

	return nil
}

var hookBeadWithRetryFn = hookBeadWithRetry
var hookBeadWithRetryWithTownRootFn = hookBeadWithRetryWithTownRoot

// slingBackoff calculates exponential backoff with ±25% jitter for a given attempt (1-indexed).
// Formula: base * 2^(attempt-1) * (1 ± 25% random), capped at max.
func slingBackoff(attempt int, base, max time.Duration) time.Duration { //nolint:unparam // base is parameterized for testability
	backoff := base
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff > max {
			backoff = max
			break
		}
	}
	// Apply ±25% jitter
	jitter := 1.0 + (rand.Float64()-0.5)*0.5 // range [0.75, 1.25]
	result := time.Duration(float64(backoff) * jitter)
	if result > max {
		result = max
	}
	return result
}

// isSlingConfigError returns true if the error indicates a configuration or
// initialization problem rather than a transient failure. Config errors should
// NOT be retried because they will fail identically on every attempt (gt-2ra).
func isSlingConfigError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not initialized") ||
		strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "table not found") ||
		strings.Contains(msg, "issue_prefix") ||
		strings.Contains(msg, "no database") ||
		strings.Contains(msg, "database not found") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "circuit breaker") ||
		strings.Contains(msg, "server appears down") ||
		strings.Contains(msg, "server down") ||
		strings.Contains(msg, "server is not running") ||
		strings.Contains(msg, "server may not be running")
}

// loadRigCommandVars reads rig settings and returns --var key=value strings
// for all configured build pipeline commands (setup, typecheck, lint, test, build)
// and the default branch (base_branch). Only non-empty values are included.
//
// Settings are resolved in priority order (lowest to highest):
//  1. Rig root config.json merge_queue (floor — operator-set at onboarding, gt-me9t)
//  2. Repository defaults: <rig>/mayor/rig/.gastown/settings.json (committed to git)
//  3. Rig-local overrides: <rig>/settings/config.json (operator tuning, final override)
//  4. User --var flags (handled by caller, not here)
func loadRigCommandVars(townRoot, rig string) []string {
	if townRoot == "" || rig == "" {
		return nil
	}
	var vars []string

	// Load default_branch from rig root config.json (single source of truth per 5ee9abcc).
	// This sets base_branch for formula instantiation so polecats fork from the right branch.
	rigCfg, err := rigpkg.LoadRigConfig(filepath.Join(townRoot, rig))
	if err == nil && rigCfg != nil && rigCfg.DefaultBranch != "" {
		vars = append(vars, fmt.Sprintf("base_branch=%s", rigCfg.DefaultBranch))
	}

	// Resolve gate commands via the shared rig root -> repo -> rig-local
	// precedence chain. This is the same resolver gt done's --pre-verified
	// guard, buildRefineryPatrolVars, mq_submit and resolveSetupCommand all
	// call, so no site can read different state (gt-k4sy, gt-egiv).
	mq := rigpkg.ResolveMergeQueueConfig(townRoot, rig)
	if mq == nil {
		return vars
	}

	if mq.SetupCommand != "" {
		vars = append(vars, fmt.Sprintf("setup_command=%s", mq.SetupCommand))
	}
	if mq.TypecheckCommand != "" {
		vars = append(vars, fmt.Sprintf("typecheck_command=%s", mq.TypecheckCommand))
	}
	if mq.LintCommand != "" {
		vars = append(vars, fmt.Sprintf("lint_command=%s", mq.LintCommand))
	}
	if mq.TestCommand != "" {
		vars = append(vars, fmt.Sprintf("test_command=%s", mq.TestCommand))
	}
	if mq.BuildCommand != "" {
		vars = append(vars, fmt.Sprintf("build_command=%s", mq.BuildCommand))
	}
	if mq.MergeStrategy != "" {
		vars = append(vars, fmt.Sprintf("merge_strategy=%s", mq.MergeStrategy))
	}
	if mq.IsRequireReviewEnabled() {
		vars = append(vars, "require_review=true")
	}
	return vars
}

// shouldAcceptPermissionWarning checks if the agent's harness emits a
// bypass-permissions warning on startup that must be acknowledged via tmux.
// preset/ok come from SessionAgentPreset; a session without GT_AGENT is
// Claude by default.
func shouldAcceptPermissionWarning(agentName string, preset *config.AgentPresetInfo, ok bool) bool {
	if agentName == "" {
		preset, ok = config.GetAgentPreset(config.AgentClaude), true
	}
	return ok && preset != nil && preset.EmitsPermissionWarning
}

// updateAgentMode updates the mode field on the agent bead.
// This is needed so the stuck detector can read the mode from agent fields
// and apply appropriate thresholds (ralphcats get longer leash).
func updateAgentMode(agentID, mode, workDir, townBeadsDir string) {
	_ = townBeadsDir // Not used - BEADS_DIR breaks redirect mechanism

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return
	}
	if workDir == "" {
		workDir = townRoot
	}

	agentBeadID := agentIDToBeadID(agentID, townRoot)
	if agentBeadID == "" {
		return
	}

	agentWorkDir := beads.ResolveHookDir(townRoot, agentBeadID, workDir)
	bd := beads.New(agentWorkDir)
	if err := bd.UpdateAgentDescriptionFields(agentBeadID, beads.AgentFieldUpdates{Mode: &mode}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't set agent %s mode: %v\n", agentBeadID, err)
	}
}

// clearReassignedPolecatState clears agent_state and hook_bead on a polecat's
// agent bead after its work was force-reassigned to a different agent (gt-skwt).
//
// Nothing else touches these fields, so without this the outgoing polecat
// keeps agent_state=working and hook_bead=<old bead> indefinitely, and a dead
// session paired with an "active" agent state reads as a permanent zombie.
// Clearing them here, synchronously, on the one code path guaranteed to run,
// closes that gap.
func clearReassignedPolecatState(townRoot, assignee string) {
	if townRoot == "" {
		return
	}
	agentBeadID := agentIDToBeadID(assignee, townRoot)
	if agentBeadID == "" {
		return
	}

	agentWorkDir := beads.ResolveHookDir(townRoot, agentBeadID, townRoot)
	bd := beads.New(agentWorkDir)

	emptyHook := ""
	if err := bd.UpdateAgentDescriptionFields(agentBeadID, beads.AgentFieldUpdates{HookBead: &emptyHook}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear hook_bead on %s: %v\n", agentBeadID, err)
	}
	idle := string(beads.AgentStateIdle)
	if err := bd.UpdateAgentState(agentBeadID, idle); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear agent_state on %s: %v\n", agentBeadID, err)
	}
}

// lookupPriorAttempt checks if there are existing open MRs for the given issue.
// If found, returns formula variables with the prior branch name so the new
// polecat can cherry-pick or reference prior work instead of starting from scratch.
// Returns nil if no prior attempt exists. (GH#gt-zqvj)
func lookupPriorAttempt(beadsDir, issueID string) []string {
	bd := beads.New(beadsDir)
	mrs, err := bd.FindOpenMRsForIssue(issueID)
	if err != nil || len(mrs) == 0 {
		return nil
	}

	// Use the most recent MR (last in list) as the prior attempt.
	prior := mrs[len(mrs)-1]
	fields := beads.ParseMRFields(prior)
	if fields == nil || fields.Branch == "" {
		return nil
	}

	vars := []string{
		fmt.Sprintf("prior_branch=%s", fields.Branch),
	}
	if fields.CloseReason != "" {
		vars = append(vars, fmt.Sprintf("prior_failure=%s", fields.CloseReason))
	}
	return vars
}
