package refinery

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/util"
)

// Editorial-drop marks (gt-crvw0). An infra-class review result (Exit 2 — a
// tooling/rehearsal failure, not a verdict about the diff) leaves its
// candidate queued and untouched for the next cycle (see
// reviewBatchCandidates). That is the right call when the failure is
// transient, but when it is deterministic the same candidate is retried
// forever with zero visibility and no path to ever land — gt-wisp-06j7 sat
// dropped for 7-8 consecutive batches before anyone noticed.
//
// Recording a per-head drop count on the MR bead lets a batch recognize
// "this has already failed at this exact revision N times" and escalate
// once, rather than looping silently. The head is the key, like
// batch-culprit's mark: a rework that moves the branch head means the next
// failure (if any) is a new problem, so the count starts over.
const editorialDropLabelPrefix = "editorial-drop:"

// editorialDropEscalatedLabelPrefix marks a head whose drop count already
// triggered an escalation, so recordEditorialDrop fires it once per head
// rather than every remaining cycle.
const editorialDropEscalatedLabelPrefix = "editorial-drop-escalated:"

// editorialDropEscalationThreshold is how many consecutive drops at the same
// branch head trigger an escalation. Greater than 1 so a single transient
// blip (a busy host, a momentary fetch failure) never pages anyone — gt-crvw0
// asked for "N>1".
const editorialDropEscalationThreshold = 3

// editorialDropCount returns the recorded consecutive-drop count for head
// from labels, or 0 if head has never been marked — including when the only
// mark present names a different, now-superseded head.
func editorialDropCount(labels []string, head string) int {
	prefix := editorialDropLabelPrefix + head + ":"
	for _, label := range labels {
		countStr, ok := strings.CutPrefix(strings.TrimSpace(label), prefix)
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(countStr); err == nil {
			return n
		}
	}
	return 0
}

// editorialDropEscalated reports whether head's drop count already triggered
// an escalation.
func editorialDropEscalated(labels []string, head string) bool {
	want := editorialDropEscalatedLabelPrefix + head
	for _, label := range labels {
		if strings.TrimSpace(label) == want {
			return true
		}
	}
	return false
}

// recordEditorialDrop marks mr's consecutive-drop count for its current
// branch head, clears marks naming a superseded head, and escalates once the
// count reaches editorialDropEscalationThreshold.
//
// Best-effort by design, like recordBatchCulprits: a batch that already
// dropped this candidate (correctly, per reviewBatchCandidates) must not be
// failed by a beads write, so every failure here is reported and skipped.
func (e *Engineer) recordEditorialDrop(mr *MRInfo, class editorial.FailureClass, stderr string) {
	if e.beads == nil || mr == nil || strings.TrimSpace(mr.ID) == "" || e.isSyntheticMergeMechanicsMR(mr) {
		return
	}
	head := strings.TrimSpace(mr.CommitSHA)
	if head == "" {
		return
	}

	count := editorialDropCount(mr.Labels, head) + 1
	newLabel := fmt.Sprintf("%s%s:%d", editorialDropLabelPrefix, head, count)

	if err := e.beads.Update(mr.ID, beads.UpdateOptions{AddLabels: []string{newLabel}}); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Batch] Warning: could not record drop count for MR %s: %v\n", mr.ID, err)
		return
	}

	// Marks naming a head this MR no longer carries, or an earlier count for
	// the current head, are stale — cleared as a separate write, after the
	// new mark lands, so a cleanup failure cannot cost the count just
	// recorded (mirrors recordBatchCulprits).
	var stale []string
	for _, label := range mr.Labels {
		trimmed := strings.TrimSpace(label)
		if strings.HasPrefix(trimmed, editorialDropLabelPrefix) && trimmed != newLabel {
			stale = append(stale, trimmed)
		}
	}
	if len(stale) > 0 {
		if err := e.beads.Update(mr.ID, beads.UpdateOptions{RemoveLabels: stale}); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Batch] Warning: could not clear stale drop marks on MR %s: %v\n", mr.ID, err)
		}
	}

	if count < editorialDropEscalationThreshold || editorialDropEscalated(mr.Labels, head) {
		return
	}
	e.escalateEditorialDrop(mr, class, stderr, head, count)
}

// escalateEditorialDrop fires the first time recordEditorialDrop's count
// reaches the threshold for head: a deterministic infra failure with no
// rework path otherwise loops forever with nobody told (gt-crvw0). It marks
// the head escalated first, so a failure in the exec or comment steps below
// cannot leave escalation stuck re-firing every cycle.
func (e *Engineer) escalateEditorialDrop(mr *MRInfo, class editorial.FailureClass, stderr, head string, count int) {
	if err := e.beads.Update(mr.ID, beads.UpdateOptions{AddLabels: []string{editorialDropEscalatedLabelPrefix + head}}); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Batch] Warning: could not record editorial-drop-escalated mark on MR %s: %v\n", mr.ID, err)
	}

	_, _ = fmt.Fprintf(e.output, "[Batch] MR %s: dropped %d times at %s with no rework path — escalating\n", mr.ID, count, shortSHA(head))
	msg := fmt.Sprintf("MR %s dropped from %d consecutive editorial batches at %s (%s): %s",
		mr.ID, count, shortSHA(head), class, stderr)
	escalateCmd := exec.Command("gt", "escalate", "--severity", "high", "--reason", "editorial-drop-stuck", msg)
	util.SetDetachedProcessGroup(escalateCmd)
	escalateCmd.Dir = e.workDir
	if err := escalateCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Batch] Warning: editorial-drop escalation failed: %v\n", err)
	}

	if commentErr := e.beads.AddComment(mr.ID, fmt.Sprintf("editorial_drop_escalated: %d consecutive drops at %s (%s): %s", count, shortSHA(head), class, stderr)); commentErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Batch] Warning: could not record editorial_drop_escalated comment on %s: %v\n", mr.ID, commentErr)
	}
}
