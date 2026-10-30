//go:build unix

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// awaitEventReadBudget is the far budget the bounded-read test passes in.
// It outlives the hang guard, so a read that ignores its context is caught
// by the guard rather than answered by the budget.
const awaitEventReadBudget = 10 * time.Minute

// TestReadPendingEventsBoundedCtxDone drives readPendingEventsBounded with an
// already-cancelled context. The event file is a fifo nobody writes, so
// reading it blocks until a writer opens it: a read that ignored its context
// would sit there until the far budget ran out instead of returning.
//
// The fifo needs syscall.Mkfifo, which is why this test lives in a
// unix-only file.
func TestReadPendingEventsBoundedCtxDone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "blocked.event")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", fifo, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan []EventFile, 1)
	go func() {
		done <- readPendingEventsBounded(ctx, dir, awaitEventReadBudget)
	}()

	awaitEventWithin(t, done, "readPendingEventsBounded")

	// Open the fifo for writing so the blocked read sees EOF and its
	// goroutine exits.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s for write: %v", fifo, err)
	}
	w.Close()
}
