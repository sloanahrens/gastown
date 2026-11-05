package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
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
// listing of its two cache trees, and the removal of one planned path. A test
// points Root at a t.TempDir tree and scripts either seam, so no test reads or
// removes a real cache (gt-hlo07.2).
type CacheTrim struct {
	// Root is the claim's mount, holding the GOCACHE and GOMODCACHE trees.
	Root string
	// CapBytes is the size the claim is brought under; zero takes
	// sweepCacheCapBytes.
	CapBytes int64
	// List is the claim's cache files as PlanTrim's entries, named relative to
	// Root.
	List func(root string) ([]cachevol.Entry, error)
	// Remove deletes name, a Root-relative path the plan chose, and refuses one
	// that leaves the two cache trees.
	Remove func(root, name string) error
}

// diskCacheTrim is the trim of the claim the run mounts, with the process
// filesystem behind its seams.
func diskCacheTrim(root string) *CacheTrim {
	return &CacheTrim{Root: root, List: listSweepCache, Remove: removeSweepCacheFile}
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

// trimSweepCache brings the shared cache claim under its cap: it lists the two
// cache trees, plans the removals with cachevol.PlanTrim and removes what the
// plan names. A trim that is not wired, a listing that cannot be read, a
// removal that fails and a plan that cannot reach the cap are all returned, so
// a claim the run could not trim is never reported as trimmed.
func trimSweepCache(trim *CacheTrim) error {
	switch {
	case trim == nil:
		return errors.New("the cache trim is not wired")
	case trim.Root == "":
		return errors.New("the cache trim names no claim root")
	case !filepath.IsAbs(trim.Root):
		return fmt.Errorf("the cache trim names the claim root %q: want an absolute path", trim.Root)
	case trim.CapBytes < 0:
		return fmt.Errorf("the cache trim names a %d-byte cap", trim.CapBytes)
	case trim.List == nil:
		return errors.New("the cache trim has no listing")
	case trim.Remove == nil:
		return errors.New("the cache trim has no removal")
	}
	capBytes := trim.CapBytes
	if capBytes == 0 {
		capBytes = sweepCacheCapBytes
	}
	listing, err := trim.List(trim.Root)
	if err != nil {
		return fmt.Errorf("listing the cache claim %s: %w", trim.Root, err)
	}
	// Another Job on the node may be building against the module cache while
	// this run ends, so PlanTrim's cautious case leaves that cache alone.
	plan := cachevol.PlanTrim(listing, capBytes, true)
	var errs []error
	for _, removal := range plan.Removals {
		// One failed removal does not stop the rest: the claim is still over
		// its cap, and the ones that succeed bring it closer.
		if err := trim.Remove(trim.Root, removal.Path); err != nil {
			errs = append(errs, err)
		}
	}
	if plan.Overflow {
		errs = append(errs, fmt.Errorf("the cache claim is still %d bytes over its %d-byte cap after every removal the rules allow", plan.OverBytes, capBytes))
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

// listSweepCache is the claim at root as PlanTrim's entries: every regular file
// under GOCACHE and GOMODCACHE, named relative to root. A claim that is not
// mounted lists nothing, and any other failure is returned, so a claim the run
// cannot read is never trimmed as if it were empty.
func listSweepCache(root string) ([]cachevol.Entry, error) {
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
	var listing []cachevol.Entry
	for _, c := range []struct {
		dir   string
		cache cachevol.Cache
	}{
		{dir: cachevol.SeedBuildCacheDir, cache: cachevol.BuildCache},
		{dir: cachevol.SeedModuleCacheDir, cache: cachevol.ModuleCache},
	} {
		files, err := listCacheTree(root, c.dir, c.cache)
		if err != nil {
			return nil, err
		}
		listing = append(listing, files...)
	}
	return listing, nil
}

// listCacheTree is one cache tree's regular files as PlanTrim's entries. A tree
// the run has not written yet lists nothing; a symbolic link in it is refused
// whole, because a walk does not follow links and PlanTrim's removal does not
// distinguish one from the file a build wrote.
func listCacheTree(root, dir string, cache cachevol.Cache) ([]cachevol.Entry, error) {
	var out []cachevol.Entry
	treeRoot := filepath.Join(root, dir)
	err := filepath.Walk(treeRoot, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s in the cache is a symbolic link, not a cache file", p)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		// Every child the walk visits lies under treeRoot, so its own prefix is
		// trimmed rather than filepath.Rel, whose error no such path takes.
		rel := strings.TrimPrefix(p, treeRoot+string(filepath.Separator))
		// Go touches an entry's mtime when it reuses it, so that is the walk's
		// last-use figure for both times: a noatime or relatime mount leaves
		// atime stale, and PlanTrim orders by the later of the two.
		used := info.ModTime()
		out = append(out, cachevol.Entry{
			Path:  path.Join(dir, filepath.ToSlash(rel)),
			Cache: cache,
			Size:  info.Size(),
			Atime: used,
			Mtime: used,
		})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// removeSweepCacheFile deletes name, relative to the claim at root, through an
// os.Root so the kernel refuses a path that leaves the claim through a symlink.
// A name that is not a clean path under one of the two cache trees is refused
// before anything is opened: PlanTrim chose it, but only those trees are a
// trim's to remove.
func removeSweepCacheFile(root, name string) (err error) {
	if !cacheRelative(name) {
		return fmt.Errorf("the cache trim names %q: want a clean path under %s/ or %s/", name, cachevol.SeedBuildCacheDir, cachevol.SeedModuleCacheDir)
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

// cacheRelative reports whether p is a path a walk of the claim gives: relative
// to the claim root, already clean, and below it rather than at it or above it.
// It is cachevol's rule for a path its planner may name (claimRelative in
// internal/polecat/cachevol/trimjob.go), which keeps a ".." component from
// reaching the removal wherever it sits.
func cacheRelative(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	first, _, _ := strings.Cut(p, "/")
	return first != "." && first != ".."
}
