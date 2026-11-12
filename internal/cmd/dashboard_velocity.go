package cmd

import (
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// The velocity series reads three days: storypoints.Derive judges a difference
// over a six-hour smoothing and three-hour steps, which the trend's 24 hours
// cannot fill. The figures read the last day of it, and the trend window stays
// trendHours (gt-vd91ny.5).
const velocityHours = 72

// velocityWindowStart is the oldest hour the velocity series covers.
func velocityWindowStart(now time.Time) time.Time {
	return windowStart(now, velocityHours)
}

// buildVelocity builds the Landings pane's velocity reading from the landing
// records and rejections the pane already reads. The series starts at the
// oldest hour the feed covers — never earlier, so an hour the feed does not
// hold is not invented as an empty one — and ends with the hour now falls in,
// which storypoints.Derive leaves out of every difference because it is
// partial. A feed with no records and no rejections has no reading at all.
//
// A landing's time and agent come from its record. A rejection's time is read
// from polecat with a zero asOf, the same reading a rejected Landings row
// takes; its agent is the one on the bead's newest landed record, which names
// the work that was rejected, and a rejection whose bead has no landed record
// counts in the town's series only. polecat is asked once per rejection and
// must not be nil when rejs is non-empty.
func buildVelocity(now time.Time, recs []omRecord, rejs []omRejection, polecat func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool)) *dashboard.Velocity {
	return buildVelocityIn(now, recs, rejs, polecat, time.Local)
}

// buildVelocityIn is buildVelocity with its hours read on loc.
func buildVelocityIn(now time.Time, recs []omRecord, rejs []omRejection, polecat func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool), loc *time.Location) *dashboard.Velocity {
	if len(recs) == 0 && len(rejs) == 0 {
		return nil
	}
	base := localHourIn(now, loc)
	first := windowStartIn(now, velocityHours, loc)
	// The log may begin inside the window. The series then begins at the
	// oldest hour it holds, so the hours before the feed's own history are not
	// drawn as hours with no landings.
	oldest := base
	for _, rec := range recs {
		if h := localHourIn(rec.LandedAt, loc); h.Before(oldest) {
			oldest = h
		}
	}
	for _, rj := range rejs {
		if h := localHourIn(rj.At, loc); h.Before(oldest) {
			oldest = h
		}
	}
	if oldest.After(first) {
		first = oldest
	}
	hours := int(base.Sub(first)/time.Hour) + 1

	landings := make([]storypoints.Landing, 0, len(recs))
	agentOf := make(map[string]string, len(recs))
	for _, rec := range recs {
		landings = append(landings, storypoints.Landing{
			At:          rec.LandedAt,
			Agent:       rec.Agent,
			PolecatSecs: rec.PolecatSecs,
			LowerBound:  rec.PolecatLowerBound,
			UpperBound:  rec.PolecatUpperBound,
		})
		if rec.Agent != "" {
			// The log is oldest first, so the last record of a bead is its
			// newest landing: a resubmission's agent replaces the one it
			// replaced.
			agentOf[rec.Bead] = rec.Agent
		}
	}
	rejections := make([]storypoints.Rejection, 0, len(rejs))
	for _, rj := range rejs {
		secs, _, lower := polecat(rj.Bead, time.Time{})
		rejections = append(rejections, storypoints.Rejection{
			At:          rj.At,
			PolecatSecs: secs,
			LowerBound:  lower,
			Agent:       agentOf[rj.Bead],
		})
	}

	series := storypoints.Hourly(landings, rejections, first, hours, loc)
	classes := make(map[string][]dashboard.VelocityHour, len(series.Classes))
	for key, buckets := range series.Classes {
		classes[key] = velocityHourSeries(buckets, deriveSeries(buckets, now))
	}
	all := classes[storypoints.AllClass]
	return &dashboard.Velocity{
		Hours:   all,
		Classes: classes,
		Figures: velocityFigures(all, landings, now),
	}
}

// buildTrendWithVelocity is the trend the hub publishes: buildTrend's last 24
// hours plus the velocity reading, both built from one poll's records,
// rejections and polecat-time lookup. It is the call site of both, so the
// pane's bars and its figures always describe the same reading.
func buildTrendWithVelocity(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, loads []dashboard.LoadPoint, polecat func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool)) *dashboard.Trend {
	tr := buildTrend(now, recs, stages, rejs, loads)
	tr.Velocity = buildVelocity(now, recs, rejs, polecat)
	return tr
}

// deriveSeries takes the smoothed points and the two differences of one
// class's buckets. It passes storypoints.Derive one point total and one
// weighted landing count per hour, the hours consecutive as storypoints.Hourly
// builds them.
func deriveSeries(buckets []storypoints.HourPoint, now time.Time) storypoints.Derived {
	points := make([]float64, len(buckets))
	weighted := make([]int, len(buckets))
	starts := make([]time.Time, len(buckets))
	for i, b := range buckets {
		points[i] = float64(b.Points)
		weighted[i] = b.Weighted
		starts[i] = b.Hour
	}
	return storypoints.Derive(points, weighted, now, starts)
}

// velocityHourSeries joins one class's hourly buckets with its differences.
// The buckets always carry their counts. When the differences could not be
// computed — Derive refused its input, or answered fewer hours than it was
// given — every hour carries no difference and the state insufficient, which
// is where an hour with too little data sits and never a quiet one.
func velocityHourSeries(buckets []storypoints.HourPoint, d storypoints.Derived) []dashboard.VelocityHour {
	out := make([]dashboard.VelocityHour, len(buckets))
	matched := d.Err == nil && len(d.Hours) == len(buckets)
	for i, b := range buckets {
		out[i] = dashboard.VelocityHour{
			Hour:               b.Hour,
			Points:             b.Points,
			Weighted:           b.Weighted,
			Unweighted:         b.Unweighted,
			RejectedPoints:     b.RejectedPoints,
			RejectedUnweighted: b.RejectedUnweighted,
			D1State:            dashboard.VelocityStateInsufficient,
			D2State:            dashboard.VelocityStateInsufficient,
		}
		if matched {
			out[i].Smoothed = d.Hours[i].Smoothed
			out[i].D1 = d.Hours[i].D1
			out[i].D2 = d.Hours[i].D2
			out[i].D1State = velocityState(d.Hours[i].D1State)
			out[i].D2State = velocityState(d.Hours[i].D2State)
		}
	}
	return out
}

// velocityState names a difference's state as the feed carries it. The three
// storypoints states map one to one; a state outside them is insufficient,
// never a signal or a noise.
func velocityState(s storypoints.State) string {
	switch s {
	case storypoints.StateSignal:
		return dashboard.VelocityStateSignal
	case storypoints.StateNoise:
		return dashboard.VelocityStateNoise
	default:
		return dashboard.VelocityStateInsufficient
	}
}

// velocityFigures is the figures row's reading of a class's series: the points
// and unweighted landings of its last 24 complete hours, the mean points per
// hour over those hours and over the last three, and the median points a
// landing carried in the same 24 hours. The hour now falls in is partial and is
// in none of them; a series shorter than a day is read over the hours it has. A
// figure with no complete hour behind it is nil, never zero.
func velocityFigures(hours []dashboard.VelocityHour, landings []storypoints.Landing, now time.Time) dashboard.VelocityFigures {
	var figs dashboard.VelocityFigures
	complete := 0
	for complete < len(hours) && !hours[complete].Hour.Add(time.Hour).After(now) {
		complete++
	}
	day := min(complete, 24)
	for _, h := range hours[complete-day : complete] {
		figs.Points24h += h.Points
		figs.Unweighted24h += h.Unweighted
	}
	if day > 0 {
		avg := float64(figs.Points24h) / float64(day)
		figs.AvgPerHour = &avg
	}
	if now3 := min(complete, 3); now3 > 0 {
		sum := 0
		for _, h := range hours[complete-now3 : complete] {
			sum += h.Points
		}
		rate := float64(sum) / float64(now3)
		figs.NowPerHour = &rate
	}
	if day > 0 {
		from := hours[complete-day].Hour
		until := hours[complete-1].Hour.Add(time.Hour)
		if med, ok := medianLandingPoints(landings, from, until); ok {
			figs.MedianPerLanding = &med
		}
	}
	return figs
}

// medianLandingPoints is the median points of the weighted landings that
// landed in [from, until). An unweighted landing adds no point and is not a
// zero. An even count takes the mean of the two middle points rounded down,
// because the feed carries the median as whole points.
func medianLandingPoints(landings []storypoints.Landing, from, until time.Time) (int, bool) {
	var points []int
	for _, l := range landings {
		if l.At.Before(from) || !l.At.Before(until) {
			continue
		}
		if p, weighted := storypoints.Actual(l.PolecatSecs, l.LowerBound, l.UpperBound); weighted {
			points = append(points, p)
		}
	}
	if len(points) == 0 {
		return 0, false
	}
	sort.Ints(points)
	n := len(points)
	if n%2 == 1 {
		return points[n/2], true
	}
	return (points[n/2-1] + points[n/2]) / 2, true
}
