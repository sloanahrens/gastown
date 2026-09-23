package editorial

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/plugin"
)

// pluginName is the receipt plugin name recorded by both RecordReceipt and
// RecordFailure — the same name om-gate.sh recorded under before this
// package took over writing it, so gt plugin history quality-review-result
// keeps working unchanged.
const pluginName = "quality-review-result"

// ReceiptOutcome is which of the two things a quality-review result wisp
// records. It needs a label of its own because result: cannot say it:
// result:success means "a verdict was obtained", so a request_changes
// rejection is written result:success, and result:failure covers both an
// unreviewed merge and a push refusal. A consumer reading result: alone
// reads a rejected diff as a clean merge and an unreviewed diff as a
// rejected one (gt-47nf).
type ReceiptOutcome string

const (
	// OutcomeVerdict marks a receipt carrying a score and a
	// recommendation: om looked at the diff and returned a judgment. See
	// RecordReceipt.
	OutcomeVerdict ReceiptOutcome = "verdict"
	// OutcomeInfraFailure marks a receipt carrying a failure_class and no
	// score: no judgment about the diff exists, so the merge went
	// unreviewed. See RecordFailure.
	OutcomeInfraFailure ReceiptOutcome = "infra_failure"
)

// RecordReceipt records the aggregation-only receipt bead for a verdict
// that was obtained (approve or request_changes). result is always
// "success" here — a verdict, not the review outcome, is what "success"
// means for this receipt; the outcome itself lives in the outcome: and
// recommendation: labels and in the note.
//
// A note whose verdict is not one the schema admits is refused rather than
// recorded: a receipt reading result:success with no usable
// recommendation: is a merge the aggregator counts as reviewed and nobody
// can read (gt-47nf).
func RecordReceipt(rec *plugin.Recorder, n Note) (string, error) {
	if !ValidVerdict(n.Verdict) {
		return "", fmt.Errorf("refusing to record a receipt for verdict %q: want %s or %s",
			n.Verdict, VerdictApprove, VerdictRequestChanges)
	}
	labels := []string{
		fmt.Sprintf("outcome:%s", OutcomeVerdict),
		fmt.Sprintf("worker:%s", n.Worker),
		fmt.Sprintf("score:%g", n.Score),
		fmt.Sprintf("recommendation:%s", n.Verdict),
		fmt.Sprintf("patch_id:%s", n.PatchID),
		fmt.Sprintf("head_sha:%s", n.HeadSHA),
		fmt.Sprintf("om_version:%s", n.OMVersion),
		fmt.Sprintf("attempt:%d", n.Attempt),
	}
	if len(n.Followups) > 0 {
		labels = append(labels, fmt.Sprintf("followups:%d", len(n.Followups)))
	}
	return rec.RecordRun(plugin.PluginRunRecord{
		PluginName:  pluginName,
		RigName:     n.Rig,
		Result:      plugin.ResultSuccess,
		Title:       fmt.Sprintf("quality-review: Score %g, %s", n.Score, n.Verdict),
		ExtraLabels: labels,
	})
}

// maxFailureDescription caps the stderr captured into a failure receipt's
// description, per the spec's "truncated 4 KiB".
const maxFailureDescription = 4 * 1024

// RecordFailure records a failure receipt for an infra failure that never
// produced a verdict (or failed to record one that was produced). No note
// is written for a failure — a note's existence means "om produced a
// verdict", which did not happen here.
//
// The class is validated first: it is the only quality signal this receipt
// carries, so an empty or unregistered one would write a wisp that looks
// like a failure and answers nothing. A refused write returns the error
// instead of recording (gt-47nf).
func RecordFailure(rec *plugin.Recorder, rig, worker, mr string, fc FailureClass, stderr string, retries int) (string, error) {
	if !fc.Valid() {
		return "", fmt.Errorf("refusing to record an untyped failure receipt for %s: failure class %q is not one this binary writes", mr, fc)
	}
	desc := stderr
	if len(desc) > maxFailureDescription {
		desc = desc[:maxFailureDescription]
	}
	labels := []string{
		fmt.Sprintf("outcome:%s", OutcomeInfraFailure),
		fmt.Sprintf("worker:%s", worker),
		fmt.Sprintf("mr:%s", mr),
		fmt.Sprintf("failure_class:%s", fc),
		fmt.Sprintf("retries:%d", retries),
	}
	return rec.RecordRun(plugin.PluginRunRecord{
		PluginName:  pluginName,
		RigName:     rig,
		Result:      plugin.ResultFailure,
		Title:       fmt.Sprintf("quality-review: failure (%s)", fc),
		Body:        desc,
		ExtraLabels: labels,
	})
}
