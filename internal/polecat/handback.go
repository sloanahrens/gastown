package polecat

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// NeedsSplitLabel is the label HandBack puts on a bead. The dispatch hold rule
// reads it (internal/dispatch humanHoldLabels), so the bead stays out of the
// pool until a person cuts it smaller and removes the label.
const NeedsSplitLabel = "needs-split"

// ReworkLabel is the label HandBackRework puts on a bead. It is the label the
// landing path puts on a rejected branch (land.LabelRework), which dispatch
// already hands to a polecat without a hold.
const ReworkLabel = "rework"

// ErrHandBackSkipped is what HandBack wraps when it finds the bead no longer
// the stopped polecat's to release: closed, or assigned to another polecat. It
// wrote nothing. The caller treats it as a hand-back that needs no repeat.
var ErrHandBackSkipped = errors.New("hand-back skipped")

// BeadWriter is the bead read and writes HandBack makes. It is deliberately
// narrow: no method reaches a branch or a worktree, so a hand-back cannot lose
// a polecat's work.
type BeadWriter interface {
	// State returns the bead's current status and assignee.
	State(ctx context.Context, id string) (status, assignee string, err error)
	SetStatus(ctx context.Context, id, status string) error
	ClearAssignee(ctx context.Context, id string) error
	AddLabel(ctx context.Context, id, label string) error
	Comment(ctx context.Context, id, text string) error
}

// HandBack returns a stopped polecat's bead to the pool: status open, assignee
// cleared, label needs-split, and one comment naming the kept branch and how
// long the polecat ran. The branch and worktree are not touched; the comment is
// where the next reader finds the work.
//
// The writes happen in that order and stop at the first error, leaving the
// earlier ones in place. A caller repeats a failed hand-back, and in between
// the dispatcher may have slung the half-released bead (open, unassigned) to
// another polecat. So HandBack first reads the bead and writes nothing when it
// is closed or assigned to anyone but assignee, the stopped polecat's bead
// assignee. An empty assignee is the half-released bead and is finished.
func HandBack(ctx context.Context, w BeadWriter, beadID, assignee, branch string, elapsed time.Duration) error {
	return HandBackNoted(ctx, w, beadID, assignee, branch, elapsed, "")
}

// HandBackNoted is HandBack with note appended to the comment: the caller's
// warning to the next reader, such as that the worktree could not be
// checkpointed and may hold uncommitted work. An empty note adds nothing.
func HandBackNoted(ctx context.Context, w BeadWriter, beadID, assignee, branch string, elapsed time.Duration, note string) error {
	text := fmt.Sprintf("polecat stopped after %s; work kept on branch %s. Split the bead, then remove the %s label.",
		elapsed.Round(time.Second), branch, NeedsSplitLabel)
	if note != "" {
		text += " " + note
	}
	return handBackLabelled(ctx, w, beadID, assignee, NeedsSplitLabel, text)
}

// HandBackRework is HandBackNoted for a stopped polecat whose finished work is
// already on origin: sha is the head of branch there. The bead is not split but
// sent back to dispatch as rework, and the comment tells the next polecat to
// reuse the branch and run gt done. The writes, their order and the checks
// before them are HandBack's, with the label rework in place of needs-split.
func HandBackRework(ctx context.Context, w BeadWriter, beadID, assignee, branch, sha string, elapsed time.Duration, note string) error {
	text := fmt.Sprintf("polecat stopped after %s; its finished work at %s is already on origin as branch %s. "+
		"The next polecat must reuse branch %s and run gt done; do not split the bead.",
		elapsed.Round(time.Second), sha, branch, branch)
	if note != "" {
		text += " " + note
	}
	return handBackLabelled(ctx, w, beadID, assignee, ReworkLabel, text)
}

// HandBackNoWork is HandBackNoted for a stopped polecat that made no commit of its own: the bead
// goes back open and unassigned with no label, and the comment says the stop is not a size
// finding, so the dispatcher hands it to the next polecat fresh and nobody splits a bead whose
// seat never worked on it (an idle seat after a cut model stream, gt-efj3xs). The writes, their
// order and the checks before them are HandBack's, without the label step.
func HandBackNoWork(ctx context.Context, w BeadWriter, beadID, assignee string, elapsed time.Duration, note string) error {
	text := fmt.Sprintf("polecat stopped after %s with no commit on its branch: this is not a size finding, so do not split the bead; the next polecat starts it fresh.",
		elapsed.Round(time.Second))
	if note != "" {
		text += " " + note
	}
	return handBackLabelled(ctx, w, beadID, assignee, "", text)
}

// HandBackEmptyBranch is HandBackNoted for a stopped polecat whose kept branch
// holds no commit beyond target, the rig's landing branch: nothing to resume or
// split. Like HandBackNoWork, the bead goes back open, unassigned and unlabelled.
func HandBackEmptyBranch(ctx context.Context, w BeadWriter, beadID, assignee, branch, target string, elapsed time.Duration, note string) error {
	text := fmt.Sprintf("polecat stopped after %s; branch %s holds no commits beyond %s: nothing to resume, the bead starts clean.",
		elapsed.Round(time.Second), branch, target)
	if note != "" {
		text += " " + note
	}
	return handBackLabelled(ctx, w, beadID, assignee, "", text)
}

// handBackLabelled is the hand-back every HandBack variant shares: it releases
// the bead, puts label on it (none when label is empty) and adds text as the comment.
func handBackLabelled(ctx context.Context, w BeadWriter, beadID, assignee, label, text string) error {
	status, current, err := w.State(ctx, beadID)
	if err != nil {
		return fmt.Errorf("hand back %s: read bead: %w", beadID, err)
	}
	if status == "closed" || status == "tombstone" {
		return fmt.Errorf("hand back %s: %w: bead is %s", beadID, ErrHandBackSkipped, status)
	}
	if current != "" && current != assignee {
		return fmt.Errorf("hand back %s: %w: bead is assigned to %s, not %s", beadID, ErrHandBackSkipped, current, assignee)
	}
	if err := w.SetStatus(ctx, beadID, "open"); err != nil {
		return fmt.Errorf("hand back %s: set status: %w", beadID, err)
	}
	if err := w.ClearAssignee(ctx, beadID); err != nil {
		return fmt.Errorf("hand back %s: clear assignee: %w", beadID, err)
	}
	if label != "" {
		if err := w.AddLabel(ctx, beadID, label); err != nil {
			return fmt.Errorf("hand back %s: add label: %w", beadID, err)
		}
	}
	if err := w.Comment(ctx, beadID, text); err != nil {
		return fmt.Errorf("hand back %s: comment: %w", beadID, err)
	}
	return nil
}
