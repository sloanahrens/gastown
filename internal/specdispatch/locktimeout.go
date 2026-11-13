package specdispatch

import (
	"encoding/json"
	"strconv"
	"strings"
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
//
// The bound fails safe, because a count that cannot be read must not restart
// the run. A count whose write fails is no bound at all, so the caller labels
// the bead at once instead of deferring it on a count nothing remembers. A
// count that is gone while the bead's comments still record a timeout run in
// progress reads as the retries spent, never as zero (LockTimeoutRecorded): a
// bead whose record was lost, and a lock that really is stuck, both surface.

// LockTimeoutAttemptsKey is the metadata key holding a bead's consecutive
// assignee-lock-timeout count.
const LockTimeoutAttemptsKey = "spec_dispatch_lock_timeout_attempts"

// LockTimeoutPhrase is the phrase every dispatcher line about an
// assignee-lock timeout carries: the bead's comments are where the run is
// recorded when its count is gone.
const LockTimeoutPhrase = "hook write lock stayed held"

// LockTimeoutDeferralPhrase is the phrase a comment recording a timeout the
// dispatcher deferred carries. The failed line that labels the bead does not
// carry it: that line ends the run, and a person who clears the label gets a
// fresh one.
const LockTimeoutDeferralPhrase = "sling deferred"

// LockTimeoutRetries bounds the consecutive assignee-lock timeouts a bead is
// deferred for: the timeout that reaches this count labels the bead, and the
// counter is cleared so a person's label clear gets a fresh set of retries.
const LockTimeoutRetries = 3

// LockTimeoutAttempts reads the consecutive timeout count a bead's metadata
// records. Metadata that holds no count (none at all, or another writer's keys)
// reads as 0, which is the count only because the caller asks the bead's
// comments next (LockTimeoutRecorded): a run the record lost is still in them.
// A count that is present but unreadable (metadata that is not JSON, a value
// that is not a number) reads as the retries spent: a bead whose record cannot
// be trusted is held for a person rather than deferred without end.
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
		// A count outside 0..LockTimeoutRetries cannot be one the dispatcher wrote
		// (it writes 1 and 2 and clears the key at the bound): a negative one would
		// defer the bead on every tick, since it never reaches the bound, and a huge
		// one overflows the int. Both read as the retries spent.
		if v >= 0 && v <= LockTimeoutRetries {
			return int(v)
		}
	case string:
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= LockTimeoutRetries {
			return n
		}
	}
	return LockTimeoutRetries
}

// LockTimeoutRecorded reports whether comments, newest last as bd returns them,
// record an assignee-lock-timeout run still in progress. The newest comment
// carrying LockTimeoutPhrase decides: one that also carries
// LockTimeoutDeferralPhrase records a run that has not reached the bound, and
// the failed line that ends the run (it labels the bead) does not. No such
// comment is no run.
//
// A deferral from an earlier dispatch of the same bead reads as a run in
// progress too, since nothing tells the two runs apart once the count is gone.
// That labels a bead which timed out once long ago and again now, which is the
// safe direction: the label is a person's to clear, and clearing it starts a
// fresh run.
func LockTimeoutRecorded(comments []string) bool {
	for i := len(comments) - 1; i >= 0; i-- {
		if !strings.Contains(comments[i], LockTimeoutPhrase) {
			continue
		}
		return strings.Contains(comments[i], LockTimeoutDeferralPhrase)
	}
	return false
}
