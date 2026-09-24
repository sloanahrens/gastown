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
