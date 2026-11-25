package cmd

import (
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// specEstimatePrior is the reader the command hands to estimate.Deps.Prior for
// a bead whose text was estimated before: the embed and Jev minutes of the
// newest log record of the same bead text. Embed is what makes a record a
// prior; a record without it, no log, and an unreadable log are each no prior,
// and the caller then estimates as usual, never a wrong number. Jev is 0 when
// the record has none, as a row that omits jev does; today's estimate reuses a
// prior only when both values are valid minutes, so that 0 keeps it estimating
// as usual. An unreadable log says so on errw: without that line a forced
// re-estimate would print a fresh, different estimate with no sign of why.
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
		if newest == nil || newest.Minutes.Embed == nil {
			return estimate.PriorMinutes{}, false
		}
		prior := estimate.PriorMinutes{Embed: *newest.Minutes.Embed}
		if newest.Minutes.Jev != nil {
			prior.Jev = *newest.Minutes.Jev
		}
		return prior, true
	}
}
