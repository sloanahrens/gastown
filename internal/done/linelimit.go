package done

import (
	"regexp"
	"strconv"
)

// declaredLineLimitRe matches the wordings in which a bead states its
// added-line limit (gt-obyuam): the worded forms ("about 90 added lines", "at
// most 50 added lines") and the hyphenated one ("a 70-added-line constraint").
// No other wording is a declaration, so a minutes or file count states no
// limit.
var declaredLineLimitRe = regexp.MustCompile(`(?i)\b(?:(?:about|under|at most|roughly|up to)\s+(\d+)\s+added lines?|(\d+)-added-line)\b`)

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
