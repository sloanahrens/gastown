package cmd

import (
	"strings"
	"time"
)

// Bounds on the retry of a bd write that Dolt aborted for contention. Five
// attempts spanning ~1.5s of jittered backoff absorb the burst of 1213s a busy
// town produces without holding a dispatch open long enough to look hung.
const (
	bdContentionAttempts   = 5
	bdContentionBackoffMin = 100 * time.Millisecond
	bdContentionBackoffMax = 2 * time.Second
)

// bdContentionSleep pauses before the next attempt. A var so the retry loop's
// tests drive it without waiting out real backoff — the same seam
// hookBeadWithRetryFn provides for the hook path.
var bdContentionSleep = time.Sleep

// bdSerializationFailure reports whether cause — the text bd reported for a
// failed write — is a Dolt serialization failure, SQLSTATE 40001.
//
// Only an abort qualifies, and only an abort is safe to repeat: Dolt rolls the
// conflicting transaction back before it reports 40001, so the failed attempt
// provably wrote nothing and a repeat cannot duplicate the wisp, proto or hook
// the first attempt would have created. The markers are the three ways Dolt
// spells that one condition: the MySQL error code, the SQLSTATE text, and the
// restart advice it appends.
//
// Everything else stays out, on purpose. A timeout, an unreachable server and a
// reset connection can each mean the write landed and only the answer was lost,
// and repeating one of those strands a second wisp that nothing will ever close
// — the flood closeRemainingSteps exists to prevent (gt-ye21). A refused
// connection or an open circuit breaker is not a competing writer either: the
// town has a different problem, and a retry loop would only delay the report.
func bdSerializationFailure(cause string) bool {
	if cause == "" {
		return false
	}
	for _, marker := range []string{
		"Error 1213",                 // deadlock/serialization failure, MySQL code
		"serialization failure",      // Dolt's SQLSTATE 40001 text
		"try restarting transaction", // Dolt's own advice for the same class
	} {
		if strings.Contains(cause, marker) {
			return true
		}
	}
	return false
}
