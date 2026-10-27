package polecatimage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// cacheTrimDirs are the subtrees a trim empties, in the order it empties them:
// Go's build cache first, and the module cache only when the build cache alone
// could not bring the tree under the cap (gt-3g7p2).
var cacheTrimDirs = []string{"go-build", filepath.Join("go", "pkg", "mod")}

// cacheFile is one file a trim may delete: where it is, how big it is, and when
// it was last used.
type cacheFile struct {
	path string
	size int64
	used time.Time
}

// trimWalk is the filesystem a trim works on. A test answers both calls so a
// failure is injected by the path it belongs to, not by how many walks the
// implementation happens to make (gt-tpg0p.7).
type trimWalk struct {
	// list returns the regular files under dir, without following a symlinked
	// directory. A dir that does not exist answers fs.ErrNotExist.
	list func(dir string) ([]cacheFile, error)
	// remove deletes one path.
	remove func(path string) error
}

// diskTrimWalk is the walk over a real filesystem.
func diskTrimWalk() trimWalk {
	return trimWalk{list: listCacheFiles, remove: os.Remove}
}

// TrimResult is what one trim found and what it left: the tree's size in bytes
// before and after.
type TrimResult struct {
	Before int64
	After  int64
}

// TrimCacheTree brings root back under cap by deleting the least recently used
// files under cacheTrimDirs, in order, until the tree fits.
//
// It follows no symlink: a symlinked directory is left alone rather than
// treated as part of the tree, and a root that is itself a symlink is refused,
// so a trim never removes a path outside the tree it was given. The error names
// the directory whose listing failed; a file that could not be removed is left
// behind, and the re-measured size is what the caller reports (gt-3g7p2).
func TrimCacheTree(root string, cap int64, w trimWalk) (TrimResult, error) {
	before, err := measureCacheTree(w, root)
	if err != nil {
		return TrimResult{}, err
	}
	res := TrimResult{Before: before, After: before}
	for _, sub := range cacheTrimDirs {
		if res.After <= cap {
			break
		}
		dir := filepath.Join(root, sub)
		if err := trimCacheDir(w, dir, res.After-cap); err != nil {
			return res, fmt.Errorf("listing %s: %w", dir, err)
		}
		after, err := measureCacheTree(w, root)
		if err != nil {
			return res, err
		}
		res.After = after
	}
	return res, nil
}

// measureCacheTree is the sum of the regular files under root, the figure the
// cap applies to.
func measureCacheTree(w trimWalk, root string) (int64, error) {
	files, err := w.list(root)
	if err != nil {
		return 0, fmt.Errorf("measuring %s: %w", root, err)
	}
	var total int64
	for _, f := range files {
		total += f.size
	}
	return total, nil
}

// trimCacheDir deletes files under dir oldest-use first until need bytes are
// freed. A directory that is not there is nothing to trim. The caller measures
// the tree again rather than trusting this arithmetic, so a file it could not
// remove leaves the next trim to the next run.
func trimCacheDir(w trimWalk, dir string, need int64) error {
	files, err := w.list(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if need <= 0 {
			break
		}
		if err := w.remove(f.path); err != nil {
			continue
		}
		need -= f.size
	}
	return nil
}

// fileWalk is the directory walk a listing walks with; a test answers it so the
// walk's own failure is driven rather than assumed.
type fileWalk func(root string, fn filepath.WalkFunc) error

// listCacheFiles returns the regular files under dir with their sizes and last
// use, walking with filepath.Walk.
func listCacheFiles(dir string) ([]cacheFile, error) {
	return listCacheFilesWith(filepath.Walk, dir)
}

// listCacheFilesWith is listCacheFiles over walk.
//
// A symlink is neither listed nor descended into, so the walk cannot leave the
// tree it was given, and a dir that is a symlink is refused outright: a cache
// volume is mounted as a directory, and trimming through a link would delete
// the files of wherever it points.
func listCacheFilesWith(walk fileWalk, dir string) ([]cacheFile, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink, not a directory", dir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	var files []cacheFile
	err = walk(dir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, cacheFile{path: path, size: info.Size(), used: lastUsed(info)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
