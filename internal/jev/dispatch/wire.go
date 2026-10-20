package dispatch

import (
	"fmt"
	"io"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this job alone.
const DefaultMaxUSDPerDay = 1.0

// Settings is the operator's configuration for the pass, read from
// patrols.jev_dispatch.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the rig's beads, and the spend read from
// the same ledger. warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	price := s.USDPerBillionTokens
	if !(price > 0 && !math.IsInf(price, 0)) {
		return nil, fmt.Errorf("jev_dispatch.usd_per_billion_tokens is required and must be above zero, not %v", price)
	}
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	if !(capUSD > 0 && !math.IsInf(capUSD, 0)) {
		return nil, fmt.Errorf("jev_dispatch.max_usd_per_day must be above zero, not %v", capUSD)
	}

	client, err := jev.NewClient(
		jev.WithLedger(jev.UsageLedgerPath(townRoot)),
		jev.WithCaller(Caller),
		jev.WithWarnings(warnings),
	)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	beadsDir := doltserver.FindRigBeadsDir(townRoot, RigName)
	if beadsDir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", RigName)
	}

	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}

	return &Pass{
		Asker:      client,
		Lister:     rigBeads{b: beads.New(beadsDir)},
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		SpentToday: func() (float64, error) { return town.Spent(time.Now) },
		CapUSD:     capUSD,
	}, nil
}

// rigBeads reads a rig's beads through the beads client. It never writes.
type rigBeads struct {
	b *beads.Beads
}

// Placed lists the beads a worker holds: in_progress, and hooked, which is the
// status a sling gives a bead it places.
func (r rigBeads) Placed() ([]*beads.Issue, error) {
	return r.b.List(beads.ListOptions{Status: "in_progress,hooked"})
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker  = (*jev.Client)(nil)
	_ Lister = rigBeads{}
)
