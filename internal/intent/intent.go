// Package intent owns the per-seat intent record: one small JSON file per
// seat under the town runtime directory that says what the town wants that
// seat to be doing, and holds the evidence the supervisor needs to decide
// whether it is doing it (ADR 0003, gt-4k3fj.1).
//
//	<town>/.runtime/agents/<rig>/<role>[.<name>].json   rig-level seats
//	<town>/.runtime/agents/<role>[.<name>].json         town-level seats
//
// It is the file the pause marker has always lived in. The pause fields
// (paused, reason, paused_at, paused_by, prior_agent_state) keep their names
// and meaning, so every existing reader — Go and the stuck-agent dog's shell —
// keeps working; the intent fields sit beside them. "paused" is always
// written and always equals Held(), because the shell reads a missing value
// as paused.
//
// The record never depends on Dolt: it is read and written with plain file
// I/O, so liveness decisions work when the store is down. Agent beads are
// display mirrors of it, written after it, never read for a decision.
//
// Every reader fails CLOSED. A record that exists but cannot be read or
// parsed reads as held (parked and frozen) and returns an error: the callers
// are don't-touch guards, and a seat left alone for one tick is recoverable
// where a restart of a seat the operator parked is not.
package intent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/atomicfile"
)

// Version is the record schema version written by this package.
const Version = 1

// Desired is what the town wants a seat to be doing.
type Desired string

const (
	// DesiredRun: the seat should have a live session. The zero value of a
	// record (no file) means run.
	DesiredRun Desired = "run"
	// DesiredStop: the seat's session was ended on purpose (idle reap,
	// patrol disabled) and nothing needs it until work is dispatched.
	DesiredStop Desired = "stop"
	// DesiredPark: the operator parked the seat. Nothing kills or restarts
	// it until it is resumed.
	DesiredPark Desired = "park"
)

// Seat names one agent seat. Rig is empty for town-level seats (mayor,
// deacon, dogs); Name is empty for singletons (witness, refinery, mayor,
// deacon).
type Seat struct {
	Rig  string
	Role string
	Name string
}

// stem is <role>[.<name>], the file name without its extension.
func (s Seat) stem() string {
	if s.Name == "" {
		return s.Role
	}
	return s.Role + "." + s.Name
}

// Path returns the seat's record path under townRoot.
func (s Seat) Path(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "agents", s.Rig, s.stem()+".json")
}

// lockPath returns the flock path for the seat's record. It lives outside
// the agents directory so that directory holds only records.
func (s Seat) lockPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "locks", "intent", s.Rig, s.stem()+".lock")
}

// String renders the seat as rig/role/name, dropping empty parts.
func (s Seat) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{s.Rig, s.Role, s.Name} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

// Progress is the last liveness sample for a seat, persisted so that stall
// evidence survives any number of daemon restarts. internal/liveness writes
// it; nothing else interprets it.
type Progress struct {
	// SessionCreated identifies the session incarnation the sample belongs
	// to; a sample from another incarnation is never compared.
	SessionCreated  time.Time `json:"session_created,omitempty"`
	PaneHash        string    `json:"pane_hash,omitempty"`
	TranscriptPath  string    `json:"transcript_path,omitempty"`
	TranscriptMtime time.Time `json:"transcript_mtime,omitempty"`
	TranscriptBytes int64     `json:"transcript_bytes,omitempty"`
	HeartbeatCycle  int64     `json:"heartbeat_cycle,omitempty"`
	HasHeartbeat    bool      `json:"has_heartbeat,omitempty"`
	// SampledAt is when this sample was taken.
	SampledAt time.Time `json:"sampled_at,omitempty"`
	// ChangedAt is the last sample at which any evidence changed.
	ChangedAt time.Time `json:"changed_at,omitempty"`
	// DeadSamples counts consecutive samples that found the seat dead, so a
	// restart can require more than one observation without a tracker in
	// memory.
	DeadSamples int `json:"dead_samples,omitempty"`
}

// Action records the last supervisor action on the seat.
type Action struct {
	Verb    string    `json:"verb"`
	Reason  string    `json:"reason,omitempty"`
	Actor   string    `json:"actor,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	At      time.Time `json:"at"`
}

// Record is the per-seat intent record.
type Record struct {
	Version       int     `json:"version,omitempty"`
	Desired       Desired `json:"desired,omitempty"`
	WorkBead      string  `json:"work_bead,omitempty"`
	IncarnationID string  `json:"incarnation_id,omitempty"`
	// Restarts are the times of restarts inside the budget window; the
	// supervisor prunes stamps older than its window when it records one.
	Restarts   []time.Time `json:"restarts,omitempty"`
	Frozen     bool        `json:"frozen,omitempty"`
	Actor      string      `json:"actor,omitempty"`
	UpdatedAt  time.Time   `json:"updated_at,omitempty"`
	LastAction *Action     `json:"last_action,omitempty"`
	Progress   *Progress   `json:"progress,omitempty"`

	// The pause marker fields, unchanged in name and meaning. Paused is
	// always serialized and always equals Held().
	Paused          bool      `json:"paused"`
	Reason          string    `json:"reason,omitempty"`
	PausedAt        time.Time `json:"paused_at"`
	PausedBy        string    `json:"paused_by,omitempty"`
	PriorAgentState string    `json:"prior_agent_state,omitempty"`
}

// EffectiveDesired returns Desired, with the absent value read as run.
func (r Record) EffectiveDesired() Desired {
	if r.Desired == "" {
		return DesiredRun
	}
	return r.Desired
}

// Held reports whether nothing may kill or restart the seat: the operator
// parked it, or the supervisor froze it. Paused is not consulted: it is the
// serialized copy of this answer, and a record read from disk with paused
// set has already been normalized to desired=park.
func (r Record) Held() bool {
	return r.Frozen || r.Desired == DesiredPark
}

// HoldReason explains a hold in one phrase, or "" when the seat is free.
func (r Record) HoldReason() string {
	if !r.Held() {
		return ""
	}
	kind := "parked"
	if r.Frozen {
		kind = "frozen"
	}
	if strings.TrimSpace(r.Reason) == "" {
		return kind
	}
	return kind + ": " + r.Reason
}

// RestartsSince counts restarts at or after since.
func (r Record) RestartsSince(since time.Time) int {
	n := 0
	for _, t := range r.Restarts {
		if !t.Before(since) {
			n++
		}
	}
	return n
}

// unreadableReason and malformedReason name a record that could not be read.
// They are shown verbatim by gt status and scanner logs.
const (
	unreadableReason = "unreadable intent record (treated as parked)"
	malformedReason  = "malformed intent record (treated as parked)"
)

// failClosed is the record a reader sees when the file exists but cannot be
// read or parsed.
func failClosed(reason string) Record {
	return Record{Desired: DesiredPark, Frozen: true, Paused: true, Reason: reason}
}

// Read returns the seat's record. An absent file is an empty record (run, not
// held) and no error. A file that exists but cannot be read or parsed returns
// the fail-closed held record and an error.
func Read(townRoot string, s Seat) (Record, error) {
	return readPath(s.Path(townRoot))
}

// ReadPath reads a record by path, with Read's fail-closed contract. It
// exists for callers that walk the agents directory.
func ReadPath(path string) (Record, error) {
	return readPath(path)
}

func readPath(path string) (Record, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path built from the town root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, nil
		}
		return failClosed(unreadableReason), fmt.Errorf("reading intent record %q: %w", path, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return failClosed(malformedReason), fmt.Errorf("malformed intent record %q: %w", path, err)
	}
	// A paused flag the intent fields do not explain — a pause marker written
	// before the intent record existed, or a hand edit — is a park: the flag
	// is what the shell dog reads, so Go must not read the seat as free.
	if r.Paused && !r.Held() {
		r.Desired = DesiredPark
	}
	return r, nil
}

// lockTimeout bounds the wait for a seat's record lock.
const lockTimeout = 10 * time.Second

// Update applies fn to the seat's record under an exclusive lock and writes
// the result atomically. An absent record starts empty; a record that cannot
// be read or parsed starts as the fail-closed held record, so a mutator that
// does not deliberately clear the hold keeps it. If fn returns an error
// nothing is written.
func Update(townRoot string, s Seat, fn func(*Record) error) (Record, error) {
	lockPath := s.lockPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return Record{}, fmt.Errorf("intent lock dir: %w", err)
	}
	fl := flock.New(lockPath)
	deadline := time.Now().Add(lockTimeout)
	for {
		ok, err := fl.TryLock()
		if err != nil {
			return Record{}, fmt.Errorf("intent lock %q: %w", lockPath, err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return Record{}, fmt.Errorf("intent lock %q: timed out after %s", lockPath, lockTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = fl.Unlock() }()

	path := s.Path(townRoot)
	rec, _ := readPath(path) // a broken record starts as the held one
	if err := fn(&rec); err != nil {
		return rec, err
	}
	rec.Version = Version
	rec.Paused = rec.Held()
	if err := atomicfile.EnsureDirAndWriteJSONWithPerm(path, rec, 0o644); err != nil {
		return rec, fmt.Errorf("writing intent record %q: %w", path, err)
	}
	return rec, nil
}
