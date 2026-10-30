package jev

import (
	"io"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

// WithScrub makes the client scrub the state of every request before it is
// sent: Ask marshals the state, passes the JSON through s, and posts the
// result. The questions are the caller's own text and go out unchanged.
// Without this option the body is what Ask has always sent. A nil s is a
// configuration error: NewClient refuses it rather than send unscrubbed state.
func WithScrub(s *scrub.Scrubber) Option {
	return func(c *Client) { c.scrub, c.scrubSet = s, true }
}

// BatteryScrub is a battery's scrubbing settings, read from its patrol config.
type BatteryScrub struct {
	// Scrub scrubs the state of every request before it is sent.
	Scrub bool
	// ScrubCheck runs the shadow scrub check on the sampled share of requests
	// (WithShadowScrub, scrub_check). It needs Allows.
	ScrubCheck bool
	// Allows is the battery's daily cap check, which the shadow check honors.
	Allows func() (bool, error)
}

// BatteryOptions returns the client options every Jev battery builds its
// client with: the town's usage ledger, the caller's name and warnings, and
// the state scrubber and shadow scrub check the settings turn on.
func BatteryOptions(townRoot, caller string, warnings io.Writer, s BatteryScrub) []Option {
	opts := []Option{
		WithLedger(UsageLedgerPath(townRoot)),
		WithCaller(caller),
		WithWarnings(warnings),
	}
	if s.Scrub {
		opts = append(opts, WithScrub(scrub.Default()))
	}
	if s.ScrubCheck {
		opts = append(opts, WithShadowScrub(SamplePercent, s.Allows))
	}
	return opts
}
