//go:build integration

package cmd

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestIntegrationExecPrimeExternalCommand_AbandonsWedgedTool is the subprocess
// half of prime's tool bound (the deadline itself is unit-tested on a fake
// clock): when the context ends, a real tool that has left a child holding
// its stdout open must be abandoned, not waited out. The child sleeps for an
// hour, so the call returning at all is the proof; a FIFO tells the test the
// tool is running before the context is cancelled, with no polling.
func TestIntegrationExecPrimeExternalCommand_AbandonsWedgedTool(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "started")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := execPrimeExternalCommand(ctx, dir, "sh", "-c", `echo started > "$0"; sleep 3600 & wait`, fifo)
		done <- err
	}()

	f, err := os.Open(fifo) // blocks until the tool opens the FIFO to write
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	_ = f.Close()
	if err != nil || line != "started\n" {
		t.Fatalf("read fifo = %q, %v; want the tool's start line", line, err)
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("abandoned tool reported success")
	}
}
