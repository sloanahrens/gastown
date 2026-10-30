package specdispatch

import (
	"encoding/json"
	"slices"
	"strconv"

	"github.com/steveyegge/gastown/internal/constants"
)

// A sling killed at its own bound is the host's load reaching the sling, not a
// defect in its bead. The daemon records such a kill on the bead with the
// timeout label and a count of the kills so far, and the dispatcher clears the
// failure label the kill left and slings the bead again while its retries
// remain, so a loaded host does not leave one stuck bead per sling for a person
// to clear (gt-vaivp).
//
// The count lives in bead metadata rather than in the note that says why the
// bead was held: notes are prose for people, and anything that reads them for
// state is a convention every other writer to the bead can break.

// TimeoutLabel marks a bead whose sling was killed at its bound. It is written
// with DispatchFailedLabel, and the failure that is not a timeout kill removes
// it, so the pair on a bead says its last failure was the one this retry
// answers. The name is constants.LabelSpecDispatchTimeout's, for the same
// reason DispatchFailedLabel is constants': the label is spelled outside this
// package and the decision core holds no I/O.
const TimeoutLabel = constants.LabelSpecDispatchTimeout

// TimeoutAttemptsKey is the metadata key holding a bead's bound-kill count.
const TimeoutAttemptsKey = "spec_dispatch_timeout_attempts"

// TimeoutRetries bounds the slings a bead whose process is killed at its bound
// is given: the kill that reaches it spends the last retry, and the label that
// kill writes stays for a person.
const TimeoutRetries = 3

// TimeoutAttempts reads the bound-kill count a bead's metadata records, and 0
// for metadata that holds none: a bead never killed at its bound, a metadata
// blob another writer set, and one this package cannot parse all read as no
// attempts, which grants the bead its retries rather than holding it out.
func TimeoutAttempts(metadata []byte) int {
	var fields map[string]any
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return 0
	}
	switch v := fields[TimeoutAttemptsKey].(type) {
	case float64:
		return int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return 0
}

// RetryAfterTimeout reads a bead a failed sling left: attempt is the bound
// kills its metadata records, and retry reports whether the bead should be
// slung again. The bead has to wear both labels — the failure label is what
// holds it out, and the timeout label says that failure was a kill at the
// bound, since every other failure clears it — and a retry has to be unspent.
func RetryAfterTimeout(labels []string, metadata []byte) (attempt int, retry bool) {
	if !slices.Contains(labels, DispatchFailedLabel) || !slices.Contains(labels, TimeoutLabel) {
		return 0, false
	}
	attempt = TimeoutAttempts(metadata)
	return attempt, attempt > 0 && attempt < TimeoutRetries
}
