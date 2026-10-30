package specdispatch

import "strings"

// A sling killed at its own bound is the host's load reaching the sling, not a
// defect in its bead. The daemon records each kill in the bead's notes, and the
// dispatcher clears the failure label the kill left and slings the bead again
// while its retries remain, so a loaded host does not leave one stuck bead per
// sling for a person to clear (gt-vaivp).

// TimeoutNoteMarker heads the note a sling killed at its bound leaves on its
// bead. Each such note is one kill, so the count of them is the attempts the
// bead has had.
const TimeoutNoteMarker = "spec-dispatch-timeout:"

// TimeoutRetries bounds the slings a bead whose process is killed at its bound
// is given: the kill that reaches it spends the last retry, and the label that
// kill writes stays for a person.
const TimeoutRetries = 3

// TimeoutAttempts counts the bound kills a failed bead's notes record.
func TimeoutAttempts(notes string) int {
	attempts := 0
	for _, line := range strings.Split(notes, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), TimeoutNoteMarker) {
			attempts++
		}
	}
	return attempts
}

// RetryAfterTimeout reads a failed bead's notes: attempt is the bound kills they
// record, and retry reports whether the bead should be slung again. The last
// failure has to be one of those kills, so a bead that timed out and then failed
// for a reason of its own is that reason's bead, and the retries have to be
// unspent.
func RetryAfterTimeout(notes string) (attempt int, retry bool) {
	attempt = TimeoutAttempts(notes)
	if attempt == 0 || attempt >= TimeoutRetries {
		return attempt, false
	}
	return attempt, lastFailureIsTimeout(notes)
}

// lastFailureIsTimeout reports whether the last failure note in notes is a
// bound kill. Both the daemon's notes start with "spec-dispatch: " except the
// timeout one, which starts with TimeoutNoteMarker, so the marker is what tells
// the two kinds apart.
func lastFailureIsTimeout(notes string) bool {
	last := ""
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, TimeoutNoteMarker) || strings.HasPrefix(line, "spec-dispatch: ") {
			last = line
		}
	}
	return strings.HasPrefix(last, TimeoutNoteMarker)
}
