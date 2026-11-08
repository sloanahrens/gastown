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

// ExportOptions carries an export's switches.
type ExportOptions struct {
	// Since keeps only regular files modified after it; the zero time keeps
	// every file. Directories are always written.
	Since time.Time
	// ops is the filesystem seam a test replaces; the zero value is the host.
	ops townOps
}

// Export writes the daemon-owned set of the town at root to w as a tar stream
// named relative to root, and returns what it wrote. Only manifest rows owned
// by the daemon are walked; a session-owned path never enters the archive.
//
// The walk never follows a symlink: a link whose relative target stays inside
// the town root is archived as a link, and a link whose target is absolute or
// leaves the root is skipped and counted, never read.
func Export(w io.Writer, root string, m *Manifest, opts ExportOptions) (ExportSummary, error) {
	s := ExportSummary{Skipped: map[SkipClass]int{}}
	if m == nil {
		return s, errors.New("townstate: nil manifest")
	}
	if err := m.Validate(); err != nil {
		return s, err
	}
	opts.ops = opts.ops.withDefaults()
	townRoot, err := resolveTownRoot(opts.ops, root)
	if err != nil {
		return s, err
	}
	rigs, _, err := townInputs(os.DirFS(townRoot), m)
	if err != nil {
		return s, fmt.Errorf("reading the town's rigs: %w", err)
	}
	tw := tar.NewWriter(w)
	for _, e := range m.Expand(rigs, nil) {
		if e.Owner != OwnerDaemon {
			continue
		}
		s.Entries = append(s.Entries, e.Path)
		if err := exportEntry(tw, townRoot, e, opts, &s); err != nil {
			return s, err
		}
	}
	if err := tw.Close(); err != nil {
		return s, fmt.Errorf("closing the tar stream: %w", err)
	}
	return s, nil
}

// exportEntry walks one daemon-owned row and writes it to tw. An absent row
// is not a failed read: a young town has not created every daemon path yet.
func exportEntry(tw *tar.Writer, root string, e Entry, opts ExportOptions, s *ExportSummary) error {
	p := filepath.Join(root, filepath.FromSlash(e.Path))
	info, err := opts.ops.lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", e.Path, err)
	}
	return exportPath(tw, p, e.Path, info, opts, s)
}

// exportPath writes one entry and, for a directory, its children in name
// order.
func exportPath(tw *tar.Writer, p, name string, info fs.FileInfo, opts ExportOptions, s *ExportSummary) error {
	if class, skip := skippedClass(name, info); skip {
		s.Skipped[class]++
		return nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return exportSymlink(tw, p, name, info, opts, s)
	}
	if !info.IsDir() {
		return exportFile(tw, p, name, info, opts, s)
	}
	hdr := &tar.Header{
		Name:     name + "/",
		Mode:     int64(info.Mode().Perm()),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeDir,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing the archive header for %s: %w", name, err)
	}
	s.Dirs++
	children, err := opts.ops.readDir(p)
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	for _, child := range children {
		childName := name + "/" + child
		childPath := filepath.Join(p, child)
		childInfo, err := opts.ops.lstat(childPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", childName, err)
		}
		if err := exportPath(tw, childPath, childName, childInfo, opts, s); err != nil {
			return err
		}
	}
	return nil
}

// exportFile writes a regular file, or counts it as older than --since.
func exportFile(tw *tar.Writer, p, name string, info fs.FileInfo, opts ExportOptions, s *ExportSummary) error {
	if !opts.Since.IsZero() && !info.ModTime().After(opts.Since) {
		s.Skipped[SkipOlderThan]++
		return nil
	}
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing the archive header for %s: %w", name, err)
	}
	f, err := opts.ops.open(p)
	if err != nil {
		return fmt.Errorf("opening %s for the archive: %w", name, err)
	}
	// The size is the one the header carries, so a file the daemon grew while
	// it was read is copied to a consistent prefix, and a file that shrank is
	// an error rather than a short entry.
	if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
		return fmt.Errorf("copying %s into the archive: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s after the copy: %w", name, err)
	}
	s.Files++
	s.Bytes += hdr.Size
	return nil
}

// exportSymlink writes a symlink entry when its target stays inside the town
// root, and counts it as skipped otherwise. The target is never opened.
func exportSymlink(tw *tar.Writer, p, name string, info fs.FileInfo, opts ExportOptions, s *ExportSummary) error {
	link, err := opts.ops.readlink(p)
	if err != nil {
		return fmt.Errorf("reading the symlink %s: %w", name, err)
	}
	if !linkStaysInside(name, link) {
		s.Skipped[SkipSymlinkOut]++
		return nil
	}
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeSymlink,
		Linkname: link,
	}
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

// skippedClass reports why p does not enter the archive, if it does not.
func skippedClass(name string, info fs.FileInfo) (SkipClass, bool) {
	for _, seg := range strings.Split(name, "/") {
		switch {
		case strings.HasPrefix(seg, ".dolt-data"):
			return SkipDoltData, true
		case seg == ".git":
			return SkipGitTree, true
		}
	}
	switch {
	case info.Mode()&fs.ModeSocket != 0:
		return SkipSocket, true
	case !info.IsDir() && strings.HasSuffix(name, ".lock"):
		// Every flock in the town is taken on path+".lock" (internal/lock,
		// internal/config, internal/events); a copied lock is stale by
		// definition.
		return SkipLockFile, true
	case !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular():
		return SkipNotRegular, true
	}
	return "", false
}

// resolveTownRoot returns root cleaned, insisting that it is a directory. It
// refuses a symlink: a root that is a link would be archived as a link rather
// than walked. A root that cannot be read is an error, never an empty town.
func resolveTownRoot(ops townOps, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("townstate: the town root is empty")
	}
	info, err := ops.lstat(root)
	if err != nil {
		return "", fmt.Errorf("reading the town root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("the town root %q is not a directory", root)
	}
	return filepath.Clean(root), nil
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
