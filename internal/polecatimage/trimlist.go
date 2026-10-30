package polecatimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// A removal list past one set of ConfigMap shards is carried in the claim itself (gt-2rq36). This
// file is the writer: the claim-carried trim Job's init container plans PlanTrim's list from the
// claim and writes it into the claim, and the Job's trim container then reads it with trim-paths
// --list-file (trimpaths.go). The Job is the whole route, as a shard trim's Job and its ConfigMaps
// are: nothing outside the tree has to write the list before the Job applies.

// TrimListFiles is the claim the list writer touches, by names relative to the claim root. A test
// answers the calls, so each failure is injected at the call it belongs to; the claim itself answers
// through an os.Root (rootTrimListFiles), so a symlink planted in the shared claim cannot take a call
// out of it.
type TrimListFiles interface {
	MkdirAll(name string, perm os.FileMode) error
	Remove(name string) error
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
	// Free is the room the filesystem holding the claim can still take, in bytes.
	Free() (int64, error)
}

// rootTrimListFiles is TrimListFiles on the claim: every name resolves inside the root, and a name
// that would leave it through a symlink is refused by the walk the kernel makes. path is the claim
// root as the filesystem knows it, which the free-space reading needs.
type rootTrimListFiles struct {
	root *os.Root
	path string
}

func (f rootTrimListFiles) MkdirAll(name string, perm os.FileMode) error {
	return f.root.MkdirAll(name, perm)
}

func (f rootTrimListFiles) Remove(name string) error { return f.root.Remove(name) }

func (f rootTrimListFiles) WriteFile(name string, data []byte, perm os.FileMode) error {
	return f.root.WriteFile(name, data, perm)
}

func (f rootTrimListFiles) Rename(oldpath, newpath string) error {
	return f.root.Rename(oldpath, newpath)
}

func (f rootTrimListFiles) Free() (int64, error) { return filesystemFree(f.path) }

const (
	// trimListDirPerm is the mode of the directory under the claim the list sits in.
	trimListDirPerm = 0o755
	// trimListFilePerm is the mode of the written list: the trim container reads it as the user the
	// claim belongs to.
	trimListFilePerm = 0o644
)

// WriteTrimList writes paths, PlanTrim's removal list, into the claim files answers for, where a
// claim-carried trim reads it, and returns the list's SHA-256 digest, the value the trim container
// checks the list against.
//
// The list and its digest are written under temporary names and renamed into place, the digest
// first, so a write that fails part way leaves no list: a reader that finds the list always finds the
// digest written for it. A path a walk of the claim could not give is refused whole rather than
// written, since the container would refuse the list it read. The claim is measured first, so a claim
// with no room for the list and its temporary fails with the room it has rather than part way through
// the write.
func WriteTrimList(paths []string, files TrimListFiles) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("the trim list names no path: a claim-carried trim with nothing to remove would apply as a success")
	}
	for _, p := range paths {
		if !claimRelative(p) {
			return "", fmt.Errorf("the trim list names %q: want a path a walk of the claim root gives, such as gocache/ab/cdef", p)
		}
	}
	// Encoding a slice of strings cannot fail, so the error is dropped.
	data, _ := json.Marshal(paths)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	// The list and its digest are each on disk twice while the second is renamed, so the writer needs
	// room for both of each. The list is written into a claim that is over its policy cap, which is
	// not the filesystem's capacity; this is where a claim with no room says so.
	free, err := files.Free()
	if err != nil {
		return "", fmt.Errorf("measuring the room the claim has for the trim list: %w", err)
	}
	if need := 2 * int64(len(data)+len(digest)); free < need {
		return "", fmt.Errorf("the claim has %d bytes free and the trim list needs %d: want room in the claim for the list and its digest", free, need)
	}

	if err := files.MkdirAll(cachevol.TrimClaimListDir, trimListDirPerm); err != nil {
		return "", fmt.Errorf("creating the trim list directory %s: %w", cachevol.TrimClaimListDir, err)
	}
	dest := cachevol.TrimClaimListPath
	tmp := dest + ".tmp"
	digestDest := cachevol.TrimClaimDigestPath
	digestTmp := digestDest + ".tmp"
	// A temporary left by an earlier write is removed before this one is made: a symlink at its name
	// would otherwise take the write to whatever it points at, even inside the claim.
	for _, name := range []string{tmp, digestTmp} {
		if err := files.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("clearing the stale trim list %s: %w", name, err)
		}
	}
	if err := files.WriteFile(tmp, data, trimListFilePerm); err != nil {
		return "", fmt.Errorf("writing the trim list under %s: %w", cachevol.TrimClaimListDir, err)
	}
	if err := files.WriteFile(digestTmp, []byte(digest+"\n"), trimListFilePerm); err != nil {
		return "", fmt.Errorf("writing the trim list digest under %s: %w", cachevol.TrimClaimListDir, err)
	}
	if err := files.Rename(digestTmp, digestDest); err != nil {
		return "", fmt.Errorf("moving the trim list digest into place under %s: %w", cachevol.TrimClaimListDir, err)
	}
	if err := files.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("moving the trim list into place under %s: %w", cachevol.TrimClaimListDir, err)
	}
	return digest, nil
}

// writeTrimListInto writes paths into the claim at root, through an os.Root on it, so every name the
// writer makes is answered inside the claim, and returns the list's digest.
func writeTrimListInto(root string, paths []string) (digest string, err error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("opening the claim %s: %w", root, err)
	}
	defer func() {
		if cerr := r.Close(); err == nil {
			err = cerr
		}
	}()
	return WriteTrimList(paths, rootTrimListFiles{root: r, path: root})
}

// TrimListPaths is PlanTrim's removal list for the claim at root, the list a claim-carried trim's init
// container writes into the claim before the Job's trim container reads it.
//
// The plan's inputs are the caller's: the cap the trim brings the claim under and whether a sandbox is
// running. The listing is of the claim as it is when the Job runs, so a claim that came under its cap,
// or lost the module cache to a running sandbox, after the caller built the Job is refused here rather
// than trimmed with nothing to do.
func TrimListPaths(root string, capBytes int64, sandboxesRunning bool, walk fileWalk) ([]string, error) {
	listing, err := claimListing(root, walk)
	if err != nil {
		return nil, err
	}
	plan := cachevol.PlanTrim(listing, capBytes, sandboxesRunning)
	if len(plan.Removals) == 0 {
		if plan.Overflow {
			return nil, fmt.Errorf("the claim at %s is %d bytes over its %d-byte cap and no removal the rules allow closes it", root, plan.OverBytes, capBytes)
		}
		return nil, fmt.Errorf("the claim at %s is under its %d-byte cap: nothing to trim", root, capBytes)
	}
	paths := make([]string, 0, len(plan.Removals))
	for _, r := range plan.Removals {
		paths = append(paths, r.Path)
	}
	return paths, nil
}

// The names trim-list prints for the two caches a listing entry can belong to. A reader maps them
// back to cachevol.BuildCache and cachevol.ModuleCache; PlanTrim chooses what to remove by that
// cache, so a name outside this set is refused rather than read as the build cache.
const (
	TrimListCacheBuild  = "build"
	TrimListCacheModule = "module"
)

// trimListEntry is one claim file as the trim-list helper prints it: the path a trim removes, the
// cache it belongs to, its size, and the last use PlanTrim orders by.
type trimListEntry struct {
	Path  string    `json:"path"`
	Cache string    `json:"cache"`
	Size  int64     `json:"size"`
	Atime time.Time `json:"atime"`
	Mtime time.Time `json:"mtime"`
}

// trimListing returns the claim at root as the listing a caller builds a trim manifest from: the
// files claimListing names, with the cache each belongs to spelled as trim-list prints it.
//
// It is the read side of the trim: a caller that does not mount the claim itself reads this through
// the helper and hands the entries to cachevol.BuildTrimManifest, which plans the removals and picks
// the route they travel by.
func trimListing(root string, walk fileWalk) ([]trimListEntry, error) {
	listing, err := claimListing(root, walk)
	if err != nil {
		return nil, err
	}
	out := make([]trimListEntry, 0, len(listing))
	for _, e := range listing {
		// claimListing names one of the two caches; the module cache is the one
		// its walk sets explicitly, and everything else is the build cache's tree.
		cache := TrimListCacheBuild
		if e.Cache == cachevol.ModuleCache {
			cache = TrimListCacheModule
		}
		out = append(out, trimListEntry{Path: e.Path, Cache: cache, Size: e.Size, Atime: e.Atime, Mtime: e.Mtime})
	}
	return out, nil
}

// claimListing is the claim's cache files as PlanTrim's entries: every regular file under GOCACHE and
// GOMODCACHE, named by the claim-relative path a trim removes. The rest of the claim is not the trim's
// to size or remove, and the list's own directory sits outside both trees, so a trim never names the
// list it is reading.
//
// Entry's times are both the file's last use, the later of its access and modification times: that is
// the one figure PlanTrim orders by, and the walk is the same one the cache trim takes.
func claimListing(root string, walk fileWalk) ([]cachevol.Entry, error) {
	// The claim root itself is checked first: a root that is gone or is a symlink would have the walk
	// of both cache trees answer "no files", which plans a trim of nothing for a claim that is there.
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("the claim at %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("the claim at %s is a symlink, not a directory", root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("the claim at %s is not a directory", root)
	}

	var listing []cachevol.Entry
	for _, c := range []struct {
		dir   string
		cache cachevol.Cache
	}{
		{dir: cachevol.SeedBuildCacheDir, cache: cachevol.BuildCache},
		{dir: cachevol.SeedModuleCacheDir, cache: cachevol.ModuleCache},
	} {
		files, err := listCacheFilesWith(walk, filepath.Join(root, c.dir))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("listing %s: %w", c.dir, err)
		}
		for _, f := range files {
			rel, err := filepath.Rel(root, f.path)
			if err != nil {
				return nil, fmt.Errorf("the claim path %s: %w", f.path, err)
			}
			listing = append(listing, cachevol.Entry{
				Path:  filepath.ToSlash(rel),
				Cache: c.cache,
				Size:  f.size,
				Atime: f.used,
				Mtime: f.used,
			})
		}
	}
	return listing, nil
}
