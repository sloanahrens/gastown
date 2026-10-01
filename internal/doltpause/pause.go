// Package doltpause reads and writes the Dolt pause marker,
// <town>/daemon/dolt.pause (gt-8z769.2).
//
// The owner of a deliberate Dolt outage (the GC actor, the nightly backup, an
// operator running `gt dolt pause`) writes the marker before it stops or
// busies the server. Every Dolt client path in gt reads it: the daemon does
// not restart a paused server, and a client whose call fails against it
// reports "Dolt paused by <actor> until <t>: <reason>" instead of a bare
// connection error. The marker is a plain JSON file so it works while Dolt is
// hung and can be read from a shell.
//
// A marker past its until is treated as absent: a crashed owner can never
// pause the town forever. gt doctor warns about one left behind.
package doltpause

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the marker's name inside <town>/daemon.
const FileName = "dolt.pause"

// MaxDuration caps how far ahead a pause may run. A longer outage is an
// incident, not maintenance, and the doctor check treats an older marker as
// stale.
const MaxDuration = 24 * time.Hour

// Marker is the pause marker's content.
type Marker struct {
	// Actor is who paused Dolt (BD_ACTOR, else the git identity).
	Actor string `json:"actor"`
	// Reason says why, for the message clients print.
	Reason string `json:"reason"`
	// Until is when the pause lapses on its own.
	Until time.Time `json:"until"`
	// Since is when the marker was written.
	Since time.Time `json:"since,omitempty"`
}

// Path is the marker's path for townRoot.
func Path(townRoot string) string {
	return filepath.Join(townRoot, "daemon", FileName)
}

// Message is the line a client reports while Dolt is paused.
func (m *Marker) Message() string {
	return fmt.Sprintf("Dolt paused by %s until %s: %s",
		m.Actor, m.Until.Local().Format("2006-01-02 15:04:05 MST"), m.Reason)
}

// Active reports whether the pause still holds at now.
func (m *Marker) Active(now time.Time) bool {
	return now.Before(m.Until)
}

// Read returns the marker for townRoot whether or not it has lapsed, nil
// when there is none. A marker that cannot be parsed is an error.
func Read(townRoot string) (*Marker, error) {
	data, err := os.ReadFile(Path(townRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", Path(townRoot), err)
	}
	if m.Until.IsZero() {
		return nil, fmt.Errorf("parsing %s: no until", Path(townRoot))
	}
	return &m, nil
}

// Current returns the pause in force for townRoot at now, or nil. A missing,
// lapsed or unreadable marker is no pause: clients never refuse on a marker
// they cannot read (gt doctor reports it instead).
func Current(townRoot string, now time.Time) *Marker {
	if townRoot == "" {
		return nil
	}
	m, err := Read(townRoot)
	if err != nil || m == nil || !m.Active(now) {
		return nil
	}
	return m
}

// Write records m as townRoot's pause marker, replacing any other. It writes
// a temporary file and renames it, so a reader never sees half a marker.
func Write(townRoot string, m Marker) error {
	if strings.TrimSpace(m.Actor) == "" {
		return errors.New("pause marker needs an actor")
	}
	if strings.TrimSpace(m.Reason) == "" {
		return errors.New("pause marker needs a reason")
	}
	if m.Until.IsZero() {
		return errors.New("pause marker needs an until")
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(Path(townRoot))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), Path(townRoot)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Remove deletes townRoot's pause marker and reports whether there was one.
func Remove(townRoot string) (bool, error) {
	err := os.Remove(Path(townRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Error is a Dolt call refused or failed because Dolt is paused. It wraps
// the underlying failure, if any, so callers that classify errors still see
// it.
type Error struct {
	Marker Marker
	Cause  error
}

func (e *Error) Error() string { return e.Marker.Message() }

func (e *Error) Unwrap() error { return e.Cause }

// Explain returns err as an *Error when townRoot is paused at now, and err
// unchanged otherwise (including nil).
func Explain(townRoot string, now time.Time, err error) error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return err
	}
	m := Current(townRoot, now)
	if m == nil {
		return err
	}
	return &Error{Marker: *m, Cause: err}
}

// Stale describes why a marker on disk is left behind, or "" when it is not:
// it lapsed (until is past) or it was written more than MaxDuration ago.
func (m *Marker) Stale(now time.Time) string {
	if !m.Active(now) {
		return fmt.Sprintf("lapsed at %s", m.Until.Local().Format(time.RFC3339))
	}
	if !m.Since.IsZero() && now.Sub(m.Since) > MaxDuration {
		return fmt.Sprintf("written %s ago, older than the %s cap", now.Sub(m.Since).Round(time.Minute), MaxDuration)
	}
	if m.Until.Sub(now) > MaxDuration {
		return fmt.Sprintf("runs until %s, more than %s ahead", m.Until.Local().Format(time.RFC3339), MaxDuration)
	}
	return ""
}
