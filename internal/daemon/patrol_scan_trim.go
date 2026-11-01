package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
)

// The directory-cap trim (gt-xlazq). The trigger counts a rig's polecat
// directories where a sling's admission counts live seats: admission must not be
// wedged by a leftover directory, and this trim is what removes it. Both read
// the same cap number (rig.EffectivePolecatDirCap).

// TrimmableSeats implements patrolscan.TrimEnv: the rig's sandbox seats, when
// the rig holds at least as many polecat directories as its cap, and nil when it
// is under the cap. A rig the cleanup config does not cover is not trimmed at
// all, which is nil: the pass does not run there.
//
// A cap that cannot be read, a directory listing that fails, or a seat whose
// agent bead cannot be classified is an error, and the tick trims nothing on it:
// a seat it cannot classify may be a tmux seat, and the trim must never remove
// one.
func (h *patrolScanHost) TrimmableSeats(rigName string) ([]string, error) {
	if !h.reapCoversRig(rigName) {
		return nil, nil
	}
	// One listing answers both questions: the directories are the size bound
	// the trim fires on, whatever their seats turned out to be, and each name is
	// a seat to classify.
	names, err := h.Polecats(rigName)
	if err != nil {
		return nil, fmt.Errorf("listing polecat directories of %s: %w", rigName, err)
	}
	cap, err := h.polecatDirCapFor(rigName)
	if err != nil {
		return nil, err
	}
	if len(names) < cap {
		return nil, nil
	}
	seats := make([]string, 0, len(names))
	for _, name := range names {
		_, sandboxed, err := h.openshellSeat(rigName, name)
		if err != nil {
			return nil, fmt.Errorf("telling a sandbox seat from a host seat in %s: %w", rigName, err)
		}
		if sandboxed {
			seats = append(seats, name)
		}
	}
	return seats, nil
}

// polecatDirCapFor is the rig's polecat directory cap, or the seam that replaces
// it in a unit test.
func (h *patrolScanHost) polecatDirCapFor(rigName string) (int, error) {
	if h.polecatDirCap != nil {
		return h.polecatDirCap(rigName)
	}
	return polecatDirCapFor(h.town(), rigName, nil)
}

// polecatDirCapFor reads rigName's polecat directory cap from the town at
// townRoot the way a sling's admission check does: the rig's max_polecats
// setting, floored at rig.MinPolecatDirsPerRig. The rig is built from the
// registry the way the spawn path builds it, so the two read the same layers
// (gt-1kbi); rigBeads is its reader of the rig identity bead, nil for a client
// rooted at the rig, so a unit test passes one that starts no bd.
func polecatDirCapFor(townRoot, rigName string, rigBeads beads.Client) (int, error) {
	rigsConfig, err := agentconfig.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return 0, fmt.Errorf("reading the rig registry for %s's directory cap: %w", rigName, err)
	}
	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		return 0, fmt.Errorf("rig %s is not in the rig registry, so its directory cap is unknown", rigName)
	}
	r := &rig.Rig{
		Name: rigName, Path: filepath.Join(townRoot, rigName),
		Config: entry.BeadsConfig, IdentityBeads: rigBeads,
	}
	return rig.EffectivePolecatDirCap(r.GetIntConfig("max_polecats")), nil
}
