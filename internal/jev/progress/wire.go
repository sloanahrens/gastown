package progress

import (
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, the queue the daemon fills, each nudged
// rig's beads and the cap read from the same ledger. warnings receives the
// ledger's own warnings and may be nil.
func Open(townRoot string, q *Queue, s Settings, warnings io.Writer) (*Pass, error) {
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: s.USDPerBillionTokens}
	if err := town.Validate(); err != nil {
		return nil, fmt.Errorf("jev_progress: %w", err)
	}

	client, err := jev.NewClient(jev.BatteryOptions(townRoot, Caller, warnings, jev.BatteryScrub{
		Scrub:      s.Scrub,
		ScrubCheck: s.ScrubCheck,
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	})...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	return &Pass{
		Asker:    client,
		Queue:    q,
		Issue:    rigIssue(townRoot),
		TownRoot: townRoot,
		LogPath:  LogPath(townRoot),
		Allows:   func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// rigIssue reads a bead from its rig's own store. A rig with no beads
// directory is an error for that nudge alone.
func rigIssue(townRoot string) func(rig, id string) (*beads.Issue, error) {
	return func(rig, id string) (*beads.Issue, error) {
		dir := doltserver.FindRigBeadsDir(townRoot, rig)
		if dir == "" {
			return nil, fmt.Errorf("no beads directory for rig %s", rig)
		}
		return beads.New(dir).Show(id)
	}
}
