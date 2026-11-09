package townstate

import (
	"archive/tar"
	"bytes"
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
	// regular file, and nothing else is replaced. It is the rollback's mode, and
	// the only one that accepts the rollback's settings files. The town's own
	// settings file keeps the Mac file's daemon.supervisor.
	MergeNewer bool
	// ops is the filesystem seam a test replaces; the zero value is the host.
	ops townOps
}

// ImportSummary counts what an import wrote and what it left alone.
type ImportSummary struct {
	// Entries are the manifest rows the archive wrote into, in first-seen
	// order. They are top-level entry names, never the files below.
	Entries  []string
	Files    int
	Dirs     int
	Symlinks int
	Bytes    int64
	// KeptNewer counts the existing files a merge left alone because they are
	// at least as new as the archive's copy.
	KeptNewer int
}

// String renders the summary for the import command: one line per entry class,
// so a run names what it wrote and what it kept.
func (s ImportSummary) String() string {
	var b strings.Builder
	b.WriteString("town state import:")
	fmt.Fprintf(&b, "\nfiles: %d (%d bytes)", s.Files, s.Bytes)
	fmt.Fprintf(&b, "\ndirectories: %d", s.Dirs)
	fmt.Fprintf(&b, "\nsymlinks: %d", s.Symlinks)
	if s.KeptNewer > 0 {
		fmt.Fprintf(&b, "\nskipped: %d existing files newer than the archive", s.KeptNewer)
	} else {
		b.WriteString("\nskipped: none")
	}
	if len(s.Entries) > 0 {
		fmt.Fprintf(&b, "\nentries: %s", strings.Join(s.Entries, ", "))
	}
	return b.String()
}

// destTree is the town root an import writes into, and the daemon-owned rows
// the archive may name, as they stand when an entry is read.
type destTree struct {
	root    string
	entries []Entry
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
// whose target, followed through the tree as extracted so far, leaves the town
// root or the daemon-owned rows. Nothing outside the daemon-owned rows is
// written, and a refused entry writes nothing.
//
// The rigs are the ones the registry names. A seed into an empty root learns
// them from the registry entry the archive carries; the manifest orders that
// entry before the <rig> rows, so a rig row that comes first is refused.
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
	rigs, err := settingsRigs(os.DirFS(townRoot))
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
	var dirs []dirTime
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return s, finishDirs(opts.ops, dirs)
		}
		if err != nil {
			return s, fmt.Errorf("reading the archive: %w", err)
		}
		name, err := cleanArchiveName(hdr.Name)
		if err != nil {
			return s, err
		}
		tree := destTree{root: townRoot, entries: daemonEntries(m, sortedRigs(known))}
		e, ok := Lookup(tree.entries, name)
		if !ok && opts.MergeNewer {
			e, ok = rollbackSettingsEntry(name, sortedRigs(known))
		}
		if !ok {
			return s, fmt.Errorf("the archive entry %q is not under a daemon-owned path of the manifest; an import writes nothing outside them", name)
		}
		if err := checkComponents(opts.ops, townRoot, name); err != nil {
			return s, err
		}
		if err := importEntry(opts.ops, tr, hdr, name, e, tree, opts, &s, &dirs); err != nil {
			return s, err
		}
		if isRegistry(name) && hdr.Typeflag == tar.TypeReg {
			// The registry is on disk now, so the rigs it names are the rows
			// the entries after it may write.
			registered, err := registryRigs(os.DirFS(townRoot))
			if err != nil {
				return s, fmt.Errorf("reading the rigs the registry in the archive names: %w", err)
			}
			for _, rig := range registered {
				known[rig] = true
			}
		}
		if !slices.Contains(s.Entries, e.Path) {
			s.Entries = append(s.Entries, e.Path)
		}
	}
}

// isRegistry reports whether name is the rig registry, the file the rigs are
// read from, on either layout.
func isRegistry(name string) bool {
	return name == machineFile || name == rigsFile
}

// cleanArchiveName refuses a tar name that cannot be a town-root-relative path.
// A directory's trailing slash is dropped; every other irregularity is refused
// with the entry named.
func cleanArchiveName(raw string) (string, error) {
	name := strings.TrimSuffix(raw, "/")
	if path.IsAbs(name) || filepath.IsAbs(name) {
		return "", fmt.Errorf("the archive entry %q is absolute; an import accepts only paths relative to the town root", name)
	}
	// A "..", "." or empty segment is exactly what makes path.Clean change a
	// name, so the loop is the whole of the check.
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "..":
			return "", fmt.Errorf("the archive entry %q contains ..; it would leave the town root", name)
		case "", ".":
			return "", fmt.Errorf("the archive entry %q has an empty or %q segment", name, seg)
		}
	}
	return name, nil
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
func importEntry(ops townOps, tr io.Reader, hdr *tar.Header, name string, e Entry, tree destTree, opts ImportOptions, s *ImportSummary, dirs *[]dirTime) error {
	dest := filepath.Join(tree.root, filepath.FromSlash(name))
	switch hdr.Typeflag {
	case tar.TypeDir:
		return importDir(ops, dest, name, fs.FileMode(hdr.Mode).Perm(), hdr.ModTime, s, dirs)
	case tar.TypeReg, tar.TypeRegA:
		return importFile(ops, tr, hdr, dest, name, e.Private, name == townSettingsRel, opts, s)
	case tar.TypeSymlink:
		return importSymlink(ops, hdr, dest, name, tree, s)
	default:
		return fmt.Errorf("the archive entry %q has type %s; an import accepts regular files, directories and symlinks only", name, tarTypeName(hdr.Typeflag))
	}
}

// dirTime is a created directory, the mode and mtime its archive entry carries.
type dirTime struct {
	dest, name string
	mode       fs.FileMode
	mtime      time.Time
}

// importDir creates a missing directory. An existing directory is kept, so a
// replay leaves the tree's mode and mtime alone where nothing was written. The
// archived mode and mtime are queued for finishDirs: the directory is created
// owner-writable so the entries after it can be written, and every child
// written after this call would move its mtime.
func importDir(ops townOps, dest, name string, mode fs.FileMode, mtime time.Time, s *ImportSummary, dirs *[]dirTime) error {
	info, err := ops.lstat(dest)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return typeConflict(name, info)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading %q in the destination: %w", name, err)
	}
	// The parents take the town's directory mode, not the directory's own, so
	// only the entry the archive names carries its archived bits.
	if err := ops.mkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating the parent of %q: %w", name, err)
	}
	if err := ops.mkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("creating %q: %w", name, err)
	}
	*dirs = append(*dirs, dirTime{dest: dest, name: name, mode: mode, mtime: mtime})
	s.Dirs++
	return nil
}

// finishDirs sets the archived mode and mtime of each directory import created,
// once the archive has been read in full. Setting the mode only now lets a
// directory the archive marks read-only still take the entries written into it;
// the mtime follows the mode, so no later write moves it. A refused import
// leaves its created directories at 0700.
func finishDirs(ops townOps, dirs []dirTime) error {
	for _, d := range dirs {
		if err := ops.chmod(d.dest, d.mode); err != nil {
			return fmt.Errorf("setting the mode of %q: %w", d.name, err)
		}
		if !d.mtime.IsZero() {
			if err := ops.chtimes(d.dest, d.mtime, d.mtime); err != nil {
				return fmt.Errorf("setting the mtime of %q: %w", d.name, err)
			}
		}
	}
	return nil
}

// importFile writes a regular file, or keeps an existing newer one in merge
// mode. A town settings file is first rewritten to keep the Mac file's
// daemon.supervisor, and a refusal there writes nothing. A failure after the file
// is created removes it, so a retry never finds a partial copy that reads as
// newer than the archive.
func importFile(ops townOps, tr io.Reader, hdr *tar.Header, dest, name string, private, townSettings bool, opts ImportOptions, s *ImportSummary) error {
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
	src := tr
	if townSettings {
		out, err := keepMacSupervisor(ops, dest, name, tr)
		if err != nil {
			return err
		}
		src = bytes.NewReader(out)
	}
	if err := ops.mkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating the directory for %q: %w", name, err)
	}
	mode := archiveMode(hdr.Mode, private)
	f, err := ops.openFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("creating %q: %w", name, err)
	}
	n, copyErr := io.Copy(f, src)
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
func importSymlink(ops townOps, hdr *tar.Header, dest, name string, tree destTree, s *ImportSummary) error {
	if err := checkLinkTarget(ops, tree, name, hdr.Linkname); err != nil {
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

// checkLinkTarget refuses a symlink target the shared lookup refuses. The
// target is followed through the tree as it stands in the destination, so a link
// the archive wrote earlier counts: a target that is clean on its face can still
// climb out through one of them.
func checkLinkTarget(ops townOps, tree destTree, name, link string) error {
	if err := resolveLinkTarget(fsLinkTree{ops: ops, root: tree.root}, path.Dir(name), link, tree.entries); err != nil {
		return fmt.Errorf("the archive symlink %q %w", name, err)
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
