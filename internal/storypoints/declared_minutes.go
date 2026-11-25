package storypoints

import "math"

// The declared-lines curve maps a bead's declared added-line count to minutes.
// It is the least-squares fit of ln(actual story points) on ln(declared lines)
// over the 657 landed beads that declared a Change line (estimator v2 design,
// gt-gl12sr; fit sd .62). It reads 6.6 minutes at 30 lines, 12.1 at 100 and
// 17.26 at 200.
const (
	declaredMinutesScale    = 1.17
	declaredMinutesExponent = 0.508
	declaredMinutesMinLines = 10
	declaredMinutesMaxLines = 300
)

// DeclaredMinutes returns the minutes the declared-lines curve predicts for a
// bead that declares lines added lines, 1.17 * lines^0.508. ok is false outside
// the fitted range of 10 to 300 lines, where the fit does not apply; the minutes
// are 0 then.
func DeclaredMinutes(lines int) (minutes float64, ok bool) {
	if lines < declaredMinutesMinLines || lines > declaredMinutesMaxLines {
		return 0, false
	}
	return declaredMinutesScale * math.Pow(float64(lines), declaredMinutesExponent), true
}
