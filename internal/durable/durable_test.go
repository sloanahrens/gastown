package durable

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFlushesRunByDefault: a policy that skips nothing reaches the file system:
// a closed file cannot be synced, and a directory that does not exist cannot be
// opened.
func TestFlushesRunByDefault(t *testing.T) {
	t.Parallel()
	var p policy
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.file(f); err != nil {
		t.Fatalf("syncing an open file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.file(f); err == nil {
		t.Errorf("syncing a closed file succeeded, so the flush did not reach the file")
	}
	if err := p.dir(t.TempDir()); err != nil && !os.IsPermission(err) {
		t.Logf("a filesystem that declines a directory sync: %v", err)
	}
	if err := p.dir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Errorf("syncing a missing directory succeeded, so the flush did not reach it")
	}
}

// TestSkippedFlushesTouchNothing: after skipForUnitTier in a test binary, File on a
// closed *os.File returns nil where Sync would fail, and Dir on a directory that is
// not there returns nil where Open would fail.
func TestSkippedFlushesTouchNothing(t *testing.T) {
	t.Parallel()
	var p policy
	skipForUnitTier(&p, func() bool { return true })
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err == nil {
		t.Fatalf("Sync on a closed file succeeded, so the check below proves nothing")
	}
	if err := p.file(f); err != nil {
		t.Errorf("a skipped file flush returned %v", err)
	}
	if err := p.dir(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("a skipped directory flush returned %v", err)
	}
}

// TestSkipRefusesOutsideATestBinary: the guard panics when the process is not a
// test binary, and leaves the flushes on.
func TestSkipRefusesOutsideATestBinary(t *testing.T) {
	t.Parallel()
	var p policy
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("skipForUnitTier did not panic outside a test binary")
			}
		}()
		skipForUnitTier(&p, func() bool { return false })
	}()
	if p.skip.Load() {
		t.Errorf("a refused skip still turned the flushes off")
	}
}
