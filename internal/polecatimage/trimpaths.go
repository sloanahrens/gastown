package polecatimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// trimPathsFS is the filesystem a trim-paths run works on. A test answers the calls, so each
// failure is injected by the path it belongs to rather than by how many calls the implementation
// happens to make, as trimWalk does for the cache trim (gt-tpg0p.7).
type trimPathsFS struct {
	// names returns the entries of dir, the kubelet's ..data symlink and the directories it points
	// at included.
	names func(dir string) ([]string, error)
	// read returns the contents of one file.
	read func(name string) ([]byte, error)
	// remove deletes rel, a path below root that has no symlink in it, and refuses a path that
	// has come to leave root since the guard resolved it.
	remove func(root, rel string) error
	// resolve follows every symlink in a path and returns the real name.
	resolve func(name string) (string, error)
}

// diskTrimPathsFS is the filesystem of the container that runs the trim Job: the claim at its mount
// and the shard volumes beside it.
func diskTrimPathsFS() trimPathsFS {
	return trimPathsFS{
		names: func(dir string) ([]string, error) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			names := make([]string, len(entries))
			for i, entry := range entries {
				names[i] = entry.Name()
			}
			return names, nil
		},
		read:    os.ReadFile,
		remove:  removeInRoot,
		resolve: filepath.EvalSymlinks,
	}
}

// removeInRoot deletes rel through an os.Root opened on root, so the kernel-level walk refuses a
// directory that was swapped for a symlink out of the claim after trimTarget resolved it: the guard
// checks the path once, the removal enforces it at the moment of the unlink.
func removeInRoot(root, rel string) (err error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := r.Close(); err == nil {
			err = cerr
		}
	}()
	return r.Remove(rel)
}

// TrimPathsResult is what one trim-paths run did: how many of the listed files it removed, and how
// many were already gone.
type TrimPathsResult struct {
	Removed int
	Absent  int
}

// RemoveTrimPaths removes every path the shard mounts under listDir list, relative to root.
//
// The list is read and checked whole before anything is removed, so a shard that cannot be read or
// an entry that could name a file outside the claim leaves the claim untouched. A listed file that
// is already gone counts as absent rather than as a failure, since the run's goal is the file's
// absence; a removal that fails does not stop the rest of the list, and the count comes back in the
// error.
func RemoveTrimPaths(root, listDir string, shards int, w trimPathsFS) (TrimPathsResult, error) {
	if shards < 1 {
		return TrimPathsResult{}, fmt.Errorf("the trim run names %d shards: want the count of mounts the Job made", shards)
	}
	list, err := trimShardPaths(listDir, shards, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	realRoot, err := resolveClaim(root, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	targets, gone, err := trimTargets(realRoot, list, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	return removeTargets(realRoot, targets, gone, w)
}

// RemoveTrimPathsFromClaim removes every path the claim-carried list holds, relative to root.
//
// listPath is the claim-relative file the Job's init container wrote into the claim, and digestPath
// the file it wrote the list's digest to beside it. The list must hash to that digest, so a list this
// Job did not write — one another trim left, or one a sandbox replaced after the init container ran —
// fails the run rather than being trimmed. Both files must resolve inside the claim, and the list is
// read and checked whole before anything is removed, as the shard route's list is. A list that is
// absent, outside the claim, unreadable, not an array of paths, or empty fails the run rather than
// reporting a success that removed nothing.
//
// The list and its digest are consumed before the first removal, so a Job that runs again finds no
// list and fails, rather than trimming the paths a run already removed.
func RemoveTrimPathsFromClaim(root, listPath, digestPath string, w trimPathsFS) (TrimPathsResult, error) {
	if !claimRelative(listPath) {
		return TrimPathsResult{}, fmt.Errorf("the trim list file %q: want a path inside the claim, such as %s", listPath, cachevol.TrimClaimListPath)
	}
	if !claimRelative(digestPath) {
		return TrimPathsResult{}, fmt.Errorf("the trim list digest file %q: want a path inside the claim, such as %s", digestPath, cachevol.TrimClaimDigestPath)
	}
	realRoot, err := resolveClaim(root, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	digestFile, want, err := trimListDigest(realRoot, digestPath, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	listFile, err := trimListFile(realRoot, listPath, trimListWhat{
		what:   "trim list",
		absent: "the Job's init container writes it before the trim container runs, and a run consumes it before its first removal, so a Job that ran already finds none",
	}, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	data, err := w.read(listFile)
	if err != nil {
		return TrimPathsResult{}, fmt.Errorf("reading the trim list %s: %w", listPath, err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
		return TrimPathsResult{}, fmt.Errorf("the trim list %s does not match the digest its init container wrote beside it at %s: a list this Job did not write", listPath, digestPath)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return TrimPathsResult{}, fmt.Errorf("the trim list %s is not a JSON array of paths", listPath)
	}
	if len(list) == 0 {
		return TrimPathsResult{}, fmt.Errorf("the trim list %s names no path: a claim-carried trim with nothing to remove would report a success that removed nothing", listPath)
	}
	targets, gone, err := trimTargets(realRoot, list, w)
	if err != nil {
		return TrimPathsResult{}, err
	}
	for _, file := range []string{listFile, digestFile} {
		rel, err := filepath.Rel(realRoot, file)
		if err == nil {
			err = w.remove(realRoot, rel)
		}
		if err != nil {
			return TrimPathsResult{}, fmt.Errorf("consuming the trim list before the removals: %w", err)
		}
	}
	return removeTargets(realRoot, targets, gone, w)
}

// trimListDigest is the digest the init container wrote beside the list at digestPath, as a name
// inside the claim, together with the file to consume.
func trimListDigest(realRoot, digestPath string, w trimPathsFS) (string, string, error) {
	file, err := trimListFile(realRoot, digestPath, trimListWhat{
		what:   "trim list digest",
		absent: "the Job's init container writes it beside the list, and a run consumes it with the list",
	}, w)
	if err != nil {
		return "", "", err
	}
	data, err := w.read(file)
	if err != nil {
		return "", "", fmt.Errorf("reading the trim list digest %s: %w", digestPath, err)
	}
	digest := strings.TrimSpace(string(data))
	sum, err := hex.DecodeString(digest)
	if err != nil || len(sum) != sha256.Size {
		return "", "", fmt.Errorf("the trim list digest %s is not a SHA-256 hex digest: want the digest the writer wrote beside the list", digestPath)
	}
	return file, digest, nil
}

// resolveClaim is the claim root with every symlink in it followed: the boundary each target is
// checked against.
func resolveClaim(root string, w trimPathsFS) (string, error) {
	realRoot, err := w.resolve(root)
	if err != nil {
		return "", fmt.Errorf("resolving the claim root %s: %w", root, err)
	}
	return realRoot, nil
}

// trimListWhat names one of the two files a claim-carried run reads, and why a file that is not there
// is not there: a run that finds one missing has to say which case it met, since a consumed list and a
// Job that never had a writer look the same on disk.
type trimListWhat struct {
	what   string
	absent string
}

// trimListFile is the claim-relative file rel as a name inside the claim. Its directory is resolved
// and must lie inside the claim; the file itself must be the file a writer put there, not a link, since
// a link would have the read and the consuming removal reach whatever it points at. A file the writer
// never put in the claim is refused as absent.
func trimListFile(realRoot, name string, f trimListWhat, w trimPathsFS) (string, error) {
	rel := filepath.FromSlash(name)
	dir, err := w.resolve(filepath.Join(realRoot, filepath.Dir(rel)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("the %s %s is not in the claim: %s", f.what, name, f.absent)
		}
		return "", fmt.Errorf("resolving the directory of the %s %s: %w", f.what, name, err)
	}
	if !withinClaim(realRoot, dir) {
		return "", fmt.Errorf("the %s %s sits in %s, outside the claim at %s", f.what, name, dir, realRoot)
	}
	file := filepath.Join(dir, filepath.Base(rel))
	resolved, err := w.resolve(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("the %s %s is not in the claim: %s", f.what, name, f.absent)
		}
		return "", fmt.Errorf("resolving the %s %s: %w", f.what, name, err)
	}
	if resolved != file {
		return "", fmt.Errorf("the %s %s is a link to %s: want the file a writer put in the claim", f.what, name, resolved)
	}
	return file, nil
}

// trimTargets resolves every entry of list to the path to remove, inside realRoot, before any removal,
// so a list that names a file outside the claim is refused whole. It returns the targets and how many
// entries were already gone. realRoot is the claim root with its symlinks followed.
func trimTargets(realRoot string, list []string, w trimPathsFS) ([]string, int, error) {
	targets := make([]string, 0, len(list))
	var gone int
	for _, entry := range list {
		target, err := trimTarget(realRoot, entry, w)
		switch {
		case errors.Is(err, errTrimGone):
			gone++
			continue
		case err != nil:
			return nil, 0, err
		}
		targets = append(targets, target)
	}
	return targets, gone, nil
}

// removeTargets removes the targets trimTargets resolved, and counts the entries it found gone as
// absent. A removal that fails does not stop the rest, and the count comes back in the error.
func removeTargets(realRoot string, targets []string, gone int, w trimPathsFS) (TrimPathsResult, error) {
	res := TrimPathsResult{Absent: gone}
	failed, firstPath := 0, ""
	var firstErr error
	for _, target := range targets {
		rel, err := filepath.Rel(realRoot, target)
		if err == nil {
			err = w.remove(realRoot, rel)
		}
		switch {
		case err == nil:
			res.Removed++
		case errors.Is(err, fs.ErrNotExist):
			res.Absent++
		default:
			if firstErr == nil {
				firstPath, firstErr = target, err
			}
			failed++
		}
	}
	if failed > 0 {
		return res, fmt.Errorf("%d of %d listed paths could not be removed, the first %s: %w", failed, len(targets), firstPath, firstErr)
	}
	return res, nil
}

// trimShardPaths is every path the mounted shards list, in shard order: shard i is the directory
// <listDir>/<i>, one per ConfigMap the Job mounted.
func trimShardPaths(listDir string, shards int, w trimPathsFS) ([]string, error) {
	var all []string
	for i := 0; i < shards; i++ {
		dir := filepath.Join(listDir, strconv.Itoa(i))
		names, err := w.names(dir)
		if err != nil {
			return nil, fmt.Errorf("reading the shard %s: %w", dir, err)
		}
		file, err := shardFile(dir, names)
		if err != nil {
			return nil, err
		}
		data, err := w.read(file)
		if err != nil {
			return nil, fmt.Errorf("reading the shard %s: %w", dir, err)
		}
		var paths []string
		if err := json.Unmarshal(data, &paths); err != nil {
			return nil, fmt.Errorf("the shard %s is not a JSON array of paths", dir)
		}
		all = append(all, paths...)
	}
	return all, nil
}

// shardFile is the one file a shard directory holds. A mounted ConfigMap also carries the kubelet's
// ..data symlink and the directories it points at, which are skipped; any other count means the
// mount is not the one this Job built, and the run refuses it rather than trimming part of a list.
func shardFile(dir string, names []string) (string, error) {
	var files []string
	for _, name := range names {
		if strings.HasPrefix(name, "..") {
			continue
		}
		files = append(files, name)
	}
	if len(files) != 1 {
		return "", fmt.Errorf("the shard %s holds %d files: want the one file its single ConfigMap key mounts", dir, len(files))
	}
	return filepath.Join(dir, files[0]), nil
}

// errTrimGone is trimTarget's answer for a listed file whose directory is already gone: the trim's
// goal is the file's absence, so the caller counts it apart from a removal rather than as a failure.
var errTrimGone = errors.New("the listed path is already gone")

// trimTarget is the path to remove for one listed entry: the claim root joined with a path a walk of
// the claim gives, through a directory that resolves inside the claim. A directory that is gone is
// errTrimGone; one that resolves outside the claim, by a symlink no walk of the claim would have
// followed, is refused rather than followed.
func trimTarget(realRoot, entry string, w trimPathsFS) (string, error) {
	if !claimRelative(entry) {
		return "", fmt.Errorf("the trim list names %q: want a path a walk of the claim root gives, such as gocache/ab/cdef", entry)
	}
	dir, err := w.resolve(filepath.Join(realRoot, filepath.Dir(entry)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errTrimGone
		}
		return "", fmt.Errorf("resolving the directory of %q: %w", entry, err)
	}
	if !withinClaim(realRoot, dir) {
		return "", fmt.Errorf("the trim list names %q: its directory resolves to %s, outside the claim at %s", entry, dir, realRoot)
	}
	return filepath.Join(dir, path.Base(entry)), nil
}

// claimRelative reports whether p is a path a walk of the claim gives: relative to the claim root,
// already clean, and below it rather than at it or above it.
func claimRelative(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	first, _, _ := strings.Cut(p, "/")
	return first != "." && first != ".."
}

// withinClaim reports whether dir is root itself or a directory below it.
func withinClaim(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
