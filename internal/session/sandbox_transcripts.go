package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/constants"
)

// A sandbox seat's Claude sessions write their transcripts inside the sandbox,
// and the sandbox is deleted when its session stops. Before it is deleted, its
// projects directory is copied to the town, where the seat end reads the seat's
// transcripts for its usage record.

// sandboxProjectsDir is the Claude projects directory inside a sandbox.
const sandboxProjectsDir = "/sandbox/.claude/projects"

// sandboxDownloadTimeout bounds the copy a delete makes of a sandbox's projects.
const sandboxDownloadTimeout = 2 * time.Minute

// sandboxTranscriptCap is the most a seat's transcript copy keeps, in bytes.
const sandboxTranscriptCap int64 = 64 << 20

// sandboxTranscriptKeep is how long a seat's copy is kept. A copy is rewritten
// when its seat's sandbox is deleted, so its age is the seat's end.
const sandboxTranscriptKeep = 7 * 24 * time.Hour

// SandboxTranscriptCopyDir is the directory a seat's copied projects directory
// is downloaded into: <town>/.runtime/usage/transcripts/<rig>/<polecat>.
func SandboxTranscriptCopyDir(townRoot, rig, polecat string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "usage", "transcripts", rig, polecat)
}

// SandboxTranscriptFolder is the project folder Claude Code names for a seat's
// sandbox clone, under a copied projects directory: the folder a session run in
// /sandbox/gt/<rig>/polecats/<polecat>/<rig> writes its transcripts to.
func SandboxTranscriptFolder(rig, polecat string) string {
	return agentlog.ProjectDirName(path.Join(sandboxTownRoot, rig, "polecats", polecat, rig))
}

// CopiedSandboxTranscripts lists the transcripts a seat's copy holds under the
// seat's sandbox project folder, as paths in the copy. A missing copy holds none;
// a copy that cannot be read is an error, not an empty list, and a seat whose
// names are not plain names has no copy to name.
func CopiedSandboxTranscripts(townRoot, rig, polecat string) ([]string, error) {
	if !plainPathName(rig) || !plainPathName(polecat) {
		return nil, fmt.Errorf("seat %q/%q names no copy: its rig or polecat is not a plain name", rig, polecat)
	}
	dir := filepath.Join(SandboxTranscriptCopyDir(townRoot, rig, polecat), SandboxTranscriptFolder(rig, polecat))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the copied transcripts %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths, nil
}

// copySeatTranscripts copies the projects directory of rec's sandbox into the
// seat's copy directory, before the sandbox is deleted, and prunes the copies
// past their keep first. It is best effort: a failed or missing copy warns once,
// naming the sandbox, and the delete goes on. A record that names no plain rig
// and polecat has no seat to copy for.
func (b *openshellBackend) copySeatTranscripts(rec SandboxRecord) {
	if !plainPathName(rec.Rig) || !plainPathName(rec.Polecat) {
		return
	}
	dest := SandboxTranscriptCopyDir(b.townRoot, rec.Rig, rec.Polecat)
	if err := pruneTranscriptCopies(filepath.Dir(filepath.Dir(dest)), time.Now()); err != nil {
		b.warn("sandbox %s: pruning old transcript copies failed: %v", rec.Session, err)
	}
	if err := os.RemoveAll(dest); err != nil {
		b.warn("sandbox %s: clearing its transcript copy failed, so it is not copied: %v", rec.Session, err)
		return
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		b.warn("sandbox %s: creating its transcript copy failed, so it is not copied: %v", rec.Session, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxDownloadTimeout)
	defer cancel()
	if err := b.cli.Download(ctx, rec.Session, sandboxProjectsDir, dest); err != nil {
		// A partial copy would read as the seat's whole transcript set.
		err = errors.Join(err, os.RemoveAll(dest))
		b.warn("sandbox %s: copying its transcripts out failed, so its seat end cannot read them: %v", rec.Session, err)
		return
	}
	b.capTranscriptCopy(rec.Session, dest)
}

// capTranscriptCopy keeps the newest files of dest, by modification time, while
// they fit under the cap, and removes the rest. The copy is cut at the cap, and
// the warning says so.
func (b *openshellBackend) capTranscriptCopy(session, dest string) {
	limit := b.transcriptCap
	if limit <= 0 {
		limit = sandboxTranscriptCap
	}
	type copied struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []copied
	var total int64
	err := filepath.Walk(dest, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		files = append(files, copied{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		err = errors.Join(err, os.RemoveAll(dest))
		b.warn("sandbox %s: reading its transcript copy failed, so it is dropped: %v", session, err)
		return
	}
	if total <= limit {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	var kept int64
	cut := len(files)
	for i, f := range files {
		if kept+f.size > limit {
			cut = i
			break
		}
		kept += f.size
	}
	for _, f := range files[cut:] {
		if err := os.Remove(f.path); err != nil {
			b.warn("sandbox %s: removing a transcript past the copy cap failed: %v", session, err)
		}
	}
	b.warn("sandbox %s: its transcript copy stopped at the %d-byte cap, so %d older files were left out", session, limit, len(files)-cut)
}

// pruneTranscriptCopies removes the seat copies under root (the transcripts
// directory, laid out as <rig>/<polecat>) that have not changed for
// sandboxTranscriptKeep. A missing root holds none.
func pruneTranscriptCopies(root string, now time.Time) error {
	rigs, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", root, err)
	}
	var errs []error
	for _, rig := range rigs {
		if !rig.IsDir() {
			continue
		}
		seats, err := os.ReadDir(filepath.Join(root, rig.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, seat := range seats {
			p := filepath.Join(root, rig.Name(), seat.Name())
			info, err := os.Stat(p)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if now.Sub(info.ModTime()) <= sandboxTranscriptKeep {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
