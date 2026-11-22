package land

import "testing"

// TestReworkRate: only landings at the given estimate with a measured polecat
// time count, and the share is the ones that took more than one attempt
// (gt-mqitno).
func TestReworkRate(t *testing.T) {
	t.Parallel()
	at := func(points int) *int { return &points }
	attempts := func(n int) []PolecatAttempt {
		out := make([]PolecatAttempt, n)
		for i := range out {
			out[i] = PolecatAttempt{Seat: "rictus", Secs: 60}
		}
		return out
	}
	cases := []struct {
		name  string
		recs  []LandingRecord
		point int
		share float64
		n     int
	}{
		{
			name: "three at the estimate, one reworked",
			recs: []LandingRecord{
				{SPEstimate: at(8), PolecatAttempts: attempts(1)},
				{SPEstimate: at(8), PolecatAttempts: attempts(2)},
				{SPEstimate: at(8), PolecatAttempts: attempts(1)},
			},
			point: 8,
			share: 1.0 / 3.0,
			n:     3,
		},
		{
			name: "another estimate is another band",
			recs: []LandingRecord{
				{SPEstimate: at(8), PolecatAttempts: attempts(1)},
				{SPEstimate: at(13), PolecatAttempts: attempts(2)},
			},
			point: 8,
			share: 0,
			n:     1,
		},
		{
			name:  "no estimate is not in any band",
			recs:  []LandingRecord{{PolecatAttempts: attempts(2)}},
			point: 8,
			share: 0,
			n:     0,
		},
		{
			name:  "no attempts is not a measured landing",
			recs:  []LandingRecord{{SPEstimate: at(8)}},
			point: 8,
			share: 0,
			n:     0,
		},
		{
			name:  "a lower-bound time is not a measured landing",
			recs:  []LandingRecord{{SPEstimate: at(8), PolecatAttempts: attempts(2), PolecatLowerBound: true}},
			point: 8,
			share: 0,
			n:     0,
		},
		{
			name:  "an upper-bound time is not a measured landing",
			recs:  []LandingRecord{{SPEstimate: at(8), PolecatAttempts: attempts(2), PolecatUpperBound: true}},
			point: 8,
			share: 0,
			n:     0,
		},
		{
			name:  "no records is 0 and 0, not NaN",
			recs:  nil,
			point: 8,
			share: 0,
			n:     0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			share, n := ReworkRate(tc.recs, tc.point)
			if share != tc.share || n != tc.n {
				t.Errorf("ReworkRate = %v, %d; want %v, %d", share, n, tc.share, tc.n)
			}
		})
	}
}
