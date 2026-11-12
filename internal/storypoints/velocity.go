package storypoints

import "time"

// AllClass is the key in Series.Classes that counts every landing and
// rejection, whatever its agent's seat class.
const AllClass = "all"

// seriesClasses are the seat classes a Series always carries beside AllClass.
// They are the classes SeatClass returns; a record whose agent has no class
// counts in AllClass only.
var seriesClasses = []string{"haiku", "flash", "sonnet"}

// Landing is one bead landing: when it landed, which agent landed it, and the
// polecat time the landing record measured. LowerBound and UpperBound mark a
// time that is an edge of the truth, not the truth.
type Landing struct {
	At          time.Time
	Agent       string
	PolecatSecs float64
	LowerBound  bool
	UpperBound  bool
}

// Rejection is one bead rejected after it landed: when the rejection was
// recorded, the polecat time it measured, and which agent's work it was.
type Rejection struct {
	At          time.Time
	PolecatSecs float64
	LowerBound  bool
	UpperBound  bool
	Agent       string
}

// HourPoint is one hour of a Series. Points is the sum of the weighted actual
// points of the landings in the hour and Weighted counts those landings;
// Unweighted counts the landings with no weight, which add no points.
// RejectedPoints is the same sum for rejections, counted apart from Points,
// and RejectedUnweighted counts rejections with no weight.
type HourPoint struct {
	Hour               time.Time
	Points             int
	Weighted           int
	Unweighted         int
	RejectedPoints     int
	RejectedUnweighted int
}

// Series is the hourly points series of a window: one slice of consecutive
// hours for AllClass, and one for each class in seriesClasses. Every slice has
// the window's length and the same hours.
type Series struct {
	Classes map[string][]HourPoint
}

// Hourly folds landings and rejections into an hourly points series. The
// window opens at the local hour that contains firstHour and holds hours
// buckets; each bucket is one elapsed hour after the window start, so the hour
// starts are always one hour apart, as Derive requires. Across a daylight
// saving fall-back this makes the repeated local hour two buckets that read
// the same wall clock; a bucket is never merged with the hour before it.
//
// Points come from Actual, so an absent, lower-bound or upper-bound polecat
// time is unweighted: it is counted in Unweighted or RejectedUnweighted and
// never adds to Points. A record before the window or at its end is ignored.
// A record whose agent has no seat class counts in AllClass only. A window of
// zero or fewer hours has no buckets. A nil loc means time.Local, the zone
// localHour in internal/cmd/dashboard_trend.go reads its hours in.
func Hourly(landings []Landing, rejections []Rejection, firstHour time.Time, hours int, loc *time.Location) Series {
	if loc == nil {
		loc = time.Local
	}
	if hours < 0 {
		hours = 0
	}
	base := HourStart(firstHour, loc)
	classes := make(map[string][]HourPoint, len(seriesClasses)+1)
	for _, key := range append([]string{AllClass}, seriesClasses...) {
		buckets := make([]HourPoint, hours)
		for i := range buckets {
			buckets[i].Hour = base.Add(time.Duration(i) * time.Hour)
		}
		classes[key] = buckets
	}

	// bucket is the index of the hour holding at: the whole elapsed hours
	// since the window start, so its instant and the start of its bucket are
	// not more than an hour apart.
	bucket := func(at time.Time) (int, bool) {
		elapsed := at.Sub(base)
		if elapsed < 0 || elapsed >= time.Duration(hours)*time.Hour {
			return 0, false
		}
		return int(elapsed / time.Hour), true
	}

	for _, l := range landings {
		i, ok := bucket(l.At)
		if !ok {
			continue
		}
		points, weighted := Actual(l.PolecatSecs, l.LowerBound, l.UpperBound)
		for _, key := range classesOf(l.Agent) {
			hp := &classes[key][i]
			if weighted {
				hp.Points += points
				hp.Weighted++
			} else {
				hp.Unweighted++
			}
		}
	}
	for _, r := range rejections {
		i, ok := bucket(r.At)
		if !ok {
			continue
		}
		points, weighted := Actual(r.PolecatSecs, r.LowerBound, r.UpperBound)
		for _, key := range classesOf(r.Agent) {
			hp := &classes[key][i]
			if weighted {
				hp.RejectedPoints += points
			} else {
				hp.RejectedUnweighted++
			}
		}
	}
	return Series{Classes: classes}
}

// classesOf returns the Series keys a record counts in: AllClass, and the
// agent's seat class when it has one.
func classesOf(agent string) []string {
	if class := SeatClass(agent); class != "" {
		return []string{AllClass, class}
	}
	return []string{AllClass}
}

// HourStart returns the start of the wall-clock hour t falls in, read on loc in
// the UTC offset in force at t; loc must not be nil.
//
// A repeated fall-back hour is two buckets, so the start keeps the occurrence
// t is in: when the offset changes mid-hour, the start is that change rather
// than the hour's first minute (gt-0kzzuz). Every hour bucket uses this rule,
// the velocity series and the dashboard trend alike.
func HourStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	_, off := t.Zone()
	start := t.Add(-(time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())))
	for start.Minute() != 0 {
		if _, prevOff := start.Add(-time.Minute).Zone(); prevOff != off {
			break
		}
		start = start.Add(-time.Minute)
	}
	return start
}
