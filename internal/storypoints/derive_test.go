package storypoints

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

var deriveStart = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// hourly returns the starts of n consecutive whole hours from deriveStart.
func hourly(n int) []time.Time {
	starts := make([]time.Time, n)
	for i := range starts {
		starts[i] = deriveStart.Add(time.Duration(i) * time.Hour)
	}
	return starts
}

// constant returns n hours of the same points and weighted count.
func constant(n int, points float64, weighted int) ([]float64, []int) {
	p := make([]float64, n)
	w := make([]int, n)
	for i := range p {
		p[i] = points
		w[i] = weighted
	}
	return p, w
}

// closed is the moment the n hours from deriveStart have all ended.
func closed(n int) time.Time {
	return deriveStart.Add(time.Duration(n) * time.Hour)
}

func near(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestDeriveConstantSeriesHasNoDifferenceAndNoSignal(t *testing.T) {
	t.Parallel()
	const n = 30
	points, weighted := constant(n, 10, 2)
	got := Derive(points, weighted, closed(n), hourly(n))
	if got.Err != nil {
		t.Fatalf("Derive: %v", got.Err)
	}
	// D2 first becomes defined at hour 11: D1 needs six complete hours and
	// then a second window three hours back.
	if got.Hours[10].D2 != nil || got.Hours[11].D2 == nil {
		t.Fatalf("D2 defined from hour 11 only, got hour 10 %v and hour 11 %v",
			got.Hours[10].D2, got.Hours[11].D2)
	}
	for h, hr := range got.Hours {
		if hr.D1 != nil && *hr.D1 != 0 {
			t.Errorf("hour %d: D1 = %v, want 0", h, *hr.D1)
		}
		if hr.D2 != nil && *hr.D2 != 0 {
			t.Errorf("hour %d: D2 = %v, want 0", h, *hr.D2)
		}
		if hr.D1Signal || hr.D2Signal {
			t.Errorf("hour %d: signal on a constant series (D1 %v, D2 %v)", h, hr.D1Signal, hr.D2Signal)
		}
	}
}

func TestDeriveStepChangeGivesD1SignalAtTheStep(t *testing.T) {
	t.Parallel()
	const n, step = 30, 20
	points, weighted := constant(n, 10, 2)
	for h := step; h < n; h++ {
		points[h] = 30
	}
	got := Derive(points, weighted, closed(n), hourly(n))
	if got.Err != nil {
		t.Fatalf("Derive: %v", got.Err)
	}
	// The smoothed series is flat until the step, so D1 first moves at the
	// step: (13.33 - 10) / 3.
	if got.Hours[step].D1 == nil || !near(*got.Hours[step].D1, 10.0/9) {
		t.Fatalf("hour %d: D1 = %v, want 10/9", step, got.Hours[step].D1)
	}
	if !got.Hours[step].D1Signal {
		t.Errorf("hour %d: no D1 signal at the step", step)
	}
	for h := 0; h < step; h++ {
		if got.Hours[h].D1Signal {
			t.Errorf("hour %d: D1 signal before the step", h)
		}
	}
}

func TestDeriveNeedsTwelveDefinedDifferencesToSignal(t *testing.T) {
	t.Parallel()
	// D1 is first defined at hour 8, so n hours give n-8 defined values. The
	// step is at the last complete hour, so one difference is nonzero and the
	// median absolute deviation is zero: only the count can refuse the signal.
	cases := []struct {
		name   string
		n      int
		signal bool
	}{
		{name: "eleven defined differences", n: 19, signal: false},
		{name: "twelve defined differences", n: 20, signal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			step := tc.n - 1
			points, weighted := constant(tc.n, 10, 2)
			points[step] = 30
			got := Derive(points, weighted, closed(tc.n), hourly(tc.n))
			if got.Err != nil {
				t.Fatalf("Derive: %v", got.Err)
			}
			if hr := got.Hours[step]; hr.D1 == nil || hr.D1Signal != tc.signal {
				t.Fatalf("hour %d: D1 %v, D1Signal %v, want signal %v", step, hr.D1, hr.D1Signal, tc.signal)
			}
		})
	}
}

func TestDeriveNoiseSignalsAtMostTenPercentOfDefinedHours(t *testing.T) {
	t.Parallel()
	const n = 400
	rng := rand.New(rand.NewSource(20261111))
	points := make([]float64, n)
	weighted := make([]int, n)
	for i := range points {
		points[i] = 10 + 3*rng.NormFloat64()
		weighted[i] = 2
	}
	got := Derive(points, weighted, closed(n), hourly(n))
	if got.Err != nil {
		t.Fatalf("Derive: %v", got.Err)
	}
	var d1Defined, d1Signal, d2Defined, d2Signal int
	for _, hr := range got.Hours {
		if hr.D1 != nil {
			d1Defined++
		}
		if hr.D1Signal {
			d1Signal++
		}
		if hr.D2 != nil {
			d2Defined++
		}
		if hr.D2Signal {
			d2Signal++
		}
	}
	if d1Defined == 0 || d2Defined == 0 {
		t.Fatalf("no defined differences: D1 %d, D2 %d", d1Defined, d2Defined)
	}
	if float64(d1Signal) > 0.10*float64(d1Defined) {
		t.Errorf("D1 signal on %d of %d defined hours, want at most 10%%", d1Signal, d1Defined)
	}
	if float64(d2Signal) > 0.10*float64(d2Defined) {
		t.Errorf("D2 signal on %d of %d defined hours, want at most 10%%", d2Signal, d2Defined)
	}
}

func TestDeriveRefusesWindowsWithFewerThanTwelveWeightedLandings(t *testing.T) {
	t.Parallel()
	const n, h = 20, 10
	// Hours 5 to 10 are the window ending at hour 10. The one-landing hour
	// makes it eleven; every other window stays at twelve or more.
	short := make([]int, n)
	full := make([]int, n)
	for i := range short {
		short[i], full[i] = 2, 2
	}
	short[h] = 1
	cases := []struct {
		name     string
		weighted []int
		defined  bool
	}{
		{name: "eleven weighted landings", weighted: short, defined: false},
		{name: "twelve weighted landings", weighted: full, defined: true},
	}
	points, _ := constant(n, 10, 2)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Derive(points, tc.weighted, closed(n), hourly(n))
			if got.Err != nil {
				t.Fatalf("Derive: %v", got.Err)
			}
			hr := got.Hours[h]
			if (hr.Smoothed != nil) != tc.defined {
				t.Fatalf("hour %d: Smoothed defined = %v, want %v", h, hr.Smoothed != nil, tc.defined)
			}
			if tc.defined {
				return
			}
			if hr.D1 != nil || hr.D2 != nil {
				t.Errorf("hour %d: D1 %v and D2 %v, want both nil", h, hr.D1, hr.D2)
			}
		})
	}
}

func TestDeriveIgnoresThePartialCurrentHour(t *testing.T) {
	t.Parallel()
	const n = 20
	// The hour that starts at hour 20 has not ended at now, so it is partial.
	now := closed(n).Add(30 * time.Minute)
	points, weighted := constant(n+1, 10, 2)
	got := Derive(points, weighted, now, hourly(n+1))
	if got.Err != nil {
		t.Fatalf("Derive: %v", got.Err)
	}
	// Its window holds twelve weighted landings, so it would be defined if the
	// partial hour were counted.
	if hr := got.Hours[n]; hr.Smoothed != nil || hr.D1 != nil || hr.D2 != nil || hr.D1Signal || hr.D2Signal {
		t.Fatalf("partial hour %d has values: %+v", n, hr)
	}
	points[n], weighted[n] = 1000, 50
	changed := Derive(points, weighted, now, hourly(n+1))
	if changed.Err != nil {
		t.Fatalf("Derive with changed partial hour: %v", changed.Err)
	}
	if !reflect.DeepEqual(got.Hours[:n], changed.Hours[:n]) {
		t.Errorf("changing the partial hour changed the complete hours")
	}
}

func TestDeriveRecoversTheSlopeOfARamp(t *testing.T) {
	t.Parallel()
	const n = 30
	points, weighted := constant(n, 0, 2)
	for h := range points {
		points[h] = float64(h)
	}
	got := Derive(points, weighted, closed(n), hourly(n))
	if got.Err != nil {
		t.Fatalf("Derive: %v", got.Err)
	}
	// A ramp of one point an hour has a first difference of one and a second
	// difference of zero.
	for h := 11; h < n; h++ {
		if hr := got.Hours[h]; !near(*hr.D1, 1) || !near(*hr.D2, 0) {
			t.Fatalf("hour %d: D1 = %v, D2 = %v, want 1 and 0", h, *hr.D1, *hr.D2)
		}
	}
}

func TestDeriveRefusesMalformedInputs(t *testing.T) {
	t.Parallel()
	const n = 12
	points, weighted := constant(n, 10, 2)
	skipped := hourly(n)
	skipped[6] = skipped[6].Add(time.Hour)
	nonFinite, _ := constant(n, 10, 2)
	nonFinite[3] = math.NaN()
	negative, _ := constant(n, 10, 2)
	negativeWeighted := make([]int, n)
	for i := range negativeWeighted {
		negativeWeighted[i] = 2
	}
	negativeWeighted[4] = -1
	cases := []struct {
		name     string
		points   []float64
		weighted []int
		starts   []time.Time
	}{
		{name: "fewer points than hours", points: points[:n-1], weighted: weighted, starts: hourly(n)},
		{name: "fewer weights than hours", points: points, weighted: weighted[:n-1], starts: hourly(n)},
		{name: "a skipped hour", points: points, weighted: weighted, starts: skipped},
		{name: "a point total that is NaN", points: nonFinite, weighted: weighted, starts: hourly(n)},
		{name: "a negative weighted count", points: negative, weighted: negativeWeighted, starts: hourly(n)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Derive(tc.points, tc.weighted, closed(n), tc.starts)
			if got.Err == nil {
				t.Fatalf("Derive returned no error for %s", tc.name)
			}
			if got.Hours != nil {
				t.Errorf("Derive returned %d hours with an error", len(got.Hours))
			}
		})
	}
}
