//go:build !windows

package cmd

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadBdCloseReasonFile_FIFODoesNotBlock: a --reason-file naming a FIFO
// with no writer would hang an open(2), and with it every Bash call the guard
// sees.
func TestReadBdCloseReasonFile_FIFODoesNotBlock(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "reason.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO support: %v", err)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := readBdCloseReasonFile(fifo)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("read a FIFO, want it refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readBdCloseReasonFile blocked on a FIFO")
	}
}
