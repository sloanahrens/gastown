package sling

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/style"
)

// Run dispatches one bead to one rig in process: it decides whether the
// dispatch is allowed, prepares the polecat, attaches the work and starts the
// session.
//
// Its callers are the single-sling cobra path for rig targets, batch and queue
// dispatch, and the daemon's convoy feeder. Two caller responsibilities are
// NOT handled here: the cross-rig guard (callers verify the bead's prefix
// matches the target rig before Run) and waking the rig's agents after the
// dispatch loop when NoBoot is false.
//
// ctx cancels the dispatch. Run checks it at every step boundary and returns
// its error there, rolling back a spawn that has already happened. The
// mechanisms Deps supplies do not take a context — the bd, git and tmux calls
// behind them are internal/cmd's — so a cancellation lands at the next
// boundary rather than inside a blocking call. That is what the daemon's
// shutdown relies on: it cancels the context the feeder dispatches under, and
// the dispatch stops at the next step instead of running to completion.
func Run(ctx context.Context, d *Deps, opts Options) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := &Result{
		BeadID: opts.BeadID,
	}
	// A seat the pool claimed for this dispatch stops standing when the
	// dispatch returns: the tmux session exists by then and is what the pool
	// counts, or the dispatch failed and no session will exist. Every spawn
	// this function makes is started here, so this is the boundary that
	// guarantees the claim cannot outlive the dispatch — the batch and
	// scheduler callers keep the process alive afterwards, where a leaked
	// claim would hold its seat for the pool's claim TTL (gt-t8q5).
	defer func() { d.ReleaseSeat(result.SpawnInfo) }()

	townRoot := opts.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = d.FindTown()
		if err != nil {
			return nil, err
		}
	}

	// Acquire per-bead flock to prevent concurrent dispatch races (TOCTOU).
	// The CLI path has its own flock; this closes the gap where batch dispatch
	// and queue dispatch could race against each other or against a concurrent
	// CLI invocation.
	releaseLock, err := d.LockBead(townRoot, opts.BeadID)
	if err != nil {
		return &Result{BeadID: opts.BeadID, ErrMsg: err.Error()}, err
	}
	defer releaseLock()

	beadsDir := opts.BeadsDir
	if beadsDir == "" {
		beadsDir = filepath.Join(townRoot, ".beads")
	}

	// The dispatch is cancellable from here on: nothing has been written yet,
	// so a cancellation costs nothing to honor.
	if err := canceled(ctx); err != nil {
		result.ErrMsg = err.Error()
		return result, err
	}

	// 0. Refuse an e-stopped, parked or docked rig before dispatching
	// (gt-4k3fj.4, gt-4owfd.1, gt-11y).
	if opts.RigName != "" {
		if label, err := Blocked(townRoot, opts.RigName, d.EstopOn, d.RigParked); err != nil {
			result.ErrMsg = label
			return result, err
		}
	}

	// 1. Get bead info + status check
	info, err := d.BeadInfoInTown(townRoot, opts.BeadID)
	if err != nil {
		result.ErrMsg = err.Error()
		return result, fmt.Errorf("could not get bead info: %w", err)
	}

	// Guard against dispatching closed/tombstone beads (defense-in-depth).
	// Not bypassed by --force — if you need to re-dispatch, reopen the bead first.
	if info.Status == "closed" || info.Status == "tombstone" {
		result.ErrMsg = "already " + info.Status
		return result, fmt.Errorf("bead %s is %s (work already completed)", opts.BeadID, info.Status)
	}

	// Guard against slinging beads with flag-like titles (gt-e0kx5). These are
	// garbage beads created by flag-parsing bugs; slinging one causes dispatch
	// loops where polecats bounce the work back.
	if beads.IsFlagLikeTitle(info.Title) {
		result.ErrMsg = "flag-like title"
		return result, fmt.Errorf("refusing to sling bead %s: title %q looks like a CLI flag (garbage bead from flag-parsing bug)",
			opts.BeadID, info.Title)
	}

	// Guard against re-dispatching work submitted for landing (gt-v4ssj.2). Its
	// session ended on purpose in gt done and its assignee is dead by design,
	// which the dead-agent auto-force below would read as abandoned work. The
	// landing worker owns it until it lands or is handed back.
	//
	// --force does not override, unlike the operator-reservation guard: the
	// automated redispatch paths — the convoy feeder, the scheduler — all pass
	// --force for the other guards they need, so a guard --force opens is a
	// guard none of them is actually held by. A human who needs the bead back
	// removes the label.
	if slices.Contains(info.Labels, land.LabelReadyToLand) {
		result.ErrMsg = "ready-to-land"
		return result, fmt.Errorf("%s %s is submitted for landing (%s); the landing worker owns it.\nRemove the label first to take it back",
			dispatch.SlingRefusalMarker, opts.BeadID, land.LabelReadyToLand)
	}

	// Save explicit force state before dead-agent auto-force, so the deferred
	// gate below still requires an explicit --force for deferred beads.
	explicitForce := opts.Force

	if (info.Status == "pinned" || info.Status == "hooked" || info.Status == "in_progress") && !opts.Force {
		// Auto-force when hooked/in_progress agent's session is confirmed dead (gt-npzy, GH#1380).
		// Mirrors the dead-agent detection in the single-sling CLI path so that
		// programmatic dispatch also handles stale hooks from nuked polecats.
		if (info.Status == "hooked" || info.Status == "in_progress") && info.Assignee != "" && d.AgentDead(info.Assignee) {
			// A dead holder does not mean dead work: refuse when the work
			// survives on a branch or survival cannot be verified, exactly as
			// the CLI path does (gt-3qfp, gt-vm5g4). Scheduler callers read the
			// refusal as a deferral.
			if opts.ResumeBranch == "" {
				if err := d.SurvivingWorkGuard(townRoot, opts.BeadID, info.Assignee); err != nil {
					result.ErrMsg = err.Error()
					return result, err
				}
			}
			fmt.Fprintf(d.out(), "  %s Hooked agent %s has no active session, auto-forcing dispatch...\n",
				style.Warning.Render("⚠"), info.Assignee)
			opts.Force = true
		} else {
			result.ErrMsg = "already " + info.Status
			return result, fmt.Errorf("already %s (use --force to re-sling)", info.Status)
		}
	}

	// Guard against slinging deferred beads (gt-1326mw).
	// Uses explicitForce (not opts.Force) so dead-agent auto-force doesn't
	// accidentally bypass the deferred gate.
	if IsDeferredBead(info) && !explicitForce {
		result.ErrMsg = "deferred"
		return result, fmt.Errorf("bead %s is deferred (use --force to override)", opts.BeadID)
	}

	// Guard against dispatching a bead the human operator owns (gt-21pl0).
	// Mirrors the guard in the single-sling CLI path so the batch and queue
	// callers — which include the daemon's convoy feeders — refuse one too, and
	// read the refusal as a deferral rather than a failed dispatch.
	if reason := dispatch.OperatorReservation(info.Labels, info.Assignee); reason != "" && !explicitForce {
		result.ErrMsg = "operator-reserved"
		return result, fmt.Errorf("%s %s is the operator's work (%s)\nAn agent does not take it. Use --force to sling it to one anyway",
			dispatch.SlingRefusalMarker, opts.BeadID, reason)
	}

	// Guard against a dispatcher taking a rework bead from the steward
	// (gt-28ibg). While the steward patrol runs live it owns every rejected
	// bead -- its rejection job repairs the branch, or re-slings it with the
	// findings -- and a second sling spends a polecat on work a job is already
	// settling. The steward's own re-sling is the --force the operator guard
	// above also answers to; an automatic dispatcher passes no force, so none
	// of them can take the bead by accident. explicitForce rather than
	// opts.Force, so the dead-agent auto-force above is not read as a
	// deliberate override.
	if slices.Contains(info.Labels, land.LabelRework) && !explicitForce && d.StewardReworkOwner != nil {
		if reason := d.StewardReworkOwner(townRoot, opts.RigName); reason != "" {
			result.ErrMsg = "steward-rework"
			return result, fmt.Errorf("%s %s is rework and %s\nThe steward's rejection job settles it. Wait for that job, or pass --force to dispatch it to a polecat anyway",
				dispatch.SlingRefusalMarker, opts.BeadID, reason)
		}
	}

	// Content duplicate check (gt-mcq): refuse a bead whose named tests and
	// files already appear on open or recently-closed work in this rig. Placed
	// after the already-hooked guard so an idempotent re-sling of the same bead
	// is not reported as a duplicate of itself, and skipped under --force, which
	// is the documented override.
	var dupCandidate *Duplicate
	if !opts.SkipDuplicateCheck && !opts.Force {
		var matches []DuplicateMatch
		var checkErr error
		dupCandidate, matches, checkErr = d.CheckDuplicates(townRoot, opts.BeadID, info)
		if checkErr != nil {
			fmt.Fprintf(d.out(), "  %s %v\n", style.Dim.Render("Warning:"), checkErr)
		}
		if decision := DecideDuplicates(opts.BeadID, matches); decision.Message != "" {
			if decision.Blocked {
				result.ErrMsg = ErrDuplicateContent.Error()
				return result, decision.Err()
			}
			_, _ = fmt.Fprint(d.out(), decision.Message)
		}
	}

	if opts.RigName != "" {
		if err := d.VerifyInTargetRig(opts.BeadID, opts.RigName, townRoot); err != nil {
			result.ErrMsg = err.Error()
			return result, err
		}
	}

	// Clear the outgoing polecat's state when force-stealing a bead from it.
	if (info.Status == "hooked" || info.Status == "in_progress") && opts.Force && info.Assignee != "" {
		assigneeParts := strings.Split(info.Assignee, "/")
		if len(assigneeParts) >= 3 && assigneeParts[1] == "polecats" {
			// gt-skwt: clear the outgoing polecat's agent-bead state now,
			// synchronously.
			d.ClearReassigned(townRoot, info.Assignee)
		}
	}

	// 2. Burn stale molecules (if formula applies)
	// Settle the formula here, not at the top: resolving it reads the rig's
	// config, and a dispatch the guards above refuse should not pay for that.
	// `gt sling` resolves it in the same place, after its own guards.
	//
	// A caller that named a formula keeps it; one that did not gets the target
	// rig's default, which is what `gt sling` has always applied to a polecat
	// target and what the callers holding only a convoy's record used to
	// inherit from the command they exec'd. Only --hook-raw-bead asks for the
	// bare bead (gt-4lor).
	if opts.FormulaName == "" && !opts.HookRawBead && d.DefaultFormula != nil {
		opts.FormulaName = d.DefaultFormula(townRoot, opts.RigName)
	}
	if opts.FormulaName != "" {
		existingMolecules, err := d.CollectMolecules(info, opts.BeadID, townRoot)
		if err != nil {
			result.ErrMsg = fmt.Sprintf("molecule check failed: %v", err)
			return result, fmt.Errorf("checking existing molecule bonds: %w", err)
		}
		if len(existingMolecules) > 0 {
			// Auto-burn when bead is unassigned (molecules are definitionally stale),
			// or when the assigned agent's session is dead. This unblocks the daemon's
			// stranded convoy scan which never passes --force.
			stale := opts.Force ||
				(info.Assignee == "" && (info.Status == "open" || info.Status == "in_progress")) ||
				(info.Assignee != "" && d.AgentDead(info.Assignee))
			if stale {
				fmt.Fprintf(d.out(), "  %s Burning %d stale molecule(s): %s\n",
					style.Warning.Render("⚠"), len(existingMolecules), strings.Join(existingMolecules, ", "))
				if err := d.BurnMolecules(existingMolecules, opts.BeadID, townRoot); err != nil {
					result.ErrMsg = fmt.Sprintf("burn failed: %v", err)
					return result, fmt.Errorf("burning stale molecules: %w", err)
				}
			} else {
				result.ErrMsg = "has existing molecule(s)"
				return result, fmt.Errorf("bead %s has existing molecule(s) (use --force)", opts.BeadID)
			}
		}
	}

	// 3. Spawn polecat
	// Last boundary before the dispatch gets expensive: a spawn allocates a
	// worktree and a session, and everything past it has to be rolled back.
	if err := canceled(ctx); err != nil {
		result.ErrMsg = err.Error()
		return result, err
	}
	spawnOpts := SpawnOptions{
		TownRoot:     townRoot,
		Force:        opts.Force,
		Account:      opts.Account,
		HookBead:     opts.BeadID,
		Agent:        opts.Agent,
		BaseBranch:   opts.BaseBranch,
		ResumeBranch: opts.ResumeBranch,
		// Create is always true for rig targets: Run only handles rig-targeted
		// dispatch, where a fresh polecat must be spawned. The single-sling CLI
		// path handles --create for non-rig targets via target resolution.
		Create: true,
		Steps:  opts.Steps,
	}
	spawnInfo, err := d.SpawnPolecat(opts.RigName, spawnOpts)
	if err != nil {
		result.ErrMsg = err.Error()
		return result, fmt.Errorf("failed to spawn polecat: %w", err)
	}
	result.SpawnInfo = spawnInfo
	result.PolecatName = spawnInfo.PolecatName
	spawnInfo.OriginalHold = &Hold{Status: info.Status, Assignee: info.Assignee}

	targetAgent := spawnInfo.AgentID()
	hookWorkDir := spawnInfo.ClonePath

	rollbackSpawnedPolecat := func(rollbackBeadID, reason string) {
		fmt.Fprintf(d.out(), "  %s %s, rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), reason, spawnInfo.PolecatName)
		d.RollbackArtifacts(spawnInfo, townRoot, rollbackBeadID, hookWorkDir)
		d.RestoreRawFields(rollbackBeadID, townRoot, hookWorkDir, info)
		if opts.Force && info.Status == "pinned" {
			d.RestorePinned(townRoot, opts.BeadID, info.Assignee)
		}
	}

	// 4. Cook formula (unless SkipCook)
	// Past the spawn now, so every exit below rolls the polecat back.
	if err := canceled(ctx); err != nil {
		rollbackSpawnedPolecat(opts.BeadID, "Dispatch canceled")
		result.ErrMsg = err.Error()
		return result, err
	}
	formulaCooked := opts.SkipCook
	if opts.FormulaName != "" && !formulaCooked {
		workDir := d.HookDir(townRoot, opts.BeadID, hookWorkDir)
		if err := d.Cook(opts.FormulaName, workDir, townRoot); err != nil {
			if opts.FormulaFailFatal {
				// Rollback spawned polecat on fatal cook failure
				rollbackSpawnedPolecat(opts.BeadID, "Formula cook failed")
				result.ErrMsg = fmt.Sprintf("cook failed: %v", err)
				return result, fmt.Errorf("cooking formula %s: %w", opts.FormulaName, err)
			}
			fmt.Fprintf(d.out(), "  %s Could not cook formula %s: %v\n", style.Dim.Render("Warning:"), opts.FormulaName, err)
		} else {
			formulaCooked = true
		}
	}

	// 5. Instantiate formula on bead (wisp + bond)
	beadToHook := opts.BeadID
	attachedMoleculeID := ""
	var allVars []string
	varsForAttachment := append([]string(nil), opts.Vars...)
	formulaVarsForAttachment := strings.Join(varsForAttachment, "\n")
	if opts.FormulaName != "" && formulaCooked {
		// Auto-inject rig command vars as defaults (user --var flags override)
		rigCmdVars := d.RigCommandVars(townRoot, opts.RigName)
		// Build per-bead vars: rig defaults first, then user vars (higher priority),
		// then spawn-derived base_branch/resume_branch (gt-a8i3: kept distinct).
		allVars = buildFormulaVars(rigCmdVars, opts.Vars, spawnInfo.BaseBranch, opts.ResumeBranch)

		// GH#gt-zqvj: Inject prior attempt context when re-dispatching an issue
		// that already has an open MR from a previous polecat. The new polecat
		// gets the old branch name so it can cherry-pick prior work instead of
		// starting from scratch.
		if priorVars := d.PriorAttempt(beadsDir, opts.BeadID); len(priorVars) > 0 {
			allVars = append(allVars, priorVars...)
			fmt.Fprintf(d.out(), "  %s Prior attempt found — context injected for polecat\n", style.Dim.Render("↻"))
		}
		varsForAttachment = append([]string(nil), allVars...)
		formulaVarsForAttachment = strings.Join(allVars, "\n")
		formulaResult, err := d.InstantiateFormula(ctx, opts.FormulaName, opts.BeadID, info.Title, hookWorkDir, townRoot, allVars)
		if err != nil {
			if opts.FormulaFailFatal {
				// Rollback spawned polecat on fatal formula failure
				rollbackSpawnedPolecat(opts.BeadID, "Formula instantiation failed")
				result.ErrMsg = fmt.Sprintf("formula failed: %v", err)
				return result, fmt.Errorf("instantiating formula %s: %w", opts.FormulaName, err)
			}
			// Best-effort: in batch mode, a formula instantiation failure should not abort or rollback the
			// spawned polecat. We still hook the raw bead so work can proceed (e.g., missing required vars).
			fmt.Fprintf(d.out(), "  %s Could not apply formula: %v (hooking raw bead)\n", style.Dim.Render("Warning:"), err)
		} else {
			fmt.Fprintf(d.out(), "  %s Formula %s applied\n", style.Bold.Render("✓"), opts.FormulaName)
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

	actor := d.Actor(opts)
	fieldUpdates := FieldUpdates{
		Dispatcher:       actor,
		Args:             opts.Args,
		Vars:             varsForAttachment,
		AttachedMolecule: attachedMoleculeID,
		NoMerge:          opts.NoMerge,
		ReviewOnly:       opts.ReviewOnly,
		Mode:             &opts.Mode,
		FormulaVars:      formulaVarsForAttachment,
	}
	if attachedMoleculeID != "" || opts.NoMerge || opts.ReviewOnly {
		fieldUpdates.AttachedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if opts.FormulaName != "" {
		if attachedMoleculeID != "" {
			fieldUpdates.AttachedFormula = opts.FormulaName
		} else {
			fieldUpdates.ClearAttachment = true
			fieldUpdates.Vars = nil
			fieldUpdates.FormulaVars = ""
		}
	}

	// 6. Hook bead with retry
	if err := canceled(ctx); err != nil {
		rollbackSpawnedPolecat(beadToHook, "Dispatch canceled")
		result.ErrMsg = err.Error()
		return result, err
	}
	// Acquire per-assignee lock to serialize concurrent hook writes (issue #3114).
	assigneeUnlock, assigneeLockErr := d.LockAssignee(townRoot, targetAgent)
	if assigneeLockErr != nil {
		// The formula is instantiated and bonded by now, so this undoes it as
		// well as the spawn: a wisp this dispatch created and did not commit
		// must not block the next attempt (gt-7evi4).
		rollbackSpawnedPolecat(beadToHook, "Assignee lock failed")
		result.ErrMsg = "assignee lock failed"
		return result, fmt.Errorf("serializing hook write for %s: %w", targetAgent, assigneeLockErr)
	}
	defer assigneeUnlock()
	if attachedMoleculeID == "" && (opts.NoMerge || opts.ReviewOnly) {
		if err := d.StoreFields(townRoot, beadToHook, fieldUpdates); err != nil {
			// The dispatch never reached a hookable bead, so there is nothing
			// for a later dispatch to pick up and everything to unwind
			// (gt-7evi4).
			rollbackSpawnedPolecat(beadToHook, RawMetadataRollbackReason)
			result.ErrMsg = "raw sling metadata failed"
			return result, fmt.Errorf("storing raw sling metadata before hook: %w", err)
		}
	}
	hookDir := d.HookDir(townRoot, beadToHook, hookWorkDir)
	// The hook write below replaces the base bead's assignee (beadToHook is the
	// wisp when a formula applies). Record the outgoing value on the base bead
	// first: assignee keeps only the last writer, so afterwards the previous
	// polecat's branch is unreachable from the bead (gt-zd7c).
	requester := opts.CallerContext
	if requester == "" {
		requester = d.Requester()
	}
	d.RecordReassignment(townRoot, opts.BeadID, info.Assignee, targetAgent, requester)
	if err := d.Hook(beadToHook, targetAgent, hookDir, townRoot); err != nil {
		// Clean up all partial dispatch state, including raw metadata stored before hook.
		rollbackSpawnedPolecat(beadToHook, "Hook failed")
		result.ErrMsg = "hook failed"
		return result, fmt.Errorf("failed to hook bead: %w", err)
	}

	fmt.Fprintf(d.out(), "  %s Work attached to %s\n", style.Bold.Render("✓"), spawnInfo.PolecatName)
	// Labels live on the base bead even when a formula wisp was hooked.
	d.ClearOrphanLabels(townRoot, opts.BeadID, hookWorkDir)

	// The bead is dispatched now, so later dispatches in this process should
	// see it in the pool even though their snapshot predates this hook.
	d.NoteDispatched(townRoot, dupCandidate)

	// 7. Log dispatch event
	_ = d.LogFeed(events.TypeSling, actor, events.SlingPayload(beadToHook, targetAgent))

	// 8. Update agent hook_bead state
	d.UpdateAgentHook(targetAgent, beadToHook, hookWorkDir, beadsDir)

	// 9. Store fields in bead (dispatcher, args, attached_molecule, no_merge, mode)
	// Use beadToHook for the update target (may differ from beadID when formula-on-bead)
	if err := d.StoreFields(townRoot, beadToHook, fieldUpdates); err != nil {
		fmt.Fprintf(d.out(), "  %s Could not store fields in bead: %v\n", style.Dim.Render("Warning:"), err)
	}

	// Update agent bead mode for stuck-detector Ralph thresholds. Reuse/reset clears stale mode.
	if opts.Mode != "" {
		d.UpdateAgentMode(targetAgent, opts.Mode, hookWorkDir, beadsDir)
	}

	// 10. Start polecat session
	if err := canceled(ctx); err != nil {
		rollbackSpawnedPolecat(beadToHook, "Dispatch canceled")
		result.ErrMsg = err.Error()
		return result, err
	}
	pane, err := d.StartSession(spawnInfo)
	if err != nil {
		fmt.Fprintf(d.out(), "  %s Could not start session: %v, cleaning up partial state...\n", style.Dim.Render("✗"), err)
		rollbackSpawnedPolecat(beadToHook, "Session failed")
		result.ErrMsg = fmt.Sprintf("session failed: %v", err)
		return result, fmt.Errorf("starting polecat session: %w", err)
	}
	fmt.Fprintf(d.out(), "  %s Session started for %s\n", style.Bold.Render("▶"), spawnInfo.PolecatName)
	_ = pane

	result.Success = true
	return result, nil
}

// canceled reports a caller's cancellation as an error a dispatch can return.
// Run checks it at its step boundaries; see Run's doc for what that bound is
// worth and what it is not.
func canceled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("dispatch canceled: %w", ctx.Err())
	default:
		return nil
	}
}

// buildFormulaVars assembles the ordered var list for formula instantiation:
// rig defaults first, then user --var overrides, then spawn-derived
// base_branch/resume_branch.
//
// gt-a8i3: base_branch and resume_branch are DISTINCT and must never collapse
// into each other. base_branch (from the spawn's merge target) is the branch
// `gt done`/`gt mq submit` read for --target; resume_branch (--branch / --pr)
// is only the polecat's resumed working branch. A formula var list that
// conflated the two previously caused a resume dispatch to submit a
// self-targeted MR.
func buildFormulaVars(rigCmdVars, userVars []string, spawnBaseBranch, resumeBranch string) []string {
	vars := append(append([]string(nil), rigCmdVars...), userVars...)
	if spawnBaseBranch != "" && spawnBaseBranch != "main" {
		vars = append(vars, fmt.Sprintf("base_branch=%s", spawnBaseBranch))
	}
	if resumeBranch != "" {
		vars = append(vars, fmt.Sprintf("resume_branch=%s", resumeBranch))
	}
	return vars
}

// IsDeferredBead checks whether a bead should be rejected from dispatching
// because it has been deferred. Returns true if the bead has status "deferred"
// or if its description contains deferral keywords like "deferred to
// post-launch".
func IsDeferredBead(info *Bead) bool {
	if info.Status == "deferred" {
		return true
	}
	desc := strings.ToLower(info.Description)
	if strings.Contains(desc, "deferred to post-launch") ||
		strings.Contains(desc, "deferred to post launch") ||
		strings.Contains(desc, "status: deferred") {
		return true
	}
	return false
}

// RawMetadataRollbackReason marks the rollback of a dispatch that failed
// storing the raw workflow metadata a hook-less bead needs: the dispatch never
// reached a hookable bead, so the rollback is the only state left to unwind
// (gt-7evi4).
const RawMetadataRollbackReason = "Raw sling metadata failed"

// ErrRigUnavailable is the sentinel for a rig an e-stop, park or dock refuses a
// dispatch to (errors.Is). These refusals carry no dispatch refusal marker —
// the rig, not the bead, is the reason — so this is how a caller tells them
// apart from a failed sling when deciding whether to print help (gt-fudap).
var ErrRigUnavailable = errors.New("rig unavailable for dispatch")

// rigUnavailableError is Blocked's refusal. It wraps the operator-facing line
// so errors.Is finds ErrRigUnavailable without adding text to that line.
type rigUnavailableError struct{ msg string }

func (e *rigUnavailableError) Error() string { return e.msg }

func (e *rigUnavailableError) Is(target error) bool { return target == ErrRigUnavailable }

// Blocked refuses a dispatch to a rig that is e-stopped, parked or docked.
// It returns a short label naming the state, and the refusal to hand back.
func Blocked(townRoot, rigName string, estopOn func(townRoot, rigName string) (bool, error), parked func(townRoot, rigName string) (bool, string)) (string, error) {
	if on, err := estopOn(townRoot, rigName); on {
		why := "E-stop active"
		if err != nil {
			why = err.Error()
		}
		return "e-stop", &rigUnavailableError{msg: fmt.Sprintf("cannot sling to rig %q: %s\nClear it with: gt thaw, or gt thaw --rig %s", rigName, why, rigName)}
	}
	if blocked, reason := parked(townRoot, rigName); blocked {
		undoCmd := "gt rig unpark"
		if reason == "docked" {
			undoCmd = "gt rig undock"
		}
		return "rig " + reason, &rigUnavailableError{msg: fmt.Sprintf("cannot sling to %s rig %q\n%s %s", reason, rigName, undoCmd, rigName)}
	}
	return "", nil
}
