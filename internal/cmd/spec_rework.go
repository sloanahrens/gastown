package cmd

import (
	"fmt"
	"math"
)

// reworkAdviceMinLandings is the landing count under which the measured rework
// rate is withheld: too few landings make the share wide enough to mislead.
const reworkAdviceMinLandings = 20

// reworkAdviceDays is the window the landings were measured over, named in the
// line so the reader knows the rate is recent.
const reworkAdviceDays = 14

// reworkAdvice words the measured rework rate for a bead estimated at points as
// a whole percent of n landings over the last reworkAdviceDays days, returning
// false and empty text when n is under reworkAdviceMinLandings or points is not
// positive. It reads nothing and prints nothing.
func reworkAdvice(points int, share float64, n int) (string, bool) {
	if n < reworkAdviceMinLandings || points <= 0 {
		return "", false
	}
	return fmt.Sprintf(
		"advice: beads estimated at %d points needed a second attempt %d%% of the time (%d landings, last %d days)",
		points, int(math.Round(share*100)), n, reworkAdviceDays,
	), true
}
