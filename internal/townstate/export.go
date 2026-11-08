package townstate

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SkipClass names a class of entries an export leaves out of the archive. The
// summary counts the classes instead of naming files, so a seed run says what
// the copy does not carry without printing the town's contents.
type SkipClass string

const (
	// SkipDoltData is a Dolt data directory; Dolt has its own backup path and
	// is not part of the town-state copy.
	SkipDoltData SkipClass = "Dolt data"
	// SkipGitTree is a .git directory; the import's follow-up step re-fetches
	// the rig clones from Forgejo.
	SkipGitTree SkipClass = "git trees"
	// SkipSocket is a unix socket, which a copy cannot recreate.
	SkipSocket SkipClass = "sockets"
	// SkipLockFile is a flock file, which a copy must not carry.
	SkipLockFile SkipClass = "lock files"
	// SkipNotRegular is a device, fifo or other entry that is neither a
	// regular file nor a directory.
	SkipNotRegular SkipClass = "files that are neither regular nor directories"
	// SkipSymlinkOut is a symlink whose target leaves the town root; it is
	// never followed and never archived.
	SkipSymlinkOut SkipClass = "symlinks leaving the town root"
	// SkipOlderThan is a regular file older than the --since time.
	SkipOlderThan SkipClass = "files older than --since"
)

// skipClassOrder is the order the summary prints the classes in, so two runs
// over the same town print the same line.
var skipClassOrder = []SkipClass{
	SkipDoltData, SkipGitTree, SkipSocket, SkipLockFile, SkipNotRegular, SkipSymlinkOut, SkipOlderThan,
}

// ExportSummary counts what an export put in the archive and what it left out.
type ExportSummary struct {
	// Entries are the daemon-owned manifest rows the export walked, in
	// manifest order. They are top-level entry names, never the files below.
	Entries  []string
	Files    int
	Dirs     int
	Symlinks int
	// Bytes is the sum of the regular files' sizes as the archive carries them.
	Bytes int64
	// Skipped counts the entries left out, by class.
	Skipped map[SkipClass]int
}

// String renders the summary for the export command's standard error.
func (s ExportSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "town state export: %d files, %d directories, %d symlinks, %d bytes",
		s.Files, s.Dirs, s.Symlinks, s.Bytes)
	if len(s.Entries) > 0 {
		fmt.Fprintf(&b, "\nentries: %s", strings.Join(s.Entries, ", "))
	}
	b.WriteString("\nskipped:")
	printed := false
	for _, class := range skipClassOrder {
		if s.Skipped[class] == 0 {
			continue
		}
		fmt.Fprintf(&b, " %d %s", s.Skipped[class], class)
		if class == SkipGitTree {
			b.WriteString(" (the rig clones are re-fetched from Forgejo after the import)")
		}
		printed = true
	}
	if !printed {
		b.WriteString(" none")
	}
	return b.String()
}

// Export writes the daemon-owned set of the town at root to w as a tar stream
// named relative to root, and returns what it wrote. Only manifest rows owned
// by the daemon are walked; a session-owned path never enters the archive.
//
// The walk never follows a symlink: a link whose relative target stays inside
// the town root is archived as a link, and a link whose target is absolute or
// leaves the root is skipped and counted, never read. A since time other than
// zero keeps only regular files modified after it; directories are always
// written so the archive's shape does not depend on which files changed.
func Export(w io.Writer, root string, m *Manifest, since time.Time) (ExportSummary, error) {
	s := ExportSummary{Skipped: map[SkipClass]int{}}
	if m == nil {
		return s, errors.New("townstate: nil manifest")
	}
	if err := m.Validate(); err != nil {
		return s, err
	}
	realRoot, err := resolveTownRoot(root)
	if err != nil {
		return s, err
	}
	rigs, _, err := townInputs(os.DirFS(realRoot), m)
	if err != nil {
		return s, fmt.Errorf("reading the town's rigs: %w", err)
	}
	tw := tar.NewWriter(w)
	for _, e := range m.Expand(rigs, nil) {
		if e.Owner != OwnerDaemon {
			continue
		}
		s.Entries = append(s.Entries, e.Path)
		if err := exportEntry(tw, realRoot, e, since, &s); err != nil {
			return s, err
		}
	}
	if err := tw.Close(); err != nil {
		return s, fmt.Errorf("closing the tar stream: %w", err)
	}
	return s, nil
}

// exportEntry walks one daemon-owned row and writes its entries to tw.
func exportEntry(tw *tar.Writer, root string, e Entry, since time.Time, s *ExportSummary) error {
	start := filepath.Join(root, filepath.FromSlash(e.Path))
	return filepath.WalkDir(start, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == start && errors.Is(walkErr, fs.ErrNotExist) {
				// A young town has not created every daemon path yet; an
				// absent row is not a failed read.
				return nil
			}
			return fmt.Errorf("walking %s: %w", e.Path, walkErr)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("naming %s: %w", e.Path, err)
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if class, skip := skippedClass(name, d, info); skip {
			s.Skipped[class]++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return exportSymlink(tw, p, name, info, s)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("preparing %s for the archive: %w", name, err)
		}
		hdr.Name = name
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if d.IsDir() {
			hdr.Name += "/"
			s.Dirs++
			return tw.WriteHeader(hdr)
		}
		if !since.IsZero() && !info.ModTime().After(since) {
			s.Skipped[SkipOlderThan]++
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing the archive header for %s: %w", name, err)
		}
		if err := copyFile(tw, p, hdr.Size); err != nil {
			return err
		}
		s.Files++
		s.Bytes += hdr.Size
		return nil
	})
}

// exportSymlink writes a symlink entry when its target stays inside the town
// root, and counts it as skipped otherwise. The target is never opened.
func exportSymlink(tw *tar.Writer, p, name string, info fs.FileInfo, s *ExportSummary) error {
	link, err := os.Readlink(p)
	if err != nil {
		return fmt.Errorf("reading the symlink %s: %w", name, err)
	}
	if !linkStaysInside(name, link) {
		s.Skipped[SkipSymlinkOut]++
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("preparing the symlink %s for the archive: %w", name, err)
	}
	hdr.Name = name
	hdr.Uid, hdr.Gid = 0, 0
	hdr.Uname, hdr.Gname = "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing the archive header for %s: %w", name, err)
	}
	s.Symlinks++
	return nil
}

// linkStaysInside reports whether a relative link target, resolved from the
// link's own directory, stays within the town root. It is a lexical check: the
// export never resolves or reads the target.
func linkStaysInside(name, link string) bool {
	if path.IsAbs(link) || filepath.IsAbs(link) {
		// An absolute target names the source host's layout, which the
		// destination does not have; the import refuses it too.
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), link))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// copyFile writes exactly size bytes of p to tw. The size is the one the
// header carries, so a file the daemon grew while it was being read is copied
// to a consistent prefix; a file that shrank is an error, never a short entry.
func copyFile(tw io.Writer, p string, size int64) error {
	f, err := os.Open(p) //nolint:gosec // G304: p is a path the walk built under the town root
	if err != nil {
		return fmt.Errorf("opening %s for the archive: %w", filepath.Base(p), err)
	}
	defer f.Close()
	if _, err := io.CopyN(tw, f, size); err != nil {
		return fmt.Errorf("copying %s into the archive: %w", filepath.Base(p), err)
	}
	return nil
}

// skippedClass reports why p does not enter the archive, if it does not.
func skippedClass(name string, d fs.DirEntry, info fs.FileInfo) (SkipClass, bool) {
	for _, seg := range strings.Split(name, "/") {
		switch {
		case strings.HasPrefix(seg, ".dolt-data"):
			return SkipDoltData, true
		case seg == ".git":
			return SkipGitTree, true
		}
	}
	switch {
	case d.Type()&fs.ModeSocket != 0:
		return SkipSocket, true
	case !d.IsDir() && strings.HasSuffix(d.Name(), ".lock"):
		// Every flock in the town is taken on path+".lock" (internal/lock,
		// internal/config, internal/events); a copied lock is stale by
		// definition.
		return SkipLockFile, true
	case !d.IsDir() && d.Type()&fs.ModeSymlink == 0 && !info.Mode().IsRegular():
		return SkipNotRegular, true
	}
	return "", false
}

// resolveTownRoot returns root's real path, insisting that it exists and is a
// directory. A root that cannot be read is an error, never an empty town.
func resolveTownRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("townstate: the town root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving the town root %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("reading the town root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("the town root %q is not a directory", root)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolving the town root %q: %w", root, err)
	}
	return real, nil
}

// daemonEntries returns the daemon-owned rows expanded for rigs, in manifest
// order. Import checks every archive name against them, so nothing outside the
// daemon-owned set is written.
func daemonEntries(m *Manifest, rigs []string) []Entry {
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Expand(rigs, nil) {
		if e.Owner == OwnerDaemon {
			out = append(out, e)
		}
	}
	return out
}

// sortedRigs returns the rig names a town has, in a stable order.
func sortedRigs(rigs map[string]bool) []string {
	return slices.Sorted(maps.Keys(rigs))
}
