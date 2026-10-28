// Package installpending reads and writes the pending-install marker,
// <daemon dir>/install-pending.json (gt-vc34c).
//
// The rebuilding job writes it when it concludes a binary-affecting install is
// due and clears it when that cycle reaches a verdict, so a caller outside the
// job can stay out of an install's way. Pending states what makes a marker
// stand; the file is plain JSON so a shell can read it.
package installpending

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/procid"
)

// FileName is the marker's name inside the daemon runtime dir.
const FileName = "install-pending.json"

// Ceiling is how long a marker outlives the cycle that last wrote it before
// readers treat it as gone: longer than one install cycle may take, build and
// gate wait included, so work in flight is never read as abandoned, and short
// enough that a writer which stopped refreshing cannot hold the town out of
// slings for long (gt-vc34c).
const Ceiling = 45 * time.Minute

// FutureSkew is how far ahead of a reader's clock a marker may be dated and
// still read as in flight: writer and reader share the host clock, so a marker
// further ahead than this is malformed and would otherwise never age out
// (gt-vc34c).
const FutureSkew = time.Minute

// Marker is the marker's content: when a cycle concluded the install was due,
// and the process that concluded it. Start is the writer's start token when the
// platform has one, which is what separates it from a process later handed the
// same pid.
type Marker struct {
	At    time.Time `json:"at"`
	PID   int       `json:"pid"`
	Start string    `json:"start,omitempty"`
}

// Note names the pending install in operator terms: when the cycle recorded it,
// by which daemon, and what lifts it. Every dispatcher that holds work under a
// marker prints this one sentence, so an operator greps a single phrase to find
// all of them (gt-2rp64).
func (m *Marker) Note() string {
	return fmt.Sprintf("an install of gt is pending (recorded %s by daemon pid %d); no new sling starts until it lands or its marker clears",
		m.At.UTC().Format(time.RFC3339), m.PID)
}

// Store is one town's marker file. Resolve it where the environment is read —
// the daemon, or a command that owns its own env — and pass it down, so a test
// hands itself a temp file instead of whatever INSTALL_GT_DAEMON_DIR names in
// the operator's environment (gt-vc34c).
type Store struct{ path string }

// StoreFor resolves the marker under the process environment, honoring the same
// INSTALL_GT_DAEMON_DIR override the install lock and scripts/install-gt.sh do.
func StoreFor(townRoot string) Store { return storeFor(townRoot, os.Getenv) }

// StoreForEnv is StoreFor over the caller's own environment, for a reader that
// carries an injectable one (the container gate's).
func StoreForEnv(townRoot string, getenv func(string) string) Store {
	return storeFor(townRoot, getenv)
}

// storeFor is StoreFor with the environment passed in, so a test hands itself a
// temp directory instead of reading INSTALL_GT_DAEMON_DIR (gt-y2ygl).
func storeFor(townRoot string, getenv func(string) string) Store {
	return StoreIn(filepath.Join(dirFor(townRoot, getenv), FileName))
}

// StoreIn is the marker at exactly path.
func StoreIn(path string) Store { return Store{path: path} }

// Dir is the daemon runtime dir for townRoot, honoring the same override the
// install lock and scripts/install-gt.sh do.
func Dir(townRoot string) string { return dirFor(townRoot, os.Getenv) }

func dirFor(townRoot string, getenv func(string) string) string {
	if dir := getenv("INSTALL_GT_DAEMON_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(townRoot, "daemon")
}

// Path is the marker file's path.
func (s Store) Path() string { return s.path }

// Write records an install that is due at at, written by pid, replacing any
// marker already there so that Ceiling measures from the last cycle that still
// found the install due.
func (s Store) Write(at time.Time, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("install-pending marker needs a writer pid, got %d", pid)
	}
	m := Marker{At: at.UTC(), PID: pid}
	if id, ok := procid.Of(pid); ok {
		m.Start = id.Start
	}
	return atomicfile.EnsureDirAndWriteJSON(s.path, m)
}

// Clear removes the marker; one that is already gone is not an error.
func (s Store) Clear() error {
	err := os.Remove(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Pending returns the marker in force at now, or nil when no install stands:
// no marker, one whose writer is gone, or one older than Ceiling. A marker
// file that cannot be read or parsed is an error rather than nil, so a caller
// that must not race an install never takes a broken marker for none.
func (s Store) Pending(now time.Time) (*Marker, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	if m.At.IsZero() || m.PID <= 0 {
		return nil, fmt.Errorf("parsing %s: the marker names no time or no writer pid", s.path)
	}
	// A marker dated past the reader's own clock by more than FutureSkew is
	// malformed, not in flight: aged from a time that was never now, it would
	// stand until the clock caught up and then for Ceiling after that.
	if m.At.After(now.Add(FutureSkew)) {
		return nil, nil
	}
	// Age is read before the writer: a marker no cycle has refreshed is spent
	// whatever its pid now names, and a pid handed to an unrelated process must
	// not revive it.
	if now.Sub(m.At) >= Ceiling {
		return nil, nil
	}
	if !writerRunning(m) {
		return nil, nil
	}
	return &m, nil
}

// writerRunning reports whether the process the marker names is still the one
// that wrote it. A recorded start token answers that exactly, so a pid the
// kernel handed to another process after the writer died is not the writer. A
// marker written where the token could not be read falls back to the pid probe,
// which Ceiling then bounds (gt-vc34c).
func writerRunning(m Marker) bool {
	if m.Start != "" {
		return procid.ID{PID: m.PID, Start: m.Start}.Running(procid.StartToken)
	}
	return processAlive(m.PID)
}
