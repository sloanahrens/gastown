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

// slingDeferralReason extracts the reason a failed sling should be deferred
// rather than logged as a failure: a capacity refusal
// (dispatch.SlingRefusalMarker), or sling's refusal to re-sling a bead whose
// dead holder's work survives or cannot be verified
// (dispatch.ReslingRefusalMarker, gt-vm5g4). That work is preserved; an
// operator resumes or discards it. It reports false for every other failure.
// The reason is the refusal's own line from its marker on.
func slingDeferralReason(stderr string) (string, bool) {
	if reason, ok := slingBackpressureReason(stderr); ok {
		return reason, true
	}
	return dispatch.ReslingRefusalReason(stderr)
}
