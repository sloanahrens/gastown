package testutil

import (
	"os"
	"testing"
)

// TestPrivateTempDirIsPrivate: the helper's directory is 0700 whatever the
// process umask left t.TempDir, so a test that refuses a group- or
// world-writable path can use it without chmodding its own directory
// (gt-aq8dej).
func TestPrivateTempDirIsPrivate(t *testing.T) {
	t.Parallel()
	dir := PrivateTempDir(t)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("%s mode = %o, want 700", dir, got)
	}
}

// TestPrivateTempDirGivesTwoDirectories: two calls return two directories, so
// one test's private files never land in another's (gt-aq8dej).
func TestPrivateTempDirGivesTwoDirectories(t *testing.T) {
	t.Parallel()
	first, second := PrivateTempDir(t), PrivateTempDir(t)
	if first == second {
		t.Errorf("two calls returned the same directory %s", first)
	}
}
