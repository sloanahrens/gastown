package daemon

import "strings"

// slingBackpressureMarker is the leading text of the refusal `gt sling` prints
// when the target rig's merge queue is over merge_queue.max_ready_for_dispatch
// (internal/cmd/sling_backpressure.go, gt-xidg / plan Task 3, A3).
//
// The feeder is a subprocess caller, so this string — not the typed error — is
// the contract between the guard and the daemon, exactly as slingStepPrefix
// carries the timing lines. It is asserted from both sides: the guard's own
// test pins the full message, and the feeder's test pins the parse below.
const slingBackpressureMarker = "sling refused:"

// slingBackpressureReason extracts the refusal from a failed sling's stderr,
// reporting false for every other failure. A refusal is the town at capacity,
// not a broken sling: the caller defers the bead instead of logging a failure.
//
// The whole line is returned from the marker on, so cobra's "Error: " and the
// spawn path's "spawning polecat: " wrapping are dropped and the logged reason
// is the guard's own sentence.
func slingBackpressureReason(stderr string) (string, bool) {
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.Index(line, slingBackpressureMarker); i >= 0 {
			return strings.TrimSpace(line[i:]), true
		}
	}
	return "", false
}
