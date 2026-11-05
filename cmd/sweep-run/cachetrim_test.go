package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/sweeprunner"
	"github.com/steveyegge/gastown/internal/tiersweep"
)

// TestTrimSweepCacheKeepsTheCap: a claim over the cap loses its least recently
// used build-cache files until it is at or under the cap and keeps its module
// cache, a claim under the cap loses nothing, and a cap of zero or less is
// refused before anything is listed.
func TestTrimSweepCacheKeepsTheCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cap     int64
		want    []string
		wantErr string
	}{
		{
			// 260 bytes in all: the oldest of the four 40-byte build entries
			// goes and the claim sits at 220, so one removal is enough.
			name: "over the cap removes the least recently used",
			cap:  220,
			want: []string{"gocache/mid", "gocache/new", "gocache/newest", "gomodcache/mod"},
		},
		{
			name: "under the cap removes nothing",
			cap:  1 << 20,
			want: []string{"gocache/mid", "gocache/new", "gocache/newest", "gocache/old", "gomodcache/mod"},
		},
		{
			// Every build entry goes and the 100-byte module cache is left
			// alone while another run may be building against it, so the cap
			// cannot be met and the trim says so.
			name:    "a module cache alone over the cap is reported",
			cap:     50,
			want:    []string{"gomodcache/mod"},
			wantErr: "still 50 bytes over its 50-byte cap",
		},
		{
			// Zero is not a default: a trim asked for no cap is refused, and the
			// claim keeps every file.
			name:    "a cap of zero is refused",
			cap:     0,
			want:    []string{"gocache/mid", "gocache/new", "gocache/newest", "gocache/old", "gomodcache/mod"},
			wantErr: "want a cap above zero",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := cacheClaim(t)
			trim := &CacheTrim{Root: root, CapBytes: tc.cap, Claim: diskClaim, Remove: removeSweepCacheFile}
			err := trimSweepCache(trim)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("trimSweepCache: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("trimSweepCache = %v, want %q", err, tc.wantErr)
			}
			if got := filesUnder(t, root); !equal(got, tc.want) {
				t.Errorf("cache files = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrimSweepCacheRefusesAnUnwiredTrim: a trim that was never wired, or that
// is missing one of its seams or its cap, is reported rather than read as a
// claim with nothing to remove.
func TestTrimSweepCacheRefusesAnUnwiredTrim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []struct {
		name string
		trim *CacheTrim
		want string
	}{
		{"no trim", nil, "not wired"},
		{"no root", &CacheTrim{CapBytes: 1, Claim: diskClaim, Remove: removeSweepCacheFile}, "no claim root"},
		{"a relative root", &CacheTrim{Root: "cache", CapBytes: 1, Claim: diskClaim, Remove: removeSweepCacheFile}, "absolute path"},
		{"a negative cap", &CacheTrim{Root: root, CapBytes: -1, Claim: diskClaim, Remove: removeSweepCacheFile}, "CapBytes is -1"},
		{"no claim", &CacheTrim{Root: root, CapBytes: 1, Remove: removeSweepCacheFile}, "has no claim"},
		{"no removal", &CacheTrim{Root: root, CapBytes: 1, Claim: diskClaim}, "no removal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := trimSweepCache(tc.trim)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("trimSweepCache(%s) = %v, want a refusal naming %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestTrimSweepCacheDoesNothingWithoutAMountedClaim: a runner started outside
// the sweep Job has no claim mount, and the trim is a no-op rather than an
// error for a claim that was never there.
func TestTrimSweepCacheDoesNothingWithoutAMountedClaim(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "not-mounted")
	trim := &CacheTrim{Root: root, CapBytes: 1, Claim: diskClaim, Remove: removeSweepCacheFile}
	if err := trimSweepCache(trim); err != nil {
		t.Fatalf("trimSweepCache without a claim root: %v", err)
	}
}

// TestTrimSweepCacheRefusesAClaimItCannotRead: a root or a cache tree that is a
// symbolic link, a root that is not a directory, and a root that cannot be
// measured are reported, never walked as an empty claim.
func TestTrimSweepCacheRefusesAClaimItCannotRead(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	linkRoot := filepath.Join(t.TempDir(), "claim")
	if err := os.Symlink(t.TempDir(), linkRoot); err != nil {
		t.Fatalf("planting the claim link: %v", err)
	}
	fileRoot := filepath.Join(t.TempDir(), "claim")
	writeFile(t, fileRoot, 10, base)
	treeRoot := emptyClaimRoot(t)
	if err := os.RemoveAll(filepath.Join(treeRoot, cachevol.SeedBuildCacheDir)); err != nil {
		t.Fatalf("clearing the build cache: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(treeRoot, cachevol.SeedBuildCacheDir)); err != nil {
		t.Fatalf("planting the tree link: %v", err)
	}
	cases := []struct {
		name string
		root string
		want string
	}{
		{"a symbolic link", linkRoot, "symbolic link"},
		{"a file", fileRoot, "not a directory"},
		{"a symbolic link for a cache tree", treeRoot, "symbolic link, not a cache tree"},
		// A path whose parent is a file cannot be measured, and that failure is
		// not a claim that is simply not mounted.
		{"a path through a file", filepath.Join(fileRoot, "claim"), "opening the cache claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trim := &CacheTrim{Root: tc.root, CapBytes: 1, Claim: diskClaim, Remove: removeSweepCacheFile}
			err := trimSweepCache(trim)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("trimSweepCache = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestTrimSweepCacheAcceptsAFileAnotherRunRemoved: a planned file that is
// already gone is the goal, not a failure.
func TestTrimSweepCacheAcceptsAFileAnotherRunRemoved(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trim := &CacheTrim{
		Root: t.TempDir(), CapBytes: 1,
		Claim:  fakeClaimOf(fakeClaim(map[string]*fstest.MapFile{"gocache/gone": fakeFile(1000, base)})),
		Remove: removeSweepCacheFile,
	}
	if err := trimSweepCache(trim); err != nil {
		t.Fatalf("trimSweepCache of a file another run removed: %v", err)
	}
}

// TestTrimSweepCacheSkipsAFileThatVanishedMidWalk: a file another Job removes
// between the directory read and its stat is not in the listing, so it is not
// planned, and the trim still reaches the cap from the files that remain.
func TestTrimSweepCacheSkipsAFileThatVanishedMidWalk(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// The vanished file is the least recently used, so a listing that kept it
	// would plan it first.
	claim := faultyClaim{
		MapFS: fakeClaim(map[string]*fstest.MapFile{
			"gocache/vanished": fakeFile(40, base.Add(-time.Hour)),
			"gocache/old":      fakeFile(40, base),
			"gocache/mid":      fakeFile(40, base.Add(time.Hour)),
			"gocache/new":      fakeFile(40, base.Add(2*time.Hour)),
			"gocache/newest":   fakeFile(40, base.Add(3*time.Hour)),
			"gomodcache/mod":   fakeFile(100, base.Add(-2*time.Hour)),
		}),
		stats: map[string]error{"gocache/vanished": fs.ErrNotExist},
	}
	var removed []string
	trim := &CacheTrim{
		Root: t.TempDir(), CapBytes: 220,
		Claim:  fakeClaimOf(claim),
		Remove: recordRemovals(&removed),
	}
	if err := trimSweepCache(trim); err != nil {
		t.Fatalf("trimSweepCache with a file that vanished mid-walk: %v", err)
	}
	if want := []string{"gocache/old"}; !equal(removed, want) {
		t.Errorf("removed = %q, want %q: the vanished file must not be planned", removed, want)
	}
}

// TestTrimSweepCacheFailsClosedOnAPartialWalk: a missing cache tree, an
// unreadable directory and a file whose stat fails for another reason each make
// the listing partial. Nothing is removed, the cap is reported unverified, and
// the error names the directory. The fake filesystem makes the unreadable
// directory fail as root as well, so no case is skipped.
func TestTrimSweepCacheFailsClosedOnAPartialWalk(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	claimFiles := func() fstest.MapFS {
		return fakeClaim(map[string]*fstest.MapFile{
			"gocache/old":    fakeFile(40, base),
			"gocache/new":    fakeFile(40, base.Add(time.Hour)),
			"gomodcache/mod": fakeFile(100, base.Add(-time.Hour)),
		})
	}
	cases := []struct {
		name  string
		claim faultyClaim
		want  string
	}{
		{
			name:  "a missing build cache",
			claim: faultyClaim{MapFS: fstest.MapFS{"gomodcache/mod": fakeFile(100, base)}},
			want:  "listing gocache:",
		},
		{
			name:  "a missing module cache",
			claim: faultyClaim{MapFS: fstest.MapFS{"gocache/old": fakeFile(40, base)}},
			want:  "listing gomodcache:",
		},
		{
			name:  "an unreadable build cache",
			claim: faultyClaim{MapFS: claimFiles(), dirs: map[string]error{"gocache": fs.ErrPermission}},
			want:  "listing gocache:",
		},
		{
			name:  "an unreadable module cache",
			claim: faultyClaim{MapFS: claimFiles(), dirs: map[string]error{"gomodcache": fs.ErrPermission}},
			want:  "listing gomodcache:",
		},
		{
			name:  "a file whose stat fails for another reason",
			claim: faultyClaim{MapFS: claimFiles(), stats: map[string]error{"gocache/old": fs.ErrPermission}},
			want:  "listing gocache/old:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var removed []string
			trim := &CacheTrim{
				Root: t.TempDir(), CapBytes: 1,
				Claim:  fakeClaimOf(tc.claim),
				Remove: recordRemovals(&removed),
			}
			err := trimSweepCache(trim)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("trimSweepCache = %v, want a refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "not verified") {
				t.Errorf("trimSweepCache = %v, want the cap reported as not verified", err)
			}
			if len(removed) != 0 {
				t.Errorf("removed %q from a partial listing, want nothing", removed)
			}
		})
	}
}

// TestTrimSweepCacheRefusesPathsOutsideTheCaches: a symbolic link in a cache
// tree and a planned path that leaves the two trees are each refused with the
// named refusal, and the files they name are still there.
func TestTrimSweepCacheRefusesPathsOutsideTheCaches(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("a symbolic link in the cache", func(t *testing.T) {
		t.Parallel()
		outside := filepath.Join(t.TempDir(), "keep")
		writeFile(t, outside, 10, base)
		root := emptyClaimRoot(t)
		link := filepath.Join(root, cachevol.SeedBuildCacheDir, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("planting the link: %v", err)
		}
		trim := &CacheTrim{Root: root, CapBytes: 1, Claim: diskClaim, Remove: removeSweepCacheFile}
		err := trimSweepCache(trim)
		if err == nil || !strings.Contains(err.Error(), "is a symbolic link, not a cache file") {
			t.Fatalf("trimSweepCache = %v, want a refusal naming the symbolic link", err)
		}
		assertPresent(t, outside)
		assertPresent(t, link)
	})

	cases := []struct {
		name string
		// path is the entry a plan names, target the file that must survive the
		// refusal, and want a phrase of the refusal the removal gives.
		path    func(outsideDir string) string
		target  func(outsideDir, root string) string
		linkOut bool
		want    string
	}{
		{
			name:   "a parent segment",
			path:   func(outsideDir string) string { return "../" + filepath.Base(outsideDir) + "/keep" },
			target: func(outsideDir, _ string) string { return filepath.Join(outsideDir, "keep") },
			want:   "want a path a walk of the claim root gives",
		},
		{
			name:   "an absolute path",
			path:   func(outsideDir string) string { return filepath.Join(outsideDir, "keep") },
			target: func(outsideDir, _ string) string { return filepath.Join(outsideDir, "keep") },
			want:   "want a path a walk of the claim root gives",
		},
		{
			name:   "a clean path under no cache tree",
			path:   func(string) string { return "other/keep" },
			target: func(_, root string) string { return filepath.Join(root, "other", "keep") },
			want:   "want a file under",
		},
		{
			name:   "the cache tree itself",
			path:   func(string) string { return cachevol.SeedBuildCacheDir },
			target: func(_, root string) string { return filepath.Join(root, cachevol.SeedBuildCacheDir) },
			want:   "want a file under",
		},
		{
			// The string is cache-relative and clean, but its parent is a link
			// out of the claim: the removal's os.Root is what refuses it.
			name:    "a path through a link out of the claim",
			path:    func(string) string { return "gocache/escape/keep" },
			target:  func(outsideDir, _ string) string { return filepath.Join(outsideDir, "keep") },
			linkOut: true,
			want:    "path escapes from parent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outsideDir := t.TempDir()
			writeFile(t, filepath.Join(outsideDir, "keep"), 10, base)
			root := emptyClaimRoot(t)
			writeFile(t, filepath.Join(root, "other", "keep"), 10, base)
			if tc.linkOut {
				if err := os.Symlink(outsideDir, filepath.Join(root, cachevol.SeedBuildCacheDir, "escape")); err != nil {
					t.Fatalf("planting the link: %v", err)
				}
			}
			path := tc.path(outsideDir)
			err := removeSweepCacheFile(root, path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("removeSweepCacheFile(%q) = %v, want a refusal naming %q", path, err, tc.want)
			}
			assertPresent(t, tc.target(outsideDir, root))
		})
	}
}

// TestTrimSweepCacheErrorDoesNotChangeTheVerdict: a planned removal that fails
// leaves the run's tier result as the sweep reported it, the run still exits 0,
// and the trim's failure is written to the run's log directory.
func TestTrimSweepCacheErrorDoesNotChangeTheVerdict(t *testing.T) {
	t.Parallel()
	removeErr := errors.New("the claim is read-only")
	cases := []struct {
		name string
		trim func(root string) *CacheTrim
		// logged is the failure the trim log must name.
		logged string
	}{
		{
			name:   "a failing remove",
			trim:   failingRemoveTrim(removeErr),
			logged: removeErr.Error(),
		},
		{
			// A directory the claim cannot list makes the listing partial, so the
			// trim fails before it removes anything.
			name: "a failing walk",
			trim: func(root string) *CacheTrim {
				claim := faultyClaim{
					MapFS: fakeClaim(map[string]*fstest.MapFile{"gocache/entry": fakeFile(10, time.Now())}),
					dirs:  map[string]error{cachevol.SeedBuildCacheDir: fs.ErrPermission},
				}
				return &CacheTrim{Root: root, CapBytes: 1, Claim: fakeClaimOf(claim), Remove: removeSweepCacheFile}
			},
			logged: "listing " + cachevol.SeedBuildCacheDir + ":",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := failingTrimHarness(t, tc.trim)

			code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", "integration", "--out", h.out, "--repo", "/repo.git")
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 with the result written (stderr %q)", code, h.stderr.String())
			}
			r, err := sweeprunner.Read(h.out, "gastown", testSHA)
			if err != nil {
				t.Fatalf("read the result: %v", err)
			}
			verdicts, err := sweeprunner.ToTierResults(r)
			if err != nil {
				t.Fatalf("convert the result: %v", err)
			}
			if v := verdicts[tiersweep.TierIntegration]; v.Verdict != "GREEN" {
				t.Errorf("integration = %+v, want the sweep's GREEN verdict", v)
			}
			logData, err := os.ReadFile(filepath.Join(h.out, "logs", cacheTrimLogName))
			if err != nil {
				t.Fatalf("read the trim log: %v", err)
			}
			if !strings.Contains(string(logData), tc.logged) {
				t.Errorf("trim log %q does not name the failure %q", logData, tc.logged)
			}
		})
	}
}

// TestTrimSweepCacheFailureFallsBackToStderr: a trim failure whose log cannot
// be written is still readable on stderr, and the result stands.
func TestTrimSweepCacheFailureFallsBackToStderr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, out string)
	}{
		{
			name: "the log directory is a file",
			setup: func(t *testing.T, out string) {
				writeFile(t, filepath.Join(out, "logs"), 10, time.Now())
			},
		},
		{
			name: "the log file is a directory",
			setup: func(t *testing.T, out string) {
				if err := os.MkdirAll(filepath.Join(out, "logs", cacheTrimLogName), 0o755); err != nil {
					t.Fatalf("making the log path a directory: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			removeErr := errors.New("the claim is read-only")
			h := failingTrimHarness(t, failingRemoveTrim(removeErr))
			tc.setup(t, h.out)
			code := h.run("--rig", "gastown", "--sha", testSHA, "--tiers", "integration", "--out", h.out, "--repo", "/repo.git")
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 with the result written (stderr %q)", code, h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), removeErr.Error()) {
				t.Errorf("stderr %q does not name the trim failure %q", h.stderr.String(), removeErr)
			}
			if !h.resultFileExists(t) {
				t.Error("the result was not written")
			}
		})
	}
}

// failingTrimHarness is a run whose sweep passes and whose claim trim is the one
// trim builds on the claim root.
func failingTrimHarness(t *testing.T, trim func(root string) *CacheTrim) *harness {
	t.Helper()
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
		fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
		return 0
	})
	h.deps.Trim = trim(emptyClaimRoot(t))
	return h
}

// failingRemoveTrim is a trim with one planned removal, which fails with err.
func failingRemoveTrim(err error) func(root string) *CacheTrim {
	return func(root string) *CacheTrim {
		claim := fakeClaim(map[string]*fstest.MapFile{"gocache/entry": fakeFile(10, time.Now())})
		return &CacheTrim{
			Root: root, CapBytes: 1,
			Claim:  fakeClaimOf(claim),
			Remove: func(string, string) error { return err },
		}
	}
}

// TestTrimSweepCacheRefusesAClaimItCannotOpen: a planned removal whose claim
// root cannot be opened is reported as a missing root rather than skipped.
func TestTrimSweepCacheRefusesAClaimItCannotOpen(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "gone")
	trim := &CacheTrim{
		Root: root, CapBytes: 1,
		Claim:  fakeClaimOf(fakeClaim(map[string]*fstest.MapFile{"gocache/entry": fakeFile(1000, time.Now())})),
		Remove: removeSweepCacheFile,
	}
	err := trimSweepCache(trim)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("trimSweepCache of a claim root that cannot be opened = %v, want a missing-root refusal", err)
	}
}

// faultyClaim is a fake claim whose listing fails where it names: a directory
// in dirs fails its read, and a file in stats fails its stat after the directory
// read, as a file another Job removes mid-walk does.
type faultyClaim struct {
	fstest.MapFS
	dirs  map[string]error
	stats map[string]error
}

// ReadDir is the fake's directory read, failing where dirs names the directory
// and returning the entries whose stat fails where stats names the file.
func (c faultyClaim) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := c.dirs[name]; err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(c.MapFS, name)
	if err != nil {
		return nil, err
	}
	for i, e := range entries {
		if err := c.stats[path.Join(name, e.Name())]; err != nil {
			entries[i] = statFailure{DirEntry: e, err: err}
		}
	}
	return entries, nil
}

// statFailure is a directory entry whose stat fails.
type statFailure struct {
	fs.DirEntry
	err error
}

// Info is the stat of the entry, which fails.
func (s statFailure) Info() (fs.FileInfo, error) { return nil, s.err }

// fakeClaim is a claim of files with both cache trees present, empty or not:
// MapFS makes a directory only of the files under it, so a tree with none would
// otherwise read as missing.
func fakeClaim(files map[string]*fstest.MapFile) fstest.MapFS {
	claim := fstest.MapFS(files)
	for _, dir := range []string{cachevol.SeedBuildCacheDir, cachevol.SeedModuleCacheDir} {
		if _, ok := claim[dir]; !ok {
			claim[dir] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		}
	}
	return claim
}

// fakeClaimOf is the Claim seam that hands out fsys for any root.
func fakeClaimOf(fsys fs.FS) func(string) (fs.FS, error) {
	return func(string) (fs.FS, error) { return fsys, nil }
}

// fakeFile is a fake claim file of size bytes, last used at mtime.
func fakeFile(size int, mtime time.Time) *fstest.MapFile {
	return &fstest.MapFile{Data: make([]byte, size), ModTime: mtime}
}

// recordRemovals is the Remove seam that records each planned path and removes
// nothing, so a fake claim is never touched on disk.
func recordRemovals(removed *[]string) func(string, string) error {
	return func(_, name string) error {
		*removed = append(*removed, name)
		return nil
	}
}

// cacheClaim builds a claim under t.TempDir(): four 40-byte build-cache files,
// oldest first, and a 100-byte module-cache file. Its entries span 260 bytes.
func cacheClaim(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"old", "mid", "new", "newest"} {
		writeFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, name), 40, base.Add(time.Duration(i)*time.Hour))
	}
	writeFile(t, filepath.Join(root, cachevol.SeedModuleCacheDir, "mod"), 100, base.Add(-time.Hour))
	return root
}

// emptyClaimRoot is a claim mount under t.TempDir() with both cache trees
// present and holding nothing.
func emptyClaimRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{cachevol.SeedBuildCacheDir, cachevol.SeedModuleCacheDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("making the %s cache: %v", dir, err)
		}
	}
	return root
}

// writeFile writes a file of size bytes with the given times, making its
// directories.
func writeFile(t *testing.T, name string, size int, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("making %s: %v", filepath.Dir(name), err)
	}
	if err := os.WriteFile(name, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	if err := os.Chtimes(name, mtime, mtime); err != nil {
		t.Fatalf("setting the times of %s: %v", name, err)
	}
}

// filesUnder is every file under root in sorted order, relative to root.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

// assertPresent reports a file the trim must have left where it was.
func assertPresent(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); err != nil {
		t.Errorf("%s is gone, want it kept: %v", name, err)
	}
}
