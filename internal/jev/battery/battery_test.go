package battery

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/lock"
)

func ledgerRec(ts time.Time, caller string, in, out int) string {
	return `{"ts":"` + ts.Format(time.RFC3339) + `","caller":"` + caller +
		`","input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}`
}

func writeLedger(t *testing.T, town string, recs ...string) string {
	t.Helper()
	path := jev.UsageLedgerPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(recs, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The cap is one town-wide cap: a second caller's spend today counts toward
// the first caller's room, and yesterday's does not.
func TestBatteryCapSpentCountsEveryCallerSinceMidnight(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	midnight := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	ledger := writeLedger(t, t.TempDir(),
		ledgerRec(midnight.Add(-time.Minute), "jev-dedupe", 5_000_000, 0), // yesterday
		ledgerRec(midnight, "jev-dedupe", 300_000_000, 100_000_000),       // today, first caller
		ledgerRec(now.Add(-time.Hour), "jev-dispatch", 900_000_000, 0),    // today, second caller
	)
	c := Cap{Ledger: ledger, MaxUSDPerDay: 1, USDPerBillionTokens: 2}

	got, err := c.Spent(func() time.Time { return now })
	if err != nil {
		t.Fatalf("Spent: %v", err)
	}
	// 1.3B tokens at 2 USD per billion.
	if math.Abs(got-2.6) > 1e-9 {
		t.Errorf("spent = %v, want 2.6 across both callers", got)
	}
}

func TestBatteryCapAllows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	clock := func() time.Time { return now }
	tests := []struct {
		name   string
		tokens int // other caller's tokens today, at 1 USD per billion
		want   bool
	}{
		{"nothing spent", 0, true},
		{"under the cap", 400_000_000, true},
		{"at the cap", 1_000_000_000, false},
		{"over the cap", 2_000_000_000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ledger := writeLedger(t, t.TempDir(), ledgerRec(now.Add(-time.Hour), "someone-else", tt.tokens, 0))
			c := Cap{Ledger: ledger, MaxUSDPerDay: 1, USDPerBillionTokens: 1}
			got, err := c.Allows(clock)
			if err != nil || got != tt.want {
				t.Errorf("Allows = %v, %v, want %v and no error", got, err, tt.want)
			}
		})
	}
}

func TestBatteryCapWithNoLedgerIsZero(t *testing.T) {
	t.Parallel()
	c := Cap{Ledger: jev.UsageLedgerPath(t.TempDir()), MaxUSDPerDay: 1, USDPerBillionTokens: 2}
	got, err := c.Spent(nil)
	if err != nil || got != 0 {
		t.Errorf("Spent = %v, %v, want 0 and no error", got, err)
	}
	if ok, err := c.Allows(nil); err != nil || !ok {
		t.Errorf("Allows = %v, %v, want true and no error", ok, err)
	}
}

func TestBatteryCapValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    Cap
		ok   bool
	}{
		{"both set", Cap{MaxUSDPerDay: 1, USDPerBillionTokens: 1}, true},
		{"no cap", Cap{USDPerBillionTokens: 1}, false},
		{"negative cap", Cap{MaxUSDPerDay: -1, USDPerBillionTokens: 1}, false},
		{"no price", Cap{MaxUSDPerDay: 1}, false},
		{"infinite price", Cap{MaxUSDPerDay: 1, USDPerBillionTokens: math.Inf(1)}, false},
		{"NaN cap", Cap{MaxUSDPerDay: math.NaN(), USDPerBillionTokens: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.c.Validate(); (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

type row struct {
	N int    `json:"n"`
	S string `json:"s,omitempty"`
}

func TestBatteryLogAppendModesAndReadRecent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "log.jsonl")
	for i := 1; i <= 5; i++ {
		if err := Append(path, row{N: i}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, %v, want 0600", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode = %v, %v, want 0700", fi.Mode(), err)
	}

	var got []row
	if err := ReadRecent(path, 3, &got); err != nil {
		t.Fatalf("ReadRecent: %v", err)
	}
	if len(got) != 3 || got[0].N != 3 || got[2].N != 5 {
		t.Errorf("last 3 = %+v, want 3,4,5 oldest first", got)
	}
	if err := ReadRecent(path, 50, &got); err != nil || len(got) != 5 {
		t.Errorf("last 50 = %d lines, %v, want all 5", len(got), err)
	}
}

func TestBatteryLogReadRecentMissingSkipsBadLinesAndRejectsBadTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	got := []row{{N: 9}}
	if err := ReadRecent(filepath.Join(dir, "none.jsonl"), 5, &got); err != nil || len(got) != 0 {
		t.Errorf("missing log = %+v, %v, want empty and no error", got, err)
	}

	path := filepath.Join(dir, "log.jsonl")
	body := `{"n":1}` + "\n" + `not json` + "\n\n" + `{"n":2}` + "\n" + `{"n":3` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The bad lines are skipped before the last n are taken.
	if err := ReadRecent(path, 2, &got); err != nil || len(got) != 2 || got[0].N != 1 || got[1].N != 2 {
		t.Errorf("last 2 = %+v, %v, want the two parseable lines", got, err)
	}

	var notSlice row
	for _, bad := range []any{&notSlice, got, nil} {
		if err := ReadRecent(path, 2, bad); err == nil {
			t.Errorf("ReadRecent into %T succeeded, want an error", bad)
		}
	}
}

func TestBatteryLogAppendWaitsForTheLogLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log.jsonl")
	release, ok, err := lock.FlockTryAcquire(path + ".flock")
	if err != nil || !ok {
		t.Fatalf("taking the log lock = %v, %v, want held", ok, err)
	}

	done := make(chan error, 1)
	go func() { done <- Append(path, row{N: 1}) }()
	// Append blocks on the held lock before it creates the log, so nothing
	// is written until release. This check cannot fail for a correct Append.
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the log was created while another holder had the log lock")
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("Append after the lock was released: %v", err)
	}
	var got []row
	if err := ReadRecent(path, 5, &got); err != nil || len(got) != 1 || got[0].N != 1 {
		t.Errorf("log = %+v, %v, want the one appended line", got, err)
	}
}

func TestBatteryLogConcurrentAppendsAfterAPartialLineKeepEveryLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(path, []byte(`{"n":0`), 0o600); err != nil {
		t.Fatal(err)
	}

	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 1; i <= writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- Append(path, row{N: n})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var got []row
	if err := ReadRecent(path, writers+1, &got); err != nil {
		t.Fatalf("ReadRecent: %v", err)
	}
	seen := make(map[int]bool, len(got))
	for _, r := range got {
		seen[r.N] = true
	}
	for i := 1; i <= writers; i++ {
		if !seen[i] {
			t.Errorf("line %d was lost or split; read %d parseable lines", i, len(got))
		}
	}
}

func TestBatteryCursorRoundTripAndMissing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "c.json")
	if _, ok, err := ReadCursor(path); ok || err != nil {
		t.Fatalf("missing cursor = ok %v, err %v, want neither", ok, err)
	}
	want := Cursor{Time: time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC), ID: "gt-x"}
	if err := WriteCursor(path, want); err != nil {
		t.Fatalf("WriteCursor: %v", err)
	}
	got, ok, err := ReadCursor(path)
	if err != nil || !ok || !got.Time.Equal(want.Time) || got.ID != want.ID {
		t.Errorf("cursor = %+v, %v, %v, want %+v", got, ok, err, want)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary file left behind: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("cursor mode = %v, want 0600", fi.Mode())
	}
}

func TestBatteryCursorCorruptIsAnError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCursor(path); err == nil {
		t.Error("a corrupt cursor read as fine, want an error")
	}
}

func TestBatteryLockRefusesASecondHolderUntilReleased(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "run.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := Lock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Lock error = %v, want ErrBusy", err)
	}
	release()
	release2, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	release2()
}
