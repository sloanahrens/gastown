package dispatch

import "strings"

// SlingRefusalMarker is the leading text of every refusal `gt sling` prints for
// a bead that is fine and simply not the sling's to take: the merge queue over
// merge_queue.max_ready_for_dispatch (internal/cmd/sling_backpressure.go,
// gt-xidg), the polecat pool with every seat at its cap
// (internal/cmd/sling_pool.go, gt-jzr1), a bead reserved for the human
// operator (dispatch.OperatorReservation, gt-21pl0), and a rework bead the
// town's steward owns while its patrol runs live
// (daemon.StewardReworkOwner, gt-28ibg). None of them is a broken sling:
// capacity refusals wait for the town to drain, an operator-reserved bead is
// never an automatic dispatcher's to take at all, and a steward-owned
// rejection is settled by the steward's own job.
//
// Automatic dispatchers run sling as a subprocess, so this string — not the
// typed error — is the contract between the guards and their callers. It lives
// here, below both internal/daemon and internal/deacon, because daemon imports
// deacon and the deacon's redispatch needs the same parse (gt-xdaq).
const SlingRefusalMarker = "sling refused:"

// SlingRefusalReason extracts a refusal from a failed sling's stderr, reporting
// false for every other failure. A refusal means "not this sling's to take",
// not "this bead failed": callers defer the bead instead of counting a failure.
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

// ReslingRefusalMarker leads sling's refusal to re-sling a bead whose dead
// holder's work survives on a branch, or whose survival cannot be verified
// (internal/cmd reslingSurvivingWorkGuard, gt-vm5g4). Unlike
// SlingRefusalMarker it is not the town at capacity: the work is preserved,
// and an operator resumes it with --branch or discards it with --force. For
// an automatic dispatcher it is still a deferral, never a failed attempt.
const ReslingRefusalMarker = "refusing to re-sling"
