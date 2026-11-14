package omclass

import (
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// Settings is the operator's configuration for the pass, read from
// patrols.jev_omclass.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it.
	Scrub bool

	// ScrubCheck runs the shadow scrub check on a sample of requests.
	ScrubCheck bool
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the town's om review log, the log and
// cursor, and the cap read from the same ledger. warnings receives the ledger's
// own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_omclass: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     town.AllowsNow,
	})...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	return &Pass{
		Asker:      client,
		Reviews:    land.TownReviewsFile(townRoot),
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     town.AllowsNow,
	}, nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker   = (*jev.Client)(nil)
	_ Reviews = (*land.ReviewsFile)(nil)
)
