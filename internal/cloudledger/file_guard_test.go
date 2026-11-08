package cloudledger_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
)

// A whole-ledger read refuses a ledger over the cap by its size, before the
// bytes are read, and refuses a FIFO named like a ledger rather than opening
// it: an open of a FIFO with no writer blocks. The FIFO read runs in a
// goroutine so a missing guard fails the test by timeout instead of hanging
// the suite.
func TestCloudLedgerReleasesSinceRefusesAnOversizeLedgerAndAFifo(t *testing.T) {
	t.Parallel()

	ledgers := t.TempDir()
	big, err := os.Create(filepath.Join(ledgers, "big.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file one byte past the cap: the size alone is the refusal.
	if err := big.Truncate(cloudledger.LedgerMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := big.Close(); err != nil {
		t.Fatal(err)
	}
	src := cloudledger.NewFileSource("", ledgers)
	if _, err := src.ReleasesSince(context.Background(), "big", ledgerNow); !errors.Is(err, cloudledger.ErrLedgerRead) || !errors.Is(err, cloudledger.ErrOversize) {
		t.Errorf("an oversize ledger = %v, want ErrLedgerRead naming ErrOversize", err)
	}

	fifo := filepath.Join(ledgers, "wire.jsonl")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Without the guard the read sits in open(2) on the FIFO; opening it
		// read-write releases it so the test leaves nothing behind.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := src.ReleasesSince(context.Background(), "wire", ledgerNow)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, cloudledger.ErrLedgerRead) || !errors.Is(err, cloudledger.ErrNotRegular) {
			t.Errorf("a FIFO ledger = %v, want ErrLedgerRead naming ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second): //testpolicy:allow no-sleep — a hang guard only: ReleasesSince refuses the FIFO through Lstat before it opens anything, so the assertion is the error the read returns, and only a regression that blocked would reach this branch
		t.Fatal("ReleasesSince blocked on a FIFO ledger")
	}
}
