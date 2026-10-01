package reaper

import "fmt"

const (
	// AlertGrowthFactor is how many times its previous value the open-wisp count
	// must reach in one cycle before callers warn. The alert is about
	// accumulation, never about size (gt-11kyy): the open-wisp count is
	// dominated by healthy, recent wisps, so its absolute value cannot separate
	// "busy town" from "runaway", and a threshold set from one town's steady
	// state false-alarms or falls silent in the next.
	AlertGrowthFactor = 2

	// AlertGrowthFloor is the smallest absolute increase that can alert, so a
	// small count doubling does not. It bounds how little the count may move,
	// never how large it may be: a noise floor, not a steady-state estimate.
	AlertGrowthFloor = 200
)

// OpenWispSample is one reaper cycle's open-wisp reading: the count at the end
// of the cycle, how many databases that count covers, and the mode the cycle
// ran in. The last two are part of the reading, not metadata — a dry run
// measures before the sweep where a live run measures after it, so samples
// from the two modes sit on different scales, and a count means nothing
// without the set it was taken over.
type OpenWispSample struct {
	OpenWisps int  `json:"open_wisps"`
	Databases int  `json:"databases"`
	DryRun    bool `json:"dry_run,omitempty"`
}

// OpenWispAlert reports whether this cycle's open-wisp count has grown enough
// over the previous sample to warn about, and describes the growth for the
// warning line. A missing baseline, a sample that is not comparable to the
// previous one (another mode, or a different number of databases), and growth
// under either threshold all report no alert.
//
// Callers record every cycle's sample, including the ones that do not alert:
// the baseline is always the previous cycle's reading, so a reaper that only
// writes on an alert has no baseline on the cycles worth comparing (gt-11kyy).
func OpenWispAlert(current OpenWispSample, previous *OpenWispSample) (bool, string) {
	if previous == nil {
		return false, ""
	}
	if previous.DryRun != current.DryRun || previous.Databases != current.Databases {
		return false, ""
	}
	growth := current.OpenWisps - previous.OpenWisps
	if current.OpenWisps < previous.OpenWisps*AlertGrowthFactor || growth < AlertGrowthFloor {
		return false, ""
	}
	return true, fmt.Sprintf("open wisps grew from %d to %d (+%d) over the same %d database(s)",
		previous.OpenWisps, current.OpenWisps, growth, current.Databases)
}
