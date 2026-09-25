package daemon

import "github.com/steveyegge/gastown/internal/dispatch"

// slingBackpressureReason extracts the refusal from a failed sling's stderr,
// reporting false for every other failure. A refusal is the town at capacity,
// not a broken sling: the caller defers the bead instead of logging a failure.
//
// The marker contract (dispatch.SlingRefusalMarker) lives in internal/dispatch
// so the deacon's redispatch, which daemon imports and which therefore cannot
// import daemon, parses refusals the same way (gt-xdaq).
func slingBackpressureReason(stderr string) (string, bool) {
	return dispatch.SlingRefusalReason(stderr)
}

// slingSurvivingWorkMarker leads sling's refusal to re-sling a bead whose
// dead holder's work survives on a branch, or whose survival cannot be
// verified (internal/cmd reslingSurvivingWorkGuard, gt-vm5g4). The work is
// preserved, not lost: an operator resumes it with --branch or discards it
// with --force. For the feeder that is a deferral, like backpressure, not a
// broken sling.
const slingSurvivingWorkMarker = "refusing to re-sling"

// slingDeferralReason extracts the reason a failed sling should be deferred
// rather than logged as a failure: a backpressure refusal or a surviving-work
// refusal. It reports false for every other failure. The reason is the
// refusal's own line from its marker on.
func slingDeferralReason(stderr string) (string, bool) {
	if reason, ok := slingBackpressureReason(stderr); ok {
		return reason, true
	}
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.Index(line, slingSurvivingWorkMarker); i >= 0 {
			return strings.TrimSpace(line[i:]), true
		}
	}
	return "", false
}
