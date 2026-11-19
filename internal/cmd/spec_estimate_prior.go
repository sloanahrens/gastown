package cmd

import (
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// specEstimatePrior is the reader the command hands to estimate.Deps.Prior once
// the next bead wires it: the embed and Jev minutes of the newest log record of
// the same bead text. No log, an unreadable log, and a record missing either value
// are each no prior: the caller then estimates as usual, never a wrong number.
func specEstimatePrior(townRoot string) func(beadID, hash string) (estimate.PriorMinutes, bool) {
	return func(beadID, hash string) (estimate.PriorMinutes, bool) {
		records, _, err := estlog.Read(townRoot)
		if err != nil {
			return estimate.PriorMinutes{}, false
		}
		var newest *estlog.Record
		for _, r := range records {
			if r.BeadID != beadID || r.DescriptionHash != hash {
				continue
			}
			// Read returns append order, so an equal Time keeps the later line.
			if newest == nil || !r.Time.Before(newest.Time) {
				newest = &r
			}
		}
		if newest == nil || newest.Minutes.Embed == nil || newest.Minutes.Jev == nil {
			return estimate.PriorMinutes{}, false
		}
		return estimate.PriorMinutes{Embed: *newest.Minutes.Embed, Jev: *newest.Minutes.Jev}, true
	}
}
