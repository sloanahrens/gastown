package rig

import (
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// OpState is a rig's operational state: whether the town will dispatch work
// to it at all.
//
// It is deliberately separate from session state. `gt rig park` stops the
// rig's agents, so a parked rig and a broken one look identical from the
// outside — no witness, no refinery, nothing moving. Only the operational
// state distinguishes "an operator decided this" from "nobody will ever
// process the work piling up here", which is why every surface that reports
// rig health has to read it.
type OpState string

const (
	// OpStateOperational means the rig accepts work.
	OpStateOperational OpState = "OPERATIONAL"
	// OpStateParked means an operator paused the rig; dispatch skips it.
	OpStateParked OpState = "PARKED"
	// OpStateDocked means the rig was shut down town-wide; dispatch skips it.
	OpStateDocked OpState = "DOCKED"
)

// Where an operational state was read from. Parked is the rig's record in
// mayor/rigs.json (the registry); docked is the identity bead's
// status:docked label, global and synced.
const (
	OpStateSourceRegistry = "registry"
	OpStateSourceGlobal   = "global - synced"
	OpStateSourceDefault  = "default"
)

// rigDockedLabel is the identity-bead label gt rig dock writes.
const rigDockedLabel = "status:docked"

// GetOpState returns a rig's operational state and the layer it came from.
//
// Parked comes first and fails closed: the registry record is read through
// the config kernel (townconfig.IsParked), and a rig whose park state cannot
// be read reads as parked (gt-y3pgh.4). Docked is the rig identity bead's
// status:docked label.
//
// This is the one definition `gt rig list` and the dashboard's Rigs and
// Merge Queue panels share, so a rig cannot read as parked on the CLI and
// active on the page that is supposed to warn about it.
func GetOpState(townRoot, rigName string) (OpState, string) {
	return getOpState(townRoot, rigName, nil)
}

// getOpState is GetOpState with the identity bead read from store (nil: bd
// in the rig's own database).
func getOpState(townRoot, rigName string, store beads.Client) (OpState, string) {
	if parked, _ := townconfig.IsParked(townRoot, rigName); parked {
		return OpStateParked, OpStateSourceRegistry
	}

	rigPath := filepath.Join(townRoot, rigName)

	// Prefix from the rig's own config.json, falling back to the town
	// registry when config.json is missing.
	var prefix string
	if rigCfg, err := LoadRigConfig(rigPath); err == nil && rigCfg.Beads != nil {
		prefix = rigCfg.Beads.Prefix
	} else {
		prefix = config.GetRigPrefix(townRoot, rigName)
	}
	if prefix == "" {
		return OpStateOperational, OpStateSourceDefault
	}

	rigBead, err := identityBeads(rigPath, store).Show(beads.RigBeadIDWithPrefix(prefix, rigName))
	if err != nil {
		// No readable identity bead: either the rig never got one, or bd could
		// not answer. Report operational — inventing a docked rig out of a
		// failed read would hide real work, and the callers that gate dispatch
		// (cmd.IsRigParkedOrDocked, the daemon) do their own fail-safe reads.
		return OpStateOperational, OpStateSourceDefault
	}

	for _, label := range rigBead.Labels {
		if label == rigDockedLabel {
			return OpStateDocked, OpStateSourceGlobal
		}
	}

	return OpStateOperational, OpStateSourceDefault
}

// Label is the state as a lowercase word for display ("parked", "docked"),
// or "" when the rig accepts work. Panels that mark rigs no work reaches use
// it as the marker text, and the empty case is what keeps every healthy rig
// from carrying a badge the eye learns to skip.
func (s OpState) Label() string {
	switch s {
	case OpStateParked, OpStateDocked:
		return strings.ToLower(string(s))
	}
	return ""
}

// identityBeads is store, or, when it is nil, bd in rigPath's own database:
// where the rig identity bead lives.
func identityBeads(rigPath string, store beads.Client) beads.Client {
	if store != nil {
		return store
	}
	return beads.NewWithBeadsDir(rigPath, beads.ResolveBeadsDir(rigPath))
}
