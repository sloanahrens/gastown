package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// A seat's name is reused and each new copy clears the last, so its earlier
// sessions survive only in the archive its copy is moved into.

// SandboxTranscriptArchiveDir is where a seat's earlier sessions are archived:
// <town>/.runtime/usage/transcript-archive/<rig>/<polecat>.
func SandboxTranscriptArchiveDir(townRoot, rig, polecat string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "usage", "transcript-archive", rig, polecat)
}

// archiveCopiedTranscripts moves every regular .jsonl file under copyDir, a
// subagent folder's included, into archiveDir, flat under its own base name and
// keeping its modification time. A name archiveDir already holds is left in the
// copy, since a session id names one transcript; a missing copy archives
// nothing, and a path it cannot read or make is an error.
func archiveCopiedTranscripts(copyDir, archiveDir string) error {
	err := filepath.WalkDir(copyDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("reading the transcript copy %s: %w", p, err)
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		dest := filepath.Join(archiveDir, d.Name())
		if _, err := os.Lstat(dest); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("checking the transcript archive %s: %w", dest, err)
		}
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return fmt.Errorf("creating the transcript archive %s: %w", archiveDir, err)
		}
		return os.Rename(p, dest)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
