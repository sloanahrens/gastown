package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// The Cost panel's ledger read carries the malformed lines it skipped, and a
// ledger that cannot be read is an empty read rather than a day of zeros
// (gt-59v3x).
func TestDashboardSpendHoursFromCarriesTheSkippedLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "balance.log")
	ledger := "2026-10-25T16:05:00 balance=80.00\nnot a row\n"
	if err := os.WriteFile(path, []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	read := dashboardSpendHoursFrom(path)()
	if read.Skipped != 1 {
		t.Errorf("skipped = %d, want the one malformed line", read.Skipped)
	}
	if len(read.Hours) != 24 {
		t.Errorf("hours = %d, want the day's 24", len(read.Hours))
	}
}

func TestDashboardSpendHoursFromReadsAnUnreadableLedgerAsEmpty(t *testing.T) {
	t.Parallel()
	read := dashboardSpendHoursFrom(t.TempDir())() // a directory is not a ledger
	if read.Hours != nil || read.Skipped != 0 {
		t.Errorf("read = %+v, want an empty read", read)
	}
}
