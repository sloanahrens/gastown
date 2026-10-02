package reaper

import "fmt"

// The open-wisp alert measures growth, never size (gt-11kyy). The fixed count
// it replaced is gone rather than kept: the open-wisp count is dominated by
// healthy, recent wisps, so no absolute value separates "busy town" from
// "runaway", and a threshold set from one town's steady state false-alarms in
// the next.
const (
	// AlertGrowthFactor is how many times the baseline the open-wisp count must
	// reach before callers warn.
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

// OpenWispAlertState is the alert's memory between cycles: the sample a rising
// count is measured from, and how many cycles have passed since it was
// recorded.
type OpenWispAlertState struct {
	Baseline OpenWispSample `json:"baseline"`
	Held     int            `json:"held"`
}

// NextOpenWispAlertState returns the memory to record after one cycle and
// whether that cycle's count has grown enough over the baseline to warn about.
//
// The baseline is held while the count rises and moves only when an alert
// fires, when the count falls back to or below it, or when the cycle is not
// comparable to the one recorded (another mode, or a different number of
// databases). A baseline that tracked every cycle could only ever see growth
// that doubled the count between two of them; accumulation spread over many
// cycles — the case that matters — would pass unremarked (gt-11kyy).
func NextOpenWispAlertState(previous *OpenWispAlertState, current OpenWispSample) (OpenWispAlertState, bool, string) {
	if previous == nil || !comparable(previous.Baseline, current) {
		return OpenWispAlertState{Baseline: current}, false, ""
	}

	growth := current.OpenWisps - previous.Baseline.OpenWisps
	if current.OpenWisps >= previous.Baseline.OpenWisps*AlertGrowthFactor && growth >= AlertGrowthFloor {
		// This cycle becomes the baseline, so a town that settles at the high
		// count reports the rise once rather than every cycle after it.
		return OpenWispAlertState{Baseline: current}, true, fmt.Sprintf(
			"open wisps grew from %d to %d (+%d) over %d cycle(s) on the same %d database(s)",
			previous.Baseline.OpenWisps, current.OpenWisps, growth, previous.Held+1, current.Databases)
	}

	if current.OpenWisps <= previous.Baseline.OpenWisps {
		// The town reaped back down, so the floor it settled on is what the next
		// rise is measured from.
		return OpenWispAlertState{Baseline: current}, false, ""
	}

	return OpenWispAlertState{Baseline: previous.Baseline, Held: previous.Held + 1}, false, ""
}

// comparable reports whether two samples were taken over the same set in the
// same mode, and so may be subtracted.
func comparable(a, b OpenWispSample) bool {
	return a.DryRun == b.DryRun && a.Databases == b.Databases
}
