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
