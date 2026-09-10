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
// (unchanged by merge-commit stacking); Base is recomputed against
// origin/target right now, not read from the reviewed note, so a rebase or
// conflict resolution that changed the diff since review produces a
// different patch-id and the precondition below catches it.
func (e *Engineer) buildLandedMRs(mrs []*MRInfo, target string) ([]editorial.LandedMR, error) {
	landed := make([]editorial.LandedMR, 0, len(mrs))
	for _, mr := range mrs {
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
// reason, records a failure receipt, escalates to the rig witness, and
// returns the error — the caller must not push. skipGates never bypasses
// this: callers invoke it unconditionally before the push slot is
// acquired.
func (e *Engineer) editorialPrecondition(logPrefix string, mrs []*MRInfo, target string) ([]editorial.Note, []editorial.LandedMR, *editorial.ClassifiedError) {
	if e.config.Editorial == nil || !e.config.Editorial.Required || len(mrs) == 0 {
		return nil, nil, nil
	}

	landed, err := e.buildLandedMRs(mrs, target)
	if err != nil {
		cerr := &editorial.ClassifiedError{Class: editorial.Precondition, MR: mrs[0].ID, Reason: err.Error()}
		_, _ = fmt.Fprintf(e.output, "%s %s\n", logPrefix, cerr.Error())
		e.recordEditorialFailure(mrs[0], cerr)
		e.escalateToWitness(editorialEscalationMessage(cerr))
		return nil, nil, cerr
	}

	notes, cerr := editorial.CheckPrecondition(e.git, *e.config.Editorial, landed)
	if cerr != nil {
		_, _ = fmt.Fprintf(e.output, "%s %s\n", logPrefix, cerr.Error())
		e.recordEditorialFailure(mrByID(mrs, cerr.MR), cerr)
		e.escalateToWitness(editorialEscalationMessage(cerr))
		return nil, nil, cerr
	}
	return notes, landed, nil
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

func editorialEscalationMessage(cerr *editorial.ClassifiedError) string {
	return fmt.Sprintf(
		"EDITORIAL_PRECONDITION: MR %s reason=%s — push refused, no approve note with matching patch-id",
		cerr.MR, cerr.Reason)
}

// recordEditorialFailure records a failure receipt (failure_class:precondition)
// for an editorial push-precondition refusal.
func (e *Engineer) recordEditorialFailure(mr *MRInfo, cerr *editorial.ClassifiedError) {
	townRoot := filepath.Dir(e.rig.Path)
	rec := plugin.NewRecorder(townRoot)
	if _, err := editorial.RecordFailure(rec, e.rig.Name, mr.Worker, mr.ID, cerr.Class, cerr.Error(), 0); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record editorial failure receipt for %s: %v\n", mr.ID, err)
	}
}

// escalateToWitness nudges the rig's witness — routine process signals use
// nudge (no permanent record), not mail, per the town's Dolt-health
// communication guidance.
func (e *Engineer) escalateToWitness(msg string) {
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
func (e *Engineer) copyEditorialNotes(logPrefix string, landed []editorial.LandedMR, notes []editorial.Note) {
	if len(notes) == 0 {
		return
	}
	if err := editorial.CopyNotesToLanded(e.git, landed, notes); err != nil {
		_, _ = fmt.Fprintf(e.output, "%s Warning: failed to copy editorial note(s): %v\n", logPrefix, err)
		return
	}
	if err := e.git.PushNotes("origin", editorial.NotesRef); err != nil {
		_, _ = fmt.Fprintf(e.output, "%s Warning: failed to push editorial notes ref: %v\n", logPrefix, err)
	}
}
