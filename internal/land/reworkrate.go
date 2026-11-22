package land

// ReworkRate reports the share of landings estimated at points that took more
// than one polecat attempt, and how many landings qualified (gt-mqitno).
//
// A record qualifies when SPEstimate is points, the record carries at least one
// attempt, and its polecat time is not flagged as a lower or upper bound. No
// qualifying record gives a share of 0 and n of 0, never NaN.
func ReworkRate(recs []LandingRecord, points int) (share float64, n int) {
	reworked := 0
	for _, rec := range recs {
		if rec.SPEstimate == nil || *rec.SPEstimate != points {
			continue
		}
		if len(rec.PolecatAttempts) == 0 || rec.PolecatLowerBound || rec.PolecatUpperBound {
			continue
		}
		n++
		if len(rec.PolecatAttempts) > 1 {
			reworked++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(reworked) / float64(n), n
}
