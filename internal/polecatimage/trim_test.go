package polecatimage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testTime is the stamp a planted cache file is given before a case moves it.
var testTime = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// plantCacheFile writes a file of size bytes with its access and modification
// times both set to when, returning its path.
func plantCacheFile(t *testing.T, path string, size int64, when time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// existsFile reports whether path is still there.
func existsFile(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestTrimCacheTreeLeavesATreeUnderTheCapAlone is the no-work path: nothing is
// removed when the sum of the files fits.
func TestTrimCacheTreeLeavesATreeUnderTheCapAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := plantCacheFile(t, filepath.Join(root, "go-build", "a"), 100, testTime)
	res, err := TrimCacheTree(root, 1000, diskTrimWalk())
	if err != nil {
		t.Fatalf("TrimCacheTree() failed: %v", err)
	}
	if res.Before != 100 || res.After != 100 {
		t.Errorf("TrimCacheTree() = %+v, want 100 before and after", res)
	}
	if !existsFile(file) {
		t.Error("a trim under the cap removed a file")
	}
}

// TestTrimCacheTreeEmptiesTheBuildCacheFirst is the order the subtrees are
// emptied in: go/pkg/mod keeps its files while go-build can still free enough.
func TestTrimCacheTreeEmptiesTheBuildCacheFirst(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	build := plantCacheFile(t, filepath.Join(root, "go-build", "a"), 400, testTime)
	buildOld := plantCacheFile(t, filepath.Join(root, "go-build", "b"), 400, testTime.Add(-time.Hour))
	module := plantCacheFile(t, filepath.Join(root, "go", "pkg", "mod", "c"), 400, testTime)
	res, err := TrimCacheTree(root, 800, diskTrimWalk())
	if err != nil {
		t.Fatalf("TrimCacheTree() failed: %v", err)
	}
	if res.Before != 1200 || res.After != 800 {
		t.Errorf("TrimCacheTree() = %+v, want 1200 before and 800 after", res)
	}
	if !existsFile(module) {
		t.Error("the module cache was trimmed before the build cache could free enough")
	}
	if existsFile(buildOld) || !existsFile(build) {
		t.Error("the build cache did not delete its least recently used file first")
	}
}

// TestTrimCacheTreeFallsBackToTheModuleCache is the second pass: the build cache
// alone cannot reach the cap, so go/pkg/mod pays the rest.
func TestTrimCacheTreeFallsBackToTheModuleCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, "go-build", "a"), 400, testTime)
	module := plantCacheFile(t, filepath.Join(root, "go", "pkg", "mod", "c"), 400, testTime)
	res, err := TrimCacheTree(root, 100, diskTrimWalk())
	if err != nil {
		t.Fatalf("TrimCacheTree() failed: %v", err)
	}
	if existsFile(module) {
		t.Error("the module cache survived a trim that had to reach its subtree")
	}
	if res.After >= res.Before {
		t.Errorf("TrimCacheTree() = %+v, want a smaller tree", res)
	}
}

// TestTrimCacheTreeStampsAFileWithTheLaterOfItsTimes drives both directions of
// the LRU rule: a fresh mtime rescues a stale atime, and a fresh atime rescues a
// stale mtime, because Go touches the mtime of an entry it reuses and a noatime
// mount leaves the atime stale (gt-3g7p2).
func TestTrimCacheTreeStampsAFileWithTheLaterOfItsTimes(t *testing.T) {
	t.Parallel()
	if got := lastUsedOf(t, writeWithTimes(t, testTime, testTime.Add(-time.Hour))); !got.Equal(testTime) {
		t.Errorf("lastUsed = %v, want the later mtime %v", got, testTime)
	}
	if got := lastUsedOf(t, writeWithTimes(t, testTime.Add(-time.Hour), testTime)); !got.Equal(testTime) {
		t.Errorf("lastUsed = %v, want the later atime %v", got, testTime)
	}
}

// writeWithTimes plants one file stamped with each time and answers its info.
func writeWithTimes(t *testing.T, atime, mtime time.Time) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, atime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func lastUsedOf(t *testing.T, info os.FileInfo) time.Time {
	t.Helper()
	return lastUsed(info)
}

// TestTrimCacheTreeNeverRemovesOutsideItsTree is the boundary: a file at the
// root, a symlink to a file outside and a symlink to a directory outside all
// survive a trim that empties both subtrees, and so does what the symlinks
// name.
func TestTrimCacheTreeNeverRemovesOutsideItsTree(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	outsideFile := plantCacheFile(t, filepath.Join(outside, "keep"), 100, testTime)
	root := t.TempDir()
	rootFile := plantCacheFile(t, filepath.Join(root, "root-file"), 100, testTime)
	plantCacheFile(t, filepath.Join(root, "go-build", "a"), 400, testTime)
	plantCacheFile(t, filepath.Join(root, "go", "pkg", "mod", "c"), 400, testTime)
	if err := os.Symlink(outsideFile, filepath.Join(root, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "go-build", "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := TrimCacheTree(root, 100, diskTrimWalk()); err != nil {
		t.Fatalf("TrimCacheTree() failed: %v", err)
	}
	for _, path := range []string{outsideFile, rootFile, filepath.Join(root, "link-to-file"), filepath.Join(root, "go-build", "link-to-dir")} {
		if !existsFile(path) {
			t.Errorf("the trim removed %s", path)
		}
	}
}

// TestTrimCacheTreeRefusesASymlinkedRoot is the other half of the boundary: a
// root that is a symlink is refused rather than trimmed through, because the
// files behind it belong to another tree.
func TestTrimCacheTreeRefusesASymlinkedRoot(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	file := plantCacheFile(t, filepath.Join(outside, "go-build", "a"), 400, testTime)
	root := filepath.Join(t.TempDir(), "cache")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	_, err := TrimCacheTree(root, 100, diskTrimWalk())
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("TrimCacheTree() = %v, want the symlinked root refused", err)
	}
	if !existsFile(file) {
		t.Error("a refused trim removed a file behind the symlinked root")
	}
}

// TestTrimCacheTreeNamesAMeasureItCannotRepeat is the second measurement: the
// tree was read, files were removed, and then it could not be read again, so the
// trim reports that rather than a size it never saw (gt-tpg0p.7).
func TestTrimCacheTreeNamesAMeasureItCannotRepeat(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, "go-build", "a"), 400, testTime)
	walk := diskTrimWalk()
	removed := false
	remove := walk.remove
	walk.remove = func(path string) error {
		removed = true
		return remove(path)
	}
	list := walk.list
	walk.list = func(dir string) ([]cacheFile, error) {
		if removed && dir == root {
			return nil, errors.New("input/output error")
		}
		return list(dir)
	}
	_, err := TrimCacheTree(root, 100, walk)
	if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("TrimCacheTree() = %v, want the failed second measurement of %s", err, root)
	}
}

// TestListCacheFilesReportsAWalkFailure drives the walk itself: a directory the
// walk cannot read is an error, never an empty file list that would read as a
// tree with nothing to trim.
func TestListCacheFilesReportsAWalkFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, "a"), 1, testTime)
	_, err := listCacheFilesWith(func(dir string, fn filepath.WalkFunc) error {
		return fn(dir, nil, errors.New("permission denied"))
	}, root)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("listCacheFilesWith() = %v, want the walk's failure", err)
	}
}

// TestListCacheFilesRefusesAPathThatIsNotADirectory is the root check: a file
// where a directory belongs measures nothing rather than reading as an empty
// tree.
func TestListCacheFilesRefusesAPathThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	file := plantCacheFile(t, filepath.Join(t.TempDir(), "file"), 1, testTime)
	_, err := listCacheFiles(file)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("listCacheFiles() = %v, want the path refused", err)
	}
}

// TestTrimCacheTreeNamesTheDirectoryItCouldNotList is the failure attribution:
// the error names go-build, the subtree whose listing failed, and the module
// cache is never touched (gt-tpg0p.7).
func TestTrimCacheTreeNamesTheDirectoryItCouldNotList(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	build := filepath.Join(root, "go-build")
	module := filepath.Join(root, "go", "pkg", "mod")
	plantCacheFile(t, filepath.Join(build, "a"), 400, testTime)
	plantCacheFile(t, filepath.Join(module, "c"), 400, testTime)
	walk := diskTrimWalk()
	list := walk.list
	walk.list = func(dir string) ([]cacheFile, error) {
		if dir == build {
			return nil, errors.New("permission denied")
		}
		return list(dir)
	}
	_, err := TrimCacheTree(root, 100, walk)
	if err == nil || !strings.Contains(err.Error(), build) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("TrimCacheTree() = %v, want the failed listing of %s", err, build)
	}
	if !existsFile(filepath.Join(module, "c")) {
		t.Error("a failed listing of go-build went on to trim the module cache")
	}
}

// TestTrimCacheTreeKeepsGoingPastAFileItCannotRemove pins the recovery: the
// file that would not go is left, the next one is removed, and the re-measured
// size is what the result reports.
func TestTrimCacheTreeKeepsGoingPastAFileItCannotRemove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stuck := plantCacheFile(t, filepath.Join(root, "go-build", "stuck"), 400, testTime.Add(-time.Hour))
	free := plantCacheFile(t, filepath.Join(root, "go-build", "free"), 400, testTime)
	walk := diskTrimWalk()
	remove := walk.remove
	walk.remove = func(path string) error {
		if path == stuck {
			return errors.New("device or resource busy")
		}
		return remove(path)
	}
	res, err := TrimCacheTree(root, 400, walk)
	if err != nil {
		t.Fatalf("TrimCacheTree() failed: %v", err)
	}
	if !existsFile(stuck) {
		t.Error("a file the walk could not remove is gone")
	}
	if existsFile(free) {
		t.Error("a failed removal stopped the trim instead of trying the next file")
	}
	if res.After != 400 {
		t.Errorf("TrimCacheTree() = %+v, want the re-measured 400", res)
	}
}

// TestTrimCacheTreeReportsAMissingSubtreeAsNothingToTrim is the absent-subtree
// path: a cache tree with no go-build still trims its module cache.
func TestTrimCacheTreeReportsAMissingSubtreeAsNothingToTrim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	module := plantCacheFile(t, filepath.Join(root, "go", "pkg", "mod", "c"), 400, testTime)
	if _, err := TrimCacheTree(root, 100, diskTrimWalk()); err != nil {
		t.Fatalf("TrimCacheTree() failed: %v", err)
	}
	if existsFile(module) {
		t.Error("the module cache survived a tree that had no go-build")
	}
}
