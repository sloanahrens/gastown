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
	// minDefined is the defined values a difference needs before it can be
	// judged signal or noise.
	minDefined = 12
	// madScale turns a median absolute deviation into a standard deviation
	// for normal data.
	madScale = 1.4826
	// signalDeviations is how many robust standard deviations a difference
	// must exceed to be a signal.
	signalDeviations = 2
)

// The smallest change each series can show: one point, carried through the
// six-hour smoothing and then divided by the step of each difference. A
// series that is zero in most hours has a median absolute deviation of zero,
// so these floors are what stop every nonzero difference from being a signal.
// They are variables, not constants, so each division rounds the way the
// differences computed in Derive do, and a one-point change equals its floor
// exactly.
var (
	smoothFloor = 1.0 / smoothHours
	d1Floor     = smoothFloor / diffHours
	d2Floor     = d1Floor / diffHours
)

// State is what a velocity value says about the series at one hour. The zero
// value is StateInsufficient, so an unset state reads as not enough data and
// never as quiet.
type State int

const (
	// StateInsufficient is a nil difference, or one whose series has fewer
	// than minDefined defined values. It is not noise.
	StateInsufficient State = iota
	// StateNoise is a defined difference that does not exceed its limit.
	StateNoise
	// StateSignal is a defined difference that exceeds its limit.
	StateSignal
)

// Hour is the velocity at one hour. A nil value is not defined: the hour is
// partial, its window holds too few weighted landings, a value it needs is
// undefined, or its difference has fewer than 12 defined values. Its state is
// StateInsufficient wherever the value is nil.
type Hour struct {
	Smoothed *float64
	D1       *float64
	D2       *float64
	D1State  State
	D2State  State
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
// A difference is a signal when its absolute value is more than the larger of
// twice 1.4826 times the median absolute deviation of its defined values over
// the hours given, and its floor. It is noise when it does not exceed that
// limit. A difference with fewer than 12 defined values is nil and insufficient,
// because it cannot be judged. The
// comparison is strict: a change exactly at the floor is the smallest change
// the series can show, and it is not a signal.
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
	d1 := sufficient(difference(smoothed))
	d2 := sufficient(difference(d1))
	d1States := states(d1, d1Floor)
	d2States := states(d2, d2Floor)

	hours := make([]Hour, n)
	for h := range hours {
		hours[h] = Hour{
			Smoothed: smoothed[h],
			D1:       d1[h],
			D2:       d2[h],
			D1State:  d1States[h],
			D2State:  d2States[h],
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

// sufficient returns d unchanged when at least minDefined of its values are
// defined. A shorter series cannot be judged, so every value is nil: it is not
// reported as a number that could be read as quiet.
func sufficient(d []*float64) []*float64 {
	defined := 0
	for _, v := range d {
		if v != nil {
			defined++
		}
	}
	if defined >= minDefined {
		return d
	}
	return make([]*float64, len(d))
}

// states classifies each value of d against its limit, the larger of the
// scaled median absolute deviation and floor. The median absolute deviation is
// taken over every defined value of d, so the hours a value is judged against
// are the hours Derive was given. A series with fewer than minDefined defined
// values is StateInsufficient throughout.
func states(d []*float64, floor float64) []State {
	out := make([]State, len(d))
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
	limit := max(signalDeviations*madScale*median(devs), floor)
	for h, v := range d {
		if v == nil {
			continue
		}
		if math.Abs(*v) > limit {
			out[h] = StateSignal
		} else {
			out[h] = StateNoise
		}
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
