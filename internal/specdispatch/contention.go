package specdispatch

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// A sling that loses its bond to database contention is the host's load, not a
// defect in its bead. Its retries wait on a growing schedule recorded in the
// bead's metadata, and ContentionWindow bounds that schedule from the first loss
// (gt-iv5n4l). Labeling and pausing a spent bead are the dispatcher's, and later
// changes wire them.
//
// Every function here takes the clock as an argument and does no I/O: the caller
// reads the metadata, passes it in, and writes back what ContentionNext returns.

// ContentionLabel marks a bead whose contention retries are spent. The name is
// constants.LabelSpecDispatchContention's, for the same reason TimeoutLabel is
// constants': the label is spelled outside this package.
const ContentionLabel = constants.LabelSpecDispatchContention

// The metadata keys the schedule is recorded under. Only the contention schedule
// reads or sets them.
const (
	ContentionFirstKey    = "spec_dispatch_contention_first"
	ContentionAttemptsKey = "spec_dispatch_contention_attempts"
	ContentionRetryAtKey  = "spec_dispatch_contention_retry_at"
)

// ContentionDelays is the wait before the retry that answers each loss, in
// order. A loss past the end of the schedule waits the last delay again.
var ContentionDelays = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	45 * time.Second,
	2 * time.Minute,
}

// ContentionWindow is how long the schedule runs from the first loss before the
// bead's retries are spent.
const ContentionWindow = 10 * time.Minute

// ContentionPause is the default wait before a labeled bead is retried.
const ContentionPause = 10 * time.Minute

// ContentionState is a bead's contention schedule as its metadata records it.
// The zero value is a bead that has lost no bond yet. Spent is set when the
// metadata holds a contention key this package cannot read: the window reads as
// spent, so the bead is held for a person and never granted retries by mistake.
type ContentionState struct {
	First    time.Time
	Attempts int
	RetryAt  time.Time
	Spent    bool
}

// ContentionRecord reads a bead's contention schedule from its metadata. Metadata
// with none of the contention keys has no record (ok false). Metadata that is not
// JSON, or that holds any contention key unreadably (a key missing, a time that is
// not RFC 3339, a count below one), has a record whose window is spent: a schedule
// that cannot be trusted must not read as a fresh bead.
func ContentionRecord(metadata []byte) (rec ContentionState, ok bool) {
	if len(metadata) == 0 {
		return ContentionState{}, false
	}
	var fields map[string]any
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return ContentionState{Spent: true}, true
	}
	_, hasFirst := fields[ContentionFirstKey]
	_, hasAttempts := fields[ContentionAttemptsKey]
	_, hasRetryAt := fields[ContentionRetryAtKey]
	if !hasFirst && !hasAttempts && !hasRetryAt {
		return ContentionState{}, false
	}
	first, okFirst := contentionTime(fields[ContentionFirstKey])
	attempts, okAttempts := contentionCount(fields[ContentionAttemptsKey])
	retryAt, okRetryAt := contentionTime(fields[ContentionRetryAtKey])
	if !okFirst || !okAttempts || !okRetryAt {
		return ContentionState{Spent: true}, true
	}
	return ContentionState{First: first, Attempts: attempts, RetryAt: retryAt}, true
}

// ContentionNext returns the schedule after one more lost bond at now, with
// RetryAt set to now plus the delay for that loss. exhausted reports whether
// ContentionWindow has passed since the first loss. A spent record stays spent
// and is exhausted. A record with no attempts yet is a first loss dated now.
func ContentionNext(rec ContentionState, now time.Time) (next ContentionState, exhausted bool) {
	if rec.Spent {
		return rec, true
	}
	first := rec.First
	if rec.Attempts == 0 {
		first = now
	}
	next = ContentionState{
		First:    first,
		Attempts: rec.Attempts + 1,
		RetryAt:  now.Add(contentionDelay(rec.Attempts + 1)),
	}
	return next, now.Sub(first) >= ContentionWindow
}

// ContentionWaiting reports whether a bead's retry is still ahead of now. A record
// with no retry set is not waiting.
func ContentionWaiting(rec ContentionState, now time.Time) bool {
	return now.Before(rec.RetryAt)
}

// ContentionPauseOver reports whether the wait a labeled bead holds has passed,
// at RetryAt and after. A spent record is never over: it carries no retry time
// to pass, so it reports false. Exhaustion is not this function's answer: a
// caller retries a labeled bead only after ContentionNext reports it not
// exhausted.
func ContentionPauseOver(rec ContentionState, now time.Time) bool {
	if rec.Spent {
		return false
	}
	return !ContentionWaiting(rec, now)
}

// contentionDelay is the wait before the retry that answers the attempt-th loss.
func contentionDelay(attempt int) time.Duration {
	i := min(max(attempt, 1), len(ContentionDelays)) - 1
	return ContentionDelays[i]
}

// contentionTime reads an RFC 3339 metadata value, in UTC. Anything else,
// including a missing key, is unreadable.
func contentionTime(raw any) (time.Time, bool) {
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// contentionCount reads a whole attempt count of at least one, written either as
// a JSON number or as a string. Anything else is unreadable.
func contentionCount(raw any) (int, bool) {
	var n int
	switch v := raw.(type) {
	case float64:
		n = int(v)
		if float64(n) != v {
			return 0, false
		}
	case string:
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	return n, n >= 1
}
