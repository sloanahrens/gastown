package cmd

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/refinery"
)

// conflictWakeBeadShower is the narrow slice of *beads.Beads this file needs, so
// the blocked->ready decision can be tested against a forced transition instead
// of a live beads database (gt-rv8h). *beads.Beads satisfies it and is already
// routing-aware, which matters because both the conflict task and the MR wisp
// it names are resolved by prefix.
type conflictWakeBeadShower func(id string) (*beads.Issue, error)

// conflictResolutionCandidates lists the beads that could be the
// conflict-resolution task a completion is finishing, most specific first.
//
// It is a candidate list rather than one id because gt done cannot assume which
// bead names the conflict task. issueID is derived from the branch name unless
// --issue was passed, and a conflict-resolution polecat may still be sitting on
// the *resolved* branch (polecat/<owner>/<source-issue>+<mol>) at completion
// time, in which case its branch-derived issue is the source issue and not the
// task. It is empty outright when the worktree ends on the base branch, which
// names no issue. The agent bead's hook_bead names the bead the polecat was
// actually slung, so it is the second candidate and callers take the first one
// that resolves.
func conflictResolutionCandidates(cwd, agentBeadID, issueID string) []string {
	candidates := []string{issueID}
	if agentBeadID != "" {
		if _, fields, err := beads.New(cwd).ForAgentBead().GetAgentBead(agentBeadID); err == nil && fields != nil {
			candidates = append(candidates, fields.HookBead)
		}
	}
	return candidates
}

// conflictResolutionTaskOnHook returns the first candidate that is a
// conflict-resolution task, or nil when none of taskIDs is one.
//
// Status is deliberately not consulted, unlike readyConflictResolvedMR: gt
// done's base-branch guard needs the shape *before* the task is closed, so it
// can tell "an open conflict task" — reported back with the command that closes
// it — from "not a conflict task at all", reported as a merge-queue rejection.
//
// refinery.ConflictTaskOriginalMR is the shape predicate readyConflictResolvedMR
// starts from, so the bead a completion is accepted for and the bead whose MR
// is announced can never be different ones.
func conflictResolutionTaskOnHook(show conflictWakeBeadShower, taskIDs ...string) *beads.Issue {
	if show == nil {
		return nil
	}
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			continue
		}
		task, err := show(taskID)
		if err != nil || task == nil {
			continue
		}
		if refinery.ConflictTaskOriginalMR(task) == "" {
			continue
		}
		return task
	}
	return nil
}

// conflictResolutionCompletionTask resolves the conflict-resolution task this gt
// done run is completing from the live beads database, or nil when this is not a
// conflict-resolution completion.
func conflictResolutionCompletionTask(cwd, agentBeadID, issueID string) *beads.Issue {
	return conflictResolutionTaskOnHook(beads.New(cwd).Show, conflictResolutionCandidates(cwd, agentBeadID, issueID)...)
}

// wakeRefineryForReadyConflict resolves the conflict-resolution task this
// completion is finishing and, when closing that task has just released its MR
// back into the ready queue, wakes the refinery for that MR. It returns the MR
// it woke the refinery for, or "" when none of taskIDs released anything.
//
// This exists because a conflict-resolution pass is the one completion shape
// the normal refinery wake cannot cover. gt done's refinery nudge is gated on
// shouldNudgeRefinery — COMPLETED plus a freshly created MR bead — and a
// conflict pass creates no MR of its own: it rewrites the branch of an MR that
// already exists, so the gate can never fire for it. Every other wake source is
// event- or patrol-driven, which leaves the MR's blocked->ready transition
// silent whenever no other event happens to land (gt-rv8h: gastown's only ready
// MR sat 65 minutes while the refinery idled at its prompt with an empty turn;
// the witness eventually nudged MERGE_READY by hand, and the refinery picked the
// MR up immediately).
//
// taskIDs is a candidate list, not one id: see conflictResolutionCandidates for
// why gt done cannot assume which bead names the conflict task, and the call
// site passes that list through unchanged.
//
// Merely finishing a conflict task is not enough to wake the refinery — the task
// must be terminal AND its MR must actually be ready. An escalated or abandoned
// pass leaves the task open, and the MR blocked behind it, which is not a
// transition to announce: waking the refinery there would burn a cycle on an MR
// the refinery would still see as blocked. Both predicates are evaluated against
// the same live beads state the refinery itself uses (beads.HasUnresolvedBlockers
// is exactly what ListReadyMRs filters on), so this can never claim a wake for an
// MR the refinery would refuse to pick up.
func wakeRefineryForReadyConflict(show conflictWakeBeadShower, rigName string, taskIDs ...string) string {
	for _, taskID := range taskIDs {
		mrID := readyConflictResolvedMR(show, taskID)
		if mrID == "" {
			continue
		}
		nudgeRefineryMergeReady(rigName, mrID)
		return mrID
	}
	return ""
}

// readyConflictResolvedMR reports the MR that taskID has just returned to the
// ready queue, or "" when taskID is not a conflict-resolution task, is still
// open, or names an MR that is not ready to merge.
func readyConflictResolvedMR(show conflictWakeBeadShower, taskID string) string {
	if show == nil {
		return ""
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ""
	}

	task, err := show(taskID)
	if err != nil || task == nil {
		return ""
	}
	mrID := refinery.ConflictTaskOriginalMR(task)
	if mrID == "" {
		return ""
	}
	// An open conflict task still blocks its MR. Only its close is the
	// transition worth announcing.
	if !beads.IssueStatus(task.Status).IsTerminal() {
		return ""
	}

	mr, err := show(mrID)
	if err != nil || mr == nil {
		return ""
	}
	// Already merged, rejected, or superseded — the refinery has nothing to do,
	// and re-nudging it for a terminal MR is noise.
	if beads.IssueStatus(mr.Status).IsTerminal() {
		return ""
	}
	if beads.HasUnresolvedBlockers(mr) {
		return ""
	}
	return mrID
}
