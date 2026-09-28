//go:build integration

package cmd

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// primeToolKillBound is how long execPrimeExternalCommand may take to return
// once its context is cancelled. The kill and primeExternalToolWaitDelay take
// about a second; a minute fails only a tool that is not being abandoned.
const primeToolKillBound = 60 * time.Second

// TestIntegrationExecPrimeExternalCommand_AbandonsWedgedTool is the subprocess
// half of prime's tool bound (the deadline itself is unit-tested on a fake
// clock): when the context ends, the call must return, and report failure,
// even though the tool has a child holding its stdout open.
//
// The child is a background sleep in the tool's own process group, so the
// group kill that util.SetProcessGroup installs as cmd.Cancel reaches it;
// cmd.WaitDelay is the backstop for a child that left the group, and this
// test does not isolate which of the two ended the wait. The tool writes its
// pid to a FIFO, so the test knows it is running before it cancels, with no
// polling; on failure the whole group is killed by that pid.
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
		_, _, err := execPrimeExternalCommand(ctx, dir, "sh", "-c", `echo $$ > "$0"; sleep 3600 & wait`, fifo)
		done <- err
	}()

	f, err := os.Open(fifo) // blocks until the tool opens the FIFO to write
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	_ = f.Close()
	pid, convErr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || convErr != nil || pid <= 0 {
		t.Fatalf("read fifo = %q, %v; want the tool's pid", line, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(-pid, syscall.SIGKILL) // the tool leads its own group
		}
	})

	cancel()
	bound := time.NewTimer(primeToolKillBound)
	defer bound.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("abandoned tool reported success")
		}
	case <-bound.C:
		t.Fatalf("execPrimeExternalCommand still waiting %v after its context was cancelled", primeToolKillBound)
	}
}
