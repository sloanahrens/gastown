package cmd

import (
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// specEstimatePrior is the reader the command hands to estimate.Deps.Prior for
// a bead whose text was estimated before: the embed and Jev minutes of the
// newest log record of the same bead text. No log, and a record missing either
// value, are each no prior: the caller then estimates as usual, never a wrong
// number. An unreadable log is no prior too, and it says so on errw: without
// that line a forced re-estimate would print a fresh, different estimate with
// no sign of why.
func specEstimatePrior(townRoot string, errw io.Writer) func(beadID, hash string) (estimate.PriorMinutes, bool) {
	return func(beadID, hash string) (estimate.PriorMinutes, bool) {
		records, _, err := estlog.Read(townRoot)
		if err != nil {
			fmt.Fprintf(errw, "gt spec estimate: estimate log is unreadable, the re-estimate cannot reuse the logged minutes: %v\n", err)
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
