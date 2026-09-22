package rig

import (
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/wisp"
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

// Where an operational state was read from. The wisp layer is local and
// ephemeral (what `gt rig park` writes); the bead label is global and synced
// (the fallback that survives wisp cleanup, upstream #2079).
const (
	OpStateSourceLocal   = "local"
	OpStateSourceGlobal  = "global - synced"
	OpStateSourceDefault = "default"
)

// The wisp config key and the values it takes. The rig identity bead carries
// the same words under a "status:" label prefix.
const (
	RigStatusKey    = "status"
	RigStatusParked = "parked"
	RigStatusDocked = "docked"
)

// GetOpState returns a rig's operational state and the layer it came from.
//
// Precedence is the wisp layer first — local, ephemeral, and what `gt rig
// park` writes — then the rig identity bead's status labels, the persistent
// fallback that keeps a rig parked after wisp cleanup.
//
// This is the one definition `gt rig list` and the dashboard's Rigs and
// Merge Queue panels share, so a rig cannot read as parked on the CLI and
// active on the page that is supposed to warn about it.
func GetOpState(townRoot, rigName string) (OpState, string) {
	wispConfig := wisp.NewConfig(townRoot, rigName)
	if status := wispConfig.GetString(RigStatusKey); status != "" {
		switch strings.ToLower(status) {
		case RigStatusParked:
			return OpStateParked, OpStateSourceLocal
		case RigStatusDocked:
			return OpStateDocked, OpStateSourceLocal
		}
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

	bd := beads.NewWithBeadsDir(rigPath, beads.ResolveBeadsDir(rigPath))
	rigBead, err := bd.Show(beads.RigBeadIDWithPrefix(prefix, rigName))
	if err != nil {
		// No readable identity bead: either the rig never got one, or bd could
		// not answer. Report operational — inventing a parked rig out of a
		// failed read would hide real work, and the callers that gate dispatch
		// (cmd.IsRigParkedOrDocked, the daemon) do their own fail-safe reads.
		return OpStateOperational, OpStateSourceDefault
	}

	for _, label := range rigBead.Labels {
		if !strings.HasPrefix(label, "status:") {
			continue
		}
		switch strings.ToLower(strings.TrimPrefix(label, "status:")) {
		case RigStatusParked:
			return OpStateParked, OpStateSourceGlobal
		case RigStatusDocked:
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
