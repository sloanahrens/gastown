//go:build !windows

package cmd

import (
	"path/filepath"
	"syscall"
	"testing"
)

// TestReadBdCloseReasonFile_FIFODoesNotBlock: a --reason-file naming a FIFO
// with no writer would hang an open(2), and with it every Bash call the guard
// sees. A regression shows up as this test running into the package timeout.
func TestReadBdCloseReasonFile_FIFODoesNotBlock(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "reason.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBdCloseReasonFile(fifo); ok {
		t.Error("read a FIFO, want it refused")
	}
}
