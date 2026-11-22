package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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
// keeping its modification time. The copy comes from an untrusted sandbox, so
// every read of it goes through an os.Root on copyDir, which refuses a path that
// leaves the copy through a link, and a file is copied out and then removed
// there, never renamed from a path the sandbox could have swapped (a rename
// across filesystems would also fail with EXDEV). A name archiveDir already holds
// is left in the copy: a session id names one transcript, and subagent file
// names are assumed unique across folders, so a second file of one name is left
// where it is and goes with the copy when the caller clears it. A copy directory
// that does not exist archives nothing; any other failure, a file it refused
// included, is returned, and the files it could move are moved.
func archiveCopiedTranscripts(copyDir, archiveDir string) error {
	return archiveCopiedTranscriptsWith(copyDir, archiveDir, moveConfined)
}

// archiveCopiedTranscriptsWith is archiveCopiedTranscripts over move, so a test
// can fail the move itself.
func archiveCopiedTranscriptsWith(copyDir, archiveDir string, move func(root *os.Root, rel, dest string) error) error {
	root, err := os.OpenRoot(copyDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening the transcript copy %s: %w", copyDir, err)
	}
	defer func() { _ = root.Close() }()
	var found []string
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("reading the transcript copy %s: %w", p, err)
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, rel := range found {
		if err := move(root, rel, filepath.Join(archiveDir, path.Base(rel))); err != nil && !errors.Is(err, fs.ErrExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// moveConfined copies the transcript rel of root to dest, keeping its mtime, and
// removes it from root. Root.Lstat, Root.Open and Root.Remove refuse a path that
// escapes the root, so a parent the sandbox swapped for a link to a directory
// outside the copy moves nothing. dest is created exclusively: a name already in
// the archive returns fs.ErrExist, which the caller reads as "already archived".
// The archive directory is made only when a file is about to be written.
func moveConfined(root *os.Root, rel, dest string) error {
	var src, out *os.File
	var mod time.Time
	steps := []func() error{
		func() error {
			info, err := root.Lstat(rel)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("is no longer a regular file")
			}
			mod = info.ModTime()
			return nil
		},
		func() (err error) { src, err = root.Open(rel); return err },
		func() error { return os.MkdirAll(filepath.Dir(dest), 0o755) },
		func() (err error) { out, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); return err },
		func() error { _, err := io.Copy(out, src); return err },
		func() error { return out.Close() },
		func() error { return os.Chtimes(dest, mod, mod) },
		func() error { return root.Remove(rel) },
	}
	defer func() {
		if src != nil {
			_ = src.Close()
		}
	}()
	for _, step := range steps {
		if err := step(); err != nil {
			if out != nil && !errors.Is(err, fs.ErrExist) {
				_ = out.Close()
				_ = os.Remove(dest)
			}
			return fmt.Errorf("moving %s into the transcript archive: %w", rel, err)
		}
	}
	return nil
}
