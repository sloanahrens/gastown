package townstate

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// VolumeMount is the path the helper container mounts the town-state volume at.
// Every argument below that names the volume names this path, never the
// volume's own name.
const VolumeMount = "/state"

// ImportArgv is what the helper runs to extract a daemon-owned archive from its
// standard input into the volume. The import refuses a root that already holds
// a daemon-owned path, so a seed never merges into an occupied volume; a delta
// onto a seeded volume runs ImportMergeArgv instead.
var ImportArgv = []string{"gt", "town", "state", "import", "--town", VolumeMount}

// ImportMergeArgv is what the helper runs to replay a delta onto a seeded
// volume: a regular file is written only when the archive's copy is newer, and
// nothing is deleted.
var ImportMergeArgv = []string{"gt", "town", "state", "import", "--town", VolumeMount, "--merge-newer"}

// ListArgv is what the helper runs to stream the volume's tree as a tar archive.
var ListArgv = []string{"tar", "-c", "-C", VolumeMount, "."}

// FreeArgv is what the helper runs to report the volume's disk in POSIX df form.
var FreeArgv = []string{"df", "-Pk", VolumeMount}

// BeadsReadArgv is the one bd read the copy runs through the volume, to show how
// bd reaches the Dolt server from inside the container.
var BeadsReadArgv = []string{"bd", "list", "--limit", "1"}

// Engine runs one command in a helper container with the named volume mounted
// at VolumeMount. It is the only seam between the copy and the container
// engine: production runs docker, and a test supplies a fake that works on a
// directory and starts no process.
type Engine interface {
	// Run runs argv in the helper with stdin streamed in and stdout streamed
	// out. A non-nil error means the command did not exit zero, and stdin or
	// stdout may be partly consumed.
	Run(volume string, argv []string, stdin io.Reader, stdout io.Writer) error
	// HasVolume reports whether the named volume exists.
	HasVolume(volume string) (bool, error)
	// CreateVolume creates the named volume. A copy calls it only when HasVolume
	// reported the volume absent, so it never creates over an existing volume.
	CreateVolume(volume string) error
}

// errHelperReturned is the reason a seed closes the export's pipe once the
// helper has returned: whatever the export still holds was not taken.
var errHelperReturned = errors.New("the import helper returned before the stream ended")

// SeedVolume streams the daemon-owned set of the town at root into the volume:
// the export's tar goes straight into the helper's standard input, and nothing
// is staged on disk. The helper's own summary goes to helperOut. A copy that
// fails part way returns an error naming the volume as holding a partial copy,
// so the caller never reads a short copy as complete.
//
// argv is ImportArgv for a seed into an empty volume and ImportMergeArgv for a
// delta onto a seeded one; opts.Only selects the delta's files.
func SeedVolume(eng Engine, volume, root string, m *Manifest, opts ExportOptions, argv []string, helperOut io.Writer) (ExportSummary, error) {
	pr, pw := io.Pipe()
	type exported struct {
		sum ExportSummary
		err error
	}
	done := make(chan exported, 1)
	go func() {
		sum, err := Export(pw, root, m, opts)
		pw.CloseWithError(err)
		done <- exported{sum, err}
	}()
	runErr := eng.Run(volume, argv, pr, helperOut)
	// Whatever the export still wants to write is not taken by the helper. A
	// write it cannot finish fails here, and the export reports that failure.
	pr.CloseWithError(errHelperReturned)
	res := <-done
	// An export that failed on its own is the cause, even when the helper then
	// failed too on the stream it was cut off from; errHelperReturned is that
	// cut-off, not a cause.
	switch {
	case res.err != nil && !errors.Is(res.err, errHelperReturned):
		return res.sum, partialCopy(volume, "the export failed", res.err)
	case runErr != nil:
		return res.sum, partialCopy(volume, "the import helper failed", runErr)
	case res.err != nil:
		return res.sum, partialCopy(volume, "the import helper ended the stream early", res.err)
	}
	return res.sum, nil
}

// partialCopy is the error a failed seed returns. It names the volume and the
// refusal to use it; the wrapped error is the failing command's exit or the
// export's refusal, which carry no file contents.
func partialCopy(volume, what string, err error) error {
	return fmt.Errorf("copy to volume %s failed (%s): the volume holds a partial copy and is not to be used: %w", volume, what, err)
}

// Mismatch is one path the volume's copy disagrees with the host on. Reason is
// ReasonMissing (the host has it, the volume does not), ReasonChanged (the
// bytes or the link target differ), ReasonExtra (the volume has it, the host
// copy set does not) or ReasonNotRegular (the volume holds an entry that is
// neither a regular file, a directory nor a link).
type Mismatch struct {
	Path   string
	Reason string
}

// The reasons a Mismatch carries.
const (
	ReasonMissing    = "missing"
	ReasonChanged    = "changed"
	ReasonExtra      = "extra"
	ReasonNotRegular = "not a regular file"
)

// VerifyResult is what a verify compared. Files counts the host's regular files
// in the copy set. SecretsOnVolume counts secret-named entries of any type found
// on the volume: the copy must not carry one, and its name is never listed.
type VerifyResult struct {
	Files           int
	Mismatches      []Mismatch
	SecretsOnVolume int
}

// Differing names the copy-set paths the volume lacks or holds with other
// bytes: the files a delta copy must send.
func (r VerifyResult) Differing() map[string]bool {
	out := map[string]bool{}
	for _, m := range r.Mismatches {
		if m.Reason == ReasonMissing || m.Reason == ReasonChanged {
			out[m.Path] = true
		}
	}
	return out
}

// entryKind is what a verify knows of one tar entry.
type entryKind string

const (
	kindFile  entryKind = "file"
	kindLink  entryKind = "link"
	kindOther entryKind = "other"
)

// entryPrint is what a verify compares for one name: a regular file's sha256,
// a link's target text, or the tar type byte of any other entry.
type entryPrint struct {
	kind entryKind
	sum  string
}

// VerifyVolume proves the volume's copy against the host: every regular file
// and symlink in the daemon-owned copy set is compared by path on both sides,
// with a sha256 for a file and the target text for a link. Secret-excluded names
// are left out of both listings. Any difference is a Mismatch; an error means
// the comparison could not be made, and is never a pass.
//
// The host side is always the full copy set: opts.Only is cleared, so a delta
// copy is verified against everything the volume should hold, not the delta.
func VerifyVolume(eng Engine, volume, root string, m *Manifest, opts ExportOptions) (VerifyResult, error) {
	opts.Only = nil
	host, _, err := hashTree(func(w io.Writer) error {
		_, err := Export(w, root, m, opts)
		return err
	}, nil)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("listing the host's copy set: %w", err)
	}
	vol, secrets, err := hashTree(func(w io.Writer) error {
		return eng.Run(volume, ListArgv, nil, w)
	}, opts.Exclude)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("listing volume %s: %w", volume, err)
	}
	res := VerifyResult{SecretsOnVolume: secrets}
	for _, p := range host {
		if p.kind == kindFile {
			res.Files++
		}
	}
	for name, got := range vol {
		_, inHost := host[name]
		switch {
		case got.kind == kindOther:
			res.Mismatches = append(res.Mismatches, Mismatch{Path: name, Reason: ReasonNotRegular})
		case !inHost:
			res.Mismatches = append(res.Mismatches, Mismatch{Path: name, Reason: ReasonExtra})
		}
	}
	for name, want := range host {
		got, ok := vol[name]
		switch {
		case !ok:
			res.Mismatches = append(res.Mismatches, Mismatch{Path: name, Reason: ReasonMissing})
		case got.kind == kindOther:
			// Already reported as not a regular file.
		case got != want:
			res.Mismatches = append(res.Mismatches, Mismatch{Path: name, Reason: ReasonChanged})
		}
	}
	sort.Slice(res.Mismatches, func(i, j int) bool {
		if res.Mismatches[i].Path != res.Mismatches[j].Path {
			return res.Mismatches[i].Path < res.Mismatches[j].Path
		}
		return res.Mismatches[i].Reason < res.Mismatches[j].Reason
	})
	return res, nil
}

// hashTree runs produce into a pipe, reads the tar it writes, and returns the
// entryPrint of each non-directory entry by its cleaned name. An entry exclude
// names is counted and not printed. A producer that fails returns that failure,
// never a listing of what it had written.
func hashTree(produce func(io.Writer) error, exclude func(string) bool) (map[string]entryPrint, int, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := produce(pw)
		pw.CloseWithError(err)
		done <- err
	}()
	prints, excluded, err := sumArchive(pr, exclude)
	if err != nil {
		pr.CloseWithError(err)
		<-done
		return nil, 0, err
	}
	// The archive's end marker is read; drain the producer's padding so it can
	// finish. A read error on the drain is the producer's own, which done reports,
	// so the drain's result is not read separately.
	_, _ = io.Copy(io.Discard, pr)
	if perr := <-done; perr != nil {
		return nil, 0, perr
	}
	return prints, excluded, nil
}

// sumArchive reads a tar stream and returns the entryPrint of each non-directory
// entry it carries, keyed by its cleaned name, and how many names exclude
// refused. Exclusion applies to every non-directory type, links included.
func sumArchive(r io.Reader, exclude func(string) bool) (map[string]entryPrint, int, error) {
	prints := map[string]entryPrint{}
	excluded := 0
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return prints, excluded, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("reading the archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		name := path.Clean(hdr.Name)
		if exclude != nil && exclude(name) {
			excluded++
			continue
		}
		switch {
		case hdr.Typeflag == tar.TypeSymlink:
			prints[name] = entryPrint{kind: kindLink, sum: hdr.Linkname}
		case hdr.FileInfo().Mode().IsRegular():
			// The member is read to its declared size, so a member the stream cuts
			// short is an error rather than a hash of what it did hold.
			h := sha256.New()
			if _, err := io.CopyN(h, tr, hdr.Size); err != nil {
				return nil, 0, fmt.Errorf("hashing %s from the archive: %w", name, err)
			}
			prints[name] = entryPrint{kind: kindFile, sum: hex.EncodeToString(h.Sum(nil))}
		default:
			prints[name] = entryPrint{kind: kindOther, sum: string(hdr.Typeflag)}
		}
	}
}

// VolumeEmpty reports whether the volume's tree holds nothing but its root. It
// reads the listing only until the first entry below the root, so a populated
// volume is answered without streaming all of it.
func VolumeEmpty(eng Engine, volume string) (bool, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := eng.Run(volume, ListArgv, nil, pw)
		pw.CloseWithError(err)
		done <- err
	}()
	empty, err := archiveEmpty(pr)
	if err != nil {
		pr.CloseWithError(errors.New("listing stopped"))
		<-done
		return false, fmt.Errorf("listing volume %s: %w", volume, err)
	}
	if !empty {
		// An entry below the root answers the question; release the helper, whose
		// own exit no longer decides the answer.
		pr.CloseWithError(errors.New("listing stopped"))
		<-done
		return false, nil
	}
	// The end marker is read; drain the padding the producer still writes. A read
	// error on the drain is the producer's own, which done reports, so the drain's
	// result is not read separately.
	_, _ = io.Copy(io.Discard, pr)
	if err := <-done; err != nil {
		return false, fmt.Errorf("listing volume %s: %w", volume, err)
	}
	return true, nil
}

// archiveEmpty reads a tar stream's entries until one names something below the
// archive's root, which makes the stream not empty.
func archiveEmpty(r io.Reader) (bool, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("reading the listing: %w", err)
		}
		if name := path.Clean(hdr.Name); name != "." {
			return false, nil
		}
	}
}

// FreeBytes returns the bytes free on the volume's disk, read by df in the
// helper. A df that prints no volume line is an error, never zero bytes.
func FreeBytes(eng Engine, volume string) (int64, error) {
	var out bytes.Buffer
	if err := eng.Run(volume, FreeArgv, nil, &out); err != nil {
		return 0, fmt.Errorf("reading the free space of volume %s: %w", volume, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df printed no volume line for %s", volume)
	}
	// POSIX df: Filesystem, 1024-blocks, Used, Available, Capacity, Mounted on.
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("df printed a volume line for %s with no available column", volume)
	}
	kb, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || kb < 0 {
		return 0, fmt.Errorf("df printed an available size for %s that is not a count", volume)
	}
	return kb * 1024, nil
}

// Inventory is what a copy of the daemon-owned set would carry, counted from
// metadata alone: no file's contents are read. Files, Dirs, Symlinks and Bytes
// are what the walk would write; Skipped counts what it would leave out, by class.
type Inventory struct {
	Entries  []string
	Files    int
	Dirs     int
	Symlinks int
	Bytes    int64
	Skipped  map[SkipClass]int
}

// Survey counts the daemon-owned set of the town at root from its metadata. It
// walks the same rows, under the same skip classes and the same --since rule, as
// Export, but does not judge symlink targets: Symlinks counts every link, where
// the export would leave out one whose target it refuses.
func Survey(root string, m *Manifest, opts ExportOptions) (Inventory, error) {
	inv := Inventory{Skipped: map[SkipClass]int{}}
	if m == nil {
		return inv, errors.New("townstate: nil manifest")
	}
	if err := m.Validate(); err != nil {
		return inv, err
	}
	ops := opts.ops.withDefaults()
	townRoot, rows, _, err := townRows(ops, root, m, opts.AllowNoRigs)
	if err != nil {
		return inv, err
	}
	for _, e := range rows {
		inv.Entries = append(inv.Entries, e.Path)
		exists, err := checkAncestors(ops, townRoot, e.Path)
		if err != nil {
			return inv, err
		}
		if !exists {
			continue
		}
		p := filepath.Join(townRoot, filepath.FromSlash(e.Path))
		info, err := ops.lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return inv, fmt.Errorf("reading %s: %w", e.Path, err)
		}
		if err := inv.count(ops, p, e.Path, info, opts); err != nil {
			return inv, err
		}
	}
	return inv, nil
}

// count adds one entry and, for a directory, its children to inv.
func (inv *Inventory) count(ops townOps, p, name string, info fs.FileInfo, opts ExportOptions) error {
	if class, skip := skippedClass(name, info); skip {
		inv.Skipped[class]++
		return nil
	}
	if !info.IsDir() && opts.Exclude != nil && opts.Exclude(name) {
		inv.Skipped[SkipSecret]++
		return nil
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		inv.Symlinks++
	case info.IsDir():
		inv.Dirs++
		children, err := ops.readDir(p)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		for _, child := range children {
			childInfo, err := ops.lstat(filepath.Join(p, child))
			if err != nil {
				return fmt.Errorf("reading %s: %w", name+"/"+child, err)
			}
			if err := inv.count(ops, filepath.Join(p, child), name+"/"+child, childInfo, opts); err != nil {
				return err
			}
		}
	default:
		if !opts.Since.IsZero() && !info.ModTime().After(opts.Since) {
			inv.Skipped[SkipOlderThan]++
			return nil
		}
		inv.Files++
		inv.Bytes += info.Size()
	}
	return nil
}
