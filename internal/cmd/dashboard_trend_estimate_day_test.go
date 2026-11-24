package cmd

import (
	"math"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landings"
)

// TestDashboardTrendEstimateDayStats: a landing day's median actual over
// estimated minutes and its within-25-percent share, with a bound-flagged
// landing counted in N but not measured, and a four-landing day carrying N
// alone (gt-mp1rh0).
func TestDashboardTrendEstimateDayStats(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("test", -5*60*60)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, loc)
	sixDay := time.Date(2026, 11, 22, 15, 0, 0, 0, loc)
	fourDay := time.Date(2026, 11, 23, 8, 0, 0, 0, loc)
	m := func(v float64) *float64 { return &v }
	rec := func(bead string, at time.Time, estimate, secs float64, bound bool) omRecord {
		return omRecord{
			Record:    landings.Record{Bead: bead, LandedAt: at, PolecatSecs: secs, PolecatLowerBound: bound},
			SPMinutes: m(estimate),
		}
	}
	recs := []omRecord{
		rec("gt-a1", sixDay, 10, 600, false),  // 1.00
		rec("gt-a2", sixDay, 10, 720, false),  // 1.20
		rec("gt-a3", sixDay, 10, 480, false),  // 0.80
		rec("gt-a4", sixDay, 10, 1200, false), // 2.00
		rec("gt-a5", sixDay, 10, 300, false),  // 0.50
		rec("gt-a6", sixDay, 10, 600, true),   // bound: counted, not measured
		{Record: landings.Record{Bead: "gt-a7", LandedAt: sixDay, PolecatSecs: 600}},
		rec("gt-a8", sixDay, 10, 0, false), // no polecat time
		rec("gt-b1", fourDay, 10, 600, false),
		rec("gt-b2", fourDay, 10, 600, false),
		rec("gt-b3", fourDay, 10, 600, false),
		rec("gt-b4", fourDay, 10, 600, false),
		rec("gt-old", sixDay.AddDate(0, 0, -3), 10, 600, false), // outside the window
	}
	tr := buildTrendIn(now, recs, nil, nil, nil, loc)
	if len(tr.EstimateDays) != 2 {
		t.Fatalf("estimate days = %+v, want two days", tr.EstimateDays)
	}
	six, four := tr.EstimateDays[0], tr.EstimateDays[1]
	if six.Day != "2026-11-22" || six.N != 6 {
		t.Errorf("six-landing day = %+v, want 2026-11-22 with N 6: the bound counted, no estimate and no time uncounted", six)
	}
	if math.Abs(six.MedianRatio-1) > 1e-9 || math.Abs(six.Within25-0.6) > 1e-9 {
		t.Errorf("six-landing day ratio = %v, within 25 = %v, want 1 and 0.6", six.MedianRatio, six.Within25)
	}
	if four.Day != "2026-11-23" || four.N != 4 || four.MedianRatio != 0 || four.Within25 != 0 {
		t.Errorf("four-landing day = %+v, want 2026-11-23, N 4, no ratio", four)
	}
}

// TestDashboardTrendEstimateDayEmpty: a landing with no usable sp: estimate is
// skipped and counted in its day's skipped total rather than dropped, a day
// with no judged landing still appears with that count, and a window with no
// landings carries an empty list, never a nil field (gt-9sh1bl).
func TestDashboardTrendEstimateDayEmpty(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("test", -5*60*60)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, loc)
	mixedDay := time.Date(2026, 11, 22, 15, 0, 0, 0, loc)
	skippedOnlyDay := time.Date(2026, 11, 23, 9, 0, 0, 0, loc)
	m := func(v float64) *float64 { return &v }
	recs := []omRecord{
		{Record: landings.Record{Bead: "gt-n1", LandedAt: mixedDay, PolecatSecs: 600}},                  // no sp: estimate
		{Record: landings.Record{Bead: "gt-n2", LandedAt: mixedDay, PolecatSecs: 600}, SPMinutes: m(0)}, // not above zero
		{Record: landings.Record{Bead: "gt-n3", LandedAt: mixedDay, PolecatSecs: 600}, SPMinutes: m(10)},
		{Record: landings.Record{Bead: "gt-n5", LandedAt: mixedDay}, SPMinutes: m(10)}, // estimate, no polecat time: counts nowhere
		{Record: landings.Record{Bead: "gt-n4", LandedAt: skippedOnlyDay, PolecatSecs: 600}},
		{Record: landings.Record{Bead: "gt-old", LandedAt: mixedDay.AddDate(0, 0, -3), PolecatSecs: 600}}, // outside the window
	}
	tr := buildTrendIn(now, recs, nil, nil, nil, loc)
	if len(tr.EstimateDays) != 2 {
		t.Fatalf("estimate days = %+v, want two days", tr.EstimateDays)
	}
	if got := tr.EstimateDays[0]; got.Day != "2026-11-22" || got.N != 1 || got.Skipped != 2 {
		t.Errorf("mixed day = %+v, want 2026-11-22, N 1, skipped 2: the estimate-less and zero-estimate landings are counted, the measured one is N", got)
	}
	if got := tr.EstimateDays[1]; got.Day != "2026-11-23" || got.N != 0 || got.Skipped != 1 {
		t.Errorf("skipped-only day = %+v, want 2026-11-23, N 0, skipped 1", got)
	}

	empty := buildTrendIn(now, nil, nil, nil, nil, loc)
	if empty.EstimateDays == nil {
		t.Error("empty window estimate days is nil, want an empty list")
	}
	if len(empty.EstimateDays) != 0 {
		t.Errorf("empty window estimate days = %+v, want none", empty.EstimateDays)
	}
}

// TestDashboardTrendEstimateDayEvenAndBounds: an even count of measured ratios
// has the median of its two middle values, and a bound-flagged landing, either
// bound, counts in N and not in the ratios it would otherwise shift (gt-j3wle4).
func TestDashboardTrendEstimateDayEvenAndBounds(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("test", -5*60*60)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, loc)
	day := time.Date(2026, 11, 22, 15, 0, 0, 0, loc)
	m := func(v float64) *float64 { return &v }
	// ratio is a landing whose polecat time is ratio times its estimate of 10 minutes.
	ratio := func(bead string, ratio float64) omRecord {
		return omRecord{
			Record:    landings.Record{Bead: bead, LandedAt: day, PolecatSecs: ratio * 10 * 60},
			SPMinutes: m(10),
		}
	}
	upper := ratio("gt-u1", 5)
	upper.PolecatUpperBound = true
	lower := ratio("gt-l1", 0.1)
	lower.PolecatLowerBound = true
	tests := []struct {
		name       string
		recs       []omRecord
		wantN      int
		wantMedian float64
		wantWithin float64
	}{
		{
			name:       "even count takes the mean of the two middle ratios",
			recs:       []omRecord{ratio("gt-e1", 0.5), ratio("gt-e2", 1), ratio("gt-e3", 1.2), ratio("gt-e4", 2), ratio("gt-e5", 0.8), ratio("gt-e6", 1.4)},
			wantN:      6,
			wantMedian: 1.1,
			wantWithin: 0.5,
		},
		{
			name:       "bound-flagged landings count in N and stay out of the ratios",
			recs:       []omRecord{ratio("gt-b1", 0.5), ratio("gt-b2", 1), ratio("gt-b3", 1.2), ratio("gt-b4", 0.8), ratio("gt-b5", 1.4), upper, lower},
			wantN:      7,
			wantMedian: 1,
			wantWithin: 0.6,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := buildTrendIn(now, tc.recs, nil, nil, nil, loc)
			if len(tr.EstimateDays) != 1 {
				t.Fatalf("estimate days = %+v, want one day", tr.EstimateDays)
			}
			got := tr.EstimateDays[0]
			if got.Day != "2026-11-22" || got.N != tc.wantN {
				t.Errorf("day = %+v, want 2026-11-22 with N %d", got, tc.wantN)
			}
			if math.Abs(got.MedianRatio-tc.wantMedian) > 1e-9 || math.Abs(got.Within25-tc.wantWithin) > 1e-9 {
				t.Errorf("ratio = %v, within 25 = %v, want %v and %v", got.MedianRatio, got.Within25, tc.wantMedian, tc.wantWithin)
			}
		})
	}
}
