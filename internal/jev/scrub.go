package jev

import (
	"io"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

// WithScrub makes the client scrub the state of every request before it is
// sent: Ask marshals the state, passes the JSON through s, and posts the
// result. The questions are the caller's own text and go out unchanged.
// Without this option the body is what Ask has always sent.
func WithScrub(s *scrub.Scrubber) Option { return func(c *Client) { c.scrub = s } }

// BatteryOptions returns the client options every Jev battery builds its
// client with: the town's usage ledger, the caller's name and warnings, and
// the state scrubber when scrubState is set.
func BatteryOptions(townRoot, caller string, warnings io.Writer, scrubState bool) []Option {
	opts := []Option{
		WithLedger(UsageLedgerPath(townRoot)),
		WithCaller(caller),
		WithWarnings(warnings),
	}
	if scrubState {
		opts = append(opts, WithScrub(scrub.Default()))
	}
	return opts
}
