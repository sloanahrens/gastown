package townstate

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ImportOptions carries the switches an import runs with.
type ImportOptions struct {
	// MergeNewer makes the import a replay onto a populated tree: a regular
	// file is written only when the archive's copy is newer than an existing
	// regular file, and nothing else is replaced. It is the rollback's mode.
	MergeNewer bool
	// ops is the filesystem seam a test replaces; the zero value is the host.
	ops townOps
}

// ImportSummary counts what an import wrote and what it left alone.
type ImportSummary struct {
	// Entries are the manifest rows the archive wrote into, in first-seen
	// order. They are top-level entry names, never the files below.
	Entries   []string
	Files     int
	Dirs      int
	Symlinks  int
	Bytes     int64
	KeptNewer int
}

// String renders the summary for the import command.
func (s ImportSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "town state import: %d files (%d bytes), %d directories, %d symlinks",
		s.Files, s.Bytes, s.Dirs, s.Symlinks)
	if s.KeptNewer > 0 {
		fmt.Fprintf(&b, "; kept %d newer existing files", s.KeptNewer)
	}
	if len(s.Entries) > 0 {
		fmt.Fprintf(&b, "\nentries: %s", strings.Join(s.Entries, ", "))
	}
	return b.String()
}

// Import reads a tar stream from r and restores the daemon-owned set into the
// town root at root. Without MergeNewer the root must hold none of the
// daemon-owned paths (the seed's empty volume); with it, only newer regular
// files are written (the rollback's replay).
//
// The archive is untrusted input. Every name must be a clean path relative to
// the root and must fall under a daemon-owned manifest row; a name that is
// absolute, contains "..", or resolves through a symlink is refused, as is any
// entry that is not a regular file, a directory or a symlink, and any symlink
// whose target leaves the town root. Nothing outside the daemon-owned rows is
// written, and a refused entry writes nothing.
//
// A rig whose top-level directory carries a config.json names itself as a rig,
// so a seed into an empty root covers the <rig> rows before the root has a
// registry. A rig's write surface is still only its daemon-owned rows.
func Import(r io.Reader, root string, m *Manifest, opts ImportOptions) (ImportSummary, error) {
	s := ImportSummary{}
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
		return s, fmt.Errorf("reading the destination's rigs: %w", err)
	}
	known := make(map[string]bool, len(rigs))
	for _, rig := range rigs {
		known[rig] = true
	}
	if !opts.MergeNewer {
		if err := refuseOccupiedRoot(opts.ops, townRoot, m, known); err != nil {
			return s, err
		}
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		if err != nil {
			return s, fmt.Errorf("reading the archive: %w", err)
		}
		name, err := cleanArchiveName(hdr.Name)
		if err != nil {
			return s, err
		}
		if rig, ok := rigFromMarker(name, m); ok {
			known[rig] = true
		}
		e, ok := Lookup(daemonEntries(m, sortedRigs(known)), name)
		if !ok {
			return s, fmt.Errorf("the archive entry %q is not under a daemon-owned path of the manifest; an import writes nothing outside them", name)
		}
		if err := checkComponents(opts.ops, townRoot, name); err != nil {
			return s, err
		}
		dest := filepath.Join(townRoot, filepath.FromSlash(name))
		if err := importEntry(opts.ops, tr, hdr, dest, name, e, opts, &s); err != nil {
			return s, err
		}
		if !slices.Contains(s.Entries, e.Path) {
			s.Entries = append(s.Entries, e.Path)
		}
	}
}

// cleanArchiveName refuses a tar name that cannot be a town-root-relative path.
// A directory's trailing slash is dropped; every other irregularity is refused
// with the entry named.
func cleanArchiveName(raw string) (string, error) {
	name := strings.TrimSuffix(raw, "/")
	if name == "" {
		return "", fmt.Errorf("the archive entry %q has an empty name", raw)
	}
	if path.IsAbs(name) || filepath.IsAbs(name) {
		return "", fmt.Errorf("the archive entry %q is absolute; an import accepts only paths relative to the town root", name)
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "..":
			return "", fmt.Errorf("the archive entry %q contains ..; it would leave the town root", name)
		case "", ".":
			return "", fmt.Errorf("the archive entry %q has an empty or %q segment", name, seg)
		}
	}
	if path.Clean(name) != name {
		return "", fmt.Errorf("the archive entry %q is not a clean relative path", name)
	}
	return name, nil
}

// rigFromMarker reports whether name is a top-level directory's config.json,
// which is how a target root with no registry learns the rigs an archive
// carries. A town-level directory the manifest names is never a rig.
func rigFromMarker(name string, m *Manifest) (string, bool) {
	top, rest, ok := strings.Cut(name, "/")
	if !ok || rest != "config.json" {
		return "", false
	}
	if top == "" || strings.HasPrefix(top, ".") || m.names(top) {
		return "", false
	}
	return top, true
}

// checkComponents refuses name when its destination, or any existing prefix of
// it, is a symlink: writing through one could leave the town root even though
// the name itself is clean.
func checkComponents(ops townOps, root, name string) error {
	segs := strings.Split(name, "/")
	cur := root
	for i, seg := range segs {
		cur = filepath.Join(cur, seg)
		info, err := ops.lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return fmt.Errorf("reading %s in the destination: %w", path.Join(segs[:i+1]...), err)
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("the archive entry %q resolves through the symlink %s; an import never writes through a symlink", name, path.Join(segs[:i+1]...))
		}
	}
	return nil
}

// refuseOccupiedRoot refuses an import without MergeNewer when the destination
// already holds a daemon-owned path. The first one found is named and nothing
// is read from the archive.
func refuseOccupiedRoot(ops townOps, root string, m *Manifest, rigs map[string]bool) error {
	for _, e := range daemonEntries(m, sortedRigs(rigs)) {
		if _, err := ops.lstat(filepath.Join(root, filepath.FromSlash(e.Path))); err == nil {
			return fmt.Errorf("the destination already holds the daemon-owned path %q; an import needs a town root empty of them, or --merge-newer", e.Path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("reading %q in the destination: %w", e.Path, err)
		}
	}
	return nil
}

// importEntry writes one archive entry, after every check that can refuse it
// has run.
func importEntry(ops townOps, tr io.Reader, hdr *tar.Header, dest, name string, e Entry, opts ImportOptions, s *ImportSummary) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return importDir(ops, dest, name, archiveMode(hdr.Mode, e.Private), hdr.ModTime, s)
	case tar.TypeReg, tar.TypeRegA:
		return importFile(ops, tr, hdr, dest, name, e.Private, opts, s)
	case tar.TypeSymlink:
		return importSymlink(ops, hdr, dest, name, s)
	default:
		return fmt.Errorf("the archive entry %q has type %s; an import accepts regular files, directories and symlinks only", name, tarTypeName(hdr.Typeflag))
	}
}

// importDir creates a missing directory. An existing directory is kept, so a
// replay leaves the tree's mtimes alone where nothing was written.
func importDir(ops townOps, dest, name string, mode fs.FileMode, mtime time.Time, s *ImportSummary) error {
	info, err := ops.lstat(dest)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return typeConflict(name, info)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading %q in the destination: %w", name, err)
	}
	if err := ops.mkdirAll(dest, mode); err != nil {
		return fmt.Errorf("creating %q: %w", name, err)
	}
	if err := ops.chmod(dest, mode); err != nil {
		return fmt.Errorf("setting the mode of %q: %w", name, err)
	}
	if !mtime.IsZero() {
		if err := ops.chtimes(dest, mtime, mtime); err != nil {
			return fmt.Errorf("setting the mtime of %q: %w", name, err)
		}
	}
	s.Dirs++
	return nil
}

// importFile writes a regular file, or keeps an existing newer one in merge
// mode. A failure after the file is created removes it, so a retry never finds
// a partial copy that reads as newer than the archive.
func importFile(ops townOps, tr io.Reader, hdr *tar.Header, dest, name string, private bool, opts ImportOptions, s *ImportSummary) error {
	info, err := ops.lstat(dest)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return typeConflict(name, info)
	case err == nil && !opts.MergeNewer:
		return fmt.Errorf("the archive entry %q already exists in the destination; an import without --merge-newer writes into an empty root", name)
	case err == nil && !hdr.ModTime.After(info.ModTime()):
		s.KeptNewer++
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading %q in the destination: %w", name, err)
	}
	if err := ops.mkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating the directory for %q: %w", name, err)
	}
	mode := archiveMode(hdr.Mode, private)
	f, err := ops.openFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("creating %q: %w", name, err)
	}
	n, copyErr := io.Copy(f, tr)
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		return errors.Join(fmt.Errorf("writing %q: %w", name, copyErr), removePartial(ops, dest))
	case closeErr != nil:
		return errors.Join(fmt.Errorf("writing %q: %w", name, closeErr), removePartial(ops, dest))
	}
	if err := ops.chmod(dest, mode); err != nil {
		return errors.Join(fmt.Errorf("setting the mode of %q: %w", name, err), removePartial(ops, dest))
	}
	if !hdr.ModTime.IsZero() {
		if err := ops.chtimes(dest, hdr.ModTime, hdr.ModTime); err != nil {
			return errors.Join(fmt.Errorf("setting the mtime of %q: %w", name, err), removePartial(ops, dest))
		}
	}
	s.Files++
	s.Bytes += n
	return nil
}

// importSymlink creates a symlink whose target stays inside the town root. A
// merge never replaces an existing path with a link.
//
// The link's own mtime is not restored: there is no portable call that sets a
// symlink's time without following it.
func importSymlink(ops townOps, hdr *tar.Header, dest, name string, s *ImportSummary) error {
	if err := checkLinkTarget(name, hdr.Linkname); err != nil {
		return err
	}
	info, err := ops.lstat(dest)
	switch {
	case err == nil:
		return typeConflict(name, info)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading %q in the destination: %w", name, err)
	}
	if err := ops.mkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating the directory for %q: %w", name, err)
	}
	if err := ops.symlink(hdr.Linkname, dest); err != nil {
		return fmt.Errorf("creating the symlink %q: %w", name, err)
	}
	s.Symlinks++
	return nil
}

// checkLinkTarget refuses a symlink target that leaves the town root. The
// check is lexical, the same rule the export applies to a link it archives: an
// absolute target, or one that climbs above the root.
func checkLinkTarget(name, link string) error {
	if link == "" {
		return fmt.Errorf("the archive symlink %q has an empty target", name)
	}
	if path.IsAbs(link) || filepath.IsAbs(link) {
		return fmt.Errorf("the archive symlink %q points at the absolute path %q; an import accepts only links that stay within the town root", name, link)
	}
	resolved := path.Clean(path.Join(path.Dir(name), link))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("the archive symlink %q points outside the town root (%q)", name, link)
	}
	return nil
}

// archiveMode masks an archive mode to the town's two file modes (0755 when
// any execute bit is set, 0644 otherwise), unless the manifest marks the row
// private, in which case the mode is kept as written.
func archiveMode(mode int64, private bool) fs.FileMode {
	perm := fs.FileMode(mode).Perm()
	if private {
		return perm
	}
	if perm&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// typeConflict names an existing destination path that an archive entry would
// have to replace with a different kind.
func typeConflict(name string, info fs.FileInfo) error {
	return fmt.Errorf("the archive entry %q would replace %s in the destination; an import does not replace one kind with another", name, describe(info))
}

// describe names a file kind without printing any of its content.
func describe(info fs.FileInfo) string {
	switch {
	case info.IsDir():
		return "a directory"
	case info.Mode()&fs.ModeSymlink != 0:
		return "a symlink"
	case info.Mode().IsRegular():
		return "a regular file"
	default:
		return "a special file"
	}
}

// removePartial deletes a file an import created but could not finish, so a
// retry cannot read it as a newer existing copy.
func removePartial(ops townOps, dest string) error {
	if err := ops.remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the partial file: %w", err)
	}
	return nil
}

// tarTypeName names a tar type flag for a refusal, without quoting a byte a
// terminal cannot show.
func tarTypeName(flag byte) string {
	switch flag {
	case tar.TypeChar:
		return "a character device"
	case tar.TypeBlock:
		return "a block device"
	case tar.TypeFifo:
		return "a fifo"
	case tar.TypeXHeader:
		return "a pax header"
	case tar.TypeXGlobalHeader:
		return "a pax global header"
	case tar.TypeGNULongName, tar.TypeGNULongLink:
		return "a GNU long name"
	case tar.TypeLink:
		return "a hard link"
	default:
		return "an unsupported type"
	}
}
