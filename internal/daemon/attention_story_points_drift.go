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
// reading is near 0.33 (gt-7gzf3g). An unmeasurable reading is not failing: a
// measurement not taken says nothing about whether the drift stands.
func storyPointsDriftFailing(r metrics.StoryPointsDriftReading) bool {
	if !r.Measurable {
		return false
	}
	return r.Hi < storyPointsDriftBar
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
	// An interval end that is not a number reads as "not failing" in the rule
	// below and would clear a live item: keep the held item and raise nothing,
	// until the unreadable item (gt-jo1lnr) takes this case.
	if math.IsNaN(reading.Lo) || math.IsInf(reading.Lo, 0) || math.IsNaN(reading.Hi) || math.IsInf(reading.Hi, 0) {
		return s.keepHeldDrift(), nil
	}
	if !storyPointsDriftFailing(reading) {
		return nil, nil
	}
	return []attention.Item{{
		Key:      storyPointsDriftKey,
		Kind:     attention.KindStoryPointsDrift,
		Severity: attention.SeverityMedium,
		Summary: fmt.Sprintf(
			"story points estimate drift: the 95%% interval %.2f to %.2f of the rank correlation %.2f over the last %d landings of the %s extract lies wholly under %.2f; weights %s",
			reading.Lo, math.Floor(reading.Hi*100)/100, reading.Rho, read.Drift.N, filepath.Base(read.Dir), storyPointsDriftBar, read.Weights),
	}}, nil
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
