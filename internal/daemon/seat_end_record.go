package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
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
