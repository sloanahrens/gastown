// Package agentpause implements a per-agent sanctioned pause state:
// a durable marker file plus the shared "pause gate" every agent
// scanner (witness zombie detection, patrol scan, polecat staleness
// assessment) consults before restarting or nuking a session.
//
// Motivation: a mayor/operator freeze (SIGSTOP) of a misbehaving agent
// is indistinguishable from a stuck agent. The stuck-agent dog respawned
// a parked flint 20 minutes after the mayor froze it, and the witness
// patrol restarted parked agents via the done-intent-dead path. gt-ahik.
//
// Two-layer design (same philosophy as internal/estop):
//
//  1. File layer (primary): .runtime/agents/<rig>/<role>.<name>.json —
//     cheap, readable from shell, no Dolt dependency.
//  2. Bead layer (fallback): agent bead agent_state=paused — survives
//     marker file loss; written by `gt agent pause`, cleared by
//     `gt agent resume`.
//
// PauseGate consults both, so a pause survives in either layer.
package agentpause

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

// State is the durable pause marker written to the marker file.
type State struct {
	// Paused is true while the agent is intentionally parked.
	Paused bool `json:"paused"`

	// Reason explains why the agent was paused (shown in gt status
	// and in scanner log output).
	Reason string `json:"reason,omitempty"`

	// PausedAt is when the agent was paused.
	PausedAt time.Time `json:"paused_at"`

	// PausedBy identifies who paused the agent (e.g. "human", "mayor").
	PausedBy string `json:"paused_by,omitempty"`

	// Address is the Gas Town address of the paused agent (e.g.
	// "gastown/flint"), derived from the marker path by the reader —
	// never serialized. gt status uses it to name the agent in the
	// PAUSED banner: without it a banner over two paused agents is two
	// identical lines and the operator cannot tell which is parked.
	Address string `json:"-"`
}

// FilePath returns the marker file path for an agent:
// <townRoot>/.runtime/agents/<rig>/<role>.<name>.json
// Singletons (witness, refinery) have an empty name and produce
// e.g. <rig>/witness.json.
func FilePath(townRoot, rig, role, name string) string {
	base := role
	if name != "" {
		base = role + "." + name
	}
	return filepath.Join(townRoot, ".runtime", "agents", rig, base+".json")
}

// IsPaused checks the file marker only. Returns (false, nil, nil)
// when no marker file exists.
func IsPaused(townRoot, rig, role, name string) (bool, *State, error) {
	data, err := os.ReadFile(FilePath(townRoot, rig, role, name)) //nolint:gosec // G304: path is built from trusted townRoot + validated role/name
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return false, nil, fmt.Errorf("malformed pause marker %q: %w", FilePath(townRoot, rig, role, name), err)
	}
	st.Address = AddressFromMarkerPath(FilePath(townRoot, rig, role, name))
	return st.Paused, &st, nil
}

// Pause writes the pause marker file.
func Pause(townRoot, rig, role, name, reason, pausedBy string) error {
	path := FilePath(townRoot, rig, role, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&State{
		Paused:   true,
		Reason:   reason,
		PausedAt: time.Now().UTC(),
		PausedBy: pausedBy,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644) //nolint:gosec // G301: intended world-readable runtime state
}

// Resume removes the pause marker file.
func Resume(townRoot, rig, role, name string) error {
	err := os.Remove(FilePath(townRoot, rig, role, name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PausedByState returns the pause state for an agent when it is paused
// in the file layer (regardless of the bead layer). Used by gt status.
func PausedByState(townRoot, rig, role, name string) *State {
	paused, st, err := IsPaused(townRoot, rig, role, name)
	if err != nil || !paused {
		return nil
	}
	return st
}

// PauseGate consults both pause layers and reports whether the agent
// is intentionally paused.
//
//   - File layer: authoritative when present (written by gt agent pause).
//   - Bead layer: fallback via ResolveAgentState(description, structured)
//     returning AgentStatePaused — covers the case where the marker file
//     was lost but the agent bead still carries agent_state=paused.
//
// A nil beadsClient disables the bead-layer check (file layer only).
// The bead read is best-effort: errors are returned in the error out
// parameter but do NOT clear a file-layer pause.
func PauseGate(townRoot, rig, role, name string, beadsClient *beads.Beads) (bool, *State, error) {
	filePaused, fileState, err := IsPaused(townRoot, rig, role, name)
	if err != nil {
		return false, nil, err
	}
	if filePaused {
		return true, fileState, nil
	}
	if beadsClient == nil || rig == "" {
		return false, nil, nil
	}
	prefix := beads.GetPrefixForRig(townRoot, rig)
	id := beads.AgentBeadIDWithPrefix(prefix, rig, role, name)
	issue, fields, berr := beadsClient.ForAgentBead().GetAgentBead(id)
	if berr != nil || fields == nil {
		return false, nil, berr
	}
	if beads.AgentState(beads.ResolveAgentState(issue.Description, fields.AgentState)) == beads.AgentStatePaused {
		st := &State{Paused: true}
		if issue.UpdatedAt != "" {
			if t, terr := time.Parse(time.RFC3339, issue.UpdatedAt); terr == nil {
				st.PausedAt = t
			}
		}
		return true, st, nil
	}
	return false, nil, nil
}

// ListPaused returns the pause states of every agent paused in the file
// layer. Scans .runtime/agents/**.json — cheap, no Dolt dependency.
// Each returned State carries its derived Address so callers can name the
// paused agent. Used by gt status to print a banner.
func ListPaused(townRoot string) []*State {
	var out []*State
	base := filepath.Join(townRoot, ".runtime", "agents")
	//nolint:errcheck // missing runtime dir = no paused agents; errors ignored by design
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if st := PausedByPath(path); st != nil {
			out = append(out, st)
		}
		return nil
	})
	return out
}

// PausedByPath reads a marker file directly (file layer only).
func PausedByPath(path string) *State {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path comes from a walk of the runtime dir
	if err != nil {
		return nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil || !st.Paused {
		return nil
	}
	st.Address = AddressFromMarkerPath(path)
	return &st
}

// AddressFromMarkerPath derives the Gas Town address of a paused agent
// from its marker path (<townRoot>/.runtime/agents/<rig>/<role>[.<name>].json):
//
//	.../agents/gastown/polecat.flint.json → "gastown/flint"
//	.../agents/gastown/witness.json       → "gastown/witness"
//	.../agents/gastown/crew.opal.json     → "gastown/crew/opal"
//
// Returns "" when the path is not a marker path — i.e. it does not sit
// under a .runtime/agents directory in one of the two valid depths.
func AddressFromMarkerPath(path string) string {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, ".json")
	if stem == "" || stem == base {
		return "" // no .json suffix
	}
	dir := filepath.Dir(path)
	switch rig := filepath.Base(dir); {
	case filepath.Base(filepath.Dir(dir)) == "agents":
		// .../.runtime/agents/<rig>/<role>[.<name>].json
		return markerAddress(rig, stem)
	case rig == "agents":
		// .../.runtime/agents/<role>.json — town-level (mayor, deacon).
		return markerAddress("", stem)
	default:
		return "" // not a marker path
	}
}

// markerAddress renders <rig>/<role>[.<name>] as a Gas Town address.
func markerAddress(rig, stem string) string {
	role, name, found := strings.Cut(stem, ".")
	if !found {
		role, name = stem, ""
	}
	if role == "" {
		return ""
	}
	switch {
	case rig == "":
		return role
	case role == constants.RoleCrew && name != "":
		// Crew addresses keep the role segment: <rig>/crew/<name>.
		return rig + "/" + constants.RoleCrew + "/" + name
	case role == constants.RolePolecat && name != "":
		// Polecat is implicit in a two-segment address: <rig>/<name>.
		return rig + "/" + name
	case name != "":
		return rig + "/" + role + "/" + name
	default:
		return rig + "/" + role
	}
}

// Reason returns the pause reason, or "(no reason given)" when empty.
// A nil state yields "(no reason given)".
func Reason(st *State) string {
	if st == nil || strings.TrimSpace(st.Reason) == "" {
		return "(no reason given)"
	}
	return st.Reason
}