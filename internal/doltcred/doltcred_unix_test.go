//go:build unix

package doltcred

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReadRefusesAFIFOWithoutBlocking: a FIFO is refused without waiting for a writer, which O_NONBLOCK guarantees.
func TestReadRefusesAFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dolt-credential")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Read(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("Read a FIFO = %v, want the regular-file refusal naming the path", err)
		}
	case <-time.After(5 * time.Second): //testpolicy:allow no-sleep — a hang guard: only an open that blocked reaches this branch
		t.Fatal("Read blocked on a FIFO with no writer")
	}
}
