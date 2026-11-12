package storypoints

import "math"

// Actual returns the measured points for a landing's polecat time in seconds,
// and whether the time is weighted at all. A time is weighted only when it is
// finite, not a lower bound or an upper bound, and FromMinutes accepts it; zero,
// negative, NaN and infinite times are refused. An unweighted time returns 0
// points, never point 1 and never the top point, with weighted false.
//
// A lower-bound time is unweighted on purpose, the same as the sp-actual label,
// which also skips lower bounds.
func Actual(polecatSecs float64, lowerBound, upperBound bool) (points int, weighted bool) {
	if lowerBound || upperBound || math.IsInf(polecatSecs, 0) {
		return 0, false
	}
	points, err := FromMinutes(polecatSecs / 60)
	if err != nil {
		return 0, false
	}
	return points, true
}
