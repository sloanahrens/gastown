package cmd

import "testing"

// TestReworkAdvice: the advisory line names the bead's points, the share as a
// whole percent and the measured landings, and it is withheld when the sample
// is too thin or the bead has no positive estimate (gt-quiycq).
func TestReworkAdvice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		points int
		share  float64
		n      int
		want   string
	}{
		{"8 points at 27%", 8, 0.27, 30, "advice: beads estimated at 8 points needed a second attempt 27% of the time (30 landings, last 14 days)"},
		{"the minimum landing count still gives a line", 13, 0.55, 20, "advice: beads estimated at 13 points needed a second attempt 55% of the time (20 landings, last 14 days)"},
		{"the share is rounded to a whole percent", 5, 0.1234, 209, "advice: beads estimated at 5 points needed a second attempt 12% of the time (209 landings, last 14 days)"},
		{"one landing under the minimum", 8, 0.27, 19, ""},
		{"zero points", 0, 0.27, 30, ""},
		{"negative points", -3, 0.27, 30, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := reworkAdvice(tc.points, tc.share, tc.n)
			if tc.want == "" {
				if ok || got != "" {
					t.Errorf("reworkAdvice(%d, %v, %d) = %q, %v; want no line", tc.points, tc.share, tc.n, got, ok)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("reworkAdvice(%d, %v, %d) = %q, %v; want %q, true", tc.points, tc.share, tc.n, got, ok, tc.want)
			}
		})
	}
}
