package polecatimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
		remove: func(root, rel string) error {
			name := filepath.Join(root, rel)
			if err, ok := f.removeErr[name]; ok {
				return err
			}
			f.removed = append(f.removed, name)
			delete(f.files, name)
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

// addClaimList plants the claim-carried list a writer puts in the claim, and returns its path inside
// the claim.
func (f *fakeTrimFS) addClaimList(root string, paths ...string) string {
	name := filepath.Join(root, filepath.FromSlash(cachevol.TrimClaimListPath))
	f.files[name] = string(claimListBytes(paths))
	return cachevol.TrimClaimListPath
}

// claimListBytes is the encoding a writer puts in the claim for paths: the JSON array, empty rather
// than null when there are none.
func claimListBytes(paths []string) []byte {
	if paths == nil {
		paths = []string{}
	}
	data, err := json.Marshal(paths)
	if err != nil {
		panic(err)
	}
	return data
}

// digestOf is the digest a Job carries for the list whose bytes are data.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// claimDigest is the digest a Job carries for the list planted in the claim at root, the one a writer
// would have put there.
func (f *fakeTrimFS) claimDigest(root string) string {
	return digestOf([]byte(f.files[filepath.Join(root, filepath.FromSlash(cachevol.TrimClaimListPath))]))
}

// claimListDigest is the digest a Job carries for the list addClaimList plants for paths.
func claimListDigest(paths ...string) string {
	return digestOf(claimListBytes(paths))
}

// TestRemoveTrimPathsFromClaimRemovesEveryListedPath is the claim route's success path: each path the
// list in the claim names is removed under the claim root, and the counts say so.
func TestRemoveTrimPathsFromClaimRemovesEveryListedPath(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	list := f.addClaimList("/cache", "gocache/ab/one", "gomodcache/three")
	res, err := RemoveTrimPathsFromClaim("/cache", list, f.claimDigest("/cache"), f.fs())
	if err != nil {
		t.Fatalf("RemoveTrimPathsFromClaim: %v", err)
	}
	if res.Removed != 2 || res.Absent != 0 {
		t.Errorf("result = %+v, want both paths removed", res)
	}
	// The list is consumed first, so a Job that runs again finds no list to trim again.
	want := []string{"/cache/" + cachevol.TrimClaimListPath, "/cache/gocache/ab/one", "/cache/gomodcache/three"}
	if !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want %v", f.removed, want)
	}
}

// TestRemoveTrimPathsFromClaimCountsAnAbsentFile pins the one failure that is not one on this route
// too: a listed file that is already gone leaves the claim as the trim wants it.
func TestRemoveTrimPathsFromClaimCountsAnAbsentFile(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	list := f.addClaimList("/cache", "gocache/ab/gone")
	f.resolveErr["/cache/gocache/ab"] = fs.ErrNotExist
	res, err := RemoveTrimPathsFromClaim("/cache", list, f.claimDigest("/cache"), f.fs())
	if err != nil {
		t.Fatalf("RemoveTrimPathsFromClaim: %v", err)
	}
	if res.Absent != 1 || res.Removed != 0 {
		t.Errorf("result = %+v, want the gone file counted absent", res)
	}
}

// TestRemoveTrimPathsFromClaimRefusesAListItCannotUse pins the fail-closed reads: a list file that is
// absent, is not an array of paths, or names no path leaves the claim untouched, since a run that
// could not read its whole list would trim part of one and report a success.
func TestRemoveTrimPathsFromClaimRefusesAListItCannotUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		plant func(*fakeTrimFS)
		list  string
	}{
		{"a list file the writer never wrote", func(f *fakeTrimFS) {}, cachevol.TrimClaimListPath},
		{"a list file that is not a JSON array", func(f *fakeTrimFS) {
			f.files[filepath.Join("/cache", filepath.FromSlash(cachevol.TrimClaimListPath))] = `{"paths": "gocache/one"}`
		}, cachevol.TrimClaimListPath},
		{"a list that names no path", func(f *fakeTrimFS) { f.addClaimList("/cache") }, cachevol.TrimClaimListPath},
		{"a list file outside the claim", func(f *fakeTrimFS) {
			f.files[filepath.Join("/cache", filepath.FromSlash(cachevol.TrimClaimListPath))] = `["gocache/one"]`
		}, "../outside/paths.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeTrimFS()
			tc.plant(f)
			if _, err := RemoveTrimPathsFromClaim("/cache", tc.list, f.claimDigest("/cache"), f.fs()); err == nil {
				t.Fatal("RemoveTrimPathsFromClaim trimmed from a list it could not use whole")
			}
			if len(f.removed) != 0 {
				t.Errorf("removed %v, want nothing removed", f.removed)
			}
		})
	}
}

// TestRemoveTrimPathsFromClaimRefusesAListedPathThatIsNotBelowTheClaim pins the same lexical guard on
// the claim route: a listed path an absolute name, a parent element or an unclean name could escape
// with is refused before anything is removed.
func TestRemoveTrimPathsFromClaimRefusesAListedPathThatIsNotBelowTheClaim(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"/etc/passwd", "../escape", "gocache/../../escape", "gocache//one"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			f := newFakeTrimFS()
			list := f.addClaimList("/cache", "gocache/fine", entry)
			if _, err := RemoveTrimPathsFromClaim("/cache", list, f.claimDigest("/cache"), f.fs()); err == nil {
				t.Fatalf("RemoveTrimPathsFromClaim removed the list holding %q", entry)
			}
			if len(f.removed) != 0 {
				t.Errorf("removed %v, want nothing removed for %q", f.removed, entry)
			}
		})
	}
}

// TestRemoveTrimPathsFromClaimRefusesAListWithAnotherDigest pins the tie between a Job and its list: a
// list whose digest is not the one the Job was built with belongs to another trim, so nothing is
// removed and the list is left in place for the writer to replace.
func TestRemoveTrimPathsFromClaimRefusesAListWithAnotherDigest(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	list := f.addClaimList("/cache", "gocache/ab/one")
	if _, err := RemoveTrimPathsFromClaim("/cache", list, claimListDigest("gocache/ab/other"), f.fs()); err == nil {
		t.Fatal("RemoveTrimPathsFromClaim trimmed a list whose digest is not the Job's")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
	if _, ok := f.files[filepath.Join("/cache", filepath.FromSlash(cachevol.TrimClaimListPath))]; !ok {
		t.Error("the refused list was consumed, want it left in place")
	}
}

// TestRemoveTrimPathsFromClaimRefusesAJobWithNoDigest pins the fail-closed default: a Job that names no
// digest has nothing to tie the list to, so it trims nothing.
func TestRemoveTrimPathsFromClaimRefusesAJobWithNoDigest(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	list := f.addClaimList("/cache", "gocache/ab/one")
	_, err := RemoveTrimPathsFromClaim("/cache", list, "", f.fs())
	if err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Fatal("RemoveTrimPathsFromClaim trimmed a list with no digest to check it against")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestRemoveTrimPathsFromClaimConsumesTheListOnce pins that a list is trimmed once: the run removes the
// list it read, so a second run of the same Job with no fresh write finds no list and fails rather than
// trimming the paths again.
func TestRemoveTrimPathsFromClaimConsumesTheListOnce(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	list := f.addClaimList("/cache", "gocache/ab/one")
	digest := f.claimDigest("/cache")
	if _, err := RemoveTrimPathsFromClaim("/cache", list, digest, f.fs()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.removed = nil
	if _, err := RemoveTrimPathsFromClaim("/cache", list, digest, f.fs()); err == nil {
		t.Fatal("a second run of the Job trimmed again with no fresh list")
	}
	if len(f.removed) != 0 {
		t.Errorf("the second run removed %v, want nothing", f.removed)
	}
}

// TestRemoveTrimPathsFromClaimRefusesAListFileOutsideTheClaim pins the symlink case on the list itself:
// a list file that resolves outside the claim, through a link in the claim, is refused rather than read.
func TestRemoveTrimPathsFromClaimRefusesAListFileOutsideTheClaim(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	listFile := filepath.Join("/cache", filepath.FromSlash(cachevol.TrimClaimListPath))
	f.files["/outside/paths.json"] = `["gocache/ab/one"]`
	f.resolves[listFile] = "/outside/paths.json"
	digest := digestOf([]byte(`["gocache/ab/one"]`))
	if _, err := RemoveTrimPathsFromClaim("/cache", cachevol.TrimClaimListPath, digest, f.fs()); err == nil {
		t.Fatal("RemoveTrimPathsFromClaim read a list file that resolves outside the claim")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestRemoveTrimPathsFromClaimRefusesADirectoryOutsideTheClaim pins the removal guard on the claim route:
// a listed directory that resolves outside the claim through a symlink is refused, and nothing is removed.
func TestRemoveTrimPathsFromClaimRefusesADirectoryOutsideTheClaim(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	list := f.addClaimList("/cache", "gocache/ab/one")
	f.resolves["/cache/gocache/ab"] = "/outside"
	if _, err := RemoveTrimPathsFromClaim("/cache", list, f.claimDigest("/cache"), f.fs()); err == nil {
		t.Fatal("RemoveTrimPathsFromClaim followed a directory out of the claim")
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed when a directory is outside the claim", f.removed)
	}
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

// TestTrimPathsRunRefusesAListDirectoryThatIsNotThere is the same refusal at the flag: the Job mounts
// the shards under --list, and a run given a directory that is not there has no list to act on.
func TestTrimPathsRunRefusesAListDirectoryThatIsNotThere(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	var out, errOut bytes.Buffer
	code := trimPathsRun([]string{"--root", "/cache", "--list", "/absent", "--shards", "1"}, &out, &errOut, f.fs())
	if code != 3 {
		t.Fatalf("trimPathsRun = %d, want 3 (%s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "reading the shard") {
		t.Errorf("stderr = %q, want the shard it could not read", errOut.String())
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestTrimPathsRunReadsAClaimCarriedList pins the flag that selects the claim route: a run given
// --list-file reads the list the writer put in the claim and removes its paths.
func TestTrimPathsRunReadsAClaimCarriedList(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addClaimList("/cache", "gocache/one")
	var out, errOut bytes.Buffer
	args := []string{"--root", "/cache", "--list-file", cachevol.TrimClaimListPath, "--list-sha256", claimListDigest("gocache/one")}
	code := trimPathsRun(args, &out, &errOut, f.fs())
	if code != 0 {
		t.Fatalf("trimPathsRun = %d, want 0 (%s)", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "removed 1 absent 0" {
		t.Errorf("trimPathsRun printed %q, want %q", got, "removed 1 absent 0")
	}
	want := []string{"/cache/" + cachevol.TrimClaimListPath, "/cache/gocache/one"}
	if !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want %v", f.removed, want)
	}
}

// TestTrimPathsRunRefusesAListWithAnotherDigest runs the claim route through the helper's flags with a
// digest that is not the list's: exit 3, and nothing is trimmed.
func TestTrimPathsRunRefusesAListWithAnotherDigest(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addClaimList("/cache", "gocache/one")
	var out, errOut bytes.Buffer
	args := []string{"--root", "/cache", "--list-file", cachevol.TrimClaimListPath, "--list-sha256", claimListDigest("gocache/other")}
	if code := trimPathsRun(args, &out, &errOut, f.fs()); code != 3 {
		t.Fatalf("trimPathsRun = %d, want 3 for a list with another digest (stdout %q)", code, out.String())
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
	}
}

// TestTrimPathsRunRefusesTwoRoutes pins the flag pair: --list-file and --shards name the two routes,
// and a run given both has no single list to trim.
func TestTrimPathsRunRefusesTwoRoutes(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addClaimList("/cache", "gocache/one")
	var out, errOut bytes.Buffer
	if code := trimPathsRun([]string{"--root", "/cache", "--list-file", cachevol.TrimClaimListPath, "--shards", "1"}, &out, &errOut, f.fs()); code != 3 {
		t.Fatalf("trimPathsRun with both routes = %d, want 3", code)
	}
	if !strings.Contains(errOut.String(), "two routes") {
		t.Errorf("stderr = %q, want the two-route refusal", errOut.String())
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v, want nothing removed", f.removed)
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

// TestTrimPathsRunRemovesTheMountedList is the helper as the Job runs it: the run deletes the listed
// file under the claim and prints the counts the caller logs.
func TestTrimPathsRunRemovesTheMountedList(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/ab/one")
	var out, errOut bytes.Buffer
	code := trimPathsRun([]string{"--root", "/cache", "--list", "/trim", "--shards", "1"}, &out, &errOut, f.fs())
	if code != 0 {
		t.Fatalf("trimPathsRun = %d, want 0 (%s)", code, errOut.String())
	}
	if want := []string{"/cache/gocache/ab/one"}; !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want %v", f.removed, want)
	}
	if got := strings.TrimSpace(out.String()); got != "removed 1 absent 0" {
		t.Errorf("trimPathsRun printed %q, want %q", got, "removed 1 absent 0")
	}
}

// TestTrimPathsRunFollowsNoSymlinkOutOfTheClaim is the refusal a run must make: a shard listing a file
// through a symlinked directory names a file outside the claim, and the helper leaves it.
func TestTrimPathsRunFollowsNoSymlinkOutOfTheClaim(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "escape/one")
	f.resolves["/cache/escape"] = "/outside"
	var out, errOut bytes.Buffer
	code := trimPathsRun([]string{"--root", "/cache", "--list", "/trim", "--shards", "1"}, &out, &errOut, f.fs())
	if code != 3 {
		t.Fatalf("trimPathsRun = %d, want 3 (%s)", code, errOut.String())
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v through a symlink out of the claim, want nothing removed", f.removed)
	}
	if !strings.Contains(errOut.String(), "outside the claim") {
		t.Errorf("stderr = %q, want the reason the path is refused", errOut.String())
	}
}

// TestTrimPathsRunReportsTheCountsOfAFailedRun pins what a run that fails part way still says: the
// caller logs the counts, so a trim that removed some of its list reports how many it removed and how
// many were already gone before it exits 3.
func TestTrimPathsRunReportsTheCountsOfAFailedRun(t *testing.T) {
	t.Parallel()
	f := newFakeTrimFS()
	f.addShard("/trim", 0, "gocache/one", "gocache/two", "gocache/gone")
	f.removeErr["/cache/gocache/two"] = errors.New("device or resource busy")
	f.removeErr["/cache/gocache/gone"] = fs.ErrNotExist
	var out, errOut bytes.Buffer
	code := trimPathsRun([]string{"--root", "/cache", "--list", "/trim", "--shards", "1"}, &out, &errOut, f.fs())
	if code != 3 {
		t.Fatalf("trimPathsRun = %d, want 3 (%s)", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "removed 1 absent 1" {
		t.Errorf("trimPathsRun printed %q, want the counts %q", got, "removed 1 absent 1")
	}
	if !strings.Contains(errOut.String(), "1 of 3") {
		t.Errorf("stderr = %q, want the one failed removal of three", errOut.String())
	}
}

// TestTrimPathsRunRefusesAShardCountThatIsNotOne pins the flag: the Job passes the count of mounts it
// made, and a run given none of them has no list to trust.
func TestTrimPathsRunRefusesAShardCountThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, count := range []string{"", "0", "-1", "two"} {
		f := newFakeTrimFS()
		var out, errOut bytes.Buffer
		if code := trimPathsRun([]string{"--shards", count}, &out, &errOut, f.fs()); code != 3 {
			t.Errorf("trimPathsRun(--shards %q) = %d, want 3", count, code)
		}
		if len(f.removed) != 0 {
			t.Errorf("--shards %q removed %v, want nothing removed", count, f.removed)
		}
	}
}

// TestDiskTrimPathsFSReportsAShardDirectoryThatIsNotThere drives the one failure the disk adapter has:
// a shard directory the Job did not mount is an error the run refuses, not an empty list. The directory
// is absent under a temporary parent, so nothing is read or written beyond the lookup itself.
func TestDiskTrimPathsFSReportsAShardDirectoryThatIsNotThere(t *testing.T) {
	t.Parallel()
	_, err := diskTrimPathsFS().names(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("names of a missing shard directory = %v, want fs.ErrNotExist", err)
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

// TestRemoveTrimPathsOnDiskRefusesADirectorySwappedForALinkOutOfTheClaim runs the helper on a real
// directory tree and swaps the listed file's directory for a symlink to a directory outside the claim
// after the guard has resolved it and before the removal runs: the removal, not the guard, must refuse
// it, and the file outside the claim must survive (gt-cahkg).
func TestRemoveTrimPathsOnDiskRefusesADirectorySwappedForALinkOutOfTheClaim(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	claim := filepath.Join(base, "claim")
	outside := filepath.Join(base, "outside")
	listDir := filepath.Join(base, "list")
	for _, dir := range []string{filepath.Join(claim, "gocache", "ab"), outside, filepath.Join(listDir, "0")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kept := filepath.Join(claim, "gocache", "ab", "kept")
	victim := filepath.Join(outside, "victim")
	for _, f := range []string{kept, victim, filepath.Join(outside, "kept")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list, err := json.Marshal([]string{"gocache/ab/kept"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(listDir, "0", cachevol.TrimShardKey), list, 0o644); err != nil {
		t.Fatal(err)
	}

	w := diskTrimPathsFS()
	resolve := w.resolve
	w.resolve = func(name string) (string, error) {
		real, err := resolve(name)
		if err == nil && strings.HasSuffix(name, filepath.Join("gocache", "ab")) {
			// the sandbox's swap, after the guard has resolved and accepted the directory
			if err := os.RemoveAll(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, name); err != nil {
				t.Fatal(err)
			}
		}
		return real, err
	}
	_, err = RemoveTrimPaths(claim, listDir, 1, w)
	if err == nil {
		t.Fatal("RemoveTrimPaths removed through a directory swapped for a link out of the claim and reported success")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "kept")); statErr != nil {
		t.Errorf("the file outside the claim is gone after the swap: %v", statErr)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Errorf("the other file outside the claim is gone after the swap: %v", statErr)
	}
}

// TestRemoveTrimPathsOnDiskRemovesAFileUnderTheClaim is the same helper on a real tree with no swap:
// the listed file goes and its neighbour stays.
func TestRemoveTrimPathsOnDiskRemovesAFileUnderTheClaim(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	claim := filepath.Join(base, "claim")
	listDir := filepath.Join(base, "list")
	for _, dir := range []string{filepath.Join(claim, "gocache", "ab"), filepath.Join(listDir, "0")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(claim, "gocache", "ab", "gone")
	stays := filepath.Join(claim, "gocache", "ab", "stays")
	for _, f := range []string{gone, stays} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list, err := json.Marshal([]string{"gocache/ab/gone", "gocache/ab/missing"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(listDir, "0", cachevol.TrimShardKey), list, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RemoveTrimPaths(claim, listDir, 1, diskTrimPathsFS())
	if err != nil {
		t.Fatalf("RemoveTrimPaths: %v", err)
	}
	if res.Removed != 1 || res.Absent != 1 {
		t.Errorf("result = %+v, want one removed and one absent", res)
	}
	if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the listed file is still there: %v", err)
	}
	if _, err := os.Stat(stays); err != nil {
		t.Errorf("the unlisted neighbour was removed: %v", err)
	}
}
