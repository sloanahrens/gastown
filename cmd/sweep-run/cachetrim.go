package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

const (
	// sweepCacheRoot is where the tier sweep Job mounts the shared cache claim
	// (gt-hlo07.1). A runner started outside that Job has no such directory and
	// its trim finds nothing.
	sweepCacheRoot = "/var/cache/gt-sweep"
	// sweepCacheCapBytes is the size a run brings the claim under: the 5 GiB
	// the claim is requested at, so a sweep's cache cannot fill it without
	// bound (gt-hlo07).
	sweepCacheCapBytes int64 = 5 << 30
	// cacheTrimLogName is the file under the run's log directory that records
	// the trim's failures, beside the tier logs the daemon reads.
	cacheTrimLogName = "cache-trim.log"
)

// CacheTrim is the trim a run ends with: the shared cache claim's root, the
// claim's files as a read-only filesystem, and the removal of one planned path.
// A test points Claim at a fake filesystem and Remove at a recorder, so no test
// reads or removes a real cache (gt-hlo07.2).
type CacheTrim struct {
	// Root is the claim's mount, holding the GOCACHE and GOMODCACHE trees.
	Root string
	// CapBytes is the size the claim is brought under. A value of zero or less
	// is refused; the runner sets sweepCacheCapBytes.
	CapBytes int64
	// Claim is the claim at Root as a read-only filesystem holding the two cache
	// trees, or nil when the claim is not mounted.
	Claim func(root string) (fs.FS, error)
	// Remove deletes name, a Root-relative path the plan chose, and refuses one
	// that leaves the two cache trees.
	Remove func(root, name string) error
}

// diskCacheTrim is the trim of the claim the run mounts, with the process
// filesystem behind its seams.
func diskCacheTrim(root string) *CacheTrim {
	return &CacheTrim{Root: root, CapBytes: sweepCacheCapBytes, Claim: diskClaim, Remove: removeSweepCacheFile}
}

// recordTrim trims the cache claim and writes a failure to the run's log
// directory. The result is already on disk, so the run's verdict stands
// whatever the trim does: a failure here is evidence beside the result, not a
// change to it (gt-hlo07.2).
func recordTrim(trim *CacheTrim, logDir string, stderr io.Writer) {
	if err := trimSweepCache(trim); err != nil {
		recordTrimError(logDir, err, stderr)
	}
}

// trimSweepCache brings the shared cache claim under its cap. The runner calls it
// at the end of every Job that writes its result, so a Job that fails before the
// result is written does not trim, and a claim already under the cap lists its
// files and removes nothing. It lists the two cache trees, plans the removals with
// cachevol.PlanTrim and removes what the plan names.
//
// A listing that is partial removes nothing: a cache tree the claim lacks, or a
// directory the walk cannot list, makes the cap unverifiable, so the error names
// that directory. A trim that is not wired, a cap of zero or less, a removal that
// fails and a plan that cannot reach the cap are all returned, so a claim the run
// could not trim is never reported as trimmed.
func trimSweepCache(trim *CacheTrim) error {
	switch {
	case trim == nil:
		return errors.New("the cache trim is not wired")
	case trim.Root == "":
		return errors.New("the cache trim names no claim root")
	case !filepath.IsAbs(trim.Root):
		return fmt.Errorf("the cache trim names the claim root %q: want an absolute path", trim.Root)
	case trim.CapBytes <= 0:
		return fmt.Errorf("the cache trim's CapBytes is %d: want a cap above zero", trim.CapBytes)
	case trim.Claim == nil:
		return errors.New("the cache trim has no claim")
	case trim.Remove == nil:
		return errors.New("the cache trim has no removal")
	}
	claim, err := trim.Claim(trim.Root)
	if err != nil {
		return fmt.Errorf("opening the cache claim %s: %w", trim.Root, err)
	}
	if claim == nil {
		// A claim that is not mounted has nothing to trim.
		return nil
	}
	listing, err := listClaim(claim)
	if err != nil {
		return fmt.Errorf("the cap on the cache claim %s is not verified, so nothing was removed: %w", trim.Root, err)
	}
	// Another Job on the node may be building against the module cache while
	// this run ends, so PlanTrim's cautious case leaves that cache alone.
	plan := cachevol.PlanTrim(listing, trim.CapBytes, true)
	var errs []error
	for _, removal := range plan.Removals {
		// One failed removal does not stop the rest: the claim is still over
		// its cap, and the ones that succeed bring it closer.
		if err := trim.Remove(trim.Root, removal.Path); err != nil {
			errs = append(errs, err)
		}
	}
	if plan.Overflow {
		errs = append(errs, fmt.Errorf("the cache claim is still %d bytes over its %d-byte cap after every removal the rules allow", plan.OverBytes, trim.CapBytes))
	}
	return errors.Join(errs...)
}

// recordTrimError writes a trim failure into the run's log directory, where the
// daemon and the operator read the run's other logs. A log that cannot be
// written falls back to stderr: a trim that failed is readable somewhere.
func recordTrimError(logDir string, err error, stderr io.Writer) {
	line := fmt.Sprintf("%s: cache trim: %s\n", name, oneLine(err.Error()))
	if mkErr := os.MkdirAll(logDir, 0o755); mkErr != nil { //nolint:gosec // G703: logDir is the run's own output directory, validated as absolute
		fmt.Fprintf(stderr, "%s", line)
		return
	}
	file, openErr := os.OpenFile(filepath.Join(logDir, cacheTrimLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: logDir is the run's own output directory
	if openErr != nil {
		fmt.Fprintf(stderr, "%s", line)
		return
	}
	defer file.Close()
	if _, writeErr := file.WriteString(line); writeErr != nil {
		fmt.Fprintf(stderr, "%s", line)
	}
}

// diskClaim is the claim at root as a read-only filesystem. A claim that is not
// mounted is nil, since a runner started outside the sweep Job has nothing to
// trim. A root that is a symbolic link or not a directory is refused; a cache
// tree that is a symbolic link is refused by listClaim.
func diskClaim(root string) (fs.FS, error) {
	info, err := os.Lstat(root) //nolint:gosec // G304: root is the claim mount this file names
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link, not the cache claim", root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	return os.DirFS(root), nil
}

// listClaim is the claim's cache files as PlanTrim's entries, named relative to
// the claim root. Any failure makes the listing partial and is returned. The
// claim root's own listing shows a cache tree that is a symbolic link as one,
// without following it out of the claim, so that refusal is made there.
func listClaim(claim fs.FS) ([]cachevol.Entry, error) {
	top, err := fs.ReadDir(claim, ".")
	if err != nil {
		return nil, fmt.Errorf("listing the claim root: %w", err)
	}
	for _, e := range top {
		if e.Type()&fs.ModeSymlink != 0 && (e.Name() == cachevol.SeedBuildCacheDir || e.Name() == cachevol.SeedModuleCacheDir) {
			return nil, fmt.Errorf("%s is a symbolic link, not a cache tree", e.Name())
		}
	}
	var listing []cachevol.Entry
	for _, c := range []struct {
		dir   string
		cache cachevol.Cache
	}{
		{dir: cachevol.SeedBuildCacheDir, cache: cachevol.BuildCache},
		{dir: cachevol.SeedModuleCacheDir, cache: cachevol.ModuleCache},
	} {
		files, err := listCacheTree(claim, c.dir, c.cache)
		if err != nil {
			return nil, err
		}
		listing = append(listing, files...)
	}
	return listing, nil
}

// listCacheTree is one cache tree's regular files as PlanTrim's entries. The
// claim is shared, so a file another Job removes between the directory read and
// its stat is skipped. Every other failure of the walk is returned, and a tree
// the claim lacks is an error rather than an empty tree, so a partial listing
// never plans a trim. A symbolic link in the tree is refused whole, because the
// walk does not follow links and PlanTrim's removal does not distinguish one from
// the file a build wrote.
func listCacheTree(claim fs.FS, dir string, cache cachevol.Cache) ([]cachevol.Entry, error) {
	var out []cachevol.Entry
	err := fs.WalkDir(claim, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("listing %s: %w", p, err)
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s in the cache is a symbolic link, not a cache file", p)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			// Another Job removed the file after the directory was read.
			return nil
		}
		if err != nil {
			return fmt.Errorf("listing %s: %w", p, err)
		}
		// Go touches an entry's mtime when it reuses it, so that is the walk's
		// last-use figure for both times: a noatime or relatime mount leaves
		// atime stale, and PlanTrim orders by the later of the two.
		used := info.ModTime()
		out = append(out, cachevol.Entry{
			Path:  p,
			Cache: cache,
			Size:  info.Size(),
			Atime: used,
			Mtime: used,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// removeSweepCacheFile deletes name, relative to the claim at root, through an
// os.Root so the kernel refuses a path that leaves the claim through a symlink.
// A name that is not a clean path under one of the two cache trees is refused
// before anything is opened: PlanTrim chose it, but only those trees are a
// trim's to remove.
func removeSweepCacheFile(root, name string) (err error) {
	if !cachevol.ClaimRelative(name) {
		return fmt.Errorf("the cache trim names %q: want a path a walk of the claim root gives, such as gocache/ab/cdef", name)
	}
	dir, _, ok := strings.Cut(name, "/")
	if !ok || (dir != cachevol.SeedBuildCacheDir && dir != cachevol.SeedModuleCacheDir) {
		return fmt.Errorf("the cache trim names %q: want a file under %s/ or %s/", name, cachevol.SeedBuildCacheDir, cachevol.SeedModuleCacheDir)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := r.Close(); err == nil {
			err = cerr
		}
	}()
	err = r.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		// Another run trimmed the file first; the goal is its absence.
		return nil
	}
	return err
}
