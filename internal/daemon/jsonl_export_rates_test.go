package daemon

import (
	"fmt"
	"math"
	"slices"
	"sync"
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

// TestJSONLExportRatesLaterSample: a second Record of the same key replaces
// the first, and PerRow and Ratio answer from the later sample.
func TestJSONLExportRatesLaterSample(t *testing.T) {
	t.Parallel()
	var r jsonlExportRates
	r.Record("gastown", "issues", 1000, 2*time.Second)
	r.Record("gastown", "issues", 500, 5*time.Second)

	rows, perRow, ok := r.PerRow("gastown", "issues")
	if !ok || rows != 500 || perRow != 10*time.Millisecond {
		t.Fatalf("PerRow = (%d, %v, %v), want (500, 10ms, true)", rows, perRow, ok)
	}
	ratio, ok := r.Ratio("gastown", "issues", 10*time.Second)
	if !ok || ratio != 2 {
		t.Fatalf("Ratio = (%v, %v), want (2, true)", ratio, ok)
	}
}

// TestJSONLExportRatesConcurrent: eight goroutines record and read a shared
// key and a key of their own at once. Each goroutine reads back its own
// sample, and the shared key ends with one of the samples recorded into it.
func TestJSONLExportRatesConcurrent(t *testing.T) {
	t.Parallel()
	const workers = 8
	var r jsonlExportRates
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own := fmt.Sprintf("table%d", i)
			ownPerRow := time.Duration(i+1) * time.Microsecond
			ownTook := time.Duration(i+1) * time.Millisecond
			r.Record("gastown", "issues", 1000*(i+1), ownTook)
			r.Record("gastown", own, 1000, ownTook)

			rows, perRow, ok := r.PerRow("gastown", own)
			if !ok || rows != 1000 || perRow != ownPerRow {
				t.Errorf("%s PerRow = (%d, %v, %v), want (1000, %v, true)", own, rows, perRow, ok, ownPerRow)
			}
			if _, ok := r.Ratio("gastown", own, 2*ownTook); !ok {
				t.Errorf("%s Ratio reported no sample after its own Record", own)
			}
			if _, _, ok := r.PerRow("gastown", "issues"); !ok {
				t.Errorf("shared key reported no sample after this goroutine recorded it")
			}
		}()
	}
	wg.Wait()

	rows, perRow, ok := r.PerRow("gastown", "issues")
	if !ok || rows < 1000 || rows > 1000*workers || rows%1000 != 0 || perRow != time.Microsecond {
		t.Fatalf("shared PerRow = (%d, %v, %v), want one of the recorded samples at 1µs per row", rows, perRow, ok)
	}
}

// TestJSONLSlowdown: the slowdown is the median of the usable ratios, never
// below 1. NaN, infinite and non-positive ratios are ignored.
func TestJSONLSlowdown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		ratios []float64
		want   float64
	}{
		{"three values give the middle one", []float64{88, 140, 143}, 140},
		{"four values give the mean of the middle two", []float64{88, 140, 143, 200}, 141.5},
		{"no ratios give 1", nil, 1},
		{"all under 1 give 1", []float64{0.5, 0.8}, 1},
		{"NaN is ignored", []float64{88, 140, 143, math.NaN()}, 140},
		{"a single Inf gives 1", []float64{math.Inf(1)}, 1},
		{"non-positive values are ignored", []float64{0, -3, 88, 140, 143}, 140},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := jsonlSlowdown(tc.ratios); got != tc.want {
				t.Errorf("jsonlSlowdown(%v) = %v, want %v", tc.ratios, got, tc.want)
			}
		})
	}
}

// TestJSONLSlowdownLeavesInputUnsorted: the caller's slice keeps its order
// after the median is taken.
func TestJSONLSlowdownLeavesInputUnsorted(t *testing.T) {
	t.Parallel()
	ratios := []float64{143, 88, 140}
	want := []float64{143, 88, 140}
	jsonlSlowdown(ratios)
	if !slices.Equal(ratios, want) {
		t.Errorf("caller's slice became %v, want %v", ratios, want)
	}
}
