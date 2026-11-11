package storypoints

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// The windows and thresholds of the velocity series (gt-vd91ny.3). A
// difference of a difference multiplies noise, so the second difference is
// taken only of the smoothed series, and a small wiggle is reported as noise.
const (
	// smoothHours is the width of the window the mean is taken over.
	smoothHours = 6
	// smoothMinWeight is the weighted landings a smoothing window must hold.
	smoothMinWeight = 12
	// diffHours is the distance between the two ends of a difference.
	diffHours = 3
	// minDefined is the defined values a difference needs before it can signal.
	minDefined = 12
	// madScale turns a median absolute deviation into a standard deviation
	// for normal data.
	madScale = 1.4826
	// signalDeviations is how many robust standard deviations a difference
	// must reach to be a signal.
	signalDeviations = 2
)

// Hour is the velocity at one hour. A nil value is not defined: the hour is
// partial, its window holds too few weighted landings, or a value it needs is
// undefined. Signal is false for an undefined value.
type Hour struct {
	Smoothed *float64
	D1       *float64
	D2       *float64
	D1Signal bool
	D2Signal bool
}

// Derived is the velocity for each hour given to Derive, in the same order.
// Err is set when the inputs are not consecutive hours of finite points and
// non-negative weights; Hours is then nil, so a caller cannot read the series
// as quiet.
type Derived struct {
	Hours []Hour
	Err   error
}

// Derive smooths points landed per hour and takes its first and second
// differences. points[i] and weighted[i] are the points and the weighted
// landings of the hour that starts at hourStarts[i]; the hours must be
// consecutive. The hour that contains now is partial and is left out of every
// calculation, and so is any hour that has not ended.
//
// A difference is a signal when its size is at least twice 1.4826 times the
// median absolute deviation of the defined values of that difference over the
// hours given, when at least 12 values are defined, and when it is not zero.
func Derive(points []float64, weighted []int, now time.Time, hourStarts []time.Time) Derived {
	if err := checkDeriveInputs(points, weighted, hourStarts); err != nil {
		return Derived{Err: err}
	}
	n := len(hourStarts)
	complete := make([]bool, n)
	for i, start := range hourStarts {
		complete[i] = !start.Add(time.Hour).After(now)
	}

	smoothed := make([]*float64, n)
	for h := smoothHours - 1; h < n; h++ {
		lo := h - smoothHours + 1
		if !allTrue(complete[lo:h+1]) || sumInts(weighted[lo:h+1]) < smoothMinWeight {
			continue
		}
		mean := sumFloats(points[lo:h+1]) / smoothHours
		smoothed[h] = &mean
	}
	d1 := difference(smoothed)
	d2 := difference(d1)
	d1Signal := signals(d1)
	d2Signal := signals(d2)

	hours := make([]Hour, n)
	for h := range hours {
		hours[h] = Hour{
			Smoothed: smoothed[h],
			D1:       d1[h],
			D2:       d2[h],
			D1Signal: d1Signal[h],
			D2Signal: d2Signal[h],
		}
	}
	return Derived{Hours: hours}
}

// checkDeriveInputs refuses inputs that do not describe consecutive hours of
// finite points and non-negative weights. Each refusal is an error, so a
// broken feed cannot pass as a quiet one.
func checkDeriveInputs(points []float64, weighted []int, hourStarts []time.Time) error {
	if len(points) != len(hourStarts) || len(weighted) != len(hourStarts) {
		return fmt.Errorf("storypoints: derive needs one point total and one weight per hour, got %d, %d and %d",
			len(points), len(weighted), len(hourStarts))
	}
	for i := range hourStarts {
		if math.IsNaN(points[i]) || math.IsInf(points[i], 0) {
			return fmt.Errorf("storypoints: derive hour %d has a point total that is not finite", i)
		}
		if weighted[i] < 0 {
			return fmt.Errorf("storypoints: derive hour %d has a negative weighted count, %d", i, weighted[i])
		}
		if i > 0 && !hourStarts[i].Equal(hourStarts[i-1].Add(time.Hour)) {
			return errors.New("storypoints: derive hours must be consecutive whole hours")
		}
	}
	return nil
}

// difference returns (series[h] - series[h-diffHours]) / diffHours for each
// hour h, and nil where either end is undefined or h has no hour diffHours back.
func difference(series []*float64) []*float64 {
	out := make([]*float64, len(series))
	for h := diffHours; h < len(series); h++ {
		later, earlier := series[h], series[h-diffHours]
		if later == nil || earlier == nil {
			continue
		}
		d := (*later - *earlier) / diffHours
		out[h] = &d
	}
	return out
}

// signals marks each defined value of d that is a signal. The median absolute
// deviation is taken over every defined value of d, so the hours a signal is
// judged against are the hours Derive was given. A zero is never a signal, so
// a flat series gives none even when its deviation is zero.
func signals(d []*float64) []bool {
	out := make([]bool, len(d))
	var defined []float64
	for _, v := range d {
		if v != nil {
			defined = append(defined, *v)
		}
	}
	if len(defined) < minDefined {
		return out
	}
	mid := median(defined)
	devs := make([]float64, len(defined))
	for i, v := range defined {
		devs[i] = math.Abs(v - mid)
	}
	limit := signalDeviations * madScale * median(devs)
	for h, v := range d {
		if v == nil || *v == 0 {
			continue
		}
		out[h] = math.Abs(*v) >= limit
	}
	return out
}

// median is the middle of xs, or the mean of the two middle values. xs must
// not be empty.
func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func allTrue(bs []bool) bool {
	for _, b := range bs {
		if !b {
			return false
		}
	}
	return true
}

func sumInts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}

func sumFloats(xs []float64) float64 {
	total := 0.0
	for _, x := range xs {
		total += x
	}
	return total
}
