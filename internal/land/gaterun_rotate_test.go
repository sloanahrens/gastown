package land

import (
	"reflect"
	"testing"
	"time"
)

func TestGateRunRotationDecisions(t *testing.T) {
	t.Parallel()
	due := func(size int64) bool { return gateRunRotationDue(size, gateRunRotateLimit) }
	if due(50<<20-1) || !due(50<<20) || !due(50<<20+1) {
		t.Error("gateRunRotationDue: want false one byte under the 50 MiB limit, true at and over it")
	}
	for _, tc := range []struct {
		ts   time.Time
		want string
	}{
		{time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), "gate-runs-2026-11.jsonl.gz"},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), "gate-runs-2026-12.jsonl.gz"},
		{time.Date(2026, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600)), "gate-runs-2027-01.jsonl.gz"},
	} {
		if got := gateRunMonthFile(tc.ts); got != tc.want {
			t.Errorf("gateRunMonthFile(%s) = %q, want %q", tc.ts, got, tc.want)
		}
	}
	rows := []GateRun{
		{Bead: "a", TS: time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)},
		{Bead: "b", TS: time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)},
		{Bead: "c", TS: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
	}
	want := []gateRunMonthGroup{
		{File: "gate-runs-2026-11.jsonl.gz", Rows: rows[1:2]},
		{File: "gate-runs-2026-12.jsonl.gz", Rows: []GateRun{rows[0], rows[2]}},
	}
	if got := groupGateRunsByMonth(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("groupGateRunsByMonth() = %+v, want %+v", got, want)
	}
}

// TestGateRunMonthExpired: a month file is expired once its month ended more than
// keepDays days before now, a month ends at the first instant of the next UTC
// month, exactly keepDays days is not expired, and a name that is not a month file
// is never expired (gt-1khwlf).
func TestGateRunMonthExpired(t *testing.T) {
	t.Parallel()
	utc := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cutoff := utc(2027, time.April, 1) // the end of 2026-12 plus 90 days
	for _, tc := range []struct {
		name string
		file string
		now  time.Time
		want bool
	}{
		{"a month that ended long before", "gate-runs-2026-10.jsonl.gz", utc(2027, time.March, 15), true},
		{"exactly the kept days after the month ended", "gate-runs-2026-12.jsonl.gz", cutoff, false},
		{"one nanosecond past the kept days", "gate-runs-2026-12.jsonl.gz", cutoff.Add(time.Nanosecond), true},
		{"a newer month", "gate-runs-2027-02.jsonl.gz", utc(2027, time.March, 15), false},
		{"the current log", "gate-runs.jsonl", utc(2027, time.March, 15), false},
		{"the rejects file", "gate-runs-rejects.jsonl", utc(2030, time.January, 1), false},
		{"a month that does not exist", "gate-runs-2026-13.jsonl.gz", utc(2030, time.January, 1), false},
		{"a month file without the gz suffix", "gate-runs-2026-10.jsonl", utc(2030, time.January, 1), false},
		{"a month before the retained years is quarantined, not expired", "gate-runs-2019-12.jsonl.gz", utc(2030, time.January, 1), false},
		{"the zero month is quarantined, not expired", "gate-runs-0001-01.jsonl.gz", utc(2030, time.January, 1), false},
		{"a far-future month is quarantined, not expired", "gate-runs-9999-12.jsonl.gz", utc(2030, time.January, 1), false},
		{"the first retained year follows the rule", "gate-runs-2020-01.jsonl.gz", utc(2030, time.January, 1), true},
		{"a now in another zone is read as the instant it is", "gate-runs-2026-12.jsonl.gz", cutoff.In(time.FixedZone("x", 5*3600)), false},
	} {
		if got := gateRunMonthExpired(tc.file, tc.now, 90); got != tc.want {
			t.Errorf("%s: gateRunMonthExpired(%q, %s, 90) = %t, want %t", tc.name, tc.file, tc.now.Format(time.RFC3339Nano), got, tc.want)
		}
	}
}

// TestGateRunMonthQuarantined: a month file whose year is outside [2020, the year
// of now plus one] is quarantined, and nothing else is (gt-jir41w).
func TestGateRunMonthQuarantined(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, time.June, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"gate-runs-2019-12.jsonl.gz", true},
		{"gate-runs-2020-01.jsonl.gz", false},
		{"gate-runs-2031-12.jsonl.gz", false}, // the year after now's
		{"gate-runs-2032-01.jsonl.gz", true},
		{"gate-runs-0001-01.jsonl.gz", true},
		{"gate-runs-9999-12.jsonl.gz", true},
		{"gate-runs.jsonl", false},
		{"gate-runs-rejects.jsonl", false},
	} {
		if got := gateRunMonthQuarantined(tc.file, now); got != tc.want {
			t.Errorf("gateRunMonthQuarantined(%q) = %t, want %t", tc.file, got, tc.want)
		}
	}
}
