package beads

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Sling contexts are how a deferred sling waits in the scheduler. The dispatch
// heartbeat scans every beads database in the town once a minute to find them,
// so a town with nothing scheduled pays eleven to fourteen bd calls a minute to
// find nothing (gt-66e5d). Two files under the town's runtime directory let
// that scan answer "nothing" without opening a store while that is true:
//
//   - the stamp records when a sling context was last created. A context is
//     created in exactly one place, beads.CreateSlingContext called from
//     internal/cmd/sling_schedule.go, which touches the stamp;
//   - the scan marker records when a full scan started and whether it found a
//     context.
//
// The heartbeat skips its scan only while the marker says the last full scan
// found none, no stamp is newer than that scan's start, and the scan started
// less than SlingContextScanMaxAge ago. A stamp or marker that cannot be read
// is an error, and the caller must scan in full rather than read it as "nothing
// exists": the skip may only rest on evidence it could read.
//
// The marker is dated by the scan's start, not its end, so a context created
// while the scan was reading it is newer than the marker and the next scan sees
// it. The ceiling bounds the damage of a creation whose stamp write was lost: a
// full scan runs at least every SlingContextScanMaxAge.

const (
	// SlingContextStampFile is the file gt sling --schedule stamps when it
	// creates a sling context.
	SlingContextStampFile = "sling-context-stamp"
	// SlingContextScanMarkerFile is the file a full sling-context scan records
	// its outcome in.
	SlingContextScanMarkerFile = "sling-context-scan"

	// SlingContextScanMaxAge is how long a full scan that found no context may
	// keep the heartbeat from reading a database. It stays a constant so a
	// creation whose stamp was lost cannot be hidden longer than this without a
	// code change (TestNoNewGTEnvReads).
	SlingContextScanMaxAge = 10 * time.Minute

	// slingContextScanFound and slingContextScanEmpty are the marker's
	// content. Only slingContextScanEmpty orders a skip; content this binary
	// does not recognise is read as "found", so a marker written by another
	// version cannot hide a context this one does not understand.
	slingContextScanFound = "found"
	slingContextScanEmpty = "empty"
)

// SlingContextStampPath is the stamp file for the town at townRoot.
func SlingContextStampPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, SlingContextStampFile)
}

// SlingContextScanMarkerPath is the scan marker for the town at townRoot.
func SlingContextScanMarkerPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, SlingContextScanMarkerFile)
}

// SlingContextScan is one full scan of the town's stores, as the marker records
// it: when the scan started and whether it found a context. The zero value
// means no full scan is on record.
type SlingContextScan struct {
	StartedAt time.Time
	Found     bool
}

// slingContextStampFS is the filesystem the stamp and marker are read and
// written through, so a test can take the failure paths a writable host does
// not offer. A scan that cannot read either file must scan in full, and a
// creation whose stamp cannot be written must not leave an empty scan standing.
type slingContextStampFS struct {
	stat      func(path string) (fs.FileInfo, error)
	mkdirAll  func(path string, perm os.FileMode) error
	writeFile func(path string, data []byte, perm os.FileMode) error
	chtimes   func(path string, atime, mtime time.Time) error
	readFile  func(path string) ([]byte, error)
	remove    func(path string) error
}

var osSlingContextStampFS = slingContextStampFS{
	stat:      os.Stat,
	mkdirAll:  os.MkdirAll,
	writeFile: os.WriteFile,
	chtimes:   os.Chtimes,
	readFile:  os.ReadFile,
	remove:    os.Remove,
}

// TouchSlingContextStamp records that townRoot created a sling context at now.
func TouchSlingContextStamp(townRoot string, now time.Time) error {
	return touchSlingContextStamp(townRoot, now, osSlingContextStampFS)
}

// RecordSlingContextScan records a full scan that started at startedAt and
// found (found) sling contexts.
func RecordSlingContextScan(townRoot string, startedAt time.Time, found bool) error {
	return recordSlingContextScan(townRoot, startedAt, found, osSlingContextStampFS)
}

// SlingContextScanIsEmpty reports whether a dispatch scan may answer with no
// contexts without opening a store: a full scan found none, no context has been
// stamped since that scan started, and the scan started less than
// SlingContextScanMaxAge ago. An unreadable stamp or marker returns false and
// the error, so the caller reads every database; a missing stamp is not an
// error, because it is the ordinary state of a town that never scheduled.
func SlingContextScanIsEmpty(townRoot string, now time.Time) (bool, error) {
	return slingContextScanIsEmpty(townRoot, now, osSlingContextStampFS)
}

func slingContextStamp(townRoot string, fsys slingContextStampFS) (time.Time, error) {
	path := SlingContextStampPath(townRoot)
	fi, err := fsys.stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the sling-context stamp %s: %w", path, err)
	}
	return fi.ModTime(), nil
}

// touchSlingContextStamp is TouchSlingContextStamp through fsys.
//
// The stamp's mtime is held strictly newer than the file it replaces, as the
// dock stamp does (internal/rig/dockstamp.go): two creations inside the
// filesystem's timestamp resolution, or a clock that steps back between them,
// would otherwise melt into one stamp and the second creation would not be
// newer than a scan the first did not hide.
func touchSlingContextStamp(townRoot string, now time.Time, fsys slingContextStampFS) error {
	if err := writeSlingContextStamp(townRoot, now, fsys); err != nil {
		// The context bead already exists, so its source cannot undo it; what
		// it can do is make sure the next scan looks. An empty-scan marker that
		// outlives a stamp that was never written would hide the new context
		// until the marker ages out, so the marker goes first.
		markerPath := SlingContextScanMarkerPath(townRoot)
		if rmErr := fsys.remove(markerPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("%w (and the empty-scan marker %s could not be removed: %v)", err, markerPath, rmErr)
		}
		return err
	}
	return nil
}

func writeSlingContextStamp(townRoot string, now time.Time, fsys slingContextStampFS) error {
	path := SlingContextStampPath(townRoot)
	if err := fsys.mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the runtime directory for the sling-context stamp: %w", err)
	}
	stamp := now
	if fi, err := fsys.stat(path); err == nil && !stamp.After(fi.ModTime()) {
		stamp = fi.ModTime().Add(time.Second)
	}
	body := stamp.UTC().Format(time.RFC3339Nano) + "\n"
	if err := fsys.writeFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing the sling-context stamp %s: %w", path, err)
	}
	if err := fsys.chtimes(path, stamp, stamp); err != nil {
		return fmt.Errorf("dating the sling-context stamp %s: %w", path, err)
	}
	return nil
}

func slingContextScanMarker(townRoot string, fsys slingContextStampFS) (SlingContextScan, error) {
	path := SlingContextScanMarkerPath(townRoot)
	fi, err := fsys.stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return SlingContextScan{}, nil
	}
	if err != nil {
		return SlingContextScan{}, fmt.Errorf("reading the sling-context scan marker %s: %w", path, err)
	}
	data, err := fsys.readFile(path)
	if err != nil {
		return SlingContextScan{}, fmt.Errorf("reading the sling-context scan marker %s: %w", path, err)
	}
	return SlingContextScan{
		StartedAt: fi.ModTime(),
		Found:     strings.TrimSpace(string(data)) != slingContextScanEmpty,
	}, nil
}

func recordSlingContextScan(townRoot string, startedAt time.Time, found bool, fsys slingContextStampFS) error {
	path := SlingContextScanMarkerPath(townRoot)
	if err := fsys.mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the runtime directory for the sling-context scan marker: %w", err)
	}
	body := slingContextScanEmpty + "\n"
	if found {
		body = slingContextScanFound + "\n"
	}
	if err := fsys.writeFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing the sling-context scan marker %s: %w", path, err)
	}
	// The marker is dated by the scan's start: a creation that lands while the
	// scan reads a database then carries a newer stamp and the next scan sees
	// it, instead of waiting out the ceiling.
	if err := fsys.chtimes(path, startedAt, startedAt); err != nil {
		return fmt.Errorf("dating the sling-context scan marker %s: %w", path, err)
	}
	return nil
}

func slingContextScanIsEmpty(townRoot string, now time.Time, fsys slingContextStampFS) (bool, error) {
	marker, err := slingContextScanMarker(townRoot, fsys)
	if err != nil {
		return false, err
	}
	if marker.StartedAt.IsZero() || marker.Found {
		return false, nil
	}
	// A marker dated after now is a clock that stepped back, not fresh
	// evidence: scan until the clock passes it.
	age := now.Sub(marker.StartedAt)
	if age < 0 || age >= SlingContextScanMaxAge {
		return false, nil
	}
	stamp, err := slingContextStamp(townRoot, fsys)
	if err != nil {
		return false, err
	}
	// A creation at exactly the scan's start instant may or may not be in what
	// the scan read, so only a strictly older stamp lets the scan be skipped.
	// The zero time is older than every scan.
	return stamp.Before(marker.StartedAt), nil
}
