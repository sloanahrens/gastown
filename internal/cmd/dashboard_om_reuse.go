package cmd

import (
	"strings"
	"time"
)

// omReusedMarker opens the suffix a landing writes when it reused a stored om
// verdict instead of running om: ", om reused (<cost>, <age> old)". It is the
// form internal/land writes (omReusedSuffix), and the stage line carries it
// after the stage times.
const omReusedMarker = ", om reused ("

// omStageReused reads the reused-verdict suffix off a stage line's stages text,
// "ci 1m32s, om reused (2m31s, 14m0s old)": cost is what om took when it ran,
// and age is how long ago its verdict was stored. ok is false for every other
// line: a plain "om 20s", the waiver "om skipped (overseer-reviewed)", a
// skipped gate, and a reused suffix that does not parse or carries a negative
// duration. A caller that must tell a malformed suffix from no suffix checks
// for omReusedMarker itself; this function reports both as not ok.
func omStageReused(stages string) (cost, age time.Duration, ok bool) {
	_, suffix, found := strings.Cut(stages, omReusedMarker)
	if !found {
		return 0, 0, false
	}
	costTok, rest, found := strings.Cut(suffix, ", ")
	if !found {
		return 0, 0, false
	}
	ageTok, found := strings.CutSuffix(rest, " old)")
	if !found {
		return 0, 0, false
	}
	cost, err := time.ParseDuration(costTok)
	if err != nil || cost < 0 {
		return 0, 0, false
	}
	age, err = time.ParseDuration(ageTok)
	if err != nil || age < 0 {
		return 0, 0, false
	}
	return cost, age, true
}
