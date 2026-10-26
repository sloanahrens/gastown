//go:build unix

package spans

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recordLockHeld reports whether another process holds a record lock on path,
// by asking the kernel (F_GETLK) rather than trying to take the lock. The probe
// takes nothing, so it does not change what it measures.
func recordLockHeld(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the lock file to probe it: %v", err)
	}
	defer f.Close()
	probe := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &probe); err != nil {
		t.Fatalf("probe the lock file: %v", err)
	}
	return probe.Type != syscall.F_UNLCK
}

// The append lock refuses a symlink at its path: it is never followed to a file
// the store does not own (gt-rm0ab).
func TestLockLog_RefusesASymlinkAtTheLockPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.lock")
	if err := os.WriteFile(elsewhere, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "gt-abc.jsonl.lock")
	if err := os.Symlink(elsewhere, lockPath); err != nil {
		t.Fatal(err)
	}
	lk, err := lockLog(lockPath)
	if err == nil {
		lk.unlock()
		t.Fatal("lockLog followed a symlink at the lock path, want a refusal")
	}
}

// A lock whose directory does not exist is an error, not a silent no-op: an
// append that cannot take its lock must not write (gt-rm0ab).
func TestLockLog_ErrorsWhenTheDirectoryIsMissing(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "no-such-dir", "gt-abc.jsonl.lock")
	lk, err := lockLog(lockPath)
	if err == nil {
		lk.unlock()
		t.Fatal("lockLog in a missing directory = nil error, want one")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lockLog error = %v, want the missing directory named", err)
	}
}

// Summarize refuses a named pipe at the log's path, and returns: a FIFO with no
// writer would otherwise hold the read until one appears (gt-rm0ab).
func TestSummarize_DoesNotBlockOnAFIFO(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	fifo := filepath.Join(dir, bead+".jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := New(dir, fixedClock{at: testEpoch}).Summarize(bead)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Summarize of a FIFO = %v, want the not-a-regular-file refusal", err)
		}
	//testpolicy:allow no-sleep — bound the wait for the read, which fails rather than hangs
	case <-time.After(spanLockWait):
		t.Fatal("Summarize blocked on a FIFO with no writer")
	}
}
