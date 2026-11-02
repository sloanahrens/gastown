package specdispatch

import "regexp"

// SlowTextHint reports whether a bead's text talks about slow or timed-out
// tests, the trigger for showing the town's prior records on slow tests (dv-fg6o).
func SlowTextHint(text string) bool {
	return slowTextRe.MatchString(text)
}

// slowTextRe matches the words and limits a bead uses to talk about slow or
// timed-out tests. Every branch is bounded to a whole word, so "slowly varying"
// is not "slow", and "timed out" and "timed-out" both count. A single-letter
// unit counts only attached to its number, so "600s" and "10m" are limits while
// "3 m tall" is prose; a spelled unit may be spaced ("5 min"). A fenced code
// block is matched like any other text, because a pasted timeout message is the
// case the hint is for.
var slowTextRe = regexp.MustCompile(`(?i)\b(?:timeouts?|tim(?:e[ds]?|ing)[\s-]+outs?|slow|\d+(?:\.\d+)?(?:[sm]|\s*(?:secs?|seconds?|mins?|minutes?)))\b`)
