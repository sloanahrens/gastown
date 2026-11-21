package daemon

import (
	"testing"
	"time"
)

// TestJSONLExportRatesRecordThenRead: a recorded sample answers PerRow with
// its rows and time per row, and Ratio with the took time over its duration.
func TestJSONLExportRatesRecordThenRead(t *testing.T) {
	t.Parallel()
	var r jsonlExportRates
	r.Record("gastown", "issues", 1000, 2*time.Second)

	rows, perRow, ok := r.PerRow("gastown", "issues")
	if !ok || rows != 1000 || perRow != 2*time.Millisecond {
		t.Fatalf("PerRow = (%d, %v, %v), want (1000, 2ms, true)", rows, perRow, ok)
	}
	ratio, ok := r.Ratio("gastown", "issues", 4*time.Second)
	if !ok || ratio != 2 {
		t.Fatalf("Ratio = (%v, %v), want (2, true)", ratio, ok)
	}
}

// TestJSONLExportRatesDatabasesAreSeparate: the same table name in two
// databases keeps two samples.
func TestJSONLExportRatesDatabasesAreSeparate(t *testing.T) {
	t.Parallel()
	var r jsonlExportRates
	r.Record("gastown", "issues", 1000, 2*time.Second)
	r.Record("tracker", "issues", 10, time.Second)

	rows, perRow, ok := r.PerRow("gastown", "issues")
	if !ok || rows != 1000 || perRow != 2*time.Millisecond {
		t.Fatalf("gastown PerRow = (%d, %v, %v), want (1000, 2ms, true)", rows, perRow, ok)
	}
	rows, perRow, ok = r.PerRow("tracker", "issues")
	if !ok || rows != 10 || perRow != 100*time.Millisecond {
		t.Fatalf("tracker PerRow = (%d, %v, %v), want (10, 100ms, true)", rows, perRow, ok)
	}
}

// TestJSONLExportRatesUnknownKey: a key with no sample reports none, from
// both PerRow and Ratio.
func TestJSONLExportRatesUnknownKey(t *testing.T) {
	t.Parallel()
	var r jsonlExportRates
	r.Record("gastown", "issues", 1000, 2*time.Second)

	if _, _, ok := r.PerRow("gastown", "events"); ok {
		t.Errorf("PerRow of an unrecorded table reported a sample")
	}
	if _, ok := r.Ratio("tracker", "issues", time.Second); ok {
		t.Errorf("Ratio of an unrecorded database reported a sample")
	}
}

// TestJSONLExportRatesRefuses: a sample with zero or negative rows, or a
// zero or negative duration, is not stored.
func TestJSONLExportRatesRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows int
		took time.Duration
	}{
		{"zero rows", 0, time.Second},
		{"negative rows", -5, time.Second},
		{"zero duration", 1000, 0},
		{"negative duration", 1000, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var r jsonlExportRates
			r.Record("gastown", "issues", tc.rows, tc.took)

			if _, _, ok := r.PerRow("gastown", "issues"); ok {
				t.Errorf("PerRow reported a sample after Record(%d rows, %v)", tc.rows, tc.took)
			}
			if _, ok := r.Ratio("gastown", "issues", time.Second); ok {
				t.Errorf("Ratio reported a sample after Record(%d rows, %v)", tc.rows, tc.took)
			}
		})
	}
}
