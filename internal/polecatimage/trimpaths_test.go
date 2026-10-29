package polecatimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// fakeTrimFS is a trim-paths filesystem held in memory: the directories and files a case names,
// and the answer each call gives by path.
type fakeTrimFS struct {
	dirs       map[string][]string
	files      map[string]string
	resolves   map[string]string
	resolveErr map[string]error
	removeErr  map[string]error
	removed    []string
}

func newFakeTrimFS() *fakeTrimFS {
	return &fakeTrimFS{
		dirs:       map[string][]string{},
		files:      map[string]string{},
		resolves:   map[string]string{},
		resolveErr: map[string]error{},
		removeErr:  map[string]error{},
	}
}

// fs is the trim-paths filesystem this case answers.
func (f *fakeTrimFS) fs() trimPathsFS {
	return trimPathsFS{
		names: func(dir string) ([]string, error) {
			names, ok := f.dirs[dir]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return names, nil
		},
		read: func(name string) ([]byte, error) {
			data, ok := f.files[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(data), nil
		},
		remove: func(name string) error {
			if err, ok := f.removeErr[name]; ok {
				return err
			}
			f.removed = append(f.removed, name)
			return nil
		},
		resolve: func(name string) (string, error) {
			if err, ok := f.resolveErr[name]; ok {
				return "", err
			}
			if to, ok := f.resolves[name]; ok {
				return to, nil
			}
			return filepath.Clean(name), nil
		},
	}
}

// addShard plants one mounted shard: the file the ConfigMap's key becomes, beside the kubelet's
// ..data symlink and the directory it points at, which the helper skips.
func (f *fakeTrimFS) addShard(listDir string, index int, paths ...string) string {
	dir := filepath.Join(listDir, strconv.Itoa(index))
	data, err := json.Marshal(paths)
	if err != nil {
		panic(err)
	}
	f.dirs[dir] = []string{"..2026_10_29_00_00_00.000000000", "..data", cachevol.TrimShardKey}
	f.files[filepath.Join(dir, cachevol.TrimShardKey)] = string(data)
	return dir
}

// TestRemoveTrimPathsRemovesEveryListedPath is the helper's success path over more than one shard:
// each listed path is removed under the claim root, and the counts say so.
func TestRemoveTrimPathsRemovesEveryListedPath(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/ab/one", "gocache/ab/two")
	f.addShard("/trim", 1, "gomodcache/three")
	res, err := RemoveTrimPaths("/cache", "/trim", 2, f.fs())
	if err != nil {
		t.Fatalf("RemoveTrimPaths: %v", err)
	}
	if res.Removed != 3 || res.Absent != 0 {
		t.Errorf("result = %+v, want all three removed", res)
	}
	want := []string{"/cache/gocache/ab/one", "/cache/gocache/ab/two", "/cache/gomodcache/three"}
	if !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want %v", f.removed, want)
	}
}

// TestRemoveTrimPathsCountsAnAbsentFile pins the one failure that is not one: a listed file that is
// already gone leaves the claim as the trim wants it, so the run reports it apart from a removal.
func TestRemoveTrimPathsCountsAnAbsentFile(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one", "gocache/gone")
	f.removeErr["/cache/gocache/gone"] = fs.ErrNotExist
	res, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs())
	if err != nil {
		t.Fatalf("RemoveTrimPaths: %v", err)
	}
	if res.Removed != 1 || res.Absent != 1 {
		t.Errorf("result = %+v, want one removed and one absent", res)
	}
}

// TestRemoveTrimPathsCountsAFileUnderAGoneDirectoryAsAbsent pins the same answer for the path whose
// directory the walk would have removed already: resolving the directory is where the run learns it.
func TestRemoveTrimPathsCountsAFileUnderAGoneDirectoryAsAbsent(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/gone/one")
	f.resolveErr["/cache/gocache/gone"] = fs.ErrNotExist
	res, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs())
	if err != nil {
		t.Fatalf("RemoveTrimPaths: %v", err)
	}
	if res.Removed != 0 || res.Absent != 1 {
		t.Errorf("result = %+v, want the file counted absent", res)
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestRemoveTrimPathsReportsAFailedRemovalAfterTheRest pins the run's tolerance: one file the helper
// cannot remove does not abandon the rest of the list, and the count it cannot vouch for comes back
// in the error, which names the path and never a file's contents.
func TestRemoveTrimPathsReportsAFailedRemovalAfterTheRest(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one", "gocache/two", "gocache/three")
	f.removeErr["/cache/gocache/two"] = errors.New("device or resource busy")
	res, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs())
	if err == nil {
		t.Fatal("RemoveTrimPaths removed two of three and reported success")
	}
	if res.Removed != 2 {
		t.Errorf("result = %+v, want the other two removed", res)
	}
	if !strings.Contains(err.Error(), "1 of 3") || !strings.Contains(err.Error(), "/cache/gocache/two") {
		t.Errorf("error = %v, want the count and the path that failed", err)
	}
}

// TestRemoveTrimPathsRemovesThePathItResolved pins the guard's answer to a symlinked directory: the
// path it removes is the one it resolved and checked, never the listed name, which would follow the
// link out of the claim.
func TestRemoveTrimPathsRemovesThePathItResolved(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one")
	f.resolves["/cache/gocache"] = "/cache/real/gocache"
	if _, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs()); err != nil {
		t.Fatalf("RemoveTrimPaths: %v", err)
	}
	if want := []string{"/cache/real/gocache/one"}; !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want the resolved path %v", f.removed, want)
	}
}

// TestRemoveTrimPathsRefusesAClaimRootItCannotResolve pins the check the run makes before it reads
// any path: a claim it cannot resolve is one whose boundaries it does not know, so it trims nothing.
func TestRemoveTrimPathsRefusesAClaimRootItCannotResolve(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one")
	f.resolveErr["/cache"] = errors.New("no such device")
	if _, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs()); err == nil {
		t.Fatal("RemoveTrimPaths trimmed under a claim root it could not resolve")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestRemoveTrimPathsRefusesAShardFileItCannotRead pins a shard whose directory lists a file the
// read then refuses: the list is unknown, so the run refuses rather than trimming what it has.
func TestRemoveTrimPathsRefusesAShardFileItCannotRead(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	dir := f.addShard("/trim", 0, "gocache/one")
	delete(f.files, filepath.Join(dir, cachevol.TrimShardKey))
	if _, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs()); err == nil {
		t.Fatal("RemoveTrimPaths trimmed a list it could not read")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestTrimPathsMainRefusesAListDirectoryThatIsNotThere is the same refusal over the real
// filesystem: the Job mounts the shards under --list, and a run given a directory that is not there
// has no list to act on.
func TestTrimPathsMainRefusesAListDirectoryThatIsNotThere(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	list := filepath.Join(t.TempDir(), "absent")
	code := trimPathsMain([]string{"--root", t.TempDir(), "--list", list, "--shards", "1"}, &out, &errOut)
	if code != 3 {
		t.Fatalf("trimPathsMain = %d, want 3 (%s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "reading the shard") {
		t.Errorf("stderr = %q, want the shard it could not read", errOut.String())
	}
}

// TestRemoveTrimPathsRefusesADirectoryItCannotResolve pins the other answer a resolve gives: an error
// that is not the directory being gone is a check that did not run, so the run refuses rather than
// counting the file absent.
func TestRemoveTrimPathsRefusesADirectoryItCannotResolve(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one")
	f.resolveErr["/cache/gocache"] = errors.New("permission denied")
	if _, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs()); err == nil {
		t.Fatal("RemoveTrimPaths reported a trim it could not check")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed when a check did not run", f.removed)
	}
}

// TestRemoveTrimPathsRefusesADirectoryOutsideTheClaim pins the refusal a symlinked directory earns
// when it leads out of the claim, and that nothing is removed when it does.
func TestRemoveTrimPathsRefusesADirectoryOutsideTheClaim(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one", "escape/file")
	f.resolves["/cache/escape"] = "/etc"
	if _, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs()); err == nil {
		t.Fatal("RemoveTrimPaths followed a directory out of the claim")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed when a directory is outside the claim", f.removed)
	}
}

// TestRemoveTrimPathsRefusesAListedPathThatIsNotBelowTheClaim pins the lexical guard: a path an
// absolute name, a parent element or an unclean name could escape with is refused before anything is
// removed.
func TestRemoveTrimPathsRefusesAListedPathThatIsNotBelowTheClaim(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"", "/etc/passwd", "../escape", "gocache/../../escape", "gocache//one", "./gocache/one", "."} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			f := newFakeTrimFS()
			f.addShard("/trim", 0, "gocache/fine", entry)
			if _, err := RemoveTrimPaths("/cache", "/trim", 1, f.fs()); err == nil {
				t.Fatalf("RemoveTrimPaths removed the list holding %q", entry)
			}
			if len(f.removed) != 0 {
				t.Errorf("removed %v, want nothing removed for %q", f.removed, entry)
			}
		})
	}
}

// TestRemoveTrimPathsRefusesAShardItCannotReadWhole pins the fail-closed read: a shard missing, a
// shard holding two files, or a shard whose contents are not the array the Job writes leaves the
// claim untouched, since trimming the part of the list that did arrive would leave it over cap while
// reporting a trim.
func TestRemoveTrimPathsRefusesAShardItCannotReadWhole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		break2 func(*fakeTrimFS) string
	}{
		{"a shard the pod did not mount", func(f *fakeTrimFS) string { return "/trim/1" }},
		{"a shard holding two files", func(f *fakeTrimFS) string {
			dir := f.addShard("/trim", 1, "gocache/two")
			f.dirs[dir] = append(f.dirs[dir], "stray")
			return dir
		}},
		{"a shard that is not a JSON array", func(f *fakeTrimFS) string {
			dir := f.addShard("/trim", 1, "gocache/two")
			f.files[filepath.Join(dir, cachevol.TrimShardKey)] = `{"paths": "gocache/two"}`
			return dir
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeTrimFS()
			f.addShard("/trim", 0, "gocache/one")
			tc.break2(f)
			if _, err := RemoveTrimPaths("/cache", "/trim", 2, f.fs()); err == nil {
				t.Fatal("RemoveTrimPaths trimmed part of a list it could not read whole")
			}
			if len(f.removed) != 0 {
				t.Errorf("removed %v, want nothing removed", f.removed)
			}
		})
	}
}

// TestTrimPathsMainRemovesTheMountedList is the helper as the Job runs it, over a real filesystem:
// the run deletes the listed file under the claim and prints the counts the caller logs.
func TestTrimPathsMainRemovesTheMountedList(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	list := t.TempDir()
	victim := plantCacheFile(t, filepath.Join(root, "gocache", "ab", "one"), 10, testNow)
	writeShard(t, list, 0, "gocache/ab/one")
	var out, errOut bytes.Buffer
	code := runProgram([]string{"trim-paths", "--root", root, "--list", list, "--shards", "1"}, &out, &errOut, envMap(map[string]string{}))
	if code != 0 {
		t.Fatalf("runProgram(trim-paths) = %d, want 0 (%s)", code, errOut.String())
	}
	if existsFile(victim) {
		t.Errorf("%s is still there, want the listed file removed", victim)
	}
	if got := strings.TrimSpace(out.String()); got != "removed 1 absent 0" {
		t.Errorf("runProgram(trim-paths) printed %q, want %q", got, "removed 1 absent 0")
	}
}

// TestTrimPathsMainFollowsNoSymlinkOutOfTheClaim is the refusal a run must make: a shard listing a
// file through a symlinked directory names a file outside the claim, and the helper leaves it.
func TestTrimPathsMainFollowsNoSymlinkOutOfTheClaim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	list := t.TempDir()
	outside := t.TempDir()
	victim := plantCacheFile(t, filepath.Join(outside, "one"), 10, testNow)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	writeShard(t, list, 0, "escape/one")
	var out, errOut bytes.Buffer
	code := runProgram([]string{"trim-paths", "--root", root, "--list", list, "--shards", "1"}, &out, &errOut, envMap(map[string]string{}))
	if code != 3 {
		t.Fatalf("runProgram(trim-paths) = %d, want 3 (%s)", code, errOut.String())
	}
	if !existsFile(victim) {
		t.Errorf("%s was removed through a symlink out of the claim", victim)
	}
	if !strings.Contains(errOut.String(), "outside the claim") {
		t.Errorf("stderr = %q, want the reason the path is refused", errOut.String())
	}
}

// TestTrimPathsMainRefusesAShardCountThatIsNotOne pins the flag: the Job passes the count of mounts
// it made, and a run given none of them has no list to trust.
func TestTrimPathsMainRefusesAShardCountThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, count := range []string{"", "0", "-1", "two"} {
		var out, errOut bytes.Buffer
		if code := trimPathsMain([]string{"--shards", count}, &out, &errOut); code != 3 {
			t.Errorf("trimPathsMain(--shards %q) = %d, want 3", count, code)
		}
	}
}

// TestRemoveTrimPathsRefusesAShardCountOfNone pins the same refusal one level down, where a future
// caller reaches it without the flag: a run with no shard has no list, and trimming nothing is not
// a trim.
func TestRemoveTrimPathsRefusesAShardCountOfNone(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one")
	if _, err := RemoveTrimPaths("/cache", "/trim", 0, f.fs()); err == nil {
		t.Fatal("RemoveTrimPaths with no shard reported a trim")
	}
	if len(f.removed) != 0 || len(f.dirs) != 1 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// writeShard plants one shard directory as the kubelet mounts it: the ConfigMap's key as a file,
// beside the ..data symlink the atomic writer leaves and the directory it points at.
func writeShard(t *testing.T, listDir string, index int, paths ...string) {
	t.Helper()
	data, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(listDir, strconv.Itoa(index))
	stamp := "..2026_10_29_00_00_00.000000000"
	if err := os.MkdirAll(filepath.Join(dir, stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stamp, cachevol.TrimShardKey), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stamp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", cachevol.TrimShardKey), filepath.Join(dir, cachevol.TrimShardKey)); err != nil {
		t.Fatal(err)
	}
}
