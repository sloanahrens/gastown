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
	// SkipSecret is a file ExportOptions.Exclude names: a secret the copy must
	// not carry. The summary counts it and never names it.
	SkipSecret SkipClass = "secret files (the secrets list)"
	// SkipUnchanged is a regular file ExportOptions.Only refuses: a delta copy
	// leaves out what the volume already holds with the same bytes.
	SkipUnchanged SkipClass = "files the volume already holds unchanged"
	// SkipCutoverRecord is the cut-over record, which a rollback export leaves
	// out: the Mac tree must not look cut over after a rollback.
	SkipCutoverRecord SkipClass = "cut-over records (a rollback leaves them out)"
	// SkipVanished is an entry a directory listing named that was gone by the
	// time it was stat'ed or opened: the live daemon writes files atomically
	// through a temporary name, and a rehearsal export runs beside it. Only
	// fs.ErrNotExist is a vanish; every other error still fails the walk.
	SkipVanished SkipClass = "files that vanished during the walk (the daemon's temporary files)"
)

// skipClassOrder is the order the summary prints the classes in, so two runs
// over the same town print the same line.
var skipClassOrder = []SkipClass{
	SkipDoltData, SkipGitTree, SkipSocket, SkipLockFile, SkipNotRegular, SkipSymlinkOut, SkipOlderThan, SkipSecret, SkipUnchanged, SkipCutoverRecord, SkipVanished,
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
	// LinkRefusals is one line for each link left out because the import would
	// refuse it: the daemon-owned row that holds it and the reason, never the
	// link's own name.
	LinkRefusals []string
	// RigRowsDropped counts the rig-scoped daemon rows the archive lacks
	// because the town's registry names no rig. It is zero for a town with
	// rigs; non-zero only under ExportOptions.AllowNoRigs.
	RigRowsDropped int
}

// String renders the summary for the export command's standard error.
func (s ExportSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "town state export: %d files, %d directories, %d symlinks, %d bytes",
		s.Files, s.Dirs, s.Symlinks, s.Bytes)
	if len(s.Entries) > 0 {
		fmt.Fprintf(&b, "\nentries: %s", strings.Join(s.Entries, ", "))
	}
	for _, line := range s.LinkRefusals {
		fmt.Fprintf(&b, "\nskipped link %s", line)
	}
	if s.RigRowsDropped > 0 {
		fmt.Fprintf(&b, "\nWARNING: the town registers no rigs, so %d rig-scoped daemon rows are NOT in this archive", s.RigRowsDropped)
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
	// AllowNoRigs lets an export of a town whose registry names no rig succeed,
	// without the rig-scoped rows. Without it that export is refused with
	// ErrNoRigs: an archive that silently lacks every rig row looks complete
	// and is not.
	AllowNoRigs bool
	// Exclude reports whether a town-root-relative file name is a secret the
	// archive must leave out. A nil Exclude excludes nothing. An excluded name
	// is counted under SkipSecret and never written or named in the summary.
	Exclude func(name string) bool
	// Only, when set, keeps only the regular files and links whose
	// town-root-relative name it accepts: a delta copy sends the entries the
	// volume lacks or holds differently. Directories are always written, and an
	// entry it refuses is counted under SkipUnchanged.
	Only func(name string) bool
	// RollbackSettings adds the town's settings/config.json and each registered
	// rig's <rig>/settings/config.json after the rows, so a replay carries the
	// settings the pod edited after the cut-over. It also leaves the cut-over
	// record out, so a replay does not mark the Mac tree cut over. Only the
	// rollback sets it.
	RollbackSettings bool
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
	townRoot, rigs, rows, dropped, err := townRowsAndRigs(opts.ops, root, m, opts.AllowNoRigs)
	s.RigRowsDropped = dropped
	if err != nil {
		return s, err
	}
	if opts.RollbackSettings {
		rows = append(rows, rollbackSettingsRows(rigs)...)
	}
	walk := &exportWalk{
		tw:      tar.NewWriter(w),
		root:    townRoot,
		opts:    opts,
		sum:     &s,
		written: map[string]fs.FileInfo{},
		links:   map[string]string{},
	}
	walk.rows = rows
	for _, e := range walk.rows {
		s.Entries = append(s.Entries, e.Path)
		walk.entry = e.Path
		if err := walk.exportEntry(e); err != nil {
			return s, err
		}
	}
	if err := walk.tw.Close(); err != nil {
		return s, fmt.Errorf("closing the tar stream: %w", err)
	}
	return s, nil
}

// exportWalk is the state one export carries down the tree: the archive, the
// town root, the daemon-owned rows a link may land in, the names already written
// (which is the tree an import will see when it reaches this point), the row
// being walked and the summary.
type exportWalk struct {
	tw    *tar.Writer
	root  string
	rows  []Entry
	entry string
	opts  ExportOptions
	sum   *ExportSummary
	// written maps a town-root-relative name to the entry the archive carries
	// for it; links also keep their target, the way the import will read it.
	written map[string]fs.FileInfo
	links   map[string]string
}

// exportEntry walks one daemon-owned row. Every ancestor below the town root is
// checked first, so a symlinked ancestor such as .runtime -> /outside is
// refused before anything under it is read. An absent row is not a failed read:
// a young town has not created every daemon path yet.
func (w *exportWalk) exportEntry(e Entry) error {
	p := filepath.Join(w.root, filepath.FromSlash(e.Path))
	exists, err := checkAncestors(w.opts.ops, w.root, e.Path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	info, err := w.opts.ops.lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", e.Path, err)
	}
	return w.path(p, e.Path, info)
}

// checkAncestors walks every ancestor of rel from the town root with lstat and
// reports whether the row can exist at all. An ancestor that is not a real
// directory is refused: a symlink would carry the walk outside the town, and a
// regular file cannot hold the row. An absent ancestor means the row is absent.
func checkAncestors(ops townOps, root, rel string) (bool, error) {
	segs := strings.Split(rel, "/")
	cur := root
	for i, seg := range segs[:len(segs)-1] {
		cur = filepath.Join(cur, seg)
		info, err := ops.lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("reading %s, an ancestor of %s: %w", path.Join(segs[:i+1]...), rel, err)
		case !info.IsDir():
			return false, fmt.Errorf("the daemon-owned path %q resolves through %s, which is not a directory; an export reads nothing through a symlink or a file", rel, path.Join(segs[:i+1]...))
		}
	}
	return true, nil
}

// path writes one entry and, for a directory, its children in name order.
func (w *exportWalk) path(p, name string, info fs.FileInfo) error {
	if class, skip := skippedClass(name, info); skip {
		w.sum.Skipped[class]++
		return nil
	}
	if !info.IsDir() && w.opts.Exclude != nil && w.opts.Exclude(name) {
		w.sum.Skipped[SkipSecret]++
		return nil
	}
	if w.opts.RollbackSettings && name == CutoverRecordRel {
		w.sum.Skipped[SkipCutoverRecord]++
		return nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return w.symlink(p, name, info)
	}
	if !info.IsDir() {
		return w.file(p, name, info)
	}
	hdr := &tar.Header{
		Name:     name + "/",
		Mode:     int64(info.Mode().Perm()),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeDir,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing the archive header for %s: %w", name, err)
	}
	w.written[name] = info
	w.sum.Dirs++
	children, err := w.opts.ops.readDir(p)
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	// Each child is a direct child of a directory already reached through real
	// directories, so its own ancestors are real; a child that is a symlink is
	// written as a link and never descended into.
	for _, child := range children {
		childName := name + "/" + child
		childPath := filepath.Join(p, child)
		childInfo, err := w.opts.ops.lstat(childPath)
		if errors.Is(err, fs.ErrNotExist) {
			// Listed a moment ago and gone now: the daemon finished an atomic
			// write and removed its temporary file.
			w.sum.Skipped[SkipVanished]++
			continue
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", childName, err)
		}
		if err := w.path(childPath, childName, childInfo); err != nil {
			return err
		}
	}
	return nil
}

// file writes a regular file, or counts it as older than --since.
func (w *exportWalk) file(p, name string, info fs.FileInfo) error {
	if !w.opts.Since.IsZero() && !info.ModTime().After(w.opts.Since) {
		w.sum.Skipped[SkipOlderThan]++
		return nil
	}
	if w.opts.Only != nil && !w.opts.Only(name) {
		w.sum.Skipped[SkipUnchanged]++
		return nil
	}
	// The file is opened before its header is written, so a file that vanished
	// between the stat and the open is skipped whole: a header already in the
	// stream with no body behind it would corrupt the archive.
	f, err := w.opts.ops.open(p)
	if errors.Is(err, fs.ErrNotExist) {
		w.sum.Skipped[SkipVanished]++
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening %s for the archive: %w", name, err)
	}
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeReg,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the archive header for %s: %w", name, err)
	}
	// The size is the one the header carries, so a file the daemon grew while
	// it was read is copied to a consistent prefix, and a file that shrank is
	// an error rather than a short entry.
	if _, err := io.CopyN(w.tw, f, hdr.Size); err != nil {
		return fmt.Errorf("copying %s into the archive: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s after the copy: %w", name, err)
	}
	w.sum.Files++
	w.sum.Bytes += hdr.Size
	w.written[name] = info
	return nil
}

// symlink writes a link the import would accept, and counts one it would refuse
// with the reason. The target is judged by the same lookup the import runs,
// against the same rows and the same tree: the names the archive has written up
// to this entry, so a link the import would refuse is never archived. The target
// is never opened.
func (w *exportWalk) symlink(p, name string, info fs.FileInfo) error {
	// A delta sends a link only where the volume differs. An unchanged link is
	// on the volume already, and a merge never replaces a link, so sending it
	// again fails the import.
	if w.opts.Only != nil && !w.opts.Only(name) {
		w.sum.Skipped[SkipUnchanged]++
		return nil
	}
	link, err := w.opts.ops.readlink(p)
	if err != nil {
		return fmt.Errorf("reading the symlink %s: %w", name, err)
	}
	if err := resolveLinkTarget(exportLinkTree{written: w.written, links: w.links}, path.Dir(name), link, w.rows); err != nil {
		// The archive tree has no read that can fail, so the error is a
		// refusal; any other error still leaves the link out rather than
		// archiving one the import may reject.
		reason := "a link whose target could not be followed"
		var refused linkRefusal
		if errors.As(err, &refused) {
			reason = refused.class
		}
		w.sum.Skipped[SkipSymlinkOut]++
		w.sum.LinkRefusals = append(w.sum.LinkRefusals, w.entry+": "+reason)
		return nil
	}
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(info.Mode().Perm()),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeSymlink,
		Linkname: link,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing the archive header for %s: %w", name, err)
	}
	w.sum.Symlinks++
	w.written[name] = info
	w.links[name] = link
	return nil
}

// exportLinkTree is the tree the export follows a link target through: the names
// the archive has written so far, which is exactly what an import holds when it
// reaches the same entry. A name the archive skipped or has not reached reads as
// absent, so a component skipped here cannot make a link acceptable that the
// import would refuse.
type exportLinkTree struct {
	written map[string]fs.FileInfo
	links   map[string]string
}

func (t exportLinkTree) at(rel string) (fs.FileInfo, string, bool, error) {
	info, ok := t.written[rel]
	if !ok {
		return nil, "", false, nil
	}
	return info, t.links[rel], true, nil
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

// ErrNoRigs is the refusal for an export of a town whose registry names no rig
// while the manifest has rig-scoped daemon rows: both mayor/town.json (its
// "registry" section) and mayor/rigs.json are absent or empty. Pass
// ExportOptions.AllowNoRigs (--allow-no-rigs) for a town that really has none.
var ErrNoRigs = errors.New("the town registers no rigs (no registry in mayor/town.json and no mayor/rigs.json), so every rig-scoped row would be missing from the archive; pass --allow-no-rigs if the town has no rigs")

// rigScopedDaemonRows counts the manifest's daemon-owned rows that carry the
// rig placeholder.
func rigScopedDaemonRows(m *Manifest) int {
	n := 0
	for _, e := range m.Entries {
		if e.Owner == OwnerDaemon && strings.Contains(e.Path, RigPlaceholder) {
			n++
		}
	}
	return n
}

// townRows resolves the town root and the daemon-owned rows it names, for an
// export and for the metadata walk a plan makes. A town whose registry names no
// rig is refused with ErrNoRigs unless allowNoRigs; dropped counts the rig-scoped
// rows the rows then lack.
func townRows(ops townOps, root string, m *Manifest, allowNoRigs bool) (townRoot string, rows []Entry, dropped int, err error) {
	townRoot, _, rows, dropped, err = townRowsAndRigs(ops, root, m, allowNoRigs)
	return townRoot, rows, dropped, err
}

// townRowsAndRigs is townRows that also names the rigs the rows expand to, so an
// export can add each rig's settings file for the same rigs.
func townRowsAndRigs(ops townOps, root string, m *Manifest, allowNoRigs bool) (townRoot string, rigs []string, rows []Entry, dropped int, err error) {
	townRoot, err = resolveTownRoot(ops, root)
	if err != nil {
		return "", nil, nil, 0, err
	}
	rigs, err = settingsRigs(os.DirFS(townRoot))
	if err != nil {
		return "", nil, nil, 0, fmt.Errorf("reading the town's rigs: %w", err)
	}
	if len(rigs) == 0 {
		dropped = rigScopedDaemonRows(m)
		if dropped > 0 && !allowNoRigs {
			return "", nil, nil, dropped, ErrNoRigs
		}
	}
	return townRoot, rigs, daemonEntries(m, rigs), dropped, nil
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

// RegisteredRigs names the rigs the town at townRoot registers, sorted. It is the
// read townRows makes to expand the rig-scoped rows of an export, so a caller
// that copies a rig's files names the same rigs the stream does.
func RegisteredRigs(townRoot string) ([]string, error) {
	return settingsRigs(os.DirFS(townRoot))
}
