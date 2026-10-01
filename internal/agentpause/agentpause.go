// Package agentpause implements a per-agent sanctioned pause state:
// a durable marker file plus the shared "pause gate" every agent
// scanner (witness zombie detection, patrol scan, polecat staleness
// assessment) consults before restarting or nuking a session.
//
// Motivation: a mayor/operator freeze (SIGSTOP) of a misbehaving agent
// is indistinguishable from a stuck agent. The stuck-agent dog respawned
// a parked flint 20 minutes after the mayor froze it, and the witness
// patrol restarted parked agents. gt-ahik.
//
// Single source of truth: the marker file, .runtime/agents/<rig>/
// <role>.<name>.json — cheap, readable from shell, no Dolt dependency.
// Since gt-4k3fj.1 that file is the seat's intent record (internal/intent):
// a pause is desired=park there, and a seat the supervisor froze on an
// exhausted restart budget reads as paused too. This package is the pause
// view of that record; it never writes the record's other fields.
// Every scanner (Go and the stuck-agent dog's shell script alike) reads
// this file and only this file to decide "is this agent paused".
//
// The agent bead's agent_state=paused is a MIRROR, written for display
// (`gt polecat identity show`, dashboards) after the marker file, and it is
// never consulted as a fallback — a two-layer design let the marker and the
// bead disagree (gt-ahik, om kgx0). One layer, one answer.
//
// Every reader here fails CLOSED. A marker that exists but cannot be read or
// parsed is reported as a pause, because the callers are don't-touch guards:
// making a stuck agent wait is recoverable, restarting an agent the operator
// deliberately parked is the incident this package exists to prevent
// (gt-ahik, gt-wisp-6ajo).
package agentpause

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
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

	// PriorAgentState is the agent bead's agent_state at the moment of
	// pause (e.g. "working", "idle"), captured because the bead is now a
	// display mirror only. `gt agent resume` restores this value rather
	// than blindly writing "idle" — a pause taken mid-work must not
	// downgrade the bead to idle on resume.
	PriorAgentState string `json:"prior_agent_state,omitempty"`

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
	return intent.Seat{Rig: rig, Role: role, Name: name}.Path(townRoot)
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
	rec, err := intent.ReadPath(path)
	if err != nil {
		return true, &State{Paused: true, Reason: rec.Reason, Address: AddressFromMarkerPath(path)}, err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return false, nil, nil
	}
	return rec.Held(), &State{
		Paused:          rec.Held(),
		Reason:          rec.Reason,
		PausedAt:        rec.PausedAt,
		PausedBy:        rec.PausedBy,
		PriorAgentState: rec.PriorAgentState,
		Address:         AddressFromMarkerPath(path),
	}, nil
}

// Pause parks the agent: it sets the seat's intent record to desired=park
// with the operator's reason, keeping every other field (the supervisor's
// restart budget, the incarnation) as it was.
//
// priorAgentState records the agent bead's agent_state at the moment of
// pause, so `gt agent resume` can restore it rather than blindly writing
// "idle". Callers that have no bead to consult (or don't care) pass "".
//
// The write is atomic under the record's lock (internal/intent): no reader
// ever sees a half-written marker. A marker that could not be read is
// replaced, with the operator's reason.
func Pause(townRoot, rig, role, name, reason, pausedBy, priorAgentState string) error {
	_, err := intent.Update(townRoot, intent.Seat{Rig: rig, Role: role, Name: name}, func(r *intent.Record) error {
		now := time.Now().UTC()
		r.Desired = intent.DesiredPark
		r.Reason = reason
		r.PausedAt = now
		r.PausedBy = pausedBy
		r.PriorAgentState = priorAgentState
		r.Actor = pausedBy
		r.UpdatedAt = now
		return nil
	})
	return err
}

// Resume clears the hold: desired=run, not frozen, no pause reason.
//
// Unlike the marker file this package used to own, the record is not
// deleted: it also carries the seat's restart budget and incarnation, which
// outlive a pause. Resuming a seat the supervisor froze also empties its
// budget, or the next restart would freeze it again. Resuming an agent that
// has no record is a no-op.
func Resume(townRoot, rig, role, name string) error {
	path := FilePath(townRoot, rig, role, name)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := intent.Update(townRoot, intent.Seat{Rig: rig, Role: role, Name: name}, func(r *intent.Record) error {
		r.Desired = intent.DesiredRun
		if r.Frozen {
			// A supervisor freeze is an exhausted restart budget; resuming
			// without emptying it would freeze the seat again on its next
			// restart.
			r.Restarts = nil
		}
		r.Frozen = false
		r.Reason = ""
		r.PausedAt = time.Time{}
		r.PausedBy = ""
		r.PriorAgentState = ""
		r.UpdatedAt = time.Now().UTC()
		return nil
	})
	return err
}

// PausedByState returns the pause state for an agent, or nil when it is not
// paused. Used by gt status.
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

// PauseGate reports whether the agent is intentionally paused. It reads the
// marker file — the ONLY source of truth (see package doc) — and is the name
// every scanner call site uses for the choke-point check, so the intent
// reads clearly at the call site even though it is exactly IsPaused.
//
// The gate fails CLOSED, and that guarantee is carried by the single `paused`
// return value: an error is only ever returned together with paused=true, so
// a caller that ignores the error still refuses to touch the agent. Callers
// should log the error, then skip.
func PauseGate(townRoot, rig, role, name string) (bool, *State, error) {
	return IsPaused(townRoot, rig, role, name)
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
