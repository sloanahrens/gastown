package cmd

import (
	"fmt"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/land"
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

// reworkLandingReader is the read seam reworkLandings uses: a rig's landings
// at or after a time, which *land.LandingsFile provides in production.
type reworkLandingReader interface {
	Since(time.Time) ([]land.LandingRecord, error)
}

// reworkLandings returns the rig's landings of the window the advisory names:
// the records landed at or after now minus reworkAdviceDays, oldest first.
//
// open is the read seam (the caller's land.RigLandingsFile in production) so a
// test supplies a fake with no file and no clock. A read that fails returns the
// error with the rig named and the underlying record's path text kept, never an
// empty list the advisory would count as no landings (gt-wihqmh).
func reworkLandings(open func(townRoot, rig string) (reworkLandingReader, error), townRoot, rig string, now time.Time) ([]land.LandingRecord, error) {
	reader, err := open(townRoot, rig)
	if err != nil {
		return nil, fmt.Errorf("rework landings for rig %s: %w", rig, err)
	}
	recs, err := reader.Since(now.AddDate(0, 0, -reworkAdviceDays))
	if err != nil {
		return nil, fmt.Errorf("rework landings for rig %s: %w", rig, err)
	}
	return recs, nil
}
