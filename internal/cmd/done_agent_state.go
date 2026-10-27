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
	"github.com/steveyegge/gastown/internal/polecat/spans"
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
//
// pending is the completion metadata and cleanup_status gt done gathered for
// the same bead. It goes out in the one write that also clears hook_bead and
// sets the final agent_state: each agent-bead write is a Dolt commit, and the
// four it replaces were most of a polecat run's agent-bead churn. consumed
// reports that pending was written, including when this function fails after
// resolving the bead; false means it never addressed the bead and the caller
// owes the write.
func updateAgentStateOnDone(cwd, townRoot, exitType, issueID string, pending beads.AgentFieldUpdates) (bool, error) {
	return updateAgentStateOnDoneIn(doneStateEnv{}, cwd, townRoot, exitType, issueID, pending)
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
	purge func(dir, townRoot string)
	// reviewHead resolves HEAD in the worktree dir the close gate is judging
	// (nil is done.CurrentReviewEvidenceHeadIn). It takes the dir rather than
	// reading the process working directory: gt done can close work for a
	// worktree it is not standing in, and evidence must be validated against
	// that worktree's head (gt-u0zq0).
	reviewHead func(dir string) (string, error)
	// spanLog opens the polecat span log a submission's end is appended to;
	// nil is the store under townRoot.
	spanLog func(townRoot string) spanStore
}

// spanStore is the part of the polecat span log gt done reads and appends.
type spanStore interface {
	Summarize(bead string) (spans.Summary, error)
	End(bead, seat string, reason spans.Reason, at time.Time) error
}

func (e doneStateEnv) routedAt(dir string) beads.Client {
	if e.routed == nil {
		return beads.NewWithBeadsDir(dir, "")
	}
	return e.routed(dir)
}

func (e doneStateEnv) spanStoreAt(townRoot string) spanStore {
	if e.spanLog == nil {
		return spans.New(spans.DefaultDir(townRoot), nil)
	}
	return e.spanLog(townRoot)
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

func (e doneStateEnv) head() func(string) (string, error) {
	if e.reviewHead == nil {
		return done.CurrentReviewEvidenceHeadIn
	}
	return e.reviewHead
}

// doneRoleInfo resolves the role gt done's agent-state write addresses, trying
// multiple sources for resilience. When detection fails it falls back to the
// environment, which handles the case where cwd is deleted but env vars are
// set. ok is false, with a warning, when neither names a role.
func doneRoleInfo(getenv func(string) string, cwd, townRoot string) (info RoleInfo, ok bool) {
	info, err := getRoleWithContextEnv(cwd, townRoot, getenv)
	if err == nil {
		return info, true
	}
	envRole := getenv("GT_ROLE")
	envRig := getenv("GT_RIG")
	envPolecat := getenv("GT_POLECAT")
	if envRole == "" || envRig == "" {
		style.PrintWarning("could not determine role for agent state update (env: GT_ROLE=%q, GT_RIG=%q)", envRole, envRig)
		return RoleInfo{}, false
	}

	// Parse role string to get Role type
	parsedRole, _, _ := parseRoleString(envRole)
	return RoleInfo{
		Role:     parsedRole,
		Rig:      envRig,
		Polecat:  envPolecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
		Source:   "env-fallback",
	}, true
}

func updateAgentStateOnDoneIn(e doneStateEnv, cwd, townRoot, exitType, issueID string, pending beads.AgentFieldUpdates) (consumed bool, err error) {
	getenv := e.lookup()
	roleInfo, ok := doneRoleInfo(getenv, cwd, townRoot)
	if !ok {
		// Can't determine role, skip agent state update
		return false, nil
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
		return false, nil
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

	// pending rides out with the final agent_state write below. A return that
	// never reaches it (the fatal close refusals) still owes the completion
	// metadata and cleanup_status, which is what a slot's reclaim reads, so
	// they go out alone on the way out. Declared before the first goto: Go
	// refuses a jump over a declaration.
	stateWritten := false
	// reworkStandDown is set when the close block refuses to resubmit a rework
	// bead still on its rejected head (gt-3e1z4). That exit is recorded as stuck
	// with a DEFERRED exit, not COMPLETED, so patrol_scan restarts the seat (gt-jfhvc).
	reworkStandDown := false
	defer func() {
		if stateWritten || pending == (beads.AgentFieldUpdates{}) {
			return
		}
		if werr := beads.UpdateAgentDescriptionFields(agentBd, agentBeadID, pending); werr != nil {
			fmt.Fprintf(os.Stderr, "Warning: couldn't record completion on %s: %v\n", agentBeadID, werr)
		}
		consumed = true
	}()

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

	// An ESCALATED exit keeps the bead for the operator: the polecat stopped on
	// a blocker, so the bead stays open with the blocker on it and the molecule
	// stays attached for the resumed seat (gt-tdxsx). Unlike DEFERRED, a
	// workflow step is not complete either — the blocker stands.
	//
	// The note says the bead stays open, so it is written only to a bead that
	// does: an already-closed bead gets its molecule cleaned up like any other
	// closed hook (gt-mddzp) and no note, and a rig identity bead gets neither.
	// A bead whose status cannot be read gets no note either; the escalation
	// itself was raised with gt escalate and does not depend on it.
	if hookedBeadID != "" && exitType == done.ExitEscalated {
		hookBd, _, _ := done.RoutedIssueBeadsIn(beadsPath, hookedBeadID, e.sourceOpener())
		hookedBead, hookedErr := hookBd.Show(hookedBeadID)
		switch {
		case hookedErr != nil:
			if !errors.Is(hookedErr, beads.ErrNotFound) {
				style.PrintWarning("could not read hooked bead %s to record the escalation: %v", hookedBeadID, hookedErr)
			}
		case beads.IssueStatus(hookedBead.Status).IsTerminal():
			closeMoleculeOfClosedBead(hookBd, hookedBeadID, hookedBead, "done")
		case beads.HasLabel(hookedBead, "gt:rig"):
			fmt.Fprintf(os.Stderr, "Note: hooked bead %s is a rig identity bead (gt:rig) — not recording the escalation on it\n", hookedBeadID)
		default:
			done.NotifyDoneEscalated(hookBd, hookedBeadID)
		}
		// The attempt is ended below, with every other exit: the seat stopped
		// on a blocker and holds nothing more (gt-dz78b).
	}

	// A DEFERRED exit keeps the bead for the next session — but only a bead
	// that is still open has work to resume. A report-only job closes its own
	// bead and then exits DEFERRED (mol-deep-review's submit step, and
	// mol-polecat-work's zero-commit route: `gt bead close --reason="no-changes:
	// ..."` followed by `gt done --status DEFERRED`), and the block below is
	// skipped for DEFERRED, so the molecule and its step wisps stayed open for
	// good — one leaked root per scheduled run, hourly, until the store held 35
	// (gt-x1d8s). The terminal-bead check is gt-mddzp's, for the same reason.
	// A workflow step's DEFERRED exit is a completion, not a pause, so the block
	// below still owns that case.
	if hookedBeadID != "" && exitType == done.ExitDeferred && !isWorkflowStep {
		hookBd, _, _ := done.RoutedIssueBeadsIn(beadsPath, hookedBeadID, e.sourceOpener())
		hookedBead, hookedErr := hookBd.Show(hookedBeadID)
		switch {
		case hookedErr != nil:
			if !errors.Is(hookedErr, beads.ErrNotFound) {
				// A read that failed for any reason but absence leaves it
				// unknown whether the bead is terminal, and this block is the
				// only place a report-only run's molecule can end: no landing
				// follows a DEFERRED exit. Warning and carrying on cleared the
				// hook over a molecule nothing left would close — the gt-x1d8s
				// orphan, reached through the read instead of the exit type. Fail
				// as the COMPLETED block below does, so the hook stays set and a
				// retry still ends the molecule (gt-7cmjp).
				style.PrintWarning("could not read hooked bead %s to close its molecule: %v", hookedBeadID, hookedErr)
				return false, fmt.Errorf("resolving hooked bead %s: %w", hookedBeadID, hookedErr)
			}
		case beads.IssueStatus(hookedBead.Status).IsTerminal():
			closeMoleculeOfClosedBead(hookBd, hookedBeadID, hookedBead, "done")
		}
	}

	if hookedBeadID != "" && exitType != done.ExitEscalated && (exitType != done.ExitDeferred || isWorkflowStep) {
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
		if hookedErr != nil && !errors.Is(hookedErr, beads.ErrNotFound) {
			// A read that failed for any reason but absence leaves the bead's
			// status unknown, and the block below is what ends an attached
			// molecule. Falling through to doneStateUpdate on that unknown
			// would clear the hook and mark the agent done while the work bead
			// stays hooked or in_progress with no polecat on it — the
			// dispatch-loop shape gt-pftz named. Fail instead, so the hook
			// stays set and the caller hears why.
			style.PrintWarning("could not read hooked bead %s: %v", hookedBeadID, hookedErr)
			return false, fmt.Errorf("resolving hooked bead %s: %w", hookedBeadID, hookedErr)
		}
		if hookedErr == nil && beads.IssueStatus(hookedBead.Status).IsTerminal() {
			// The bead was closed before gt done ran — the nothing-to-implement
			// route closes it first. The block below, which is what ends an
			// attached molecule, is skipped for a terminal bead and no landing
			// follows, so the molecule and its step wisps would stay open for
			// good (gt-mddzp).
			closeMoleculeOfClosedBead(hookBd, hookedBeadID, hookedBead, "done")
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

			// HEAD in the worktree this close is judging (cwd), not in
			// whatever directory gt done happens to run from. A head that
			// cannot be read is reported and passed on empty: the review-only
			// evidence check refuses on an empty head, and an operator reading
			// that refusal needs to see why (gt-u0zq0).
			currentHead, headErr := e.head()(cwd)
			if headErr != nil {
				style.PrintWarning("could not resolve HEAD in %s: %v", cwd, headErr)
			}
			// Whichever close check below reports the reason, a rework bead
			// still on its rejected head submitted nothing (gt-3e1z4), so the
			// seat must not be recorded as completed (gt-jfhvc). The message is
			// read once here and reused by the stand-down branch of the close
			// chain, so both name the same refusal.
			standDownMessage, standDown := done.ReworkUnchangedHeadMessage(hookedBead, currentHead)
			reworkStandDown = standDown
			if skipReason, fatal := done.DoneSourceCloseSkipReasonForHead(hookBd, hookedBeadID, hookedBead, currentHead); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				fmt.Fprintf(os.Stderr, "  The bead will remain open; the reason is recorded on it.\n")
				done.NotifyDoneCloseSkipped(hookBd, hookedBeadID, skipReason)
				if fatal {
					return false, fmt.Errorf("cannot complete hooked work: %s", skipReason)
				}
				goto doneStateUpdate
			}

			// BUG FIX: Close attached molecule (wisp) BEFORE closing hooked bead.
			// When using formula-on-bead (gt sling formula --on bead), the base bead
			// has attached_molecule pointing to the wisp. Without this fix, gt done
			// only closed the hooked bead, leaving the wisp orphaned.
			// Order matters: wisp closes -> unblocks base bead -> base bead closes.
			//
			// A stand-down is the exception: it resubmits nothing, so the bead
			// stays open for the restart and the molecule stays with it
			// (gt-9x16z). The stand-down branch of the close chain records why.
			attachment := beads.ParseAttachmentFields(hookedBead)
			if attachment != nil && attachment.AttachedMolecule != "" && !standDown {
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
				// The attempt is the submission's: every refusal block a bead
				// carries was one, whatever its kind. CountRejections counts
				// review rounds only, which is not this number (gt-phftg).
				attempt := 1 + land.RejectionBlocks(hookedBead.Notes)
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
			} else if standDown {
				// The submission stood down because the branch still sits on
				// the rejected head (gt-3e1z4). The close-time invariant below
				// would skip the close too; say why in a way that names the way
				// forward, and record that instead of the bare refusal.
				style.PrintWarning("%s", standDownMessage)
				fmt.Fprintf(os.Stderr, "  The bead will remain open; the reason is recorded on it.\n")
				done.NotifyDoneCloseSkipped(hookBd, hookedBeadID, standDownMessage)
			} else if skipReason := doneCloseTimeInvariantSkipReason(cwd, townRoot, ctx.Rig, hookedBead); skipReason != "" {
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
	// Purge closed ephemeral beads (wisps) accumulated during this and prior
	// sessions; without this, closed wisps from mol-polecat-work steps etc.
	// accumulate across sessions and pollute bd ready/list output (hq-6161m).
	// Best-effort: failures are non-fatal since the work is already done.
	for _, dir := range wispPurgeDirs(beadsPath, townRoot) {
		e.purgeClosedWisps(dir, townRoot)
	}

	// One write for everything the agent bead owes at the end of the run:
	// hook_bead cleared, the final agent_state, and the completion metadata
	// (exit_type, MR ID, branch) and cleanup_status that remain on the bead
	// for audit and reclaim. Each was its own Dolt commit.
	//
	// Clearing hook_bead (gt-qbh): the hq-l6mm5 refactor made
	// SetHookBead/ClearHookBead no-ops, but the daemon's crash detection still
	// reads hook_bead from the agent bead. If the hooked bead is a wisp that
	// gets reaped, it can't verify it was closed and flags the polecat as
	// crashed. Clearing hook_bead prevents this false positive.
	//
	// agent_state lives in the description (gt-ulom).
	//
	// cleanup_status is ZFC #10's self-report. reportDone gathers it, and
	// runDone's failure path writes it alone, so a failed submission records it
	// too: the case where polecat reclaim needs "has_unpushed".
	emptyHook := ""
	doneState := string(beads.AgentStateDone)
	if exitType != done.ExitCompleted || reworkStandDown {
		doneState = "stuck"
	}
	final := pending
	final.HookBead = &emptyHook
	final.AgentState = &doneState
	if reworkStandDown {
		// A stand-down submitted nothing, so it is not a completion. DEFERRED is
		// the exit patrol_scan recovers a stuck seat from: it restarts the seat
		// on its preserved branch once the done-close grace passes (gt-ks62m).
		deferred := done.ExitDeferred
		final.ExitType = &deferred
	}
	if err := beads.UpdateAgentDescriptionFields(agentBd, agentBeadID, final); err != nil {
		// stateWritten stays false: the deferred write retries pending alone.
		fmt.Fprintf(os.Stderr, "Warning: couldn't set agent %s to %s: %v\n", agentBeadID, doneState, err)
	} else {
		stateWritten = true
		consumed = true
	}

	// Clear legacy checkpoints on clean exit — gt done completed successfully.
	clearDoneCheckpoints(agentBd, agentBeadID)

	// The seat's turn ends here, whatever the exit: the time a bead's seats
	// spent hooked on it is what its landing record reports, so an attempt left
	// open costs the record its seconds (gt-8yk78). Every exit that reaches
	// this point ends the turn; the error returns above do not, because the
	// session stays up to retry the same turn.
	if hookedBeadID != "" && roleInfo.Role == RolePolecat && roleInfo.Polecat != "" {
		recordSpanEnd(e.spanStoreAt(townRoot), hookedBeadID, roleInfo.Rig+"/"+roleInfo.Polecat, spanEndReason(exitType))
	}
	return consumed, nil
}

// spanEndReason is how exitType ended the seat's turn, in the span log's
// vocabulary: done is the turn ending normally, whether it submitted the bead,
// closed it or found nothing left to do; a turn that deferred or escalated
// says so.
func spanEndReason(exitType string) spans.Reason {
	switch exitType {
	case done.ExitEscalated:
		return spans.ReasonEscalated
	case done.ExitDeferred:
		return spans.ReasonDeferred
	}
	return spans.ReasonDone
}

// recordSpanEnd ends seat's open attempt on bead with reason, the way its turn
// ended. A failure is logged and the exit stands, and a seat with no open
// attempt writes nothing: a repeated gt done on the same ending must not append
// a second end (gt-liseq).
func recordSpanEnd(store spanStore, bead, seat string, reason spans.Reason) {
	sum, err := store.Summarize(bead)
	if err != nil {
		style.PrintWarning("could not read the polecat span log for %s: %v", bead, err)
		return
	}
	if !sum.HasOpen(seat) {
		fmt.Fprintf(os.Stderr, "Note: no open polecat span for %s on %s — nothing to close\n", seat, bead)
		return
	}
	if err := store.End(bead, seat, reason, time.Now()); err != nil {
		style.PrintWarning("could not record the polecat span end for %s: %v", bead, err)
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

// wispPurgeDirs returns the beads directories gt done purges on completion:
// the finishing rig's, then the town (hq) database's. Closed wisps accumulate
// in both — the town's come from plugin runs — and nothing else reclaims the
// town's, since wisp_reaper is enabled nowhere (gt-j4rxz). A rig whose
// directory IS the town root has one database, not two, and is listed once.
func wispPurgeDirs(beadsPath, townRoot string) []string {
	if filepath.Clean(beadsPath) == filepath.Clean(townRoot) {
		return []string{beadsPath}
	}
	return []string{beadsPath, townRoot}
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
