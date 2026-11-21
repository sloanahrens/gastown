package land

import (
	"reflect"
	"testing"
	"time"
)

// TestGateRunRotationDecisions pins the three decisions a gate-run log rotation takes: when the
// log is due, which month file a row belongs to, and how rows group under that file name.
func TestGateRunRotationDecisions(t *testing.T) {
	t.Parallel()
	if gateRunRotateLimit != 50<<20 || gateRunRetentionDays != 90 {
		t.Fatalf("limit = %d bytes, retention = %d days; want 50 MiB and 90 days",
			gateRunRotateLimit, gateRunRetentionDays)
	}

	due := []struct {
		name string
		size int64
		want bool
	}{
		{"one byte under the limit", gateRunRotateLimit - 1, false},
		{"at the limit", gateRunRotateLimit, true},
		{"over the limit", gateRunRotateLimit + 1, true},
	}
	for _, tc := range due {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gateRunRotationDue(tc.size, gateRunRotateLimit); got != tc.want {
				t.Errorf("gateRunRotationDue(%d, %d) = %v, want %v", tc.size, gateRunRotateLimit, got, tc.want)
			}
		})
	}

	months := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"mid-month", time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), "gate-runs-2026-11.jsonl.gz"},
		{"last instant of a month", time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), "gate-runs-2026-11.jsonl.gz"},
		{"first instant of the next month", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), "gate-runs-2026-12.jsonl.gz"},
		{"year boundary", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "gate-runs-2027-01.jsonl.gz"},
		{"local December is January in UTC", time.Date(2026, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600)), "gate-runs-2027-01.jsonl.gz"},
	}
	for _, tc := range months {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gateRunMonthFile(tc.ts); got != tc.want {
				t.Errorf("gateRunMonthFile(%s) = %q, want %q", tc.ts, got, tc.want)
			}
		})
	}

	rows := []GateRun{
		{Bead: "a", TS: time.Date(2026, 12, 2, 9, 0, 0, 0, time.UTC)},
		{Bead: "b", TS: time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)},
		{Bead: "c", TS: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{Bead: "d", TS: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Bead: "e", TS: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
	}
	// Input order is not chronological: rows a and c stay in input order inside their December group.
	want := []gateRunMonthGroup{
		{File: "gate-runs-2026-11.jsonl.gz", Rows: []GateRun{rows[1], rows[4]}},
		{File: "gate-runs-2026-12.jsonl.gz", Rows: []GateRun{rows[0], rows[2]}},
		{File: "gate-runs-2027-01.jsonl.gz", Rows: []GateRun{rows[3]}},
	}
	if got := groupGateRunsByMonth(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("groupGateRunsByMonth() = %+v, want %+v", got, want)
	}
	if got := groupGateRunsByMonth(nil); len(got) != 0 {
		t.Errorf("groupGateRunsByMonth(nil) = %+v, want no groups", got)
	}
}
