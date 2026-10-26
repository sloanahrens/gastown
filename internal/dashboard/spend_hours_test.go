package dashboard

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spendLedgerFixture is a balance ledger over three hours: a drop in the 13:00
// hour, a top-up in the 14:00 hour that must not count as spend, a 15:00 hour
// with no sample, and a drop in the 16:00 hour that crosses the empty 15:00
// hour. The malformed line is skipped, as the spend tool skips it.
const spendLedgerFixture = `2026-10-25T13:10:00 balance=80.00 granted=0.00 topped=80.00
2026-10-25T13:50:00 balance=79.00 granted=0.00 topped=79.00
not a ledger row
2026-10-25T14:20:00 balance=79.50 granted=0.00 topped=79.50
2026-10-25T14:50:00 balance=78.50 granted=0.00 topped=78.50
2026-10-25T16:05:00 balance=77.50 granted=0.00 topped=77.50
`

func TestReadSpendHoursCountsTheFallOfTheBalancePerHour(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "balance.log")
	if err := os.WriteFile(path, []byte(spendLedgerFixture), 0o644); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}
	now := time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)
	hours, _, err := ReadSpendHours(path, now)
	if err != nil {
		t.Fatalf("ReadSpendHours: %v", err)
	}
	if len(hours) != spendHours {
		t.Fatalf("got %d hours, want %d", len(hours), spendHours)
	}
	byStart := map[time.Time]SpendHour{}
	for _, h := range hours {
		byStart[h.Start] = h
	}
	at := func(hour int) SpendHour {
		h, ok := byStart[time.Date(2026, 10, 25, hour, 0, 0, 0, time.Local)]
		if !ok {
			t.Fatalf("no bucket for %02d:00", hour)
		}
		return h
	}
	tests := []struct {
		hour    int
		spent   float64
		samples int
	}{
		{13, 1.00, 2}, // 80.00 -> 79.00 over the hour
		{14, 1.00, 2}, // the 79.50 top-up is not spend; 79.50 -> 78.50 is
		{15, 0, 0},    // no sample: a gap, not a zero spend
		{16, 1.00, 1}, // 78.50 -> 77.50, across the empty 15:00 hour
	}
	for _, tc := range tests {
		h := at(tc.hour)
		if math.Abs(h.Spent-tc.spent) > 1e-9 || h.Samples != tc.samples {
			t.Errorf("%02d:00 = spent %.2f over %d samples, want spent %.2f over %d", tc.hour, h.Spent, h.Samples, tc.spent, tc.samples)
		}
	}
}

func TestReadSpendHoursReadsAMissingLedgerAsAllGaps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)
	hours, _, err := ReadSpendHours(filepath.Join(t.TempDir(), "no-such-balance.log"), now)
	if err != nil {
		t.Fatalf("ReadSpendHours with no ledger: %v", err)
	}
	if len(hours) != spendHours {
		t.Fatalf("got %d hours, want %d", len(hours), spendHours)
	}
	for _, h := range hours {
		if h.Samples != 0 || h.Spent != 0 {
			t.Fatalf("a missing ledger gave %02d:00 %+v, want an empty gap", h.Start.Hour(), h)
		}
	}
}

// One line longer than any token limit must not fail the read of the rest of
// the ledger: the rows around it still count.
func TestReadSpendHoursSurvivesALineLongerThanAScannerToken(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200*1024)
	ledger := "2026-10-25T16:05:00 balance=80.00\n" + long + "\n2026-10-25T16:20:00 balance=79.25\n"
	path := filepath.Join(t.TempDir(), "balance.log")
	if err := os.WriteFile(path, []byte(ledger), 0o644); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}
	now := time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)
	hours, _, err := ReadSpendHours(path, now)
	if err != nil {
		t.Fatalf("ReadSpendHours with a long line: %v", err)
	}
	last := hours[len(hours)-1]
	if math.Abs(last.Spent-0.75) > 1e-9 || last.Samples != 2 {
		t.Errorf("16:00 hour = %+v, want 0.75 spent over the two rows around the long line", last)
	}
}

// A malformed ledger line is skipped and reported, not read as zero: the reader
// returns how many lines it skipped, and a blank line is not one (gt-idfec).
func TestReadSpendHoursReportsTheMalformedLinesItSkips(t *testing.T) {
	t.Parallel()
	ledger := "2026-10-25T16:05:00 balance=80.00\n" +
		"not a ledger row\n" +
		"\n" +
		"2026-10-25T16:20:00 balance=79.25\n" +
		"2026-10-25T16:21:00 nope\n"
	path := filepath.Join(t.TempDir(), "balance.log")
	if err := os.WriteFile(path, []byte(ledger), 0o644); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}
	now := time.Date(2026, 10, 25, 16, 30, 0, 0, time.Local)
	hours, skipped, err := ReadSpendHours(path, now)
	if err != nil {
		t.Fatalf("ReadSpendHours: %v", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want the two malformed lines (a blank line is not one)", skipped)
	}
	if last := hours[len(hours)-1]; last.Samples != 2 || last.Spent < 0.749 || last.Spent > 0.751 {
		t.Errorf("16:00 hour = %+v, want the two good rows and 0.75 spent", last)
	}
}
