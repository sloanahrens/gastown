package dispatch

import "strings"

// SlingRefusalMarker is the leading text of every capacity refusal `gt sling`
// prints: the merge queue over merge_queue.max_ready_for_dispatch
// (internal/cmd/sling_backpressure.go, gt-xidg) and the polecat pool with every
// seat at its cap (internal/cmd/sling_pool.go, gt-jzr1). Both are the town at
// capacity rather than a broken sling.
//
// Automatic dispatchers run sling as a subprocess, so this string — not the
// typed error — is the contract between the guards and their callers. It lives
// here, below both internal/daemon and internal/deacon, because daemon imports
// deacon and the deacon's redispatch needs the same parse (gt-xdaq).
const SlingRefusalMarker = "sling refused:"

// SlingRefusalReason extracts a capacity refusal from a failed sling's stderr,
// reporting false for every other failure. A refusal means "retry later", not
// "this bead failed": callers defer the bead instead of counting a failure.
//
// The line is returned from the marker on, so cobra's "Error: " and the spawn
// path's "spawning polecat: " wrapping are dropped and the reason is the
// guard's own sentence.
func SlingRefusalReason(stderr string) (string, bool) {
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.Index(line, SlingRefusalMarker); i >= 0 {
			return strings.TrimSpace(line[i:]), true
		}
	}
	return "", false
}
