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
	"syscall"
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
// every access to it goes through an os.Root on copyDir, which refuses a path
// that leaves the copy through a link, and a file is copied out and then removed
// there, never renamed from a path the sandbox could have swapped (a rename
// across filesystems would also fail with EXDEV). A name archiveDir already
// holds before this run is left in the copy as already archived. Two files of
// one base name in different folders of the copy are an error naming both: the
// second stays where it is, so a caller that clears the copy after a nil return
// never loses a transcript unreported. A copy directory that does not exist
// archives nothing; any other failure, a file it refused included, is returned,
// and the files it could move are moved.
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
	found, errs := listTranscripts(root.FS())
	moved := map[string]string{}
	for _, rel := range found {
		name := path.Base(rel)
		if first, dup := moved[name]; dup {
			errs = append(errs, fmt.Errorf("%s and %s have the same name %s: only the first was archived and the second stays in the copy", first, rel, name))
			continue
		}
		err := move(root, rel, filepath.Join(archiveDir, name))
		switch {
		case err == nil:
			moved[name] = rel
		case !errors.Is(err, fs.ErrExist):
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// moveConfined copies the transcript rel of root to dest, keeping its mtime, and
// removes it from root: see moveConfinedWith.
func moveConfined(root *os.Root, rel, dest string) error {
	return moveConfinedWith(root, rel, dest, nil)
}

// moveConfinedWith is moveConfined with a hook called before each step with the
// step's name and the source handle opened so far (nil before "open"); an error it returns is that step's failure, and it may change the
// copy first (a test swaps a leaf for a FIFO at "open"). The steps are: lstat,
// exists, open, fstat, mkdir, create, copy, close, chtimes, link, unpartial,
// remove. Root.Lstat, Root.OpenFile and Root.Remove refuse a path that escapes
// the root, so a parent the sandbox swapped for a link outside the copy moves
// nothing. The leaf is opened non-blocking and the opened handle is checked to
// be a regular file and to be the file the name still is, so a leaf swapped for a FIFO after the Lstat cannot hang
// the seat end. The bytes are written to dest+".partial" (a name nothing lists),
// the mtime is set, and the file is linked to dest, which fails with fs.ErrExist
// when dest exists: a name already archived reads as "already archived", and a
// crash mid-copy leaves a .partial the next run clears, never a truncated dest.
// Whatever fails, the source stays in the copy and the partial is removed; a
// failure after the link leaves a complete dest beside the source.
func moveConfinedWith(root *os.Root, rel, dest string, hook func(step string, src *os.File) error) error {
	partial := dest + ".partial"
	var src, out *os.File
	var mod time.Time
	steps := []struct {
		name string
		do   func() error
	}{
		{"lstat", func() error {
			info, err := root.Lstat(rel)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("is no longer a regular file")
			}
			mod = info.ModTime()
			return nil
		}},
		{"exists", func() error {
			if _, err := os.Lstat(dest); err == nil {
				return fs.ErrExist
			}
			return nil
		}},
		{"open", func() (err error) {
			src, err = root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			return err
		}},
		{"fstat", func() error {
			info, err := src.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("is not a regular file when opened")
			}
			// Root.OpenFile follows a link that stays inside the copy whatever the
			// flags say, so the opened file must also be what the name is: a leaf
			// swapped for a link is read as its target otherwise.
			now, err := root.Lstat(rel)
			if err != nil {
				return err
			}
			if !os.SameFile(info, now) {
				return errors.New("is a link or was replaced while it was opened")
			}
			return nil
		}},
		{"mkdir", func() error { return os.MkdirAll(filepath.Dir(dest), 0o755) }},
		{"create", func() (err error) {
			if err = os.Remove(partial); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			out, err = os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			return err
		}},
		{"copy", func() error { _, err := io.Copy(out, src); return err }},
		{"close", func() error { err := out.Close(); out = nil; return err }},
		{"chtimes", func() error { return os.Chtimes(partial, mod, mod) }},
		{"link", func() error { return os.Link(partial, dest) }},
		{"unpartial", func() error { return os.Remove(partial) }},
		{"remove", func() error { return root.Remove(rel) }},
	}
	defer func() {
		if src != nil {
			_ = src.Close()
		}
		if out != nil {
			_ = out.Close()
		}
	}()
	for _, st := range steps {
		err := error(nil)
		if hook != nil {
			err = hook(st.name, src)
		}
		if err == nil {
			err = st.do()
		}
		if err != nil {
			_ = os.Remove(partial)
			if errors.Is(err, fs.ErrExist) {
				return err
			}
			return fmt.Errorf("moving %s into the transcript archive (%s): %w", rel, st.name, err)
		}
	}
	return nil
}

// listTranscripts lists the regular .jsonl files of fsys, slash-separated. A
// directory it cannot read is reported, naming it, and skipped: the transcripts
// beside it are still listed, and a vanished subdirectory is an error, not the
// "missing copy" that archives nothing and lets the caller clear the copy.
func listTranscripts(fsys fs.FS) (found []string, errs []error) {
	// The callback never returns an error other than SkipDir, which WalkDir does not
	// pass on, so WalkDir's own result is always nil and is not read.
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the transcript copy %s: %w", p, err))
			return fs.SkipDir
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
			found = append(found, p)
		}
		return nil
	})
	return found, errs
}
