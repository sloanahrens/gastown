package specdispatch

import (
	"encoding/json"
	"strconv"
)

// Two slings that pick the same seat can race for the same polecat name: the
// seat decision reserves a seat slot, not the name inside it, so the loser of
// the assignee hook-write lock is the seat's contention rather than the bead's
// defect. The label a sling failure would write holds the bead out of the queue
// until a person clears it, so the dispatcher defers such a bead and slings it
// again next tick, and labels it only once the same timeout has repeated
// LockTimeoutRetries ticks in a row — a lock that really is stuck then still
// surfaces (gt-a4cdcd).
//
// The count lives in bead metadata, as the bound-kill count does: notes are
// prose for people, and anything that reads them for state is a convention
// every other writer to the bead can break.

// LockTimeoutAttemptsKey is the metadata key holding a bead's consecutive
// assignee-lock-timeout count.
const LockTimeoutAttemptsKey = "spec_dispatch_lock_timeout_attempts"

// LockTimeoutRetries bounds the consecutive assignee-lock timeouts a bead is
// deferred for: the timeout that reaches this count labels the bead, and the
// counter is cleared so a person's label clear gets a fresh set of retries.
const LockTimeoutRetries = 3

// LockTimeoutAttempts reads the consecutive timeout count a bead's metadata
// records. Metadata that holds no count (none at all, or another writer's keys)
// reads as 0: the bead has not timed out since its last success. A count that
// is present but unreadable (metadata that is not JSON, a value that is not a
// number) reads as the retries spent: a bead whose record cannot be trusted is
// held for a person rather than deferred without end.
func LockTimeoutAttempts(metadata []byte) int {
	if len(metadata) == 0 {
		return 0
	}
	var fields map[string]any
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return LockTimeoutRetries
	}
	raw, ok := fields[LockTimeoutAttemptsKey]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return LockTimeoutRetries
}
