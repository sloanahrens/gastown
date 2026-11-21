package daemon

import (
	"math"
	"slices"
	"sync"
	"time"
)

// jsonlExportKey names one (database, table) export.
type jsonlExportKey struct {
	db    string
	table string
}

// jsonlExportSample is the rows and duration of one successful export.
type jsonlExportSample struct {
	rows int
	took time.Duration
}

// jsonlExportRates remembers the last good export of each (database, table).
// The zero value is ready to use. It is safe for concurrent use.
type jsonlExportRates struct {
	mu      sync.Mutex
	samples map[jsonlExportKey]jsonlExportSample
}

// Record stores the sample for db and table, replacing any earlier one. A
// sample with zero or negative rows, or a zero or negative duration, is not
// stored, so PerRow and Ratio never divide by zero.
func (r *jsonlExportRates) Record(db, table string, rows int, took time.Duration) {
	if rows <= 0 || took <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.samples == nil {
		r.samples = make(map[jsonlExportKey]jsonlExportSample)
	}
	r.samples[jsonlExportKey{db, table}] = jsonlExportSample{rows: rows, took: took}
}

// PerRow returns the rows and the time per row of the remembered sample for
// db and table. ok is false when no sample is stored for that key.
func (r *jsonlExportRates) PerRow(db, table string) (rows int, perRow time.Duration, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.samples[jsonlExportKey{db, table}]
	if !ok {
		return 0, 0, false
	}
	return s.rows, s.took / time.Duration(s.rows), true
}

// Ratio returns took divided by the duration of the remembered sample for db
// and table. ok is false when no sample is stored for that key.
func (r *jsonlExportRates) Ratio(db, table string, took time.Duration) (ratio float64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.samples[jsonlExportKey{db, table}]
	if !ok {
		return 0, false
	}
	return float64(took) / float64(s.took), true
}

// jsonlSlowdown returns the median of the ratios of this cycle's exports to
// their remembered samples, and never less than 1. NaN, infinite and
// non-positive ratios are ignored, and 1 is returned when none remain. The
// caller's slice is not reordered: the median is taken from a sorted copy.
func jsonlSlowdown(ratios []float64) float64 {
	kept := make([]float64, 0, len(ratios))
	for _, r := range ratios {
		if math.IsNaN(r) || math.IsInf(r, 0) || r <= 0 {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return 1
	}
	slices.Sort(kept)
	n := len(kept)
	median := kept[n/2]
	if n%2 == 0 {
		median = (kept[n/2-1] + kept[n/2]) / 2
	}
	return max(median, 1)
}
