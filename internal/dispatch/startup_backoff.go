package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A sling that fails at session start leaves its bead open and unassigned, the
// exact state the convoy feeders dispatch from. Without a record of the
// failure they re-sling it on their next tick: the retry spends one of the
// bead's respawn attempts, holds the per-bead sling flock so the operator's
// own retry is refused as "already being slung", and fails the same way again
// (gt-wacl, seen on gt-g8kr 2026-09-16). `gt sling` records the failure here
// and clears it on success; the automatic dispatchers consult it before
// slinging.

const (
	// StartupBackoffBase is how long a bead rests after its first startup
	// failure. Each further consecutive failure doubles it.
	StartupBackoffBase = 5 * time.Minute

	// StartupBackoffMax caps the rest, so a bead whose startup keeps failing
	// is still retried about hourly rather than waiting on an operator.
	StartupBackoffMax = time.Hour

	// startupReasonMax bounds the recorded reason; it is a one-line hint for
	// the feeder's skip log, not the failure's full output.
	startupReasonMax = 200
)

// startupFailure is one bead's record of consecutive startup failures.
type startupFailure struct {
	BeadID      string    `json:"bead_id"`
	Count       int       `json:"count"`
	LastFailure time.Time `json:"last_failure"`
	Reason      string    `json:"reason"`
}

// startupFailureDir holds one file per bead. A file per bead needs no lock:
// each record has a single writer at a time (the sling holding that bead's
// flock) and is replaced atomically, so a reader sees the old or the new
// record, never a torn one.
func startupFailureDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "sling-startup-failures")
}

// startupFailurePath returns the record file for beadID, or "" when the ID
// could not be a bead ID (empty, or a path).
func startupFailurePath(townRoot, beadID string) string {
	if townRoot == "" || beadID == "" || beadID == "." || beadID == ".." || filepath.Base(beadID) != beadID {
		return ""
	}
	return filepath.Join(startupFailureDir(townRoot), beadID+".json")
}

func readStartupFailure(townRoot, beadID string) (*startupFailure, bool) {
	path := startupFailurePath(townRoot, beadID)
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted townRoot and a validated bead ID
	if err != nil {
		return nil, false
	}
	var rec startupFailure
	if err := json.Unmarshal(data, &rec); err != nil || rec.Count < 1 {
		return nil, false
	}
	return &rec, true
}

// RecordStartupFailure notes that a sling of beadID failed at session start.
// Consecutive failures lengthen the backoff StartupBackoff reports; a sling
// that starts its session (ClearStartupFailure) resets it.
func RecordStartupFailure(townRoot, beadID, reason string) error {
	return recordStartupFailure(townRoot, beadID, reason, time.Now())
}

func recordStartupFailure(townRoot, beadID, reason string, now time.Time) error {
	path := startupFailurePath(townRoot, beadID)
	if path == "" {
		return fmt.Errorf("no startup-failure record for bead ID %q", beadID)
	}
	rec := startupFailure{BeadID: beadID, Count: 1}
	if prev, ok := readStartupFailure(townRoot, beadID); ok {
		rec.Count = prev.Count + 1
	}
	rec.LastFailure = now.UTC()
	rec.Reason = firstLine(reason, startupReasonMax)

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), beadID+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ClearStartupFailure drops beadID's record once a sling starts its session.
func ClearStartupFailure(townRoot, beadID string) {
	if path := startupFailurePath(townRoot, beadID); path != "" {
		_ = os.Remove(path)
	}
}

// StartupBackoff reports why an automatic dispatcher must not sling beadID
// yet, or "" when it may: the bead's most recent sling failed at session
// start and its backoff window (StartupBackoffBase, doubling per consecutive
// failure up to StartupBackoffMax) has not elapsed.
//
// Only automatic dispatchers consult it. An operator's explicit `gt sling`
// is the retry the backoff leaves room for, and stays open.
//
// The backoff is a throttle, not a safety hold, so an absent or unreadable
// record fails open: a bead is never parked on a record nobody can read.
func StartupBackoff(townRoot, beadID string) string {
	return startupBackoff(townRoot, beadID, time.Now())
}

func startupBackoff(townRoot, beadID string, now time.Time) string {
	rec, ok := readStartupFailure(townRoot, beadID)
	if !ok {
		return ""
	}
	retryAt := rec.LastFailure.Add(startupBackoffWindow(rec.Count))
	if !now.Before(retryAt) {
		return ""
	}
	return fmt.Sprintf("last sling failed at session start %s ago (%d in a row: %s); backing off until %s",
		now.Sub(rec.LastFailure).Round(time.Second), rec.Count, rec.Reason, retryAt.Local().Format("15:04:05"))
}

// startupBackoffWindow is the rest after count consecutive failures:
// base, 2x, 4x, ... capped at StartupBackoffMax.
func startupBackoffWindow(count int) time.Duration {
	d := StartupBackoffBase
	for i := 1; i < count; i++ {
		d *= 2
		if d >= StartupBackoffMax {
			return StartupBackoffMax
		}
	}
	return d
}

// firstLine returns s's first non-empty line, cut to limit bytes.
func firstLine(s string, limit int) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > limit {
				l = strings.ToValidUTF8(l[:limit], "")
			}
			return l
		}
	}
	return ""
}
