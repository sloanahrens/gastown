package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/sweeprunner"
	"github.com/steveyegge/gastown/internal/tiersweep"
)

// TestTrimSweepCacheKeepsTheCap: a claim over the cap loses its least recently
// used build-cache files until it is at or under the cap and keeps its module
// cache, and a claim under the cap loses nothing.
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := cacheClaim(t)
			trim := &CacheTrim{Root: root, CapBytes: tc.cap, List: listSweepCache, Remove: removeSweepCacheFile}
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
// is missing one of its seams, is reported rather than read as a claim with
// nothing to remove.
func TestTrimSweepCacheRefusesAnUnwiredTrim(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []struct {
		name string
		trim *CacheTrim
		want string
	}{
		{"no trim", nil, "not wired"},
		{"no root", &CacheTrim{List: listSweepCache, Remove: removeSweepCacheFile}, "no claim root"},
		{"a relative root", &CacheTrim{Root: "cache", List: listSweepCache, Remove: removeSweepCacheFile}, "absolute path"},
		{"a negative cap", &CacheTrim{Root: root, CapBytes: -1, List: listSweepCache, Remove: removeSweepCacheFile}, "names a -1-byte cap"},
		{"no listing", &CacheTrim{Root: root, Remove: removeSweepCacheFile}, "no listing"},
		{"no removal", &CacheTrim{Root: root, List: listSweepCache}, "no removal"},
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
	trim := &CacheTrim{Root: root, CapBytes: 1, List: listSweepCache, Remove: removeSweepCacheFile}
	if err := trimSweepCache(trim); err != nil {
		t.Fatalf("trimSweepCache without a claim root: %v", err)
	}
}

// TestTrimSweepCacheRefusesAClaimItCannotRead: a root that is a symbolic link
// or not a directory is reported, never walked as an empty claim.
func TestTrimSweepCacheRefusesAClaimItCannotRead(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	linkRoot := filepath.Join(t.TempDir(), "claim")
	if err := os.Symlink(t.TempDir(), linkRoot); err != nil {
		t.Fatalf("planting the claim link: %v", err)
	}
	fileRoot := filepath.Join(t.TempDir(), "claim")
	writeFile(t, fileRoot, 10, fixed)
	cases := []struct {
		name string
		root string
		want string
	}{
		{"a symbolic link", linkRoot, "symbolic link"},
		{"a file", fileRoot, "not a directory"},
		// A path whose parent is a file cannot be measured, and that failure is
		// not a claim that is simply not mounted.
		{"a path through a file", filepath.Join(fileRoot, "claim"), "listing the cache claim"},
		// A claim tree that cannot be read is a failure, never an empty tree.
		{"a cache tree that cannot be read", unreadableCacheRoot(t, fixed), "listing the cache claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trim := &CacheTrim{Root: tc.root, CapBytes: 1, List: listSweepCache, Remove: removeSweepCacheFile}
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
	root := t.TempDir()
	trim := &CacheTrim{
		Root: root, CapBytes: 1,
		List: func(string) ([]cachevol.Entry, error) {
			return []cachevol.Entry{{Path: "gocache/gone", Cache: cachevol.BuildCache, Size: 1000}}, nil
		},
		Remove: removeSweepCacheFile,
	}
	if err := trimSweepCache(trim); err != nil {
		t.Fatalf("trimSweepCache of a file another run removed: %v", err)
	}
}

// TestTrimSweepCacheRefusesPathsOutsideTheCaches: a symbolic link in a cache
// tree and a planned path that leaves the two trees are refused, the files they
// name are still there, and the refusal is reported.
func TestTrimSweepCacheRefusesPathsOutsideTheCaches(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("a symbolic link in the cache", func(t *testing.T) {
		t.Parallel()
		outside := filepath.Join(t.TempDir(), "keep")
		writeFile(t, outside, 10, fixed)
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, cachevol.SeedBuildCacheDir), 0o755); err != nil {
			t.Fatalf("making the build cache: %v", err)
		}
		link := filepath.Join(root, cachevol.SeedBuildCacheDir, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("planting the link: %v", err)
		}
		trim := &CacheTrim{Root: root, CapBytes: 1, List: listSweepCache, Remove: removeSweepCacheFile}
		err := trimSweepCache(trim)
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("trimSweepCache = %v, want a refusal naming the symbolic link", err)
		}
		assertPresent(t, outside)
		assertPresent(t, link)
	})

	cases := []struct {
		name string
		// path is the entry a plan names, target the file that must survive the
		// trim's refusal, and want a phrase of the refusal; empty accepts any
		// refusal the removal gives.
		path    func(outsideDir string) string
		target  func(outsideDir, root string) string
		linkOut bool
		want    string
	}{
		{
			name:   "a parent segment",
			path:   func(outsideDir string) string { return "../" + filepath.Base(outsideDir) + "/keep" },
			target: func(outsideDir, _ string) string { return filepath.Join(outsideDir, "keep") },
			want:   "want a clean path",
		},
		{
			name:   "an absolute path",
			path:   func(outsideDir string) string { return filepath.Join(outsideDir, "keep") },
			target: func(outsideDir, _ string) string { return filepath.Join(outsideDir, "keep") },
			want:   "want a clean path",
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
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outsideDir := t.TempDir()
			writeFile(t, filepath.Join(outsideDir, "keep"), 10, fixed)
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "other", "keep"), 10, fixed)
			for _, dir := range []string{cachevol.SeedBuildCacheDir, cachevol.SeedModuleCacheDir} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatalf("making the %s cache: %v", dir, err)
				}
			}
			if tc.linkOut {
				if err := os.Symlink(outsideDir, filepath.Join(root, cachevol.SeedBuildCacheDir, "escape")); err != nil {
					t.Fatalf("planting the link: %v", err)
				}
			}
			path := tc.path(outsideDir)
			trim := &CacheTrim{
				Root: root, CapBytes: 1,
				List: func(string) ([]cachevol.Entry, error) {
					return []cachevol.Entry{{Path: path, Cache: cachevol.BuildCache, Size: 1000}}, nil
				},
				Remove: removeSweepCacheFile,
			}
			err := trimSweepCache(trim)
			if err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("trimSweepCache(%q) = %v, want a refusal naming %q", path, err, tc.want)
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
	h := failingTrimHarness(t, removeErr)

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
	if !strings.Contains(string(logData), removeErr.Error()) {
		t.Errorf("trim log %q does not name the failure %q", logData, removeErr)
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
			h := failingTrimHarness(t, removeErr)
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

// failingTrimHarness is a run whose sweep passes and whose claim removal fails.
func failingTrimHarness(t *testing.T, removeErr error) *harness {
	t.Helper()
	h := newHarness(t, nil, func(_ []string, overrides map[string]string, stdout, _ io.Writer) int {
		fmt.Fprintf(stdout, "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs %s) in 1s\n", overrides[tiersweep.EnvLogDir])
		return 0
	})
	root := t.TempDir()
	writeFile(t, filepath.Join(root, cachevol.SeedBuildCacheDir, "entry"), 10, time.Now())
	h.deps.Trim = &CacheTrim{
		Root: root, CapBytes: 1,
		List:   listSweepCache,
		Remove: func(string, string) error { return removeErr },
	}
	return h
}

// unreadableCacheRoot is a claim whose build cache an unprivileged process
// cannot read, with the mode restored when the test ends.
func unreadableCacheRoot(t *testing.T, mtime time.Time) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("a root-owned process reads a 0o000 directory, so the failure cannot be made")
	}
	root := t.TempDir()
	tree := filepath.Join(root, cachevol.SeedBuildCacheDir)
	writeFile(t, filepath.Join(tree, "entry"), 10, mtime)
	if err := os.Chmod(tree, 0o000); err != nil {
		t.Fatalf("making the build cache unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(tree, 0o755); err != nil {
			t.Errorf("restoring the mode of %s: %v", tree, err)
		}
	})
	return root
}

// TestTrimSweepCacheRefusesAClaimItCannotOpen: a planned removal whose claim
// root cannot be opened is reported rather than skipped.
func TestTrimSweepCacheRefusesAClaimItCannotOpen(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "gone")
	trim := &CacheTrim{
		Root: root, CapBytes: 1,
		List: func(string) ([]cachevol.Entry, error) {
			return []cachevol.Entry{{Path: "gocache/entry", Cache: cachevol.BuildCache, Size: 1000}}, nil
		},
		Remove: removeSweepCacheFile,
	}
	if err := trimSweepCache(trim); err == nil {
		t.Fatal("trimSweepCache of a claim root that cannot be opened = nil, want a refusal")
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
