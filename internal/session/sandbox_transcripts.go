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
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/constants"
)

// A sandbox seat's Claude sessions write their transcripts inside the sandbox,
// and the sandbox is deleted when its session stops. Before it is deleted, the
// transcripts its seat wrote are copied to the town, where the seat end reads
// them for its usage record.

// sandboxProjectsDir is the Claude projects directory inside a sandbox.
const sandboxProjectsDir = "/sandbox/.claude/projects"

// sandboxDownloadTimeout bounds the copy a delete makes of a sandbox's transcripts.
const sandboxDownloadTimeout = 2 * time.Minute

// sandboxTranscriptCap is the most a seat's transcript copy keeps, in bytes.
const sandboxTranscriptCap int64 = 64 << 20

// sandboxTranscriptKeep is how long a seat's copy is kept. A copy is rewritten
// when its seat's sandbox is deleted, so its age is the seat's end.
const sandboxTranscriptKeep = 7 * 24 * time.Hour

// sandboxTranscriptMarker names the file at the root of a copy that the cap cut
// short. It holds how many transcripts the cap left out.
const sandboxTranscriptMarker = "skipped-by-cap"

// sandboxListing prints "<mtime> <size> <path>" for each transcript under the
// projects directory. find does not follow symlinks, and -type f skips them, so
// a link in the sandbox is never copied.
var sandboxListing = []string{"find", sandboxProjectsDir, "-type", "f", "-name", "*.jsonl", "-printf", `%T@ %s %p\n`}

// sandboxTranscript is one transcript the sandbox listing names.
type sandboxTranscript struct {
	remote string    // the file's path inside the sandbox
	rel    string    // its path under the projects directory, slash-separated
	size   int64     // bytes
	mtime  time.Time // the sandbox's modification time, which the copy keeps
}

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
// seat's sandbox project folder, as paths in the copy, and how many transcripts
// the cap left out of it. Only regular files are listed, since the copy comes
// from an untrusted sandbox. A missing copy holds none; a copy that cannot be
// read is an error, not an empty list, and a seat whose names are not plain
// names has no copy to name.
func CopiedSandboxTranscripts(townRoot, rig, polecat string) ([]string, int, error) {
	if !plainPathName(rig) || !plainPathName(polecat) {
		return nil, 0, fmt.Errorf("seat %q/%q names no copy: its rig or polecat is not a plain name", rig, polecat)
	}
	copyDir := SandboxTranscriptCopyDir(townRoot, rig, polecat)
	skipped, err := readSkippedByCap(copyDir)
	if err != nil {
		return nil, 0, err
	}
	dir := filepath.Join(copyDir, SandboxTranscriptFolder(rig, polecat))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, skipped, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the copied transcripts %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths, skipped, nil
}

// readSkippedByCap reads the cap record at the root of a copy: how many of the
// seat's transcripts the cap left out. A copy with no record was not cut short.
// A record that is not a count is an error, not a copy that was not cut.
func readSkippedByCap(copyDir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(copyDir, sandboxTranscriptMarker))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the cap record of the copied transcripts: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 1 {
		return 0, errors.New("the cap record of the copied transcripts is not a count of the files left out")
	}
	return n, nil
}

// copySeatTranscripts copies the transcripts rec's seat wrote into the seat's
// copy directory, before the sandbox is deleted, and prunes the copies past their
// keep first. It is best effort: a failed copy warns once, naming the sandbox, and
// the delete goes on. A record that names no plain rig and polecat has no seat to
// copy for.
func (b *openshellBackend) copySeatTranscripts(rec SandboxRecord) {
	if !plainPathName(rec.Rig) || !plainPathName(rec.Polecat) {
		return
	}
	dest := SandboxTranscriptCopyDir(b.townRoot, rec.Rig, rec.Polecat)
	if err := pruneTranscriptCopies(filepath.Dir(filepath.Dir(dest)), b.clock()); err != nil {
		b.warn("sandbox %s: pruning old transcript copies failed: %v", rec.Session, err)
	}
	if err := os.RemoveAll(dest); err != nil {
		b.warn("sandbox %s: clearing its transcript copy failed, so it is not copied: %v", rec.Session, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxDownloadTimeout)
	defer cancel()
	listed, err := b.listSeatTranscripts(ctx, rec)
	if isSandboxNotFound(err) {
		// A sandbox that is already gone has no transcripts to copy: its delete goes on.
		return
	}
	if err != nil {
		b.warn("sandbox %s: copying its transcripts out failed, so its seat end cannot read them: %v", rec.Session, err)
		return
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		b.warn("sandbox %s: creating its transcript copy failed, so it is not copied: %v", rec.Session, err)
		return
	}
	skipped, err := b.downloadSeatTranscripts(ctx, rec, listed, dest)
	if err != nil {
		// A partial copy would read as the seat's whole transcript set.
		err = errors.Join(err, os.RemoveAll(dest))
		b.warn("sandbox %s: copying its transcripts out failed, so its seat end cannot read them: %v", rec.Session, err)
		return
	}
	if skipped > 0 {
		b.warn("sandbox %s: its transcript copy stopped at the %d-byte cap, so %d older files were left out", rec.Session, b.capLimit(), skipped)
	}
}

// capLimit is the most a seat's transcript copy keeps, in bytes.
func (b *openshellBackend) capLimit() int64 {
	if b.transcriptCap > 0 {
		return b.transcriptCap
	}
	return sandboxTranscriptCap
}

// clock is the time the copies are pruned against: the test clock when one is
// set, else the wall clock.
func (b *openshellBackend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// listSeatTranscripts lists the transcripts of rec's seat inside its sandbox:
// only those under the seat's own project folder, so another folder's files can
// neither use up the cap nor be counted as left out by it. A listing that fails,
// or does not parse, is an error: a missing line would be a silently partial copy.
func (b *openshellBackend) listSeatTranscripts(ctx context.Context, rec SandboxRecord) ([]sandboxTranscript, error) {
	out, _, err := b.cli.Exec(ctx, rec.Session, nil, sandboxListing, nil)
	if err != nil {
		return nil, fmt.Errorf("listing its transcripts: %w", err)
	}
	all, err := parseSandboxListing(string(out))
	if err != nil {
		return nil, err
	}
	return seatFolderTranscripts(all, SandboxTranscriptFolder(rec.Rig, rec.Polecat)), nil
}

// seatFolderTranscripts keeps the listed transcripts under folder, the seat's
// project folder under the projects directory, its subagents' subfolders
// included.
func seatFolderTranscripts(files []sandboxTranscript, folder string) []sandboxTranscript {
	prefix := folder + "/"
	var own []sandboxTranscript
	for _, f := range files {
		if strings.HasPrefix(f.rel, prefix) {
			own = append(own, f)
		}
	}
	return own
}

// downloadSeatTranscripts downloads the listed transcripts rec's seat wrote into
// dest, one file per download, so nothing past the cap is ever written. Each file
// keeps its sandbox modification time, which the seat end reads to tell the seat's
// own transcripts from its predecessor's. It returns how many of the seat's
// transcripts the cap left out, and records that count at the root of dest.
func (b *openshellBackend) downloadSeatTranscripts(ctx context.Context, rec SandboxRecord, files []sandboxTranscript, dest string) (int, error) {
	keep, skipped := selectSandboxTranscripts(files, rec.CreatedAt, b.capLimit())
	for _, f := range keep {
		local := filepath.Join(dest, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return 0, err
		}
		if err := b.cli.Download(ctx, rec.Session, f.remote, filepath.Dir(local)); err != nil {
			return 0, err
		}
		// A download that wrote no file where it was asked to fails here, not later
		// as a seat that reads a partial copy.
		if err := os.Chtimes(local, f.mtime, f.mtime); err != nil {
			return 0, fmt.Errorf("keeping the sandbox mtime of a copied transcript: %w", err)
		}
	}
	if skipped > 0 {
		marker := filepath.Join(dest, sandboxTranscriptMarker)
		if err := os.WriteFile(marker, []byte(strconv.Itoa(skipped)+"\n"), 0o644); err != nil {
			return 0, fmt.Errorf("recording the transcripts the cap left out: %w", err)
		}
	}
	return skipped, nil
}

// parseSandboxListing reads the sandbox listing: one "<mtime> <size> <path>" line
// per transcript under the projects directory. Output that does not parse is an
// error, not a shorter list, since a missing line would be a silently partial copy.
func parseSandboxListing(out string) ([]sandboxTranscript, error) {
	var files []sandboxTranscript
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// The listing is captured sandbox output, so an error names the field, not its value.
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			return nil, errors.New(`sandbox transcript listing has a line that is not "<mtime> <size> <path>"`)
		}
		mtime, err := parseListingTime(fields[0])
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 {
			return nil, errors.New("sandbox transcript listing has a size that is not a byte count")
		}
		rel, ok := strings.CutPrefix(fields[2], sandboxProjectsDir+"/")
		if !ok || !plainTranscriptPath(rel) {
			return nil, fmt.Errorf("sandbox transcript listing has a path that is not a transcript under %s", sandboxProjectsDir)
		}
		files = append(files, sandboxTranscript{remote: fields[2], rel: rel, size: size, mtime: mtime})
	}
	return files, nil
}

// parseListingTime reads find's %T@ stamp, "<seconds>.<fraction>", to the nanosecond.
func parseListingTime(s string) (time.Time, error) {
	bad := errors.New("sandbox transcript listing has an mtime that is not a time")
	secs, frac, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, bad
	}
	var nsec int64
	if frac != "" {
		nsec, err = strconv.ParseInt((frac + "000000000")[:9], 10, 64)
		if err != nil {
			return time.Time{}, bad
		}
	}
	return time.Unix(sec, nsec), nil
}

// plainTranscriptPath is a path under the projects directory that names a
// transcript in a project folder: each element a plain name, the last a .jsonl.
// The sandbox chooses these names, so none may step out of the copy.
func plainTranscriptPath(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || !strings.HasSuffix(parts[len(parts)-1], ".jsonl") {
		return false
	}
	for _, p := range parts {
		if !plainPathName(p) {
			return false
		}
	}
	return true
}

// selectSandboxTranscripts keeps the transcripts a seat wrote: those last written
// at or after since (all of them when since is zero), newest first, while they fit
// under limit. It returns them with how many the limit left out. The cut is always
// of the oldest, and a transcript larger than the limit alone is left out at once.
func selectSandboxTranscripts(files []sandboxTranscript, since time.Time, limit int64) ([]sandboxTranscript, int) {
	var own []sandboxTranscript
	for _, f := range files {
		if since.IsZero() || !f.mtime.Before(since) {
			own = append(own, f)
		}
	}
	sort.SliceStable(own, func(i, j int) bool { return own[i].mtime.After(own[j].mtime) })
	var kept []sandboxTranscript
	var total int64
	for i, f := range own {
		// total never exceeds limit, so limit-total cannot overflow, while total+f.size
		// would wrap for a size the sandbox reported near the top of int64.
		if f.size > limit-total {
			return kept, len(own) - i
		}
		total += f.size
		kept = append(kept, f)
	}
	return kept, 0
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
