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
	// The walk only lists: a rename inside its callback would act on a path the
	// sandbox could swap for a link between the listing and the move (gosec G122).
	var found []string
	err := filepath.WalkDir(copyDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("reading the transcript copy %s: %w", p, err)
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
			found = append(found, p)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, p := range found {
		dest := filepath.Join(archiveDir, filepath.Base(p))
		if _, err := os.Lstat(dest); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("checking the transcript archive %s: %w", dest, err)
		}
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return fmt.Errorf("creating the transcript archive %s: %w", archiveDir, err)
		}
		if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
			continue // gone, or swapped for a link since the listing
		}
		if err := os.Rename(p, dest); err != nil {
			return fmt.Errorf("moving %s into the transcript archive: %w", p, err)
		}
	}
	return nil
}
