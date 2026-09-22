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
// PauseGate consults both, so a pause survives in either layer — and
// `gt agent resume` clears both, so a pause that survives in only one is
// still undoable.
//
// Every reader here fails CLOSED. A marker that exists but cannot be read or
// parsed is reported as a pause, because the callers are don't-touch guards:
// making a stuck agent wait is recoverable, restarting an agent the operator
// deliberately parked is the incident this package exists to prevent
// (gt-ahik, gt-wisp-6ajo).
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
//
// A marker that exists but cannot be read or parsed is reported as a pause
// AND as an error, with a State whose Reason names the problem: the error is
// for the operator's log, the pause is for the guard (fail closed).
func IsPaused(townRoot, rig, role, name string) (bool, *State, error) {
	return readMarker(FilePath(townRoot, rig, role, name))
}

// readMarker is the single marker reader behind IsPaused and PausedByPath.
// It fails closed (see the package comment): an existing marker that cannot
// be read or parsed reads as paused, so a guard never mistakes a broken
// write or a hand-edit for "not paused".
func readMarker(path string) (bool, *State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: caller-built trusted path
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return true, &State{Paused: true, Reason: unreadableReason, Address: AddressFromMarkerPath(path)},
			fmt.Errorf("reading pause marker %q: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return true, &State{Paused: true, Reason: malformedReason, Address: AddressFromMarkerPath(path)},
			fmt.Errorf("malformed pause marker %q: %w", path, err)
	}
	st.Address = AddressFromMarkerPath(path)
	return st.Paused, &st, nil
}

// Reasons attached to a marker that could not be read or parsed. They are
// shown verbatim by gt status and by the scanner logs, so they say what is
// wrong rather than pretending to be an operator reason.
const (
	unreadableReason = "unreadable pause marker (treated as paused)"
	malformedReason  = "malformed pause marker (treated as paused)"
)

// Pause writes the pause marker file, overwriting any existing marker.
//
// The write is atomic: a temp file in the same directory, fsynced, then
// renamed over the target. A truncated marker reads as paused (fail closed),
// but it also loses the operator's reason and cannot be told apart from a
// hand-edit, so no reader should ever see a half-written one.
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
	return writeFileAtomic(path, append(data, '\n'))
}

// writeFileAtomic writes data to path via a sibling temp file and a rename,
// so a reader sees either the previous contents or the complete new ones —
// never a partial write. The temp file is created in the target directory so
// the rename stays on one filesystem.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil { //nolint:gosec // G302: intended world-readable runtime state
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
//
// The read error is deliberately dropped: a broken marker still yields a
// State (with a Reason naming the problem), and showing that in gt status is
// how an operator learns the marker needs rewriting.
func PausedByState(townRoot, rig, role, name string) *State {
	paused, st, _ := IsPaused(townRoot, rig, role, name)
	if !paused {
		return nil
	}
	return st
}

// BeadReader is the narrow slice of *beads.Beads the bead-layer pause check
// needs. *beads.Beads satisfies it; tests substitute a fake so the fallback
// layer is covered without a Dolt fixture.
type BeadReader interface {
	GetAgentBead(id string) (*beads.Issue, *beads.AgentFields, error)
}

// BeadPaused reports whether the bead layer marks the agent paused
// (agent_state=paused) and returns the pause state derived from the bead.
//
// A nil reader or an empty beadID means there is no bead layer to consult
// and reports not paused. A read error is returned: the caller must treat it
// as "unknown", which for a don't-touch guard means paused (fail closed).
func BeadPaused(reader BeadReader, beadID string) (bool, *State, error) {
	if reader == nil || beadID == "" {
		return false, nil, nil
	}
	issue, fields, err := reader.GetAgentBead(beadID)
	if err != nil {
		return false, nil, err
	}
	if issue == nil || fields == nil {
		// No agent bead: there is no bead-layer pause to honor.
		return false, nil, nil
	}
	if beads.AgentState(beads.ResolveAgentState(issue.Description, fields.AgentState)) != beads.AgentStatePaused {
		return false, nil, nil
	}
	st := &State{Paused: true}
	if issue.UpdatedAt != "" {
		if t, terr := time.Parse(time.RFC3339, issue.UpdatedAt); terr == nil {
			st.PausedAt = t
		}
	}
	return true, st, nil
}

// PauseGate consults both pause layers and reports whether the agent
// is intentionally paused.
//
//   - File layer: authoritative when present (written by gt agent pause).
//   - Bead layer: fallback via ResolveAgentState(description, structured)
//     returning AgentStatePaused — covers the case where the marker file
//     was lost but the agent bead still carries agent_state=paused.
//
// A nil reader disables the bead-layer check (file layer only), which is what
// the witness restart hot loop uses: it must not take a config lookup or a
// Dolt read to decide, and `gt agent pause` always writes the marker file
// before it touches the bead.
//
// The gate fails CLOSED, and that guarantee is carried by the single `paused`
// return value: an error is only ever returned together with paused=true, so
// a caller that ignores the error still refuses to touch the agent. Callers
// should log the error, then skip.
func PauseGate(townRoot, rig, role, name string, reader BeadReader) (bool, *State, error) {
	beadID := ""
	if reader != nil && rig != "" {
		beadID = beads.AgentBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, rig), rig, role, name)
	}
	return PauseGateBeadID(townRoot, rig, role, name, beadID, reader)
}

// PauseGateBeadID is PauseGate for a caller that already knows the agent bead
// ID — town-level agents (mayor, deacon) have no rig segment to derive one
// from, so `gt agent resume` passes the ID it resolved from the address
// instead of leaving the bead layer unreachable.
func PauseGateBeadID(townRoot, rig, role, name, beadID string, reader BeadReader) (bool, *State, error) {
	layers := ReadLayers(townRoot, rig, role, name, beadID, reader)
	if !layers.Paused() {
		return false, nil, nil
	}
	return true, layers.State(), layers.Err()
}

// Layers is one agent's pause state, layer by layer. PauseGate collapses the
// layers into a single fail-closed answer, which is what a don't-touch guard
// wants; a caller that must ACT on each layer needs them apart. `gt agent
// resume` is that caller — it clears both layers, so a pause held in only one
// (a lost marker file) is still undoable (gt-wisp-6ajo).
type Layers struct {
	FilePaused bool
	FileState  *State
	FileErr    error
	BeadPaused bool
	BeadState  *State
	BeadErr    error
}

// Paused reports whether the agent must be treated as paused: either layer
// says so, or a layer could not be read (fail closed, like PauseGate). An
// unknown pause state is never reported as "not paused".
func (l Layers) Paused() bool {
	return l.FilePaused || l.BeadPaused || l.FileErr != nil || l.BeadErr != nil
}

// State returns the pause state to show an operator, preferring the file
// layer — it carries the reason they typed. Nil when neither layer produced
// one: a bead-only pause records a state, not a reason, and a failed read
// records nothing.
func (l Layers) State() *State {
	if l.FileState != nil {
		return l.FileState
	}
	return l.BeadState
}

// Err returns the read failure that matters: the file layer's when the file
// layer decided the answer, else the bead layer's. A bead read that failed
// after the file layer already answered "paused" is not a check failure, and
// reporting it as one would send a scanner down its error path for a decision
// it never needed to make.
func (l Layers) Err() error {
	if l.FilePaused || l.FileErr != nil {
		return l.FileErr
	}
	return l.BeadErr
}

// ReadLayers reads both pause layers without collapsing their outcomes.
// beadID is passed explicitly because town-level agents have no rig segment to
// derive one from; an empty beadID or nil reader means there is no bead layer
// (and no read to pay for — the restart hot loop calls PauseGate with a nil
// reader for exactly that reason).
func ReadLayers(townRoot, rig, role, name, beadID string, reader BeadReader) Layers {
	var l Layers
	l.FilePaused, l.FileState, l.FileErr = IsPaused(townRoot, rig, role, name)
	l.BeadPaused, l.BeadState, l.BeadErr = BeadPaused(reader, beadID)
	return l
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

// PausedByPath reads a marker file directly (file layer only). Like every
// reader here it fails closed: an unreadable marker is reported as a pause
// (with a Reason naming the problem), not as an absent one — otherwise
// gt status would show nothing for an agent the rest of the system refuses
// to touch.
func PausedByPath(path string) *State {
	paused, st, _ := readMarker(path)
	if !paused {
		return nil
	}
	return st
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

// AddressFor renders the Gas Town address of an agent from the same
// coordinates FilePath takes:
//
//	AddressFor("gastown", "polecat", "flint") → "gastown/flint"
//	AddressFor("gastown", "crew", "opal")     → "gastown/crew/opal"
//	AddressFor("", "mayor", "")               → "mayor"
//
// It is the exact inverse of AddressFromMarkerPath: AddressFor(rig, role,
// name) == AddressFromMarkerPath(FilePath(townRoot, rig, role, name)) for
// every agent. That invariant is why `gt agent pause`/`resume` print this
// form rather than the caller's address string: the operator reads the pause
// output and the gt status banner side by side, and one agent under two names
// is a copy-paste bug waiting to happen (gt-wisp-6ajo).
func AddressFor(rig, role, name string) string {
	stem := role
	if name != "" {
		stem = role + "." + name
	}
	return markerAddress(rig, stem)
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