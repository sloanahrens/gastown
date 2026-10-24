// Package deadseats holds the town's one predicate for "has this seat's death
// been confirmed": whether the consecutive dead samples in the seat's record
// reach the count the patrol acts on.
//
// It is a leaf package so every reader can share the answer: the patrol that
// acts on it, the seat roster that counts the seat, and the health line that
// reports it. liveness makes the samples but cannot hold the rule: it imports
// agentlog and tmux, which import internal/config, and config imports
// internal/townhealth, one of the readers.
package deadseats

// DefaultSamples is how many consecutive dead samples confirm a seat's death:
// the compiled default of the patrol's dead_samples, and the count a reader
// with no configuration uses, so one death is confirmed at one count wherever
// it is read (gt-4rxdi).
const DefaultSamples = 2

// Confirmed reports whether deadSamples consecutive dead samples reach
// threshold, the count the patrol acts on; below it the patrol only waits
// ("dead sample 1 of 2"), so a reader that confirmed sooner would call a seat
// dead the patrol still holds (gt-4rxdi). A threshold of zero or less is
// DefaultSamples.
func Confirmed(deadSamples, threshold int) bool {
	if threshold <= 0 {
		threshold = DefaultSamples
	}
	return deadSamples >= threshold
}
