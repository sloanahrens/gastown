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
