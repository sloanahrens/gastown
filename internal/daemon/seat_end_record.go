package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/seatship"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// readSeatEndRecord returns the newest SEAT-END comment on bead that names rig,
// polecat and bead, found false when none; other comments skipped, read errors returned.
func readSeatEndRecord(client beads.Client, rig, polecat, bead string) (seatship.Payload, bool, error) {
	comments, err := client.Comments(bead)
	if err != nil {
		return seatship.Payload{}, false, err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		p, found, err := seatship.Decode(comments[i].Text)
		if err != nil || !found || p.Rig != rig || p.Polecat != polecat || p.Bead != bead {
			continue
		}
		return p, true, nil
	}
	return seatship.Payload{}, false, nil
}

// recordOfSeat reports whether p can be the record of the seat that started at
// start: its usage must end after that start. A SEAT-END comment names its seat
// only by rig, polecat and bead, and a pool seat takes a later task under those
// same three, so a predecessor's comment on the bead would otherwise pass for
// this seat's and skip the pull that holds this seat's transcripts. An unknown
// start is the zero time, which denies the record, and the pull runs (gt-g73e2q).
func recordOfSeat(p seatship.Payload, start time.Time) bool {
	return !start.IsZero() && p.Usage != nil && p.Usage.End.After(start)
}

// storeSeatEndRecord files a finished seat's usage record with its bead's
// workspace, and its outbox where a seat end reads one
// (session.SandboxOutboxCopyDir). It returns the record file's path, set when
// that file was written. Each name the payload carries is checked before use.
func storeSeatEndRecord(ctx context.Context, store usageRecordStore, townRoot string, p seatship.Payload) (string, error) {
	return storeSeatEndRecordWith(ctx, store, townRoot, p, seatEndOutboxOps{os.RemoveAll, os.MkdirAll, os.WriteFile})
}

// seatEndOutboxOps is the filesystem the outbox copy is written through, so a
// test can take each failure path.
type seatEndOutboxOps struct {
	removeAll func(string) error
	mkdirAll  func(string, os.FileMode) error
	writeFile func(string, []byte, os.FileMode) error
}

func storeSeatEndRecordWith(ctx context.Context, store usageRecordStore, townRoot string, p seatship.Payload, ops seatEndOutboxOps) (string, error) {
	if p.Usage == nil || p.Usage.Bead != p.Bead {
		return "", fmt.Errorf("seat end record for %s: it carries no usage record of its own", p.Bead)
	}
	dir := session.SandboxOutboxCopyDir(townRoot, p.Rig, p.Polecat)
	if filepath.Dir(filepath.Dir(dir)) != filepath.Join(constants.TownRuntimePath(townRoot), "outbox") {
		return "", fmt.Errorf("seat end record for %s: rig %q and polecat %q name no seat directory", p.Bead, p.Rig, p.Polecat)
	}
	for name := range p.Outbox {
		if name != seatOutboxResultFile && name != seatOutboxNotesFile {
			return "", fmt.Errorf("seat end record for %s: outbox names %q, not %s or %s", p.Bead, name, seatOutboxResultFile, seatOutboxNotesFile)
		}
	}
	workspace, err := usageattrib.WorkspaceOf(townRoot, p.Bead)
	if err != nil {
		return "", err
	}
	p.Usage.Workspace = workspace
	usageDir := filepath.Join(constants.TownRuntimePath(townRoot), "usage")
	path := usageattrib.WorkRecordPath(usageDir, p.Bead)
	if err := writeUsageRecord(ctx, store, usageDir, *p.Usage, p.SessionIDs); err != nil {
		var rowOnly *usageRowNotWrittenError
		if !errors.As(err, &rowOnly) {
			path = "" // the file is not there to report
		}
		return path, err
	}
	// A stale copy would read as this seat's, so it goes even when this seat wrote none.
	if err := ops.removeAll(dir); err != nil {
		return path, fmt.Errorf("seat end record for %s: clearing its outbox copy: %w", p.Bead, err)
	}
	if len(p.Outbox) == 0 {
		return path, nil
	}
	if err := ops.mkdirAll(dir, 0o755); err != nil {
		return path, fmt.Errorf("seat end record for %s: creating its outbox copy: %w", p.Bead, err)
	}
	for name, body := range p.Outbox {
		if err := ops.writeFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			// A partial copy would read as the seat's whole outbox.
			return path, errors.Join(fmt.Errorf("seat end record for %s: writing outbox %s: %w", p.Bead, name, err), ops.removeAll(dir))
		}
	}
	return path, nil
}

// seatEndNoRecord is one seat's marker that its end found no record of its work.
// The path is the seat's identity, so only the bead is in the file: a reader
// can match that bead's SEAT-END comment to the seat that lost it (gt-gnxclz).
type seatEndNoRecord struct {
	Bead    string `json:"bead"`
	Rig     string `json:"-"`
	Polecat string `json:"-"`
	path    string
}

// seatEndNoRecordsDir groups the markers by rig, so a reader walks one rig's
// seats without listing the whole town.
func seatEndNoRecordsDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "seat-end-no-record")
}

// seatEndNoRecordPath names one seat's marker, refusing a rig or polecat name a
// path cannot hold.
func seatEndNoRecordPath(townRoot, rig, name string) (string, error) {
	if err := polecat.CheckSandboxSeatNames(rig, name); err != nil {
		return "", err
	}
	return filepath.Join(seatEndNoRecordsDir(townRoot), rig, name+".json"), nil
}

// recordSeatEndNoRecord marks that the seat's end found neither its SEAT-END
// comment nor a transcript copy. A marker that names no bead could never be
// cleared by one, so it is refused (gt-gnxclz).
func recordSeatEndNoRecord(townRoot, rig, name, bead string) error {
	if bead == "" {
		return fmt.Errorf("seat end no record for %s/%s: it names no bead to clear it by", rig, name)
	}
	path, err := seatEndNoRecordPath(townRoot, rig, name)
	if err != nil {
		return err
	}
	return atomicfile.EnsureDirAndWriteJSON(path, seatEndNoRecord{Bead: bead})
}

// readSeatEndNoRecords returns every seat's marker that its end found no record
// of its work, one per seat. The path names the seat, so each marker carries
// its rig, polecat and file. Only *.json files are markers, so an atomic
// write's temp file is skipped; a marker the read cannot use is an error naming
// its file, and no marker is returned with it, so a caller never acts on a
// half-read store (gt-wssxhd).
func readSeatEndNoRecords(townRoot string) ([]seatEndNoRecord, error) {
	dir := seatEndNoRecordsDir(townRoot)
	rigs, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var markers []seatEndNoRecord
	var bad []error
	for _, rigEntry := range rigs {
		rigPath := filepath.Join(dir, rigEntry.Name())
		if !rigEntry.IsDir() {
			bad = append(bad, fmt.Errorf("seat end no record marker %s: it is not under a rig", rigPath))
			continue
		}
		entries, err := os.ReadDir(rigPath)
		if err != nil {
			bad = append(bad, fmt.Errorf("seat end no record markers in %s: %w", rigPath, err))
			continue
		}
		for _, entry := range entries {
			name, isMarker := strings.CutSuffix(entry.Name(), ".json")
			if !isMarker {
				continue // an atomic write's temp file, or something that is not a marker
			}
			path := filepath.Join(rigPath, entry.Name())
			if err := polecat.CheckSandboxSeatNames(rigEntry.Name(), name); err != nil {
				bad = append(bad, fmt.Errorf("seat end no record marker %s: %w", path, err))
				continue
			}
			marker, err := readSeatEndNoRecord(path)
			if err != nil {
				bad = append(bad, err)
				continue
			}
			marker.Rig, marker.Polecat, marker.path = rigEntry.Name(), name, path
			markers = append(markers, marker)
		}
	}
	if err := errors.Join(bad...); err != nil {
		return nil, err
	}
	return markers, nil
}

// readSeatEndNoRecord reads one marker. A file that will not read or decode,
// or a marker that names no bead, is an error naming the file: the writer
// refuses to make such a marker, so it can only be a corrupt one.
func readSeatEndNoRecord(path string) (seatEndNoRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the town's own runtime path
	if err != nil {
		return seatEndNoRecord{}, fmt.Errorf("seat end no record marker %s: %w", path, err)
	}
	var marker seatEndNoRecord
	if err := json.Unmarshal(data, &marker); err != nil {
		return seatEndNoRecord{}, fmt.Errorf("seat end no record marker %s: %w", path, err)
	}
	if marker.Bead == "" {
		return seatEndNoRecord{}, fmt.Errorf("seat end no record marker %s: it names no bead", path)
	}
	return marker, nil
}
