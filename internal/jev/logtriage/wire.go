package logtriage

import (
	"fmt"
	"io"
	"math"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this battery
// alone.
const DefaultMaxUSDPerDay = 1.0

// Settings is the operator's configuration for the pass, read from
// patrols.jev_logtriage.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool

	// ScrubCheck runs the shadow scrub check on a sample of requests. The
	// scrub_check key sets it (gt-jb20i).
	ScrubCheck bool
}

// DaemonLogPath is the town daemon's log, the only file a pass reads.
func DaemonLogPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "daemon.log")
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the daemon log under the town and the
// cap read from the same ledger. warnings receives the ledger's own warnings
// and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	price := s.USDPerBillionTokens
	if !(price > 0 && !math.IsInf(price, 0)) {
		return nil, fmt.Errorf("jev_logtriage.usd_per_billion_tokens is required and must be above zero, not %v", price)
	}
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_logtriage: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	})...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}
	return wirePass(townRoot, town, client), nil
}

// wirePass is the pass a town runs with the given Jev client and cap: the
// daemon log, the triage log and cursor under the town, and the cap's spend.
func wirePass(townRoot string, town battery.Cap, asker Asker) *Pass {
	return &Pass{
		Asker:      asker,
		DaemonLog:  DaemonLogPath(townRoot),
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	}
}

// Ensure the production client satisfies the pass's seam.
var _ Asker = (*jev.Client)(nil)
