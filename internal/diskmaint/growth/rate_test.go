package growth

import (
	"math"
	"testing"
	"time"
)

// series returns n hourly readings from base with used rising gibPerDay GiB a
// day, each on a 500 GiB disk that starts 40 GiB full.
func series(n int, gibPerDay float64) []Sample {
	out := make([]Sample, n)
	for i := range out {
		used := int64(40<<30) + int64(gibPerDay*float64(i)/24*float64(gibBytes))
		out[i] = Sample{At: base.Add(time.Duration(i) * time.Hour), UsedBytes: used, TotalBytes: 500 << 30}
	}
	return out
}

// samplesAt returns the readings at the given hours from base, rising 1 GiB an
// hour.
func samplesAt(hours ...int) []Sample {
	out := make([]Sample, len(hours))
	for i, h := range hours {
		out[i] = at(h)
	}
	return out
}

func TestRateSteadyGrowth(t *testing.T) {
	t.Parallel()
	samples := series(25, 10) // one reading an hour for a day
	got, ok := Rate(samples, base.Add(24*time.Hour), 24*time.Hour)
	if !ok {
		t.Fatal("Rate reported not ok on 25 hourly readings filling the window")
	}
	if math.Abs(got-10) > 0.01 {
		t.Fatalf("Rate = %v GiB/day, want about 10", got)
	}
}

// A prune between the last two readings would read as a fall of about 14 GiB a
// day to anything that compared neighbours; the fit reports the week's trend.
func TestRatePruneDropStillReportsGrowth(t *testing.T) {
	t.Parallel()
	samples := series(169, 10) // one reading an hour for a week
	samples[len(samples)-1].UsedBytes -= 15 << 30
	got, ok := Rate(samples, base.Add(168*time.Hour), 7*24*time.Hour)
	if !ok {
		t.Fatal("Rate reported not ok on 169 hourly readings filling the window")
	}
	if got <= 0 {
		t.Fatalf("Rate = %v GiB/day after one prune drop; want the positive trend, not the step", got)
	}
	if got >= 10 {
		t.Fatalf("Rate = %v GiB/day; the drop must bend the trend below the pre-drop 10", got)
	}
}

func TestRateRefuses(t *testing.T) {
	t.Parallel()
	now := base.Add(24 * time.Hour)
	cases := []struct {
		name    string
		samples []Sample
		now     time.Time
		window  time.Duration
		wantOK  bool
	}{
		{"no samples", nil, now, 24 * time.Hour, false},
		{"two samples", series(2, 10), base.Add(time.Hour), 24 * time.Hour, false},
		{"window zero", series(25, 10), now, 0, false},
		{"every timestamp equal", samplesAt(3, 3, 3), now, 24 * time.Hour, false},
		{"span six hours of a 24h window", samplesAt(0, 3, 6), now, 24 * time.Hour, false},
		{"span just under half the window", samplesAt(12, 15, 23), now, 24 * time.Hour, false},
		{"span exactly half the window", samplesAt(12, 18, 24), now, 24 * time.Hour, true},
		{"every sample older than the window", samplesAt(0, 1, 2), base.Add(48 * time.Hour), 24 * time.Hour, false},
		{"one sample in the future leaves two", samplesAt(0, 3, 26), now, 24 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Rate(tc.samples, tc.now, tc.window)
			if ok != tc.wantOK {
				t.Fatalf("Rate = %v, ok = %v; want ok = %v", got, ok, tc.wantOK)
			}
		})
	}
}

func TestDaysLeft(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		total    int64
		used     int64
		gib      float64
		wantDays float64
		wantOK   bool
	}{
		{"a quarter full at ten GiB a day", 100 << 30, 25 << 30, 10, 7.5, true},
		{"already at capacity", 100 << 30, 100 << 30, 10, 0, true},
		{"over capacity", 100 << 30, 101 << 30, 10, 0, true},
		{"flat disk", 100 << 30, 25 << 30, 0, 0, false},
		{"falling disk", 100 << 30, 25 << 30, -2, 0, false},
		{"NaN rate", 100 << 30, 25 << 30, math.NaN(), 0, false},
		{"unknown capacity", 0, 25 << 30, 10, 0, false},
		{"negative used", 100 << 30, -1, 10, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DaysLeft(tc.total, tc.used, tc.gib)
			if ok != tc.wantOK {
				t.Fatalf("DaysLeft = %v, ok = %v; want ok = %v", got, ok, tc.wantOK)
			}
			if got != tc.wantDays {
				t.Fatalf("DaysLeft = %v days, want %v", got, tc.wantDays)
			}
		})
	}
}
