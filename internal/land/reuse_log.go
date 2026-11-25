package land

import (
	"fmt"
	"strings"
	"time"
)

// omReusedSuffix is the stage-line suffix for a landing that reused a stored om
// verdict instead of running om: cost is what om took when it ran, and age is
// how long ago its verdict was stored. It has the shape of the
// ", om skipped (overseer-reviewed)" suffix. Both durations print as
// stageLine prints om's time, Duration.Round(time.Second). A negative age
// prints with its sign rather than being clamped to zero, so a bad input shows.
func omReusedSuffix(cost, age time.Duration) string {
	return fmt.Sprintf(", om reused (%s, %s old)", cost.Round(time.Second), age.Round(time.Second))
}

// applyBar applies the rig's bar to v: an approve scored below bar becomes a
// request for changes, with the summary prefix reviewOnce writes. Any other
// verdict is returned unchanged. reviewOnce keeps its own copy of this rule,
// and TestVerdictReuseApplyBarMatchesReviewOnce pins the two together, so a
// reused verdict is judged the way a fresh one is.
func applyBar(v Verdict, bar float64) Verdict {
	if v.Verdict == VerdictApprove && v.Score < bar {
		v.Verdict = VerdictRequestChanges
		v.Summary = strings.TrimSpace(fmt.Sprintf("score %.2f is below the rig's .om.json threshold %.2f. %s", v.Score, bar, v.Summary))
	}
	return v
}
