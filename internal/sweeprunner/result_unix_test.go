//go:build !windows

package sweeprunner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// resultReadyBound is how long a Read may take before the test calls it
// blocked. A read of a few KB from a regular file takes microseconds; the
// bound exists to end the test at once if the read is waiting on a FIFO that
// will never have a writer (gt-u00wo).
const resultReadyBound = 5 * time.Second

// TestResultReadRefusesNonRegularFiles: the output directory is written by the
// container that runs the sweep, so a symlink or a FIFO at the result's name is
// a value this package did not write. Read refuses both, and refuses the FIFO
// without waiting for a writer.
func TestResultReadRefusesNonRegularFiles(t *testing.T) {
	t.Parallel()
	valid := sampleResult()

	// A symlink to a valid result is the case that matters: a plain open would
	// follow it and return a result from outside the run's output.
	t.Run("a symlink is refused, not followed", func(t *testing.T) {
		t.Parallel()
		elsewhere := t.TempDir()
		if err := Write(elsewhere, valid); err != nil {
			t.Fatalf("Write the linked-to result: %v", err)
		}
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(elsewhere, ResultFileName), filepath.Join(dir, ResultFileName)); err != nil {
			t.Fatalf("create the symlink: %v", err)
		}

		_, err := Read(dir, valid.Rig, valid.SHA)
		if err == nil {
			t.Fatal("Read followed the symlink and returned the result it points at")
		}
		if !strings.Contains(err.Error(), "a symlink") {
			t.Fatalf("error %q does not name the symlink", err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the symlink error %v reads as a missing file", err)
		}
	})

	t.Run("a FIFO is refused without waiting for a writer", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, ResultFileName), 0o600); err != nil {
			t.Fatalf("create the FIFO: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := Read(dir, valid.Rig, valid.SHA)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("Read returned a result from a FIFO")
			}
			if !strings.Contains(err.Error(), "a FIFO") {
				t.Fatalf("error %q does not name the FIFO", err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the FIFO error %v reads as a missing file", err)
			}
		case <-time.After(resultReadyBound): //testpolicy:allow no-sleep — a hang guard only: Read refuses the FIFO through Lstat before it opens anything, so the assertion is the error the read returns, and only a regression that blocked would ever reach this branch
			t.Fatalf("Read blocked on a FIFO for %s", resultReadyBound)
		}
	})
}
