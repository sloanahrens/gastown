package daemon

import (
	"context"
	"fmt"
	"math"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/metrics"
)

// storyPointsDriftBar is the rank correlation the drift measure must hold: below
// it the a priori estimate no longer ranks landings the way the measured
// polecat time does, so the estimator has drifted from the work (gt-my0o23).
const storyPointsDriftBar = 0.5

// storyPointsLongAUCBar is the bar the upper bound of the AUC for beads over 20
// minutes must lie under for the drift decision to fail. The rank correlation
// alone reads low on the newest 100 landings (0.34, interval 0.16 to 0.50)
// because beads are cut to land under the trigger and the estimates cover a
// narrow band; there the AUC for beads over 20 minutes read 0.71 (interval 0.60
// to 0.82), so the estimates still tell the long beads apart (gt-61owu9).
const storyPointsLongAUCBar = 0.65

// storyPointsDriftKey is the one item the drift measure raises. A second
// reading under the bar is the same condition, so it keeps this key and with it
// the item's first_seen and ack.
const storyPointsDriftKey = "storypoints-drift"

// storyPointsDriftUnreadableKey is the item raised when the measure cannot be
// read at all: a stopped extract, an unreadable table or a weights file the
// estimator would refuse. Without it the queue of a town whose extract stopped
// would look exactly like a town without drift (gt-my0o23).
const storyPointsDriftUnreadableKey = "storypoints-drift:unreadable"

// storyPointsDriftFailing is the drift decision: the estimate is failing on the
// rank correlation only when the whole 95% interval of Rho lies under
// storyPointsDriftBar. The point reading alone is mostly noise, since it fires
// on the windows whose interval straddles the bar; the cost is that a true rank
// correlation of 0.40 reads as failing only on the windows where the point
// reading is near 0.33 (gt-7gzf3g). The rank rule also needs the AUC for beads
// over 20 minutes to be low, when that AUC was measured: its 95% interval must
// end under storyPointsLongAUCBar. A reading with no such AUC (no bead over 20
// minutes in the window) is decided by the rank rule alone, so the missing figure
// never silences it. An unmeasurable reading is not failing: a measurement not
// taken says nothing about whether the drift stands.
func storyPointsDriftFailing(r metrics.StoryPointsDriftReading) bool {
	if !r.Measurable {
		return false
	}
	return r.Hi < storyPointsDriftBar && (!r.LongMeasurable || r.LongHi < storyPointsLongAUCBar)
}

// collectStoryPointsDrift raises the drift item when the whole 95% interval of
// the newest extract's rank correlation lies under storyPointsDriftBar, and the
// unreadable item when no measure could be taken. A reading whose interval
// straddles or ends at the bar raises nothing and drops a drift item already
// held. A reading nobody could measure raises nothing either and keeps a drift
// item already held, since a measurement not taken says nothing about whether
// the drift stands. A failed read keeps it too, beside the unreadable item.
func (s *attentionSources) collectStoryPointsDrift(context.Context) ([]attention.Item, error) {
	// The production sources always carry the read (attentionSources, pinned by
	// TestStoryPointsDriftAttentionIsWiredAndRegistered). A hand-built sources
	// value without it raises nothing: the shared test fixture has no drift
	// read, and an item raised for it would change every tick test built on
	// that fixture.
	if s.storyPointsDrift == nil {
		return s.keepHeldDrift(), nil
	}
	read, err := s.storyPointsDrift(s.now)
	if err != nil {
		return s.keepHeldDrift(storyPointsDriftUnreadableItem(err.Error())), nil
	}
	reading := read.Drift.Current
	// StoryPointsDriftOver reports a reading nobody could measure as NaN and
	// Measurable false, so a measurable one that is not a number is a fault in
	// the read and not a value to compare with the bar.
	if reading.Measurable && (math.IsNaN(reading.Rho) || math.IsInf(reading.Rho, 0)) {
		return s.keepHeldDrift(
			storyPointsDriftUnreadableItem(fmt.Sprintf("the measurable reading %v is not a number", reading.Rho)),
		), nil
	}
	if !reading.Measurable {
		return s.keepHeldDrift(), nil
	}
	// An interval that is not an interval would read as "not failing" in the
	// rule below and clear a live item: it is the unreadable item instead, and a
	// drift item already held stays.
	if fault := driftIntervalFault(reading); fault != "" {
		return s.keepHeldDrift(storyPointsDriftUnreadableItem("the measurable reading's interval: " + fault)), nil
	}
	if !storyPointsDriftFailing(reading) {
		return nil, nil
	}
	// The AUC for beads over 20 minutes is named beside the rank interval when it
	// was measured, so the reader sees both signals that raised the item.
	auc := ""
	if reading.LongMeasurable {
		auc = fmt.Sprintf("; AUC for over 20 minutes %.2f (95%% interval %.2f to %.2f)", reading.LongAUC, reading.LongLo, reading.LongHi)
	}
	return []attention.Item{{
		Key:      storyPointsDriftKey,
		Kind:     attention.KindStoryPointsDrift,
		Severity: attention.SeverityMedium,
		Summary: fmt.Sprintf(
			"story points estimate drift: the 95%% interval %.2f to %.2f of the rank correlation %.2f over the last %d landings of the %s extract lies wholly under %.2f%s; weights %s",
			reading.Lo, math.Floor(reading.Hi*100)/100, reading.Rho, read.Drift.N, filepath.Base(read.Dir), storyPointsDriftBar, auc, read.Weights),
	}}, nil
}

// driftIntervalFault names what is wrong with a measurable reading's rank
// interval, empty when it is fine: an end that is not a number, or a Lo above
// Hi. The text names the field and never the value.
func driftIntervalFault(r metrics.StoryPointsDriftReading) string {
	switch {
	case math.IsNaN(r.Lo) || math.IsInf(r.Lo, 0):
		return "Lo is not a number"
	case math.IsNaN(r.Hi) || math.IsInf(r.Hi, 0):
		return "Hi is not a number"
	case r.Lo > r.Hi:
		return "Lo is above Hi"
	}
	return ""
}

// keepHeldDrift is items plus the drift item the previous tick raised, when one
// is held. The unreadable item has its own key, so both can be queued at once.
func (s *attentionSources) keepHeldDrift(items ...attention.Item) []attention.Item {
	if held, ok := attention.Find(s.prev, storyPointsDriftKey); ok {
		items = append(items, held)
	}
	return items
}

// storyPointsDriftUnreadableItem is the item a measure that could not be taken
// raises. reason names the file or the reading at fault, so the reader has
// something to look at.
func storyPointsDriftUnreadableItem(reason string) attention.Item {
	return attention.Item{
		Key:      storyPointsDriftUnreadableKey,
		Kind:     attention.KindStoryPointsDrift,
		Severity: attention.SeverityMedium,
		Summary:  "story points drift cannot be read: " + reason,
	}
}
