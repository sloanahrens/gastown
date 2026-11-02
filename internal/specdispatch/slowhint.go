package specdispatch

import "regexp"

// SlowTextHint reports whether a bead's text talks about slow or timed-out
// tests, the trigger for showing the town's prior records on slow tests (dv-fg6o).
func SlowTextHint(text string) bool {
	return slowTextRe.MatchString(text)
}

// slowTextRe matches the words and limits a bead uses to talk about slow or
// timed-out tests. Every branch is bounded to a whole word, so "slowly varying"
// is not "slow". A fenced code block is matched like any other text, because a
// pasted timeout message is the case the hint is for.
var slowTextRe = regexp.MustCompile(`(?i)\b(?:timeouts?|tim(?:e[ds]?|ing)\s+out|slow|\d+(?:\.\d+)?\s*(?:s|secs?|seconds?|m|mins?|minutes?))\b`)
