package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
)

// The directory-cap trim (gt-xlazq).
//
// A polecat directory cap bounds the directories a rig holds. A sandbox seat's
// directory is single-use host state: bead A's seat end removes it once the
// seat's work has landed, but a seat that finishes by another road, or whose end
// step never ran, keeps its directory. Those directories are what puts a rig at
// its cap while it runs nothing, and the reap pass alone cannot clear them: a
// clean finished seat is reusable capacity, which the pass never touches.
//
// So when a rig holds at least its cap in polecat directories, the pass may trim
// its idle, clean sandbox seats, one directory apiece, oldest first. The trim
// lifts the reusable-capacity veto and nothing else: every fact the pass
// measures about the seat's work — its landing, its cleanup status, its hook,
// its branch, the grace clock — still decides, and a seat whose work is not
// proven safe stays. A tmux seat is never trimmed: its directory is its seat.
//
// This trigger counts directories, though the cap a sling admits against counts
// live seats (internal/cmd's liveSeatCounter). They answer different questions.
// Admission must not be wedged by a leftover directory, so it discounts them;
// the trim is what removes them, so it has to see them, and waiting for live
// seats to reach the cap would let leftovers grow without bound. Both use the
// same cap number (rig.EffectivePolecatDirCap).

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
