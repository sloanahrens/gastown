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

// RecordReceipt records the aggregation-only receipt bead for a verdict
// that was obtained (approve or request_changes). result is always
// "success" here — a verdict, not the review outcome, is what "success"
// means for this receipt; the outcome itself lives in the
// recommendation: label and in the note.
func RecordReceipt(rec *plugin.Recorder, n Note) (string, error) {
	labels := []string{
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
func RecordFailure(rec *plugin.Recorder, rig, worker, mr string, fc FailureClass, stderr string, retries int) (string, error) {
	desc := stderr
	if len(desc) > maxFailureDescription {
		desc = desc[:maxFailureDescription]
	}
	labels := []string{
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
