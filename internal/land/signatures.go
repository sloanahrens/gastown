package land

import "strings"

// runMainMarker is the line the runner prints when it starts a uses: step. Every
// gate workflow begins with uses: actions/checkout@v4, so a job that reached a
// step always carries the marker and a job the runner never started carries
// none. Captured from a real run: testdata/normal-job-head.log has the marker,
// testdata/start-failure.log (an unreachable image) does not (gt-fn9e6.30).
const runMainMarker = "⭐ Run Main"

// gateFacts is what an infrastructure signature may read about a red gate: the
// run's terminal status, and the gate job's log when the whole of it is in hand.
// A signature reads the runner's own lifecycle lines and never test output: a
// run that got as far as executing a test has judged the work, whatever it said
// (design 2.1, gt-fn9e6.30).
type gateFacts struct {
	// RunStatus is the status Forgejo reports for the run behind the red
	// context, or "" when no run could be read.
	RunStatus string
	// Log is the gate job's log, set only alongside LogWhole. A tail with its
	// head cut off cannot answer for a marker the head carries.
	Log string
	// LogWhole reports that the log came back complete: the server returned
	// less than the byte cap, so nothing was cut from either end.
	LogWhole bool
}

// infraSignature is one structural sign that a red gate is infrastructure, not
// a verdict on the work.
type infraSignature struct {
	// name identifies the signature in the error the landing worker classifies.
	name string
	// match reports whether the facts carry the signature.
	match func(gateFacts) bool
}

// infraSignatures is the table consulted when a required gate context is red.
// First match wins; no match leaves the red a verdict on the work. Adding a
// signature is a row here plus a test, and never a read of test output
// (gt-fn9e6.30). The other two infrastructure outcomes are not reds and so are
// not rows: a context that never reports inside the wait window, and a Forgejo
// call that fails, already return ErrCISilence from wait and state
// (candidate.go).
var infraSignatures = []infraSignature{
	{
		name: "the run ended without judging the work",
		match: func(f gateFacts) bool {
			return f.RunStatus == candidateRunCancelled || f.RunStatus == candidateRunSkipped
		},
	},
	{
		name: "the runner failed before the first step: the job log carries no step marker (image pull or container start failure)",
		match: func(f gateFacts) bool {
			return f.RunStatus == candidateRunFailure && f.LogWhole && !logNamesStep(f.Log)
		},
	},
}

// matchInfraSignature returns the first signature the facts carry.
func matchInfraSignature(f gateFacts) (infraSignature, bool) {
	for _, sig := range infraSignatures {
		if sig.match(f) {
			return sig, true
		}
	}
	return infraSignature{}, false
}

// logNamesStep reports whether a job log carries a step marker. A log with none
// is a runner that failed before it started a step; the marker is at the head,
// so only a whole log can say so.
func logNamesStep(log string) bool {
	return strings.Contains(log, runMainMarker)
}
