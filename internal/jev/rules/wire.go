package rules

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/stale"
	"github.com/steveyegge/gastown/internal/land"
)

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger, not this job alone.
const DefaultMaxUSDPerDay = 1.0

// Settings is the operator's configuration for the pass, read from
// patrols.jev_rules.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Rigs are the rigs whose submissions a pass reads.
	Rigs []string

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool
}

// beadLister is the part of *beads.Beads the lister uses. It is an interface
// so a test can run the lister without a Dolt server.
type beadLister interface {
	List(beads.ListOptions) ([]*beads.Issue, error)
}

// Open wires the production pass for a town: the Jev client writing to the
// usage ledger under the Caller name, each rig's beads, the worktrees the
// polecats run in and the spend read from the same ledger. warnings receives
// the ledger's own warnings and may be nil.
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
		return nil, fmt.Errorf("jev_rules.usd_per_billion_tokens is required and must be above zero, not %v", price)
	}
	capUSD := s.MaxUSDPerDay
	if capUSD == 0 {
		capUSD = DefaultMaxUSDPerDay
	}
	if !(capUSD > 0 && !math.IsInf(capUSD, 0)) {
		return nil, fmt.Errorf("jev_rules.max_usd_per_day must be above zero, not %v", capUSD)
	}

	client, err := newClient(jev.BatteryOptions(townRoot, Caller, warnings, s.Scrub)...)
	if err != nil {
		return nil, fmt.Errorf("opening the Jev client: %w", err)
	}

	stores := map[string]beadLister{}
	for _, rig := range s.Rigs {
		dir := doltserver.FindRigBeadsDir(townRoot, rig)
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("no beads directory for rig %s", rig)
		}
		stores[rig] = listerOf(dir)
	}

	town := battery.Cap{Ledger: jev.UsageLedgerPath(townRoot), MaxUSDPerDay: capUSD, USDPerBillionTokens: price}

	return &Pass{
		Asker: client,
		Lister: rigDones{
			stores:   stores,
			refusals: func() ([]attention.Refusal, error) { return attention.ReadRefusals(townRoot) },
		},
		Locate:     func(rig, polecat string) string { return locateWorktree(townRoot, rig, polecat) },
		Latest:     func(worktree string) (string, error) { return agentlog.LatestTranscript(townRoot, worktree) },
		TownRoot:   townRoot,
		LogPath:    LogPath(townRoot),
		CursorPath: CursorPath(townRoot),
		Allows:     func() (bool, error) { return town.Allows(time.Now) },
	}, nil
}

// rigDones lists each polecat's finished work from two records: each rig's
// beads, for submissions that reached the landing queue, and the revert guard's
// refusal ledger, for submissions gt done refused before they got there. A
// refused gt done writes no READY TO LAND block, so the ledger is the only
// place it shows. It never writes.
type rigDones struct {
	stores   map[string]beadLister
	refusals func() ([]attention.Refusal, error)
}

// Done lists every submission and every revert-guard refusal stamped at or
// after t, whether the submission is still held or has already landed.
func (r rigDones) Done(t time.Time) ([]Done, error) {
	rigs := make([]string, 0, len(r.stores))
	for rig := range r.stores {
		rigs = append(rigs, rig)
	}
	sort.Strings(rigs)

	var out []Done
	for _, rig := range rigs {
		b := r.stores[rig]
		held, err := b.List(beads.ListOptions{Label: land.LabelReadyToLand, Status: "open,in_progress,hooked,blocked", Limit: 0})
		if err != nil {
			return nil, fmt.Errorf("listing %s's submitted beads: %w", rig, err)
		}
		closed, err := b.List(beads.ListOptions{Status: "closed", ClosedAfter: t, Limit: 0})
		if err != nil {
			return nil, fmt.Errorf("listing %s's closed submissions: %w", rig, err)
		}
		seen := map[string]bool{}
		for _, is := range append(held, closed...) {
			if is == nil || seen[is.ID] {
				continue
			}
			// The landing removes the label in the update that closes the
			// bead, so a landed submission is found by its close reason.
			if !beads.HasLabel(is, land.LabelReadyToLand) && !stale.IsLandedReason(is.CloseReason) {
				continue
			}
			seen[is.ID] = true
			w, ok := land.ParseReadyNote(is.Notes)
			if !ok || w.Submitted.IsZero() || w.Submitted.Before(t) {
				continue
			}
			out = append(out, Done{Rig: rig, Bead: is.ID, Polecat: w.Worker, SubmittedAt: w.Submitted})
		}
	}

	refusals, err := r.refusals()
	if err != nil {
		return nil, fmt.Errorf("reading the refusal ledger: %w", err)
	}
	for _, ref := range refusals {
		if ref.Kind != attention.KindRevertGuard || ref.Bead == "" || ref.Rig == "" || ref.TS.Before(t) {
			continue
		}
		if _, ok := r.stores[ref.Rig]; !ok {
			continue
		}
		out = append(out, Done{Rig: ref.Rig, Bead: ref.Bead, Polecat: ref.Worker, SubmittedAt: ref.TS})
	}
	return out, nil
}

// locateWorktree returns the worktree a polecat runs in: polecats/<name>/<rig>
// when that is a git worktree, else polecats/<name>. It returns "" when neither
// is one.
func locateWorktree(townRoot, rig, polecat string) string {
	if polecat == "" {
		return ""
	}
	for _, dir := range []string{
		filepath.Join(townRoot, rig, "polecats", polecat, rig),
		filepath.Join(townRoot, rig, "polecats", polecat),
	} {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
	}
	return ""
}
