package done

import (
	"regexp"
	"strconv"
)

// declaredLineLimitRe matches the wordings in which a bead states its
// added-line limit (gt-obyuam): a lead-in ("about 90 added lines", "at most 50
// added lines"), the budget sentence ("the budget is 300 added lines", "stop
// at 100 added lines", "a limit of 50 added lines", "the cap: 40 added lines",
// gt-uq5b9u), and the hyphenated form ("a 70-added-line constraint"). No other
// wording is a declaration, so a bare "300 added lines", a minutes count or a
// file count states no limit.
var declaredLineLimitRe = regexp.MustCompile(`(?i)\b(?:(?:about|under|at most|roughly|up to)\s+(\d+)\s+added lines?|(\d+)-added-line|(?:budget|limit|cap|stop)(?:\s+(?:is|of|at))?\s*(?::\s*|\s+)(\d+)\s+added lines?)\b`)

// DeclaredLineLimits returns the added-line limits a bead's text declares, in
// order of appearance and once each, or nil when it declares none.
func DeclaredLineLimits(text string) []int {
	var limits []int
	seen := make(map[int]bool)
	for _, m := range declaredLineLimitRe.FindAllStringSubmatch(text, -1) {
		digits := m[1]
		if digits == "" {
			digits = m[2]
		}
		if digits == "" {
			digits = m[3]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			// A count too large for an int is not a line limit a bead means,
			// and []int has no room to carry it, so the match states none.
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		limits = append(limits, n)
	}
	return limits
}
