package daemon

import (
	"context"
	"fmt"
	"math"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/attention"
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

// collectStoryPointsDrift raises the drift item when the newest extract's
// landings rank below storyPointsDriftBar, and the unreadable item when no
// measure could be taken. A reading at or above the bar raises nothing, and
// neither does one nobody could measure: a measurement not taken says nothing
// about whether an open drift stands, so a held item stays until a number clears
// it.
func (s *attentionSources) collectStoryPointsDrift(context.Context) ([]attention.Item, error) {
	// The production sources always carry the read (attentionSources); a
	// hand-built sources value without it raises nothing, as the other
	// collectors whose tests build one do.
	if s.storyPointsDrift == nil {
		return nil, nil
	}
	read, err := s.storyPointsDrift(s.now)
	if err != nil {
		return []attention.Item{storyPointsDriftUnreadableItem(err.Error())}, nil
	}
	reading := read.Drift.Current
	// StoryPointsDriftOver reports a reading nobody could measure as NaN and
	// Measurable false, so a measurable one that is not a number is a fault in
	// the read and not a value to compare with the bar.
	if reading.Measurable && (math.IsNaN(reading.Rho) || math.IsInf(reading.Rho, 0)) {
		return []attention.Item{
			storyPointsDriftUnreadableItem(fmt.Sprintf("the measurable reading %v is not a number", reading.Rho)),
		}, nil
	}
	if !reading.Measurable {
		if held, ok := attention.Find(s.prev, storyPointsDriftKey); ok {
			return []attention.Item{held}, nil
		}
		return nil, nil
	}
	if reading.Rho >= storyPointsDriftBar {
		return nil, nil
	}
	return []attention.Item{{
		Key:      storyPointsDriftKey,
		Kind:     attention.KindStoryPointsDrift,
		Severity: attention.SeverityMedium,
		Summary: fmt.Sprintf(
			"story points estimate drift: rank correlation %.2f over the last %d landings of the %s extract is below %.2f; weights %s",
			reading.Rho, read.Drift.N, filepath.Base(read.Dir), storyPointsDriftBar, read.Weights),
	}}, nil
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
