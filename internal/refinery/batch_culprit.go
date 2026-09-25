package refinery

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// Batch-culprit marks (gt-gz8l). Batching runs before the single-MR path on
// every refinery cycle, so a culprit bisection isolated stays "ready" in the
// queue and gets stacked again by the next batch: the full gate, the flaky
// retry, and the bisection all re-derive the answer already known, while the
// batch's good MRs wait behind it. Recording the verdict on the MR bead lets
// 'gt mq batch candidates' skip it until its branch head moves, so the culprit
// is handled alone by process-branch (gate → reject with the real failure).
//
// The head is the key rather than the MR: a rework that moves the branch head
// no longer matches the mark, so the MR is batch-eligible again.
const batchCulpritLabelPrefix = "batch-culprit:"

// batchCulpritMarkedHeads returns every branch head the batch-culprit marks on
// labels name, in label order.
func batchCulpritMarkedHeads(labels []string) []string {
	var heads []string
	for _, label := range labels {
		if head, ok := strings.CutPrefix(strings.TrimSpace(label), batchCulpritLabelPrefix); ok && head != "" {
			heads = append(heads, head)
		}
	}
	return heads
}

// MRMarkedBatchCulprit reports whether mr carries a batch-culprit mark naming
// its current head — i.e. the MR is the one a previous batch isolated at the
// revision it still carries.
//
// An MR with no recorded commit_sha counts as marked: the head cannot be shown
// to have moved, and such an MR cannot be stacked anyway (submittedBranchHead
// refuses a missing submitted head), so keeping it out of a batch costs
// nothing.
func MRMarkedBatchCulprit(mr *MRInfo) bool {
	if mr == nil {
		return false
	}
	heads := batchCulpritMarkedHeads(mr.Labels)
	if len(heads) == 0 {
		return false
	}
	head := strings.TrimSpace(mr.CommitSHA)
	if head == "" {
		return true
	}
	for _, marked := range heads {
		if marked == head {
			return true
		}
	}
	return false
}

// recordCulprits reports culprits on result and marks their beads, so a caller
// cannot record a culprit verdict without also recording it for the next
// batch. Both halves belong together: a culprit left unmarked is stacked again
// by the next batch (see the comment on the label prefix above).
func (e *Engineer) recordCulprits(result *BatchResult, culprits []*MRInfo) {
	result.Culprits = culprits
	e.recordBatchCulprits(culprits)
}

// recordBatchCulprits marks every MR a bisection or failed gate isolated.
//
// Best-effort by design: a batch that already landed its good MRs must not be
// failed by a beads write, so every failure here is reported and skipped. The
// mark is written first and marks naming other heads cleared after, as a
// separate write — a cleanup failure then cannot cost the mark.
func (e *Engineer) recordBatchCulprits(culprits []*MRInfo) {
	if e.beads == nil {
		return
	}
	for _, mr := range culprits {
		if mr == nil || strings.TrimSpace(mr.ID) == "" || e.isSyntheticMergeMechanicsMR(mr) {
			continue
		}
		head := strings.TrimSpace(mr.CommitSHA)
		if head == "" {
			_, _ = fmt.Fprintf(e.output, "[Batch] MR %s: no submitted head recorded, cannot mark it a batch culprit\n", mr.ID)
			continue
		}
		if err := e.beads.Update(mr.ID, beads.UpdateOptions{AddLabels: []string{batchCulpritLabelPrefix + head}}); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Batch] Warning: could not mark MR %s as a batch culprit: %v\n", mr.ID, err)
			continue
		}
		_, _ = fmt.Fprintf(e.output, "[Batch] MR %s marked as a batch culprit at %s — the next batch skips it until its head moves\n", mr.ID, shortSHA(head))

		// Marks naming a head this MR no longer carries are stale; clearing
		// them keeps the bead's labels from accumulating across reworks. The
		// head just marked is left alone so no add/remove pair can cancel it.
		var stale []string
		for _, marked := range batchCulpritMarkedHeads(mr.Labels) {
			if marked != head {
				stale = append(stale, batchCulpritLabelPrefix+marked)
			}
		}
		if len(stale) == 0 {
			continue
		}
		if err := e.beads.Update(mr.ID, beads.UpdateOptions{RemoveLabels: stale}); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Batch] Warning: could not clear stale batch-culprit marks on MR %s: %v\n", mr.ID, err)
		}
	}
}
