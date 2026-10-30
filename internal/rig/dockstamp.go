package rig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// The dock stamp is how `gt rig dock` and `gt rig undock` tell a running daemon
// that the identity bead changed, without a bead write of its own.
//
// The daemon memoizes each rig's identity-bead read for rigOperationalCacheTTL
// (internal/daemon/rig_status.go), and that window is longer than its heartbeat,
// so without a nudge a dock would go unnoticed for the whole window. The stamp
// is a file both sides can reach: the command touches it after the label write,
// and the daemon compares its mtime with the one its memoized read recorded. A
// newer stamp drops the memo, so a dock through gt costs one extra bead read
// rather than five minutes of a daemon that still believes the rig is active.
//
// mtime rather than file content on purpose: it is a stat, not a read, on a path
// inside the same town the daemon already serves. Only this command writes it,
// so a stamp that cannot be read is an error the daemon must not read as "no
// dock happened".

// DockStampFile is the file gt rig dock and gt rig undock stamp.
const DockStampFile = "dock-stamp"

// dockStampDirs is the per-rig runtime directory the stamp lives in, under the
// town's .runtime (gitignored, and local to the town the daemon runs in).
const dockStampDirs = "rigs"

// DockStampPath is the stamp file for rigName in townRoot.
func DockStampPath(townRoot, rigName string) string {
	return filepath.Join(townRoot, constants.DirRuntime, dockStampDirs, rigName, DockStampFile)
}

// DockStamp reports when rigName's dock state was last stamped by gt, or the
// zero time when it never has. A stamp that exists but cannot be stat'd is an
// error: the caller must not read a failed stat as "no dock happened".
func DockStamp(townRoot, rigName string) (time.Time, error) {
	fi, err := os.Stat(DockStampPath(townRoot, rigName))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the dock stamp for rig %s: %w", rigName, err)
	}
	return fi.ModTime(), nil
}

// TouchDockStamp records that rigName was just docked or undocked, at now.
//
// The stamp's mtime is held strictly newer than the file it replaces. The daemon
// drops a memo only for a stamp newer than the one it recorded, so two docks
// inside the filesystem's timestamp resolution - or a clock that steps back
// between them - would otherwise melt into one, and the second would be
// invisible to a daemon that had already read the bead between them. The step is
// a second rather than a nanosecond so it survives a filesystem that stores
// whole seconds.
func TouchDockStamp(townRoot, rigName string, now time.Time) error {
	path := DockStampPath(townRoot, rigName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the runtime directory for rig %s: %w", rigName, err)
	}
	stamp := now
	if fi, err := os.Stat(path); err == nil && !stamp.After(fi.ModTime()) {
		stamp = fi.ModTime().Add(time.Second)
	}
	if err := os.WriteFile(path, []byte(stamp.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing the dock stamp for rig %s: %w", rigName, err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		return fmt.Errorf("stamping the dock file for rig %s: %w", rigName, err)
	}
	return nil
}
