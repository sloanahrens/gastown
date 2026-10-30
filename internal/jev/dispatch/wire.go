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

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool
}

// beadLister is the part of *beads.Beads the pass's lister uses. It is an
// interface so a test can see the query the production lister sends without a
// Dolt server.
type beadLister interface {
	List(beads.ListOptions) ([]*beads.Issue, error)
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the rig's beads, and the spend read from
// the same ledger. warnings receives the ledger's own warnings and may be nil.
func Open(townRoot string, s Settings, warnings io.Writer) (*Pass, error) {
	return open(townRoot, s, warnings, jev.NewClient, func(dir string) beadLister { return beads.New(dir) })
}

// open is Open with the Jev client and the bead store's constructors injected,
// so a test can check the wiring without a key file in the environment or a
// Dolt server.
func open(townRoot string, s Settings, warnings io.Writer,
	newClient func(...jev.Option) (*jev.Client, error), listerOf func(string) beadLister) (*Pass, error) {
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

	client, err := newClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
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
		Lister:     rigBeads{b: listerOf(beadsDir)},
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		RetryPath:  RetryPath(townRoot),
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// rigBeads reads a rig's beads through the beads client. It never writes.
type rigBeads struct {
	b beadLister
}

// Placed lists the beads a pass considers since t: the ones a worker holds
// (in_progress, and hooked, which is the status a sling gives a bead it
// places), and the ones closed since t. The second read is what keeps a fast
// placement in the battery: a bead slung at 10:01 and closed by 10:10 is on no
// seat when a quarter-hourly pass runs, so only its close shows it was placed.
// A zero t means no pass has run yet, so there is nothing closed to catch up.
func (r rigBeads) Placed(t time.Time) ([]*beads.Issue, error) {
	held, err := r.b.List(beads.ListOptions{Status: "in_progress,hooked"})
	if err != nil {
		return nil, err
	}
	if t.IsZero() {
		return held, nil
	}
	closed, err := r.b.List(beads.ListOptions{Status: "closed", ClosedAfter: t, Priority: -1, Limit: 0})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(held)+len(closed))
	out := make([]*beads.Issue, 0, len(held)+len(closed))
	for _, issue := range held {
		if issue == nil {
			continue
		}
		seen[issue.ID] = true
		out = append(out, issue)
	}
	for _, issue := range closed {
		if issue == nil || seen[issue.ID] {
			continue
		}
		seen[issue.ID] = true
		out = append(out, issue)
	}
	return out, nil
}

// Ensure the production dependencies satisfy the pass's seams.
var (
	_ Asker  = (*jev.Client)(nil)
	_ Lister = rigBeads{}
)
