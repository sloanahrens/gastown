package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/style"
)

// SlingParams captures everything needed to sling one bead to a rig.
// This is the serialization boundary for queue dispatch: at enqueue time,
// these fields are stored as queue metadata; at dispatch time, they are
// reconstructed into a SlingParams and passed to executeSling().
type SlingParams struct {
	// What to sling
	BeadID      string // Base bead
	FormulaName string // Formula to apply ("mol-polecat-work", user formula, or "")
	RigName     string // Target rig (always a rig for queue)

	// CLI flag passthrough
	Args         string   // --args
	Vars         []string // --var (key=value pairs)
	Merge        string   // --merge (convoy strategy)
	BaseBranch   string   // --base-branch
	ResumeBranch string   // --branch / --pr (resume existing PR branch, gh#3602)
	Account      string   // --account
	Agent        string   // --agent
	// AgentBeatsRoute makes Agent outrank the bead's route:* labels in the
	// polecat pool (spec dispatcher only; see SlingSpawnOptions).
	AgentBeatsRoute bool
	NoConvoy        bool   // --no-convoy
	Owned           bool   // --owned
	NoMerge         bool   // --no-merge
	Force           bool   // --force
	HookRawBead     bool   // --hook-raw-bead
	NoBoot          bool   // --no-boot
	Mode            string // --ralph: "" (normal) or "ralph"
	ReviewOnly      bool   // --review-only: review and report back only, no merge/commit/push

	// Execution behavior (set by caller, not serialized to queue)
	SkipCook         bool   // Batch optimization: formula already cooked
	FormulaFailFatal bool   // true=rollback+error (single/queue), false=hook raw bead (batch)
	CallerContext    string // Identifies the caller for shutdown messages (e.g., "queue-dispatch", "batch-sling")
	TownRoot         string
	BeadsDir         string

	// SkipDuplicateCheck disables the pre-dispatch content duplicate check
	// (gt-mcq). No production dispatcher sets it: convoy, epic and capacity-queue
	// dispatch are each a bead's first dispatch and run the check (gt-skk7,
	// gt-eisp2). Tests that target a later guard set it to reach that guard.
	SkipDuplicateCheck bool
}

// SlingResult captures the outcome of executeSling for caller-level tracking.
type SlingResult struct {
	BeadID           string
	PolecatName      string
	SpawnInfo        *SpawnedPolecatInfo
	Success          bool
	ErrMsg           string
	AttachedMolecule string
}

// buildSlingFormulaVars assembles the ordered var list for formula
// instantiation: rig defaults first, then user --var overrides, then
// spawn-derived base_branch/resume_branch.
//
// gt-a8i3: base_branch and resume_branch are DISTINCT and must never
// collapse into each other. base_branch (from spawnBaseBranch, i.e.
// SpawnedPolecatInfo.BaseBranch) is the merge-target branch `gt done`/
// `gt mq submit` read for --target; resume_branch (from resumeBranch, i.e.
// SlingParams.ResumeBranch / --branch / --pr) is only the polecat's resumed
// working branch. A formula var list that conflated the two previously
// caused a resume dispatch to submit a self-targeted MR.
func buildSlingFormulaVars(rigCmdVars, userVars []string, spawnBaseBranch, resumeBranch string) []string {
	vars := append(append([]string(nil), rigCmdVars...), userVars...)
	if spawnBaseBranch != "" && spawnBaseBranch != "main" {
		vars = append(vars, fmt.Sprintf("base_branch=%s", spawnBaseBranch))
	}
	if resumeBranch != "" {
		vars = append(vars, fmt.Sprintf("resume_branch=%s", resumeBranch))
	}
	return vars
}

// executeSling performs the unified per-bead polecat/rig dispatch.
// Batch sling and queue dispatch call this function. The single-sling path
// (runSling) retains its own implementation for now (handles dogs, mayor,
// nudge, and other non-rig targets). See TODO in sling.go.
//
// Caller responsibilities (NOT handled by executeSling):
//   - Cross-rig guard: callers must call checkCrossRigGuard() before executeSling
//     to verify the bead's prefix matches the target rig. Batch sling does this
//     pre-loop; queue dispatch skips the guard because the bead prefix was
//     validated at enqueue time and is immutable.
//   - wakeRigAgents: callers must call wakeRigAgents() after the dispatch loop
//     when NoBoot is false. Batch sling calls it post-loop; queue dispatch sets
//     NoBoot=true to avoid lock contention in the daemon.
//
// Steps:
//  1. Get bead info + status check
//  2. Burn stale molecules (if formula and force)
//  3. Spawn polecat (via spawnPolecatForSling)
//  4. Auto-convoy (if !NoConvoy)
//  5. Cook formula (unless SkipCook)
//  6. Instantiate formula on bead (wisp + bond)
//  7. Hook bead with retry
//  8. Log sling event
//  9. Update agent hook_bead state
//  10. Store fields in bead (dispatcher, args, attached_molecule, no_merge)
//  11. Create Dolt branch
//  12. Start polecat session
func executeSling(params SlingParams) (*SlingResult, error) {
	return realSlingDeps().executeSling(params)
}

// executeSling is the package executeSling on these collaborators.
func (d *slingDeps) executeSling(params SlingParams) (*SlingResult, error) {
	// A seat the pool claimed for this dispatch stops standing when the
	// dispatch returns: the tmux session exists by then and is what the pool
	// counts, or the dispatch failed and no session will exist. Every spawn
	// this function makes is started here, so this is the boundary that
	// guarantees the claim cannot outlive the dispatch — the batch and
	// scheduler callers (sling_batch.go, scheduler_convoy.go, scheduler_epic.go,
	// capacity_dispatch.go) keep the process alive afterwards, where a leaked
	// claim would hold its seat for poolSeatClaimTTL (gt-t8q5).
	defer d.releaseSeat()

	townRoot := params.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = d.findTown()
		if err != nil {
			return nil, err
		}
	}

	// Acquire per-bead flock to prevent concurrent dispatch races (TOCTOU).
	// The CLI path (runSling) has its own flock; this closes the gap where
	// batch sling and queue dispatch could race against each other or against
	// a concurrent CLI invocation.
	releaseLock, err := d.lockBead(townRoot, params.BeadID)
	if err != nil {
		return &SlingResult{BeadID: params.BeadID, ErrMsg: err.Error()}, err
	}
	defer releaseLock()

	beadsDir := params.BeadsDir
	if beadsDir == "" {
		beadsDir = filepath.Join(townRoot, ".beads")
	}

	result := &SlingResult{
		BeadID: params.BeadID,
	}

	// 0. Refuse an e-stopped, parked or docked rig before dispatching
	// (gt-4k3fj.4, gt-4owfd.1, gt-11y).
	if params.RigName != "" {
		if label, err := slingBlocked(townRoot, params.RigName, d.estopOn, d.rigParked); err != nil {
			result.ErrMsg = label
			return result, err
		}
	}

	// 1. Get bead info + status check
	info, err := d.beadInfoInTown(townRoot, params.BeadID)
	if err != nil {
		result.ErrMsg = err.Error()
		return result, fmt.Errorf("could not get bead info: %w", err)
	}

	// Guard against dispatching closed/tombstone beads (defense-in-depth).
	// Not bypassed by --force — if you need to re-dispatch, reopen the bead first.
	if info.Status == "closed" || info.Status == "tombstone" {
		result.ErrMsg = "already " + info.Status
		return result, fmt.Errorf("bead %s is %s (work already completed)", params.BeadID, info.Status)
	}

	// Save explicit force state before dead-agent auto-force, so the deferred
	// gate below still requires an explicit --force for deferred beads.
	explicitForce := params.Force

	if (info.Status == "pinned" || info.Status == "hooked" || info.Status == "in_progress") && !params.Force {
		// Auto-force when hooked/in_progress agent's session is confirmed dead (gt-npzy, GH#1380).
		// Mirrors the dead-agent detection in runSling (sling.go) so that
		// programmatic dispatch also handles stale hooks from nuked polecats.
		if (info.Status == "hooked" || info.Status == "in_progress") && info.Assignee != "" && d.agentDead(info.Assignee) {
			// A dead holder does not mean dead work: refuse when the work
			// survives on a branch or survival cannot be verified, exactly as
			// runSling does (gt-3qfp, gt-vm5g4). Scheduler callers read the
			// refusal (errReslingRefused) as a deferral.
			if params.ResumeBranch == "" {
				if err := d.survivingWorkGuard(townRoot, params.BeadID, info.Assignee); err != nil {
					result.ErrMsg = err.Error()
					return result, err
				}
			}
			fmt.Fprintf(d.out, "  %s Hooked agent %s has no active session, auto-forcing dispatch...\n",
				style.Warning.Render("⚠"), info.Assignee)
			params.Force = true
		} else {
			result.ErrMsg = "already " + info.Status
			return result, fmt.Errorf("already %s (use --force to re-sling)", info.Status)
		}
	}

	// Guard against slinging deferred beads (gt-1326mw).
	// Uses explicitForce (not params.Force) so dead-agent auto-force doesn't
	// accidentally bypass the deferred gate.
	if isDeferredBead(info) && !explicitForce {
		result.ErrMsg = "deferred"
		return result, fmt.Errorf("bead %s is deferred (use --force to override)", params.BeadID)
	}

	// Guard against dispatching a bead the human operator owns (gt-21pl0).
	// Mirrors the guard in runSling so the batch and queue callers — which
	// include the daemon's convoy feeders — refuse one too, and read the
	// refusal as a deferral rather than a failed dispatch.
	if reason := dispatch.OperatorReservation(info.Labels, info.Assignee); reason != "" && !explicitForce {
		result.ErrMsg = "operator-reserved"
		return result, fmt.Errorf("%s %s is the operator's work (%s)\nAn agent does not take it. Use --force to sling it to one anyway",
			dispatch.SlingRefusalMarker, params.BeadID, reason)
	}

	// Content duplicate check (gt-mcq): refuse a bead whose named tests and
	// files already appear on open or recently-closed work in this rig. Placed
	// after the already-hooked guard so an idempotent re-sling of the same bead
	// is not reported as a duplicate of itself, and skipped under --force, which
	// is the documented override.
	var dupCandidate *duplicateCandidate
	if !params.SkipDuplicateCheck && !params.Force {
		var matches []duplicateMatch
		var checkErr error
		dupCandidate, matches, checkErr = d.checkDuplicates(townRoot, params.BeadID, info)
		if checkErr != nil {
			fmt.Fprintf(d.out, "  %s %v\n", style.Dim.Render("Warning:"), checkErr)
		}
		if decision := decideSlingDuplicates(params.BeadID, matches); decision.Message != "" {
			if decision.Blocked {
				result.ErrMsg = errSlingDuplicateContent.Error()
				return result, errors.New(decision.Message)
			}
			_, _ = fmt.Fprint(d.out, decision.Message)
		}
	}

	if params.RigName != "" {
		if err := d.verifyInTargetRig(params.BeadID, params.RigName, townRoot); err != nil {
			result.ErrMsg = err.Error()
			return result, err
		}
	}

	// Clear the outgoing polecat's state when force-stealing a bead from it.
	// Mirrors the same logic in runSling (sling.go).
	if (info.Status == "hooked" || info.Status == "in_progress") && params.Force && info.Assignee != "" {
		assigneeParts := strings.Split(info.Assignee, "/")
		if len(assigneeParts) >= 3 && assigneeParts[1] == "polecats" {
			// gt-skwt: clear the outgoing polecat's agent-bead state now,
			// synchronously (see clearReassignedPolecatState).
			d.clearReassigned(townRoot, info.Assignee)
		}
	}

	// 2. Burn stale molecules (if formula applies)
	if params.FormulaName != "" {
		existingMolecules, err := d.collectMolecules(info, params.BeadID, townRoot)
		if err != nil {
			result.ErrMsg = fmt.Sprintf("molecule check failed: %v", err)
			return result, fmt.Errorf("checking existing molecule bonds: %w", err)
		}
		if len(existingMolecules) > 0 {
			// Auto-burn when bead is unassigned (molecules are definitionally stale),
			// or when the assigned agent's session is dead. This unblocks the daemon's
			// stranded convoy scan which never passes --force.
			stale := params.Force ||
				(info.Assignee == "" && (info.Status == "open" || info.Status == "in_progress")) ||
				(info.Assignee != "" && d.agentDead(info.Assignee))
			if stale {
				fmt.Fprintf(d.out, "  %s Burning %d stale molecule(s): %s\n",
					style.Warning.Render("⚠"), len(existingMolecules), strings.Join(existingMolecules, ", "))
				if err := d.burnMolecules(existingMolecules, params.BeadID, townRoot); err != nil {
					result.ErrMsg = fmt.Sprintf("burn failed: %v", err)
					return result, fmt.Errorf("burning stale molecules: %w", err)
				}
			} else {
				result.ErrMsg = "has existing molecule(s)"
				return result, fmt.Errorf("bead %s has existing molecule(s) (use --force)", params.BeadID)
			}
		}
	}

	// 3. Spawn polecat (via spawnPolecatForSling)
	spawnOpts := SlingSpawnOptions{
		TownRoot:        townRoot,
		Force:           params.Force,
		Account:         params.Account,
		HookBead:        params.BeadID,
		Agent:           params.Agent,
		AgentBeatsRoute: params.AgentBeatsRoute,
		BaseBranch:      params.BaseBranch,
		ResumeBranch:    params.ResumeBranch,
		// Create is always true for rig targets: executeSling only handles
		// rig-targeted dispatch (batch sling + queue dispatch), where a fresh
		// polecat must be spawned. The single-sling path (runSling) handles
		// the --create flag for non-rig targets via resolveTarget.
		Create: true,
	}
	spawnInfo, err := d.spawnPolecat(params.RigName, spawnOpts)
	if err != nil {
		result.ErrMsg = err.Error()
		return result, fmt.Errorf("failed to spawn polecat: %w", err)
	}
	result.SpawnInfo = spawnInfo
	result.PolecatName = spawnInfo.PolecatName
	spawnInfo.originalHold = &beadHold{Status: info.Status, Assignee: info.Assignee}

	targetAgent := spawnInfo.AgentID()
	hookWorkDir := spawnInfo.ClonePath

	// 4. Auto-convoy (if !NoConvoy)
	convoyID := ""
	rollbackSpawnedPolecat := func(rollbackBeadID, reason string) {
		fmt.Fprintf(d.out, "  %s %s, rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), reason, spawnInfo.PolecatName)
		d.rollbackArtifacts(spawnInfo, rollbackBeadID, hookWorkDir, convoyID)
		d.restoreRawFields(rollbackBeadID, townRoot, hookWorkDir, info)
		if params.Force && info.Status == "pinned" {
			d.restorePinned(townRoot, params.BeadID, info.Assignee)
		}
	}
	if !params.NoConvoy {
		existingConvoy := d.trackedByConvoy(params.BeadID)
		if existingConvoy == "" {
			var err error
			// Persist the requested agent and formula so a convoy re-feed
			// keeps them (gt-yg24, gt-4lor).
			convoyID, err = d.createConvoy(params.BeadID, info.Title, params.Owned, params.Merge, params.BaseBranch, params.Agent, params.FormulaName)
			if err != nil {
				fmt.Fprintf(d.out, "  %s Could not create auto-convoy: %v\n", style.Dim.Render("Warning:"), err)
			} else {
				fmt.Fprintf(d.out, "  %s Created convoy %s\n", style.Bold.Render("→"), convoyID)
			}
		} else {
			fmt.Fprintf(d.out, "  %s Already tracked by convoy %s\n", style.Dim.Render("○"), existingConvoy)
		}
	}

	// 5. Cook formula (unless SkipCook)
	formulaCooked := params.SkipCook
	if params.FormulaName != "" && !formulaCooked {
		workDir := d.hookDir(townRoot, params.BeadID, hookWorkDir)
		if err := d.cook(params.FormulaName, workDir, townRoot); err != nil {
			if params.FormulaFailFatal {
				// Rollback spawned polecat on fatal cook failure
				rollbackSpawnedPolecat(params.BeadID, "Formula cook failed")
				result.ErrMsg = fmt.Sprintf("cook failed: %v", err)
				return result, fmt.Errorf("cooking formula %s: %w", params.FormulaName, err)
			}
			fmt.Fprintf(d.out, "  %s Could not cook formula %s: %v\n", style.Dim.Render("Warning:"), params.FormulaName, err)
		} else {
			formulaCooked = true
		}
	}

	// 6. Instantiate formula on bead (wisp + bond)
	beadToHook := params.BeadID
	attachedMoleculeID := ""
	var allVars []string
	varsForAttachment := append([]string(nil), params.Vars...)
	formulaVarsForAttachment := strings.Join(varsForAttachment, "\n")
	if params.FormulaName != "" && formulaCooked {
		// Auto-inject rig command vars as defaults (user --var flags override)
		rigCmdVars := d.rigCommandVars(townRoot, params.RigName)
		// Build per-bead vars: rig defaults first, then user vars (higher priority),
		// then spawn-derived base_branch/resume_branch (gt-a8i3: kept distinct).
		allVars = buildSlingFormulaVars(rigCmdVars, params.Vars, spawnInfo.BaseBranch, params.ResumeBranch)

		// GH#gt-zqvj: Inject prior attempt context when re-dispatching an issue
		// that already has an open MR from a previous polecat. The new polecat
		// gets the old branch name so it can cherry-pick prior work instead of
		// starting from scratch.
		if priorVars := d.priorAttempt(beadsDir, params.BeadID); len(priorVars) > 0 {
			allVars = append(allVars, priorVars...)
			fmt.Fprintf(d.out, "  %s Prior attempt found — context injected for polecat\n", style.Dim.Render("↻"))
		}
		varsForAttachment = append([]string(nil), allVars...)
		formulaVarsForAttachment = strings.Join(allVars, "\n")
		formulaResult, err := d.instantiateFormula(context.Background(), params.FormulaName, params.BeadID, info.Title, hookWorkDir, townRoot, allVars)
		if err != nil {
			if params.FormulaFailFatal {
				// Rollback spawned polecat on fatal formula failure
				rollbackSpawnedPolecat(params.BeadID, "Formula instantiation failed")
				result.ErrMsg = fmt.Sprintf("formula failed: %v", err)
				return result, fmt.Errorf("instantiating formula %s: %w", params.FormulaName, err)
			}
			// Best-effort: in batch mode, a formula instantiation failure should not abort or rollback the
			// spawned polecat. We still hook the raw bead so work can proceed (e.g., missing required vars).
			fmt.Fprintf(d.out, "  %s Could not apply formula: %v (hooking raw bead)\n", style.Dim.Render("Warning:"), err)
		} else {
			fmt.Fprintf(d.out, "  %s Formula %s applied\n", style.Bold.Render("✓"), params.FormulaName)
			beadToHook = formulaResult.BeadToHook
			attachedMoleculeID = formulaResult.WispRootID
			if len(formulaResult.FormulaVars) > 0 {
				allVars = formulaResult.FormulaVars
				varsForAttachment = append([]string(nil), allVars...)
				formulaVarsForAttachment = strings.Join(allVars, "\n")
			}
		}
	}
	result.AttachedMolecule = attachedMoleculeID

	actor := d.actor()
	fieldUpdates := beadFieldUpdates{
		Dispatcher:       actor,
		Args:             params.Args,
		Vars:             varsForAttachment,
		AttachedMolecule: attachedMoleculeID,
		NoMerge:          params.NoMerge,
		ReviewOnly:       params.ReviewOnly,
		Mode:             &params.Mode,
		FormulaVars:      formulaVarsForAttachment,
	}
	if params.FormulaName != "" {
		if attachedMoleculeID != "" {
			fieldUpdates.AttachedFormula = params.FormulaName
		} else {
			fieldUpdates.ClearAttachment = true
			fieldUpdates.Vars = nil
			fieldUpdates.FormulaVars = ""
		}
	}

	// 7. Hook bead with retry
	// Acquire per-assignee lock to serialize concurrent hook writes (issue #3114).
	assigneeUnlock, assigneeLockErr := d.lockAssignee(townRoot, targetAgent)
	if assigneeLockErr != nil {
		d.cleanupSpawned(spawnInfo, params.RigName, convoyID)
		result.ErrMsg = "assignee lock failed"
		return result, fmt.Errorf("serializing hook write for %s: %w", targetAgent, assigneeLockErr)
	}
	defer assigneeUnlock()
	if attachedMoleculeID == "" && (params.NoMerge || params.ReviewOnly) {
		if err := d.storeFields(townRoot, beadToHook, fieldUpdates); err != nil {
			d.cleanupSpawned(spawnInfo, params.RigName, convoyID)
			d.restoreRawFields(beadToHook, townRoot, hookWorkDir, info)
			result.ErrMsg = "raw sling metadata failed"
			return result, fmt.Errorf("storing raw sling metadata before hook: %w", err)
		}
	}
	hookDir := d.hookDir(townRoot, beadToHook, hookWorkDir)
	// The hook write below replaces the base bead's assignee (beadToHook is the
	// wisp when a formula applies). Record the outgoing value on the base bead
	// first: assignee keeps only the last writer, so afterwards the previous
	// polecat's branch is unreachable from the bead (gt-zd7c).
	requester := params.CallerContext
	if requester == "" {
		requester = d.requester()
	}
	d.recordReassignment(townRoot, params.BeadID, info.Assignee, targetAgent, requester)
	if err := d.hook(beadToHook, targetAgent, hookDir, townRoot); err != nil {
		// Clean up all partial sling state, including raw metadata stored before hook.
		rollbackSpawnedPolecat(beadToHook, "Hook failed")
		result.ErrMsg = "hook failed"
		return result, fmt.Errorf("failed to hook bead: %w", err)
	}

	fmt.Fprintf(d.out, "  %s Work attached to %s\n", style.Bold.Render("✓"), spawnInfo.PolecatName)
	// Labels live on the base bead even when a formula wisp was hooked.
	d.clearOrphanLabels(townRoot, params.BeadID, hookWorkDir)

	// The bead is dispatched now, so later dispatches in this process should
	// see it in the pool even though their snapshot predates this hook.
	d.noteDispatched(townRoot, dupCandidate)

	// 8. Log sling event
	_ = d.logFeed(events.TypeSling, actor, events.SlingPayload(beadToHook, targetAgent))

	// 9. Update agent hook_bead state
	d.updateAgentHook(targetAgent, beadToHook, hookWorkDir, beadsDir)

	// 10. Store fields in bead (dispatcher, args, attached_molecule, no_merge, mode)
	// Use beadToHook for the update target (may differ from beadID when formula-on-bead)
	if err := d.storeFields(townRoot, beadToHook, fieldUpdates); err != nil {
		fmt.Fprintf(d.out, "  %s Could not store fields in bead: %v\n", style.Dim.Render("Warning:"), err)
	}

	// Update agent bead mode for stuck-detector Ralph thresholds. Reuse/reset clears stale mode.
	if params.Mode != "" {
		d.updateAgentMode(targetAgent, params.Mode, hookWorkDir, beadsDir)
	}

	// 11. Start polecat session
	pane, err := d.startSession(spawnInfo)
	if err != nil {
		fmt.Fprintf(d.out, "  %s Could not start session: %v, cleaning up partial state...\n", style.Dim.Render("✗"), err)
		rollbackSpawnedPolecat(beadToHook, "Session failed")
		result.ErrMsg = fmt.Sprintf("session failed: %v", err)
		return result, fmt.Errorf("starting polecat session: %w", err)
	}
	fmt.Fprintf(d.out, "  %s Session started for %s\n", style.Bold.Render("▶"), spawnInfo.PolecatName)
	_ = pane

	result.Success = true
	return result, nil
}

// findTownRoot is defined in hook.go
