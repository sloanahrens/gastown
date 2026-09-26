package refinery

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/util"
)

// buildLandedMRs computes push-time editorial.LandedMR descriptors for mrs
// about to land on target. Head is each MR's own submitted branch tip
// (unchanged by merge-commit stacking) and is what Base/Head range checking
// (patch-id) is anchored to; Base is recomputed against origin/target right
// now, not read from the reviewed note, so a rebase or conflict resolution
// that changed the diff since review produces a different patch-id and the
// precondition below catches it.
//
// LandedCommit — the commit that will actually carry the note once pushed —
// is read off HEAD's first-parent chain: mrs is called here after every MR
// has already been merged onto the local, not-yet-pushed target (one
// MergeNoFF commit per MR, single-MR path included), so origin/target..HEAD
// first-parent has exactly len(mrs) commits, in the same order mrs were
// merged.
func (e *Engineer) buildLandedMRs(mrs []*MRInfo, target string) ([]editorial.LandedMR, error) {
	landedCommits, err := e.git.FirstParentLog("origin/"+target, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("first-parent log for %s: %w", target, err)
	}
	if len(landedCommits) != len(mrs) {
		return nil, fmt.Errorf("first-parent log for %s: found %d commit(s), expected %d for %d MR(s)", target, len(landedCommits), len(mrs), len(mrs))
	}
	// Same tip for every mr in this push: one target, checked once rather
	// than once per MR (see LandedMR.TargetTip / Note.ReviewedTargetTip).
	targetTip, err := e.git.Rev("origin/" + target)
	if err != nil {
		return nil, fmt.Errorf("resolve origin/%s: %w", target, err)
	}

	landed := make([]editorial.LandedMR, 0, len(mrs))
	for i, mr := range mrs {
		head, err := e.submittedBranchHead(mr)
		if err != nil {
			return nil, err
		}
		base, err := e.git.MergeBase("origin/"+target, head)
		if err != nil {
			return nil, fmt.Errorf("merge-base for %s: %w", mr.ID, err)
		}
		landed = append(landed, editorial.LandedMR{
			MRID:         mr.ID,
			ReviewedHead: mr.EditorialReviewedHead,
			Base:         base,
			Head:         head,
			TargetTip:    targetTip,
			LandedCommit: landedCommits[i],
		})
	}
	return landed, nil
}

// editorialPrecondition runs the om editorial push precondition for mrs
// about to land on target: every MR must have an approve note whose
// patch-id still matches its range and whose om version meets the
// configured floor (see editorial.CheckPrecondition). It is a no-op,
// returning (nil, nil, nil), when the rig has not set
// merge_queue.editorial.required — upstream behavior is unchanged.
//
// On success it returns the notes and the LandedMR descriptors used to
// compute them, so the caller can copy each note onto its landed commit
// after the push succeeds. On a classified failure it logs the exact
// reason, records a failure receipt, escalates to the rig witness, closes
// the offending MR when the reason is a verdict rather than a state the
// next cycle can still resolve (see rejectEditorialVerdict), and returns
// the error — the caller must not push. skipGates never bypasses this:
// callers invoke it unconditionally before the push slot is acquired.
//
// Landing via the VCS provider instead (merge_strategy=pr) has its own
// entry point, editorialPreconditionPR, because there is no local merge to
// read the landed commits off. Both funnel into checkEditorialPrecondition,
// so the two landing paths cannot drift about what the gate checks.
func (e *Engineer) editorialPrecondition(logPrefix string, mrs []*MRInfo, target string) ([]editorial.Note, []editorial.LandedMR, *editorial.PreconditionError) {
	if e.config.Editorial == nil || !e.config.Editorial.Required || len(mrs) == 0 {
		return nil, nil, nil
	}

	landed, err := e.buildLandedMRs(mrs, target)
	if err != nil {
		return nil, nil, e.failEditorialRange(logPrefix, mrs[0], err)
	}
	return e.checkEditorialPrecondition(logPrefix, mrs, landed)
}

// editorialPreconditionPR is editorialPrecondition for the merge_strategy=pr
// path, where the landing push is the VCS provider's PR merge API rather
// than a local merge followed by a push to origin.
//
// buildLandedMRs cannot serve this path: it reads each landed commit off
// HEAD's first-parent chain, and doMerge never merges locally before it
// dispatches to doMergePR. The descriptor is built from the MR's own range
// instead — merge-base(origin/target, submitted head)..submitted head, the
// same range the note's patch-id was computed over — with LandedCommit left
// empty, since the commit that will carry the note does not exist until the
// provider merges. The caller fills it in from the provider's result once
// the merge has succeeded, and copies the note (copyEditorialNotes).
//
// Without this, merge_strategy=pr merged through the provider with no
// approve-note/patch-id check at all: doMerge returns at its Step 4.5 PR
// dispatch, before the Step 7-8 block that runs the precondition on the
// local-merge path.
func (e *Engineer) editorialPreconditionPR(logPrefix string, mr *MRInfo, target string) ([]editorial.Note, []editorial.LandedMR, *editorial.PreconditionError) {
	if e.config.Editorial == nil || !e.config.Editorial.Required || mr == nil {
		return nil, nil, nil
	}
	head, err := e.submittedBranchHead(mr)
	if err != nil {
		return nil, nil, e.failEditorialRange(logPrefix, mr, err)
	}
	base, err := e.git.MergeBase("origin/"+target, head)
	if err != nil {
		return nil, nil, e.failEditorialRange(logPrefix, mr, fmt.Errorf("merge-base for %s: %w", mr.ID, err))
	}
	targetTip, err := e.git.Rev("origin/" + target)
	if err != nil {
		return nil, nil, e.failEditorialRange(logPrefix, mr, fmt.Errorf("resolve origin/%s: %w", target, err))
	}
	return e.checkEditorialPrecondition(logPrefix, []*MRInfo{mr}, []editorial.LandedMR{{
		MRID:         mr.ID,
		ReviewedHead: mr.EditorialReviewedHead,
		Base:         base,
		Head:         head,
		TargetTip:    targetTip,
	}})
}

// failEditorialRange is the ReasonRangeUnresolvable tail shared by both
// descriptor builders: the MR's range could not be resolved at all, so no
// note can be shown to apply to it. Logs the underlying git failure,
// records the failure receipt, escalates to the rig witness, and returns
// the classified error — the caller must not land the MR.
func (e *Engineer) failEditorialRange(logPrefix string, mr *MRInfo, err error) *editorial.PreconditionError {
	cerr := &editorial.PreconditionError{Class: editorial.Precondition, MR: mr.ID, Reason: editorial.ReasonRangeUnresolvable}
	_, _ = fmt.Fprintf(e.output, "%s %s: %v\n", logPrefix, cerr.Error(), err)
	e.recordEditorialFailure(mr, cerr)
	e.escalateToWitness(editorialEscalationMessage(cerr))
	return cerr
}

// checkEditorialPrecondition runs the precondition over descriptors the
// caller has already built and classifies a refusal. On success it returns
// the notes and the descriptors they were computed from, so the caller can
// copy each note onto its landed commit once the push has succeeded.
func (e *Engineer) checkEditorialPrecondition(logPrefix string, mrs []*MRInfo, landed []editorial.LandedMR) ([]editorial.Note, []editorial.LandedMR, *editorial.PreconditionError) {
	notes, cerr := editorial.CheckPrecondition(e.git, *e.config.Editorial, landed)
	if cerr == nil {
		return notes, landed, nil
	}
	_, _ = fmt.Fprintf(e.output, "%s %s\n", logPrefix, cerr.Error())
	offending := mrByID(mrs, cerr.MR)
	e.recordEditorialFailure(offending, cerr)
	e.escalateToWitness(editorialEscalationMessage(cerr))
	// The exact MR, not mrByID's fallback: closing a neighboring member
	// because the failing id was not in this push would dequeue work that
	// passed the precondition.
	e.rejectEditorialVerdict(mrByExactID(mrs, cerr.MR), cerr)
	return nil, nil, cerr
}

// editorialRefusalResult classifies an editorial push-precondition refusal
// in the ProcessResult a caller returns: a gate verdict about this exact
// diff, not a build/test/conflict failure. Callers route on
// EditorialRefused/EditorialReason rather than on Error's message text (see
// the fail-closed table in the design spec).
func editorialRefusalResult(cerr *editorial.PreconditionError) ProcessResult {
	return ProcessResult{
		Success:          false,
		EditorialRefused: true,
		EditorialReason:  cerr.Reason,
		Error:            cerr.Error(),
	}
}

// editorialVerdictReasons are the push-precondition failures that ARE a verdict
// about the diff being pushed, as opposed to a state the next cycle can still
// resolve on its own.
//
// ReasonVerdictNotApprove is the one that matters: the reviewed head carries a
// request_changes note covering exactly this range, i.e. a reviewer saw this
// diff and rejected it. The others are not verdicts — ReasonMissing means this
// range was never reviewed (the next cycle reviews it; refusing to push a
// range with no verdict yet is the precondition working as designed),
// ReasonPatchIDMismatch means the head moved since the verdict (re-reviewed,
// not rejected), ReasonVersionBelowMin is a rubric floor,
// ReasonRangeUnresolvable is a git failure, not a judgment, and
// ReasonTargetDriftMaterial means target moved onto files this range also
// touches since the note was written — nobody has reviewed the two
// combined, which is a gap in coverage, not a verdict against this diff.
var editorialVerdictReasons = map[editorial.PreconditionReason]bool{
	editorial.ReasonVerdictNotApprove: true,
}

// rejectEditorialVerdict closes mr when the push precondition refused it for a
// verdict reason (gt-bsmp). This is where an editorial rejection surfaces on
// the single-MR path: the batch path reviews its own candidates, but a batch
// of one goes straight to doMerge, so a request_changes note comes back as
// ReasonVerdictNotApprove rather than as a dropped candidate.
//
// Left open, the MR stays merge-eligible and is re-gated and re-reviewed on an
// unchanged head every cycle; a re-roll that comes back approve writes an
// approve note whose patch-id now matches, which is what lets a rejected diff
// land (gt-bveg records why a re-roll can disagree). The close happens here
// rather than in the caller for the reason refuseEmptyMerge states: a caller
// that inspects this error is not guaranteed to dequeue the MR.
//
// Unlike rejectReviewedCandidate this runs no dead-worker recovery: a
// verdict_not_approve can only fire on a range whose request_changes note was
// written by a path that owns its own redispatch — `gt mq review`'s
// quality-review step (which checks polecat liveness and mails RECOVERED_BEAD
// itself) or rejectReviewedCandidate just above (which recovers on its close).
// A second recovery here would send the deacon a duplicate redispatch request
// for work already queued for it.
func (e *Engineer) rejectEditorialVerdict(mr *MRInfo, cerr *editorial.PreconditionError) {
	if mr == nil || !editorialVerdictReasons[cerr.Reason] {
		return
	}
	if e.isSyntheticMergeMechanicsMR(mr) {
		return
	}
	reason := fmt.Sprintf("EDITORIAL REJECTION: om gate %s for %s — push refused, no approve note for this range",
		cerr.Reason, cerr.MR)
	closeReason := "rejected: " + reason
	if closeErr := e.closeMRWithReason(mr, closeReason); closeErr != nil {
		e.failRejectionClose("[Engineer]", mr, closeReason, closeErr)
		return
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: push refused for %s — closed (rejected)\n", mr.ID, cerr.Reason)
}

// failRejectionClose reports a rejection whose close did not take effect, from
// either automatic editorial-rejection path (rejectReviewedCandidate,
// rejectEditorialVerdict).
//
// The close is what takes the MR out of the merge queue (gt-bsmp). When it
// fails the MR is still ready, so the rejection is not in force: the diff
// stays merge-eligible and a re-roll that comes back approve can land it
// (gt-bveg). Refusal is no longer available — the close has already run and
// failed — so the remaining lever is the one copyEditorialNotes took for a
// landing left without its proof (gt-qvxf): record the failure so it
// aggregates, and escalate loudly. A rejection that did not take must not be
// a warning line that scrolls past.
//
// The next cycle re-reviews the same unchanged head and re-attempts this
// close, so a transient store failure clears itself; the escalation is what
// covers the case where it does not, where every later cycle reports the same
// refusal against a diff om has already rejected.
func (e *Engineer) failRejectionClose(logPrefix string, mr *MRInfo, closeReason string, closeErr error) {
	detail := fmt.Sprintf("close MR %s as %q: %v", mr.ID, closeReason, closeErr)
	_, _ = fmt.Fprintf(e.output, "%s EDITORIAL_REJECT_CLOSE_FAILED: %s — MR still ready, rejected diff still merge-eligible\n", logPrefix, detail)
	e.recordEditorialRecordFailed(mr, detail)
	e.escalateToWitness(fmt.Sprintf(
		"EDITORIAL_REJECT_CLOSE_FAILED: MR %s — %s. The rejection is not in force: the MR is still ready, so a re-roll can still land the diff om rejected. The next cycle retries this close on the same head; if it keeps failing the MR must be dequeued by hand.",
		mr.ID, detail))
}

// mrByExactID returns the MR in mrs matching id, or nil when this push does
// not contain it. It is the lookup for decisions that ACT on the MR (closing
// it); mrByID's fallback is for naming a real worker/rig in a receipt.
func mrByExactID(mrs []*MRInfo, id string) *MRInfo {
	for _, mr := range mrs {
		if mr.ID == id {
			return mr
		}
	}
	return nil
}

// mrByID returns the MR in mrs matching id, falling back to the first MR
// (so the failure receipt always names a real worker/rig) if none match.
func mrByID(mrs []*MRInfo, id string) *MRInfo {
	for _, mr := range mrs {
		if mr.ID == id {
			return mr
		}
	}
	if len(mrs) > 0 {
		return mrs[0]
	}
	return &MRInfo{ID: id}
}

func editorialEscalationMessage(cerr *editorial.PreconditionError) string {
	return fmt.Sprintf(
		"EDITORIAL_PRECONDITION: MR %s reason=%s — push refused, no approve note with matching patch-id",
		cerr.MR, cerr.Reason)
}

// recordEditorialFailure records a failure receipt (failure_class:precondition)
// for an editorial push-precondition refusal.
func (e *Engineer) recordEditorialFailure(mr *MRInfo, cerr *editorial.PreconditionError) {
	townRoot := filepath.Dir(e.rig.Path)
	rec := plugin.NewRecorder(townRoot)
	if _, err := editorial.RecordFailure(rec, e.rig.Name, mr.Worker, mr.ID, cerr.Class, cerr.Error(), 0); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record editorial failure receipt for %s: %v\n", mr.ID, err)
	}
}

// recordEditorialRecordFailed records a failure_class:record_failed receipt
// for mr — used when a verdict exists but the write that carries it through
// did not land: the proof could not be attached to (or published on) the
// commit that actually landed (copyEditorialNotes), or the rejection's close
// left the MR in the queue (failRejectionClose). Both are the same defect —
// a verdict with no durable effect — which is the class the design doc
// reserves: "a verdict without proof is the defect class this design
// removes." Mirrors recordEditorialFailure's precondition receipt.
func (e *Engineer) recordEditorialRecordFailed(mr *MRInfo, reason string) {
	townRoot := filepath.Dir(e.rig.Path)
	rec := plugin.NewRecorder(townRoot)
	if _, err := editorial.RecordFailure(rec, e.rig.Name, mr.Worker, mr.ID, editorial.RecordFailed, reason, 0); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record editorial record_failed receipt for %s: %v\n", mr.ID, err)
	}
}

// escalateToWitness nudges the rig's witness — routine process signals use
// nudge (no permanent record), not mail, per the town's Dolt-health
// communication guidance. escalateFn replaces the nudge in tests, which must
// not reach a live witness.
func (e *Engineer) escalateToWitness(msg string) {
	if e.escalateFn != nil {
		e.escalateFn(msg)
		return
	}
	target := fmt.Sprintf("%s/witness", e.rig.Name)
	cmd := exec.Command("gt", "nudge", target, msg)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = e.workDir
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge witness about editorial precondition failure: %v\n", err)
	}
}

// copyEditorialNotes copies each note onto its landed commit and pushes
// the notes ref, when the precondition actually ran (notes is nil when
// editorial is not required, in which case there is nothing to copy).
//
// The code has already landed on target by the time this runs (it is
// called after the push succeeds), so a failure here cannot refuse the
// push the way a precondition failure does — main has already moved.
// What it must not do is what it did before: log a Warning and return,
// leaving the landed commit(s) with no reachable proof and nobody told.
// A verdict without proof on the commit `git log` actually shows is
// exactly the defect class the design removes (see record_failed in the
// fail-closed table), so a copy or push failure here records that class
// per MR and escalates to the witness instead — fail-closed by loud
// escalation, since fail-closed by refusal is no longer possible.
func (e *Engineer) copyEditorialNotes(logPrefix string, landed []editorial.LandedMR, notes []editorial.Note, mrs []*MRInfo) {
	if len(notes) == 0 {
		return
	}
	if err := editorial.CopyNotesToLanded(e.git, landed, notes); err != nil {
		e.failEditorialRecordFailed(logPrefix, mrs, fmt.Sprintf("copy editorial note(s) to landed commit: %v", err))
		return
	}
	if err := e.git.PushNotes("origin", editorial.NotesRef); err != nil {
		e.failEditorialRecordFailed(logPrefix, mrs, fmt.Sprintf("push editorial notes ref: %v", err))
	}
}

// failEditorialRecordFailed logs reason, records a record_failed failure
// receipt for every landed MR (each one's proof is separately missing
// from target), and nudges the witness once — used by copyEditorialNotes
// when the landed commit(s) end up without reachable editorial proof.
func (e *Engineer) failEditorialRecordFailed(logPrefix string, mrs []*MRInfo, reason string) {
	_, _ = fmt.Fprintf(e.output, "%s EDITORIAL_RECORD_FAILED: %s\n", logPrefix, reason)
	for _, mr := range mrs {
		e.recordEditorialRecordFailed(mr, reason)
	}
	e.escalateToWitness(fmt.Sprintf(
		"EDITORIAL_RECORD_FAILED: %s — landed commit(s) missing editorial proof", reason))
}
