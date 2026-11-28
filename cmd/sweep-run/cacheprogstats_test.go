package main

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// stagedStats plants a stats file of content under a fresh cache root.
func stagedStats(t *testing.T, content []byte) (root, src string) {
	t.Helper()
	root = t.TempDir()
	src = filepath.Join(root, cacheProgDirName, cacheProgStatsName)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, src
}

// sparseStats plants a stats file of size bytes without writing its contents.
func sparseStats(t *testing.T, size int64) (root, src string) {
	t.Helper()
	root, src = stagedStats(t, nil)
	if err := os.Truncate(src, size); err != nil {
		t.Fatal(err)
	}
	return root, src
}

// refuse returns a removal that fails with err and records each path asked for.
func refuse(asked *[]string) func(string) error {
	return func(name string) error {
		*asked = append(*asked, name)
		return os.ErrPermission
	}
}

// mustGone fails the test when path still exists.
func mustGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s should be gone, Lstat err = %v", path, err)
	}
}

// mustKeep fails the test when path is gone.
func mustKeep(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s should still exist: %v", path, err)
	}
}

// TestCacheProgStatsMove: the file is copied byte for byte, its source removed, and nil returned.
func TestCacheProgStatsMove(t *testing.T) {
	t.Parallel()
	content := []byte("{\"hits\":1}\n\x00\xff\xfe tail\n")
	root, src := stagedStats(t, content)
	logDir := t.TempDir()

	if err := moveCacheProgStats(root, logDir, os.Remove); err != nil {
		t.Fatalf("moveCacheProgStats: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(logDir, cacheProgStatsCopyName))
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("copy = %q, %v; want %q", got, err, content)
	}
	mustGone(t, src)
}

// TestCacheProgStatsAbsent: a run whose cache program wrote no stats file returns nil and writes no copy.
func TestCacheProgStatsAbsent(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()

	if err := moveCacheProgStats(t.TempDir(), logDir, os.Remove); err != nil {
		t.Fatalf("moveCacheProgStats with no stats file: %v", err)
	}
	mustGone(t, filepath.Join(logDir, cacheProgStatsCopyName))
}

// TestCacheProgStatsOversize: a file over 8 MiB is removed uncopied with an error. When that removal fails,
// the error is returned and the file stays.
func TestCacheProgStatsOversize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		remove   func(string) error
		wantGone bool
	}{
		{"removed", os.Remove, true},
		{"removal fails", func(string) error { return os.ErrPermission }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, src := sparseStats(t, cacheProgStatsMaxBytes+1)
			logDir := t.TempDir()

			if err := moveCacheProgStats(root, logDir, tc.remove); err == nil {
				t.Fatal("moveCacheProgStats over 8 MiB returned nil")
			}
			mustGone(t, filepath.Join(logDir, cacheProgStatsCopyName))
			if tc.wantGone {
				mustGone(t, src)
			} else {
				mustKeep(t, src)
			}
		})
	}
}

// TestCacheProgStatsLimitBoundary: a file of exactly 8 MiB is copied; the limit refuses only what is over it.
func TestCacheProgStatsLimitBoundary(t *testing.T) {
	t.Parallel()
	root, src := sparseStats(t, cacheProgStatsMaxBytes)
	logDir := t.TempDir()

	if err := moveCacheProgStats(root, logDir, os.Remove); err != nil {
		t.Fatalf("moveCacheProgStats at exactly 8 MiB: %v", err)
	}
	info, err := os.Stat(filepath.Join(logDir, cacheProgStatsCopyName))
	if err != nil || info.Size() != cacheProgStatsMaxBytes {
		t.Errorf("copy = %v, %v; want %d bytes", info, err, cacheProgStatsMaxBytes)
	}
	mustGone(t, src)
}

// TestCacheProgStatsRemoveFailsAfterCopy: when the source cannot be removed after the copy, the copy is dropped
// and the source is kept, so the lines are not counted twice.
func TestCacheProgStatsRemoveFailsAfterCopy(t *testing.T) {
	t.Parallel()
	content := []byte("kept\n")
	root, src := stagedStats(t, content)
	logDir := t.TempDir()
	var asked []string

	if err := moveCacheProgStats(root, logDir, refuse(&asked)); err == nil {
		t.Fatal("moveCacheProgStats whose removal fails returned nil")
	}
	if len(asked) != 1 || asked[0] != src {
		t.Errorf("removal asked for %v, want [%s]", asked, src)
	}
	mustGone(t, filepath.Join(logDir, cacheProgStatsCopyName))
	if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, content) {
		t.Errorf("source = %q, %v; want it unchanged", got, err)
	}
}

// TestCacheProgStatsWriteFails: a copy that cannot be created is an error; the source is not removed.
func TestCacheProgStatsWriteFails(t *testing.T) {
	t.Parallel()
	root, src := stagedStats(t, []byte("kept\n"))
	var asked []string

	logDir := filepath.Join(t.TempDir(), "no-such-dir")
	if err := moveCacheProgStats(root, logDir, refuse(&asked)); err == nil {
		t.Fatal("moveCacheProgStats into a missing log directory returned nil")
	}
	if len(asked) != 0 {
		t.Errorf("removal asked for %v after a failed copy", asked)
	}
	mustKeep(t, src)
}

// TestCacheProgStatsExistingCopy: an earlier copy in the log directory is never overwritten, and the source stays.
func TestCacheProgStatsExistingCopy(t *testing.T) {
	t.Parallel()
	root, src := stagedStats(t, []byte("new\n"))
	logDir := t.TempDir()
	copyPath := filepath.Join(logDir, cacheProgStatsCopyName)
	prior := []byte("an earlier copy\n")
	if err := os.WriteFile(copyPath, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	var asked []string

	if err := moveCacheProgStats(root, logDir, refuse(&asked)); err == nil {
		t.Fatal("moveCacheProgStats over an existing copy returned nil")
	}
	if len(asked) != 0 {
		t.Errorf("removal asked for %v over an existing copy", asked)
	}
	if got, err := os.ReadFile(copyPath); err != nil || !bytes.Equal(got, prior) {
		t.Errorf("earlier copy = %q, %v; want it unchanged", got, err)
	}
	mustKeep(t, src)
}

// TestCacheProgStatsNotRegular: a named pipe is refused before it is read, since a read would block.
func TestCacheProgStatsNotRegular(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, cacheProgDirName, cacheProgStatsName)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(src, 0o600); err != nil {
		t.Fatal(err)
	}
	var asked []string

	if err := moveCacheProgStats(root, t.TempDir(), refuse(&asked)); err == nil {
		t.Fatal("moveCacheProgStats on a named pipe returned nil")
	}
	if len(asked) != 0 {
		t.Errorf("removal asked for %v on a named pipe", asked)
	}
	mustKeep(t, src)
}

// TestCacheProgStatsNilRemove: a move without a removal is refused before any file is touched.
func TestCacheProgStatsNilRemove(t *testing.T) {
	t.Parallel()
	root, src := stagedStats(t, []byte("kept\n"))
	logDir := t.TempDir()

	if err := moveCacheProgStats(root, logDir, nil); err == nil {
		t.Fatal("moveCacheProgStats with no removal returned nil")
	}
	mustGone(t, filepath.Join(logDir, cacheProgStatsCopyName))
	mustKeep(t, src)
}
