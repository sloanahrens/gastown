// Package deepreview is a deep-review run's plan as pure functions: the
// ledger's cursor, the classified landings, and the window and landing list one
// run reviews.
//
// Nothing here opens a file, runs a command or reads a clock. The notes text,
// the landings bytes and the run's now arrive as arguments, so every rule is a
// table row and the command and daemon layers own the I/O. Until the port is
// finished, scripts/deep-review.sh is the behavior these functions must
// reproduce (gt-xx2da).
package deepreview

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DefaultWindow is the span a run reviews when its ledger carries no cursor
// line, the shell's DEFAULT_WINDOW_SECONDS.
const DefaultWindow = 24 * time.Hour

// Timestamp is a ledger timestamp: the instant it names and the text it was
// written as.
//
// The text is kept because the plan echoes it. The newest landing's spelling
// becomes the next cursor line, so a fraction or an offset the ledger wrote is
// not normalised away into a different-looking ledger.
type Timestamp struct {
	Time time.Time
	Text string
}

// ParseTimestamp parses an RFC3339 timestamp as the landing record writes one:
// with an optional fraction and a Z or ±HH:MM offset.
func ParseTimestamp(s string) (Timestamp, error) {
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return Timestamp{}, fmt.Errorf("not an RFC3339 timestamp: %q", s)
	}
	return Timestamp{Time: at, Text: s}, nil
}

// String returns the timestamp as the ledger wrote it.
func (t Timestamp) String() string { return t.Text }

// After reports whether t names an instant after u.
func (t Timestamp) After(u Timestamp) bool { return t.Time.After(u.Time) }

// Before reports whether t names an instant before u.
func (t Timestamp) Before(u Timestamp) bool { return t.Time.Before(u.Time) }

// MarshalJSON writes the timestamp as the ledger wrote it, not as a struct with
// an instant inside it.
func (t Timestamp) MarshalJSON() ([]byte, error) { return json.Marshal(t.Text) }

// Cursor is a ledger's newest cursor line: the landing the last run covered and
// the slice path the ledger wrote beside it.
type Cursor struct {
	At Timestamp
	// Path is the slice path the line named, empty when it named none: an
	// empty slice writes the timestamp alone.
	Path string
}

// cursorLine matches a cursor line's timestamp and the optional path one space
// after it. Everything after that space is the path, spaces included; a tab
// there does not match, and the shell refuses such a line too.
var cursorLine = regexp.MustCompile(`^cursor: (\S+)(?: (.*))?$`)

// ParseCursor returns the newest cursor line in a ledger's notes.
//
// Newest is by timestamp, not by position: a replayed or hand-edited ledger can
// list its cursor lines out of order, and the one the next run resumes from is
// the latest instant. found is false when the notes hold no cursor line, which
// the caller reads as DefaultWindow.
//
// A line that starts "cursor: " is a cursor line, and one that does not parse
// is an error rather than a line to skip: falling back to an older cursor would
// review landings this ledger says are covered.
func ParseCursor(notes string) (Cursor, bool, error) {
	var newest Cursor
	found := false
	for _, line := range strings.Split(notes, "\n") {
		if !strings.HasPrefix(line, "cursor: ") {
			continue
		}
		m := cursorLine.FindStringSubmatch(line)
		if m == nil {
			return Cursor{}, false, fmt.Errorf("malformed cursor line: %q", line)
		}
		at, err := ParseTimestamp(m[1])
		if err != nil {
			return Cursor{}, false, fmt.Errorf("bad timestamp in cursor line: %q", m[1])
		}
		if !found || at.After(newest.At) {
			newest = Cursor{At: at, Path: m[2]}
			found = true
		}
	}
	return newest, found, nil
}

// Window is the span of a ledger one run reviews from.
type Window struct {
	// Since is the newest landing already covered; a landing at that instant
	// is not reviewed again.
	Since Timestamp
	// Cursor is the slice path the newest cursor line named, empty when the
	// ledger has no cursor line or that line named no path.
	Cursor string
}

// ReviewWindow returns the window a run reviews, from the ledger's notes and the
// run's now.
//
// No cursor line means the default window ending at now, truncated to whole
// seconds as the shell's todate is, so both plans see one window for the same
// instant. A cursor after now is an error: every landing would fall behind it,
// and the run would report quiet while reviewing nothing.
func ReviewWindow(notes string, now time.Time) (Window, error) {
	cur, found, err := ParseCursor(notes)
	if err != nil {
		return Window{}, err
	}
	if !found {
		since := now.UTC().Truncate(time.Second).Add(-DefaultWindow)
		return Window{Since: Timestamp{Time: since, Text: since.Format(time.RFC3339)}}, nil
	}
	if cur.At.Time.After(now) {
		return Window{}, fmt.Errorf("cursor %s is after now %s", cur.At, now.Format(time.RFC3339Nano))
	}
	return Window{Since: cur.At, Cursor: cur.Path}, nil
}
