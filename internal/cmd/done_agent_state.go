package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/style"
)

// This file is the CLI layer's half of gt done's completion: closing the
// hooked work bead and reporting the agent's terminal state. It stays in
// internal/cmd because it is built on the command layer's role detection, its
// bead-close invariants, and the molecule-close helpers in
// molecule_lifecycle.go — none of which a leaf package can reach. The
// submission half lives in internal/done, which calls
// updateAgentStateOnDone through done.Options.RecordAgentState.

// clearDoneCheckpoints removes done-cp:* labels from the agent bead. gt done
// no longer writes them (its push is idempotent under a lease), but agent
// beads from earlier runs still carry them.
func clearDoneCheckpoints(bd beads.Client, agentBeadID string) {
	if agentBeadID == "" {
		return
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return
	}
	var toRemove []string
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, "done-cp:") {
			toRemove = append(toRemove, label)
		}
	}
	if len(toRemove) == 0 {
		return
	}
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		RemoveLabels: toRemove,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear done checkpoints on %s: %v\n", agentBeadID, err)
	}
}

// updateAgentStateOnDone closes the hooked work bead and reports cleanup status.
// Uses issueID directly to find the hooked bead instead of reading the agent bead's
// hook_bead slot (hq-l6mm5: direct bead tracking).
//
// Clean completions use "done" to prevent dead completed sessions from
// re-entering the idle reuse pool before their cleanup finishes.
// Escalated/deferred exits use "stuck" because they need recovery.
//
// cleanup_status is NOT written here — gt done's reportDone self-reports it
// through selfReportCleanupStatus so that failed submissions record it too.
//
// BUG FIX (hq-3xaxy): This function must be resilient to working directory deletion.
// If the polecat's worktree is deleted before gt done finishes, we use env vars as fallback.
// All errors are warnings, not failures - gt done must complete even if bead ops fail.
func updateAgentStateOnDone(cwd, townRoot, exitType, issueID string) error {
	return updateAgentStateOnDoneIn(doneStateEnv{}, cwd, townRoot, exitType, issueID)
}

// doneStateEnv is what updateAgentStateOnDone reads beyond its arguments:
// the environment, the bead stores, and the HEAD that review evidence is
// checked against. The zero value is the real process: os.Getenv, bd on
// PATH, and git in the process's working directory. A test passes an env map
// and beadsfake stores, so it needs no PATH stub, t.Setenv or chdir and can
// run in parallel.
type doneStateEnv struct {
	getenv func(string) string
	// routed opens a store at dir that routes each ID by its prefix; nil is
	// beads.NewWithBeadsDir.
	routed func(dir string) beads.Client
	// source opens the database a hooked bead routes to; nil is
	// done.OpenSourceStore.
	source done.SourceStoreOpener
	// purge removes the closed wisps of the store at dir; nil is bd purge.
	purge      func(dir, townRoot string)
	reviewHead func() (string, error)
}

func (e doneStateEnv) routedAt(dir string) beads.Client {
	if e.routed == nil {
		return beads.NewWithBeadsDir(dir, "")
	}
	return e.routed(dir)
}

func (e doneStateEnv) sourceOpener() done.SourceStoreOpener {
	if e.source == nil {
		return done.OpenSourceStore
	}
	return e.source
}

func (e doneStateEnv) purgeClosedWisps(dir, townRoot string) {
	if e.purge == nil {
		purgeClosedEphemeralBeads(beads.NewWithBeadsDir(dir, ""), townRoot)
		return
	}
	e.purge(dir, townRoot)
}

func (e doneStateEnv) lookup() func(string) string {
	if e.getenv == nil {
		return os.Getenv
	}
	return e.getenv
}

func (e doneStateEnv) head() func() (string, error) {
	if e.reviewHead == nil {
		return done.CurrentReviewEvidenceHead
	}
	return e.reviewHead
}

func updateAgentStateOnDoneIn(e doneStateEnv, cwd, townRoot, exitType, issueID string) error {
	getenv := e.lookup()
	// Get role context - try multiple sources for resilience
	roleInfo, err := getRoleWithContextEnv(cwd, townRoot, getenv)
	if err != nil {
		// Fallback: try to construct role info from environment variables
		// This handles the case where cwd is deleted but env vars are set
		envRole := getenv("GT_ROLE")
		envRig := getenv("GT_RIG")
		envPolecat := getenv("GT_POLECAT")

		if envRole == "" || envRig == "" {
			// Can't determine role, skip agent state update
			style.PrintWarning("could not determine role for agent state update (env: GT_ROLE=%q, GT_RIG=%q)", envRole, envRig)
			return nil
		}

		// Parse role string to get Role type
		parsedRole, _, _ := parseRoleString(envRole)

		roleInfo = RoleInfo{
			Role:     parsedRole,
			Rig:      envRig,
			Polecat:  envPolecat,
			TownRoot: townRoot,
			WorkDir:  cwd,
			Source:   "env-fallback",
		}
	}

	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}

	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		style.PrintWarning("no agent bead ID found for %s/%s, skipping agent state update", ctx.Rig, ctx.Polecat)
		return nil
	}

	// Use rig path for bd commands.
	// IMPORTANT: Use the rig's directory (not polecat worktree) so bd commands
	// work even if the polecat worktree is deleted.
	beadsPath := filepath.Join(townRoot, ctx.Rig)
	bd := e.routedAt(beadsPath)
	// agentBd resolves agent beads dual-scope: their canonical (rig-local)
	// database first, with a town fallback for legacy beads created before
	// the rig-local migration. See beads.ForAgentBead docstring (gt-8we).
	agentBd := beads.ForAgentBead(bd)

	// Find the hooked bead to close. Use issueID directly instead of reading
	// agent bead's hook_bead slot (hq-l6mm5: direct bead tracking).
	hookedBeadID := issueID
	if hookedBeadID == "" {
		// Fallback: query for hooked beads assigned to this agent
		agentID := roleInfo.ActorString()
		if found := done.FindHookedBeadForAgent(bd, agentID); found != "" {
			hookedBeadID = found
		}
	}

	// Workflow step beads (*-wfs-*) are ephemeral formula steps managed by the workflow
	// engine. For these, DEFERRED means "step complete, no code commits" not "work
	// paused for resumption". Close them on DEFERRED so the workflow can advance.
	isWorkflowStep := strings.Contains(hookedBeadID, "-wfs-")

	if hookedBeadID != "" && (exitType != done.ExitDeferred || isWorkflowStep) {
		// BUG FIX (gt-pftz): Close hooked bead unless already terminal (closed/tombstone).
		// Previously checked hookedBead.Status == StatusHooked, but polecats update
		// their work bead to in_progress during work. The exact-match check caused
		// gt done to skip closing the bead, leaving it as unassigned open work after
		// the hook was cleared — triggering infinite dispatch loops.
		//
		// DEFERRED exits preserve the bead: work is paused, not done. The bead
		// stays open/in_progress so it can be resumed on the next session.
		// Exception: workflow step beads (*-wfs-*) are always closed — see above.
		hookBd, _, _ := done.RoutedIssueBeadsIn(beadsPath, hookedBeadID, e.sourceOpener())
		hookedBead, hookedErr := hookBd.Show(hookedBeadID)
		if hookedErr == nil && beads.IssueStatus(hookedBead.Status).IsTerminal() {
			// The bead was closed before gt done ran — the nothing-to-implement
			// route closes it first. The block below, which is what ends an
			// attached molecule, is skipped for a terminal bead and no landing
			// follows, so the molecule and its step wisps would stay open for
			// good (gt-mddzp).
			closeMoleculeOfClosedBead(hookBd, hookedBeadID, hookedBead)
			goto doneStateUpdate
		}
		if hookedErr == nil && !beads.IssueStatus(hookedBead.Status).IsTerminal() {
			// Guard: never close a rig identity bead. Polecats dispatched with the
			// rig bead as their hook (via mol-polecat-work) must not close permanent
			// infrastructure. Skip close and fall through to idle state update.
			if beads.HasLabel(hookedBead, "gt:rig") {
				fmt.Fprintf(os.Stderr, "Note: hooked bead %s is a rig identity bead (gt:rig) — skipping close\n", hookedBeadID)
				goto doneStateUpdate
			}

			currentHead, _ := e.head()()
			if skipReason, fatal := done.DoneSourceCloseSkipReasonForHead(hookBd, hookedBeadID, hookedBead, currentHead); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				fmt.Fprintf(os.Stderr, "  The bead will remain open; the reason is recorded on it.\n")
				done.NotifyDoneCloseSkipped(hookBd, hookedBeadID, skipReason)
				if fatal {
					return fmt.Errorf("cannot complete hooked work: %s", skipReason)
				}
				goto doneStateUpdate
			}

			// BUG FIX: Close attached molecule (wisp) BEFORE closing hooked bead.
			// When using formula-on-bead (gt sling formula --on bead), the base bead
			// has attached_molecule pointing to the wisp. Without this fix, gt done
			// only closed the hooked bead, leaving the wisp orphaned.
			// Order matters: wisp closes -> unblocks base bead -> base bead closes.
			attachment := beads.ParseAttachmentFields(hookedBead)
			if attachment != nil && attachment.AttachedMolecule != "" {
				// Close molecule step descendants before closing the wisp root.
				// bd close doesn't cascade — without this, open/in_progress steps
				// from the molecule stay stuck forever after gt done completes.
				// Order: step children -> wisp root -> base bead.
				//
				// Then close the wisp root with --force and audit reason.
				// ForceCloseWithReason handles any status (hooked, open, in_progress)
				// and records the reason + session for attribution.
				// Not found = already burned/deleted by another path, continue.
				n, molErr := closeStepsThenRoot(hookBd, attachment.AttachedMolecule, func() error {
					if err := hookBd.ForceCloseWithReason("done", attachment.AttachedMolecule); err != nil && !errors.Is(err, beads.ErrNotFound) {
						return err
					}
					return nil
				})
				if n > 0 {
					fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, attachment.AttachedMolecule)
				}
				if molErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: couldn't close attached molecule %s: %v\n", attachment.AttachedMolecule, molErr)
					// Don't try to close hookedBeadID - it may still be blocked.
					// But DO clear hooks and update agent state (goto doneStateUpdate)
					// so the polecat isn't stuck in 'working' state (za-o9e).
					goto doneStateUpdate
				}
			}

			// Acceptance criteria gate: skip close if criteria are unchecked.
			if beads.HasLabel(hookedBead, land.LabelReadyToLand) {
				// Submitted for landing: the landing worker closes it when it
				// lands, with the landed commit (ADR 0004). "Closed" means
				// landed; closing here would claim that for a pushed branch.
				attempt := 1 + land.CountRejections(hookedBead.Notes)
				note := fmt.Sprintf("Submitted for landing (attempt %d)", attempt)
				if w, ok := land.ParseReadyNote(hookedBead.Notes); ok {
					note = fmt.Sprintf("Submitted for landing: %s @ %s onto %s (attempt %d)", w.Branch, done.ShortSHA(w.Head), w.Target, attempt)
				}
				if err := hookBd.AddComment(hookedBeadID, note); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: couldn't record the submission on %s: %v\n", hookedBeadID, err)
				}
			} else if unchecked := beads.HasUncheckedCriteria(hookedBead); unchecked > 0 {
				style.PrintWarning("hooked bead %s has %d unchecked acceptance criteria — skipping close", hookedBeadID, unchecked)
				fmt.Fprintf(os.Stderr, "  The bead will remain open; see the warning above.\n")
			} else if message, standDown := done.ReworkUnchangedHeadMessage(hookedBead, currentHead); standDown {
				// The submission stood down because the branch still sits on
				// the rejected head (gt-3e1z4). The close-time invariant below
				// would skip the close too; say why in a way that names the way
				// forward, and record that instead of the bare refusal.
				style.PrintWarning("%s", message)
				fmt.Fprintf(os.Stderr, "  The bead will remain open; the reason is recorded on it.\n")
				done.NotifyDoneCloseSkipped(hookBd, hookedBeadID, message)
			} else if skipReason := doneCloseTimeInvariantSkipReason(cwd, townRoot, ctx.Rig, hookedBeadID); skipReason != "" {
				// gt-6hmz: refuse rather than close a bead whose branch carries
				// commits the target lacks; only the landing worker closes
				// work that has code to land.
				style.PrintWarning("%s", skipReason)
				fmt.Fprintf(os.Stderr, "  The bead will remain open; the reason is recorded on it.\n")
				done.NotifyDoneCloseSkipped(hookBd, hookedBeadID, skipReason)
			} else if err := hookBd.Close(hookedBeadID); err != nil {
				// Non-fatal: warn but continue
				fmt.Fprintf(os.Stderr, "Warning: couldn't close hooked bead %s: %v\n", hookedBeadID, err)
			}
		}
	}

doneStateUpdate:
	// Clear hook_bead on the agent bead (gt-qbh). The hq-l6mm5 refactor made
	// SetHookBead/ClearHookBead no-ops, but the daemon's crash detection
	// still reads the hook_bead field from the agent bead. If the hooked bead
	// is a wisp that gets reaped, it can't verify it was closed and flags the
	// polecat as crashed. Clearing hook_bead prevents this false positive.
	emptyHook := ""
	if err := beads.UpdateAgentDescriptionFields(agentBd, agentBeadID, beads.AgentFieldUpdates{HookBead: &emptyHook}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear hook_bead on %s: %v\n", agentBeadID, err)
	}

	// Purge closed ephemeral beads (wisps) accumulated during this and prior sessions.
	// Without this, closed wisps from mol-polecat-work steps etc. accumulate
	// across sessions and pollute bd ready/list output (hq-6161m).
	// Best-effort: failures are non-fatal since the work is already done.
	e.purgeClosedWisps(beadsPath, townRoot)

	// Completion metadata (exit_type, MR ID, branch) remains on the agent bead
	// for audit purposes.
	doneState := string(beads.AgentStateDone)
	if exitType != done.ExitCompleted {
		doneState = "stuck"
	}
	// agent_state lives in the description (gt-ulom).
	if err := beads.UpdateAgentDescriptionFields(agentBd, agentBeadID, beads.AgentFieldUpdates{AgentState: &doneState}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't set agent %s to %s: %v\n", agentBeadID, doneState, err)
	}

	// ZFC #10 cleanup_status self-report moved to reportDone and runDone's
	// failure path (selfReportCleanupStatus): this function only ever ran when
	// the submission succeeded, so a failed submission recorded nothing at
	// all — the exact case where polecat reclaim needs "has_unpushed". The report
	// also used to be skipped whenever the status was empty/unknown, leaving
	// cleanup_status=<missing> on a slot that can never be reclaimed.

	// Clear legacy checkpoints on clean exit — gt done completed successfully.
	clearDoneCheckpoints(agentBd, agentBeadID)
	return nil
}

// closeMoleculeOfClosedBead ends the workflow an already-closed work bead
// carries, steps then root and unforced, so a bead closed before gt done ran
// (the nothing-to-implement route) does not leave its molecule and step wisps
// open for good (gt-mddzp). A root bd still refuses is reported, not forced:
// the bead is already closed and no landing record is at stake.
func closeMoleculeOfClosedBead(b beads.Client, beadID string, bead *beads.Issue) {
	attachment := beads.ParseAttachmentFields(bead)
	if attachment == nil || attachment.AttachedMolecule == "" {
		return
	}
	molID := attachment.AttachedMolecule
	// A molecule another path already ended (the landing, a burn) closes
	// nothing here, which also keeps a second gt done on the same bead quiet.
	if root, err := b.Show(molID); err != nil || root == nil || beads.IssueStatus(root.Status).IsTerminal() {
		return
	}
	n, err := closeStepsThenRoot(b, molID, func() error {
		if err := b.CloseWithReason("done", molID); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return err
		}
		return nil
	})
	if n > 0 {
		fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, molID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't close attached molecule %s of already-closed bead %s: %v\n", molID, beadID, err)
	}
}

// defaultClosedWispDeleteAge is the grace period before a closed ephemeral
// bead becomes eligible for purge, used when no lifecycle.reaper.delete_age
// override is configured. Matches the wisp-reaper patrol's own default
// (internal/daemon/wisp_reaper.go) so the two purge paths agree.
const defaultClosedWispDeleteAge = "168h"

// closedWispDeleteAge returns the configured grace period (lifecycle.reaper.delete_age)
// before a closed ephemeral bead may be purged, falling back to
// defaultClosedWispDeleteAge if unset or invalid.
func closedWispDeleteAge(townRoot string) string {
	cfg := daemon.LoadPatrolConfig(townRoot)
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.WispReaper == nil {
		return defaultClosedWispDeleteAge
	}
	age := cfg.Patrols.WispReaper.DeleteAgeStr
	if age == "" {
		return defaultClosedWispDeleteAge
	}
	if _, err := time.ParseDuration(age); err != nil {
		return defaultClosedWispDeleteAge
	}
	return age
}

// ephemeralPurger is the purge purgeClosedEphemeralBeads asks of a bead store.
type ephemeralPurger interface {
	PurgeClosedEphemeral(olderThan string) (string, error)
}

// purgeClosedEphemeralBeads removes closed ephemeral beads (wisps) that accumulated
// during this and prior sessions. Polecat sessions create mol-polecat-work
// steps etc. as wisps. These get closed during normal
// operation but are never deleted, accumulating hundreds of rows that pollute
// bd ready/list output. (hq-6161m)
//
// An --older-than grace period (gt-1q46) is REQUIRED here: MR beads (label
// gt:merge-request) are also closed ephemeral wisps, and a same-session
// supersede/rejection close (see FindOpenMRsForIssue below) can land just
// moments before this purge runs. Purging unconditionally deletes that MR
// bead outright — destroying its close reason/verdict — instead of leaving
// a "superseded by X" or rejection-verdict record for the next attempt.
//
// Best-effort: errors are logged but don't block gt done completion.
func purgeClosedEphemeralBeads(bd ephemeralPurger, townRoot string) {
	olderThan := closedWispDeleteAge(townRoot)
	out, err := bd.PurgeClosedEphemeral(olderThan)
	if err != nil {
		// Non-fatal: purge failure shouldn't block session completion
		fmt.Fprintf(os.Stderr, "Warning: wisp purge failed: %v\n", err)
		return
	}
	// bd purge --force --quiet outputs the count of purged beads
	outStr := out
	if outStr != "" && outStr != "0" {
		fmt.Fprintf(os.Stderr, "Purged closed ephemeral beads: %s\n", outStr)
	}
}
