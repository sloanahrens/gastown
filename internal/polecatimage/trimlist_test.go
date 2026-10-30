package polecatimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// fakeListFS records the writer's calls and answers the failure a case names. Names are the ones the
// writer makes, relative to the claim root.
type fakeListFS struct {
	mkdirErr    error
	removeErr   error
	writeErrOn  map[string]error
	renameErrOn map[string]error
	free        int64
	freeErr     error
	mkdirs      []string
	removes     []string
	writes      map[string]string
	renames     [][2]string
}

func newFakeListFS() *fakeListFS {
	return &fakeListFS{
		writeErrOn:  map[string]error{},
		renameErrOn: map[string]error{},
		free:        1 << 40,
		writes:      map[string]string{},
	}
}

func (f *fakeListFS) MkdirAll(name string, _ os.FileMode) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.mkdirs = append(f.mkdirs, name)
	return nil
}

func (f *fakeListFS) Remove(name string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removes = append(f.removes, name)
	return nil
}

func (f *fakeListFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	if err, ok := f.writeErrOn[name]; ok {
		return err
	}
	f.writes[name] = string(data)
	return nil
}

func (f *fakeListFS) Rename(oldpath, newpath string) error {
	if err, ok := f.renameErrOn[oldpath]; ok {
		return err
	}
	f.renames = append(f.renames, [2]string{oldpath, newpath})
	return nil
}

func (f *fakeListFS) Free() (int64, error) { return f.free, f.freeErr }

// TestWriteTrimListWritesTheListAndItsDigest pins the two files the trim container reads: the list
// lands at the path the reader names as the JSON array it decodes, the digest of that list lands
// beside it, and the digest is moved into place first so a reader that finds a list always finds the
// digest written for it. Both arrive by a rename, so a Job never reads a file that is half written.
func TestWriteTrimListWritesTheListAndItsDigest(t *testing.T) {
	t.Parallel()
	f := newFakeListFS()
	paths := []string{"gocache/ab/cdef", "gomodcache/mod/x"}
	digest, err := WriteTrimList(paths, f)
	if err != nil {
		t.Fatalf("WriteTrimList: %v", err)
	}

	dest := cachevol.TrimClaimListPath
	tmp := dest + ".tmp"
	digestDest := cachevol.TrimClaimDigestPath
	digestTmp := digestDest + ".tmp"
	if want := []string{cachevol.TrimClaimListDir}; !reflect.DeepEqual(f.mkdirs, want) {
		t.Errorf("mkdirs = %v, want %v", f.mkdirs, want)
	}
	if want := []string{tmp, digestTmp}; !reflect.DeepEqual(f.removes, want) {
		t.Errorf("removes = %v, want the stale temporaries cleared: %v", f.removes, want)
	}
	data, ok := f.writes[tmp]
	if !ok || len(f.writes) != 2 {
		t.Fatalf("writes = %v, want the list and its digest beside their destinations", f.writes)
	}
	var got []string
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("the written list is not a JSON array of paths: %v", err)
	}
	if !reflect.DeepEqual(got, paths) {
		t.Errorf("the written list is %v, want %v", got, paths)
	}
	sum := sha256.Sum256([]byte(data))
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Errorf("WriteTrimList returned digest %q, want the list's %q", digest, want)
	}
	if want := digest + "\n"; f.writes[digestTmp] != want {
		t.Errorf("the written digest is %q, want %q", f.writes[digestTmp], want)
	}
	if want := [][2]string{{digestTmp, digestDest}, {tmp, dest}}; !reflect.DeepEqual(f.renames, want) {
		t.Errorf("renames = %v, want %v: the digest lands before the list it belongs to", f.renames, want)
	}
}

// TestWriteTrimListRefusesAListTheContainerWouldRefuse pins the guard that keeps the writer and the
// reader on one contract: a list with nothing to remove or a path the claim walk could not give is
// refused whole, and nothing is written.
func TestWriteTrimListRefusesAListTheContainerWouldRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths []string
	}{
		{"no path", nil},
		{"the claim root itself", []string{"."}},
		{"an absolute path", []string{"/etc/passwd"}},
		{"a parent element", []string{"gocache/../../escape"}},
		{"an unclean path", []string{"gocache//one"}},
		{"one bad path beside good ones", []string{"gocache/fine", "../escape"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeListFS()
			if _, err := WriteTrimList(tc.paths, f); err == nil {
				t.Fatalf("WriteTrimList(%v) wrote a list the container would refuse", tc.paths)
			}
			if len(f.mkdirs) != 0 || len(f.removes) != 0 || len(f.writes) != 0 || len(f.renames) != 0 {
				t.Errorf("the writer touched the claim for a refused list: mkdirs %v, removes %v, writes %v, renames %v", f.mkdirs, f.removes, f.writes, f.renames)
			}
		})
	}
}

// TestWriteTrimListRefusesAClaimWithNoRoomForTheList pins the check a claim over its policy cap
// needs: the writer measures the filesystem's room first, so a claim with no space for the list and
// its temporary fails before it writes rather than part way through.
func TestWriteTrimListRefusesAClaimWithNoRoomForTheList(t *testing.T) {
	t.Parallel()
	f := newFakeListFS()
	f.free = 16
	_, err := WriteTrimList([]string{"gocache/one"}, f)
	if err == nil {
		t.Fatal("WriteTrimList wrote a list into a claim with no room for it")
	}
	if !strings.Contains(err.Error(), "has 16 bytes free") {
		t.Errorf("error = %q, want the room the claim has", err)
	}
	if len(f.writes) != 0 || len(f.renames) != 0 {
		t.Errorf("the writer touched the claim with no room: writes %v, renames %v", f.writes, f.renames)
	}
}

// TestWriteTrimListRefusesAClaimWhoseRoomItCannotMeasure pins the fail-closed reading: a room figure
// the writer could not take is not "room enough", so nothing is written.
func TestWriteTrimListRefusesAClaimWhoseRoomItCannotMeasure(t *testing.T) {
	t.Parallel()
	f := newFakeListFS()
	f.freeErr = errors.New("stale file handle")
	_, err := WriteTrimList([]string{"gocache/one"}, f)
	if err == nil {
		t.Fatal("WriteTrimList wrote a list into a claim whose room it could not measure")
	}
	if !strings.Contains(err.Error(), "measuring the room") {
		t.Errorf("error = %q, want the measurement it could not take", err)
	}
	if len(f.writes) != 0 {
		t.Errorf("the writer wrote %v to a claim whose room it could not measure", f.writes)
	}
}

// TestWriteTrimListReportsEachFailedStep pins the fail-closed errors: a directory it cannot create, a
// temporary it cannot clear, a file it cannot write, or a rename it cannot make leaves no list at its
// destination, and the error names the step.
func TestWriteTrimListReportsEachFailedStep(t *testing.T) {
	t.Parallel()
	listTmp := cachevol.TrimClaimListPath + ".tmp"
	digestTmp := cachevol.TrimClaimDigestPath + ".tmp"
	tests := []struct {
		name    string
		breakFS func(*fakeListFS)
		wantErr string
	}{
		{"the directory cannot be created", func(f *fakeListFS) { f.mkdirErr = errors.New("read-only file system") }, "creating the trim list directory"},
		{"the temporary cannot be cleared", func(f *fakeListFS) { f.removeErr = errors.New("permission denied") }, "clearing the stale trim list"},
		{"the list cannot be written", func(f *fakeListFS) { f.writeErrOn[listTmp] = errors.New("no space left on device") }, "writing the trim list"},
		{"the digest cannot be written", func(f *fakeListFS) { f.writeErrOn[digestTmp] = errors.New("no space left on device") }, "writing the trim list digest"},
		{"the digest cannot be moved into place", func(f *fakeListFS) { f.renameErrOn[digestTmp] = errors.New("permission denied") }, "moving the trim list digest into place"},
		{"the list cannot be moved into place", func(f *fakeListFS) { f.renameErrOn[listTmp] = errors.New("permission denied") }, "moving the trim list into place"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeListFS()
			tc.breakFS(f)
			_, err := WriteTrimList([]string{"gocache/one"}, f)
			if err == nil {
				t.Fatal("WriteTrimList reported success for a list it did not write")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
			if _, ok := f.writes[cachevol.TrimClaimListPath]; ok {
				t.Errorf("the list was written at its destination despite the failure")
			}
		})
	}
}

// TestClaimListingNamesOnlyTheCacheTrees pins the listing PlanTrim decides on: the regular files under
// GOCACHE and GOMODCACHE, named by the claim-relative path a trim removes, each carrying the cache it
// belongs to. The rest of the claim is not the trim's, the list's own directory included, so a trim
// never names the list it is reading.
func TestClaimListingNamesOnlyTheCacheTrees(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "ab", "cdef"), 100, testTime)
	plantCacheFile(t, filepath.Join(root, cachevol.SeedModuleCacheDir, "mod", "x"), 200, testTime)
	plantCacheFile(t, filepath.Join(root, "source", "rig", ".git", "HEAD"), 10, testTime)
	plantCacheFile(t, filepath.Join(root, cachevol.TrimClaimListDir, cachevol.TrimClaimListName), 10, testTime)

	listing, err := claimListing(root, filepath.Walk)
	if err != nil {
		t.Fatalf("claimListing: %v", err)
	}
	got := make(map[string]cachevol.Cache, len(listing))
	for _, e := range listing {
		got[e.Path] = e.Cache
	}
	want := map[string]cachevol.Cache{
		cachevol.SeedBuildCacheDir + "/ab/cdef": cachevol.BuildCache,
		cachevol.SeedModuleCacheDir + "/mod/x":  cachevol.ModuleCache,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the listing is %v, want the two cache files %v", got, want)
	}
}

// TestClaimListingRefusesAClaimRootThatIsNotADirectory pins the root check: a claim that is a symlink
// or a plain file has no cache trees to walk, and the walk would answer "no files" for a claim that is
// there, so the listing is refused rather than planning a trim of nothing.
func TestClaimListingRefusesAClaimRootThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir := filepath.Join(base, "claim")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := plantCacheFile(t, filepath.Join(base, "not-a-dir"), 1, testTime)
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		root    string
		wantErr string
	}{
		{"a root that is a link", link, "is a symlink, not a directory"},
		{"a root that is a file", file, "is not a directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := claimListing(tc.root, filepath.Walk)
			if err == nil {
				t.Fatal("claimListing listed a claim root that is not a directory")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestClaimListingRefusesAPathTheWalkPutsOutsideTheClaim pins the guard on the path a walk returns: a
// walk that answers a name below the claim would have the trim name a path the claim root does not
// give, so the listing is refused rather than carrying it.
func TestClaimListingRefusesAPathTheWalkPutsOutsideTheClaim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, cachevol.SeedBuildCacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	walk := func(root string, fn filepath.WalkFunc) error {
		return fn(cachevol.SeedBuildCacheDir+"/elsewhere", &fakeFileInfo{name: "elsewhere"}, nil)
	}
	_, err := claimListing(root, walk)
	if err == nil {
		t.Fatal("claimListing carried a path its walk did not put under the claim")
	}
	if !strings.Contains(err.Error(), "the claim path") {
		t.Errorf("error = %q, want it to name the path the claim does not give", err)
	}
}

// fakeFileInfo is the one file a walk answers with: a regular file of a byte, so a listing of it is
// built the way a real walk's would be.
type fakeFileInfo struct{ name string }

func (f *fakeFileInfo) Name() string       { return f.name }
func (f *fakeFileInfo) Size() int64        { return 1 }
func (f *fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return testTime }
func (f *fakeFileInfo) IsDir() bool        { return false }
func (f *fakeFileInfo) Sys() any           { return nil }

// TestClaimListingRefusesACacheTreeItCannotRead pins the fail-closed walk: a cache tree the run cannot
// list, a symlinked one included, leaves the plan unbuilt rather than trimming from a partial listing.
func TestClaimListingRefusesACacheTreeItCannotRead(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "claim")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(root, cachevol.SeedBuildCacheDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := claimListing(root, filepath.Walk); err == nil {
		t.Fatal("claimListing listed a cache tree that is a link out of the claim")
	}
}

// TestTrimListPathsPlansTheRemovals pins the plan the init container writes: PlanTrim's rules over
// the claim's own listing, with the caller's cap and sandbox count, so the list the trim container
// removes is the one the rules choose and the module cache stays while a sandbox runs.
func TestTrimListPathsPlansTheRemovals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		sandboxes int
		wantPaths []string
	}{
		// 200 bytes of build cache and 100 of module cache under a 50-byte cap: the build cache alone
		// cannot reach the cap, so the module cache goes only when no sandbox runs.
		{name: "idle", sandboxes: 0, wantPaths: []string{cachevol.SeedBuildCacheDir + "/a", cachevol.SeedBuildCacheDir + "/b", cachevol.SeedModuleCacheDir + "/m"}},
		{name: "one running", sandboxes: 1, wantPaths: []string{cachevol.SeedBuildCacheDir + "/a", cachevol.SeedBuildCacheDir + "/b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			// a is the older file, so the rules name it first.
			plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "a"), 100, testTime.Add(-time.Hour))
			plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "b"), 100, testTime)
			plantCacheFile(t, filepath.Join(root, cachevol.SeedModuleCacheDir, "m"), 100, testTime)

			paths, err := TrimListPaths(root, 50, tc.sandboxes > 0, filepath.Walk)
			if err != nil {
				t.Fatalf("TrimListPaths: %v", err)
			}
			if !reflect.DeepEqual(paths, tc.wantPaths) {
				t.Errorf("the plan removes %v, want %v", paths, tc.wantPaths)
			}
		})
	}
}

// TestTrimListPathsRefusesAClaimWithNothingToTrim pins the refusal of a Job that would trim nothing:
// a claim that came under its cap after the Job applied, and a claim the rules cannot bring under it,
// both fail with the reason rather than writing a list that removes nothing.
func TestTrimListPathsRefusesAClaimWithNothingToTrim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		cap       int64
		sandboxes int
		wantErr   string
	}{
		{"under the cap", 1000, 0, "nothing to trim"},
		{"only the module cache, and a sandbox runs", 50, 1, "no removal the rules allow"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			plantCacheFile(t, filepath.Join(root, cachevol.SeedModuleCacheDir, "m"), 100, testTime)
			_, err := TrimListPaths(root, tc.cap, tc.sandboxes > 0, filepath.Walk)
			if err == nil {
				t.Fatal("TrimListPaths planned a trim with nothing to remove")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestTrimListWriteHelperWritesThePlannedListIntoTheClaim drives the init container end to end over a
// temporary claim: it plans the list from the claim, writes it and its digest where the trim container
// reads them, and says which file it wrote.
func TestTrimListWriteHelperWritesThePlannedListIntoTheClaim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "ab", "cdef"), 100, testTime)
	plantCacheFile(t, filepath.Join(root, cachevol.SeedModuleCacheDir, "mod", "x"), 100, testTime)

	var out, errOut bytes.Buffer
	code := trimListWriteMain([]string{"--root", root, "--cap-bytes", "0", "--sandboxes", "0"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("trim-list-write = %d, want 0 (%s)", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(cachevol.TrimClaimListPath)))
	if err != nil {
		t.Fatalf("reading the written list: %v", err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the written list is not a JSON array of paths: %v", err)
	}
	// No sandbox ran in this pass, so the module cache goes too; the cap of 0 puts everything in the
	// list, the older file first.
	want := []string{cachevol.SeedBuildCacheDir + "/ab/cdef", cachevol.SeedModuleCacheDir + "/mod/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the written list is %v, want the plan's %v", got, want)
	}
	digest, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(cachevol.TrimClaimDigestPath)))
	if err != nil {
		t.Fatalf("reading the written digest: %v", err)
	}
	sum := sha256.Sum256(data)
	if want := hex.EncodeToString(sum[:]) + "\n"; string(digest) != want {
		t.Errorf("the written digest is %q, want the list's %q", digest, want)
	}
	if want := "wrote 2 paths " + cachevol.TrimClaimListPath + " " + hex.EncodeToString(sum[:]); !strings.Contains(out.String(), want) {
		t.Errorf("trim-list-write printed %q, want %q", out.String(), want)
	}
}

// TestTrimListWriteHelperRefusesWhatItCannotPlan pins the helper's flags and refusals: a cap or sandbox
// count that is not a size, a claim it cannot open, and a claim with nothing to trim each exit 3 with
// the reason and write no list.
func TestTrimListWriteHelperRefusesWhatItCannotPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    func(root string) []string
		wantErr string
	}{
		{"no cap", func(root string) []string { return []string{"--root", root, "--sandboxes", "0"} }, "--cap-bytes needs a whole number of bytes"},
		{"no sandbox count", func(root string) []string { return []string{"--root", root, "--cap-bytes", "0"} }, "--sandboxes needs a whole number of running sandboxes"},
		{"a claim under its cap", func(root string) []string {
			return []string{"--root", root, "--cap-bytes", "1000", "--sandboxes", "0"}
		}, "nothing to trim"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "a"), 100, testTime)
			var out, errOut bytes.Buffer
			if code := trimListWriteMain(tc.args(root), &out, &errOut); code != 3 {
				t.Fatalf("trim-list-write = %d, want 3", code)
			}
			if !strings.Contains(errOut.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", errOut.String(), tc.wantErr)
			}
			if _, err := os.Stat(filepath.Join(root, cachevol.TrimClaimListDir)); !os.IsNotExist(err) {
				t.Errorf("the helper wrote a list directory for a run it refused (%v)", err)
			}
		})
	}
}

// TestWriteTrimListIntoRefusesAClaimItCannotOpen pins the writer's own open of the claim, below the
// helper: a claim it cannot open is refused before any name is written through it.
func TestWriteTrimListIntoRefusesAClaimItCannotOpen(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-claim")
	_, err := writeTrimListInto(missing, []string{"gocache/one"})
	if err == nil {
		t.Fatal("writeTrimListInto wrote into a claim it could not open")
	}
	if !strings.Contains(err.Error(), "opening the claim") {
		t.Errorf("error = %q, want it to name the open", err)
	}
}

// TestFilesystemFreeReportsARootItCannotMeasure pins the free-space reading's own failure: a path the
// filesystem does not have cannot be measured, and the error is the caller's to fail closed on.
func TestFilesystemFreeReportsARootItCannotMeasure(t *testing.T) {
	t.Parallel()
	if _, err := filesystemFree(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("filesystemFree measured a path the filesystem does not have")
	}
}

// TestTrimListWriteRefusesAClaimItCannotOpen pins the writer's open of the claim: a claim that is not
// there is refused with exit 3, and nothing is written.
func TestTrimListWriteRefusesAClaimItCannotOpen(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-claim")
	var out, errOut bytes.Buffer
	code := trimListWriteMain([]string{"--root", missing, "--cap-bytes", "0", "--sandboxes", "0"}, &out, &errOut)
	if code != 3 {
		t.Fatalf("trim-list-write exit = %d, want 3 for a claim it cannot open", code)
	}
	if !strings.Contains(errOut.String(), "the claim at "+missing) {
		t.Errorf("stderr = %q, want the claim it could not open", errOut.String())
	}
}

// TestTrimListWriteRefusesAClaimWhoseDirectoryLeavesIt runs the writer's production call on a real
// claim whose list directory is a symlink to a directory outside the claim: a sandbox can plant that
// link, and the write must be refused, with nothing landing outside the claim.
func TestTrimListWriteRefusesAClaimWhoseDirectoryLeavesIt(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	claim := filepath.Join(base, "claim")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{claim, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plantCacheFile(t, filepath.Join(claim, cachevol.SeedBuildCacheDir, "ab", "one"), 100, testTime)
	if err := os.Symlink(outside, filepath.Join(claim, cachevol.TrimClaimListDir)); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := trimListWriteMain([]string{"--root", claim, "--cap-bytes", "0", "--sandboxes", "0"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("trim-list-write exit = %d, want 3 for a claim whose list directory leaves it (stderr %q)", code, stderr.String())
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the writer put %d entries outside the claim, want none", len(entries))
	}
}

// TestTrimListWriteDoesNotWriteThroughAStaleTemporary runs the writer's production call on a claim
// that holds a symlink at the temporary's name, pointing at a file outside the claim. The writer must
// replace the link rather than write through it, so the file it points at keeps its contents and the
// list lands at its destination.
func TestTrimListWriteDoesNotWriteThroughAStaleTemporary(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	claim := filepath.Join(base, "claim")
	victim := filepath.Join(base, "victim")
	plantCacheFile(t, filepath.Join(claim, cachevol.SeedBuildCacheDir, "ab", "one"), 100, testTime)
	if err := os.MkdirAll(filepath.Join(claim, cachevol.TrimClaimListDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(claim, filepath.FromSlash(cachevol.TrimClaimListPath)+".tmp")
	if err := os.Symlink(victim, tmp); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := trimListWriteMain([]string{"--root", claim, "--cap-bytes", "0", "--sandboxes", "0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("trim-list-write exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Errorf("the file outside the claim is %q, want it untouched", got)
	}
	list, err := os.ReadFile(filepath.Join(claim, filepath.FromSlash(cachevol.TrimClaimListPath)))
	if err != nil {
		t.Fatalf("the list is not at its destination: %v", err)
	}
	if want := `["` + cachevol.SeedBuildCacheDir + `/ab/one"]`; string(list) != want {
		t.Errorf("the list at its destination is %q, want %q", list, want)
	}
}

// TestTrimListingNamesTheCacheEachFileBelongsTo pins the listing the helper prints and its caller
// builds a trim from: every cache file, by its claim-relative path, with the cache that decides which
// rule may remove it and the times PlanTrim orders by. A reader that cannot tell the two caches apart
// would hand PlanTrim a module-cache file as a build-cache one.
func TestTrimListingNamesTheCacheEachFileBelongsTo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "ab", "cdef"), 100, testTime)
	plantCacheFile(t, filepath.Join(root, cachevol.SeedModuleCacheDir, "mod", "x"), 200, testTime)
	plantCacheFile(t, filepath.Join(root, "source", "rig", ".git", "HEAD"), 10, testTime)

	listing, err := trimListing(root, filepath.Walk)
	if err != nil {
		t.Fatalf("trimListing: %v", err)
	}
	want := map[string]struct {
		cache string
		size  int64
	}{
		cachevol.SeedBuildCacheDir + "/ab/cdef": {TrimListCacheBuild, 100},
		cachevol.SeedModuleCacheDir + "/mod/x":  {TrimListCacheModule, 200},
	}
	got := make(map[string]struct {
		cache string
		size  int64
	}, len(listing))
	for _, entry := range listing {
		got[entry.Path] = struct {
			cache string
			size  int64
		}{entry.Cache, entry.Size}
		if !entry.Atime.Equal(testTime) || !entry.Mtime.Equal(testTime) {
			t.Errorf("the entry %s carries atime %v mtime %v, want both %v", entry.Path, entry.Atime, entry.Mtime, testTime)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the listing is %+v, want %+v", got, want)
	}
}

// TestTrimListMainPrintsTheListing drives the helper a caller runs: the claim's listing on stdout as
// JSON, and a claim it cannot read is exit 3 with the reason, never a partial listing.
func TestTrimListMainPrintsTheListing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plantCacheFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "ab", "cdef"), 100, testTime)

	var stdout, stderr bytes.Buffer
	if code := trimListMain([]string{"--root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("trim-list exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	var entries []trimListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("trim-list printed %q, want a JSON listing: %v", stdout.String(), err)
	}
	if len(entries) != 1 || entries[0].Path != cachevol.SeedBuildCacheDir+"/ab/cdef" || entries[0].Cache != TrimListCacheBuild {
		t.Errorf("trim-list printed %+v, want the one build-cache file", entries)
	}
}

// TestTrimListMainRefusesAClaimItCannotRead pins the fail-closed read: a root that is not there is
// exit 3 with the reason, since an empty listing would plan a trim of a claim nothing looked at.
func TestTrimListMainRefusesAClaimItCannotRead(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := trimListMain([]string{"--root", filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("trim-list exit = %d, want 3", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("trim-list printed %q for a claim it could not read, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "trim-list:") {
		t.Errorf("stderr = %q, want the reason", stderr.String())
	}
}
