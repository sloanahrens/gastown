package cmd

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// dashVelocityRec is a landing record with the fields the velocity series reads.
func dashVelocityRec(bead, agent string, at time.Time, secs float64, lower, upper bool) omRecord {
	return omRecord{Record: landings.Record{
		Bead: bead, Agent: agent, LandedAt: at,
		PolecatSecs: secs, PolecatLowerBound: lower, PolecatUpperBound: upper,
	}}
}

// dashVelocityLookup answers the polecat-time reading a rejection takes: the
// seconds and the lower-bound mark per bead, and it records the asOf each bead
// was asked with.
func dashVelocityLookup(secs map[string]float64, lower map[string]bool, asked map[string]time.Time) func(string, time.Time) (float64, []land.PolecatAttempt, bool) {
	return func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool) {
		if asked != nil {
			asked[bead] = asOf
		}
		return secs[bead], nil, lower[bead]
	}
}

func dashVelocityPtr(v float64) *float64 { return &v }

func dashVelocityNear(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// A fixed feed over the full three-day window: one hour per seat class, an
// unweighted landing, a record older than the window that is not counted, and
// every figure worked out over the last 24 complete hours.
func TestDashboardVelocitySeriesAndFigures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 18, 30, 0, 0, time.Local)
	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, 11, day, hour, minute, 0, 0, time.Local)
	}
	recs := []omRecord{
		// Older than the 72-hour window (it opens 2026-11-08 19:00): it must
		// neither count nor shorten the series.
		dashVelocityRec("gt-old", "sbx-haiku", at(8, 10, 0), 60, false, false),
		dashVelocityRec("gt-a", "sbx-haiku", at(11, 15, 10), 90, false, false),       // 1.5 min -> 1 point
		dashVelocityRec("gt-b", "deepseek-flash", at(11, 16, 20), 120, false, false), // 2 min -> 2 points
		dashVelocityRec("gt-c", "claude-sonnet", at(11, 17, 40), 300, false, false),  // 5 min -> 5 points
		dashVelocityRec("gt-d", "sbx-haiku", at(11, 17, 50), 0, false, false),        // no time: unweighted
	}

	v := buildVelocity(now, recs, nil, dashVelocityLookup(nil, nil, nil))
	if v == nil {
		t.Fatal("buildVelocity = nil, want a reading")
	}
	if len(v.Hours) != velocityHours {
		t.Fatalf("hours = %d, want %d", len(v.Hours), velocityHours)
	}
	if want := at(8, 19, 0); !v.Hours[0].Hour.Equal(want) {
		t.Errorf("first hour = %v, want %v", v.Hours[0].Hour, want)
	}
	if want := at(11, 18, 0); !v.Hours[velocityHours-1].Hour.Equal(want) {
		t.Errorf("last hour = %v, want %v", v.Hours[velocityHours-1].Hour, want)
	}

	type hourWant struct {
		points, weighted, unweighted int
	}
	wants := map[int]hourWant{
		68: {points: 1, weighted: 1},                // 15:00, haiku
		69: {points: 2, weighted: 1},                // 16:00, flash
		70: {points: 5, weighted: 1, unweighted: 1}, // 17:00, sonnet and the unweighted landing
		71: {},                                      // 18:00, the partial hour
	}
	for i, want := range wants {
		h := v.Hours[i]
		if h.Points != want.points || h.Weighted != want.weighted || h.Unweighted != want.unweighted {
			t.Errorf("hour %d (%v): points %d, weighted %d, unweighted %d; want %d, %d, %d",
				i, h.Hour, h.Points, h.Weighted, h.Unweighted, want.points, want.weighted, want.unweighted)
		}
	}

	classes := map[string]struct{ points, weighted, unweighted int }{
		"haiku":  {points: 1, weighted: 1},
		"flash":  {points: 2, weighted: 1},
		"sonnet": {points: 5, weighted: 1},
	}
	for class, want := range classes {
		buckets, ok := v.Classes[class]
		if !ok {
			t.Errorf("classes has no %q series", class)
			continue
		}
		if len(buckets) != velocityHours {
			t.Errorf("class %q has %d hours, want %d", class, len(buckets), velocityHours)
		}
		// The class' points land in the hour its landing did.
		idx := map[string]int{"haiku": 68, "flash": 69, "sonnet": 70}[class]
		h := buckets[idx]
		if h.Points != want.points || h.Weighted != want.weighted {
			t.Errorf("class %q hour %d: points %d, weighted %d; want %d, %d", class, idx, h.Points, h.Weighted, want.points, want.weighted)
		}
	}
	if _, ok := v.Classes[storypoints.AllClass]; !ok {
		t.Error("classes has no all series")
	}
	if v.Hours[0] != v.Classes[storypoints.AllClass][0] {
		t.Error("Hours is not the all class' series")
	}
	if got := v.Classes["haiku"][70].Unweighted; got != 1 {
		t.Errorf("haiku unweighted landings = %d, want 1", got)
	}
	if len(v.Classes) != 4 {
		t.Errorf("classes = %d series, want all, haiku, flash and sonnet", len(v.Classes))
	}

	// Three landings cannot fill a six-hour smoothing window of twelve
	// weighted landings, so no difference is defined, in every class.
	for _, h := range v.Hours {
		if h.Smoothed != nil || h.D1 != nil || h.D2 != nil {
			t.Fatalf("hour %v has a difference on three landings: %+v", h.Hour, h)
		}
		if h.D1State != dashboard.VelocityStateInsufficient || h.D2State != dashboard.VelocityStateInsufficient {
			t.Fatalf("hour %v states = %q/%q, want insufficient", h.Hour, h.D1State, h.D2State)
		}
	}

	f := v.Figures
	if f.Points24h != 8 || f.Unweighted24h != 1 {
		t.Errorf("points/unweighted 24h = %d/%d, want 8/1", f.Points24h, f.Unweighted24h)
	}
	if f.AvgPerHour == nil || !dashVelocityNear(*f.AvgPerHour, 8.0/24) {
		t.Errorf("avg per hour = %v, want 8/24", f.AvgPerHour)
	}
	if f.NowPerHour == nil || !dashVelocityNear(*f.NowPerHour, 8.0/3) {
		t.Errorf("now per hour = %v, want 8/3", f.NowPerHour)
	}
	if f.MedianPerLanding == nil || *f.MedianPerLanding != 2 {
		t.Errorf("median per landing = %v, want 2", f.MedianPerLanding)
	}
}

// A feed whose log begins a few hours ago has a series of those hours, not a
// three-day series with invented empty hours, and every figure is read over the
// hours there are.
func TestDashboardVelocitySeriesBeginsAtOldestRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 18, 30, 0, 0, time.Local)
	recs := []omRecord{dashVelocityRec("gt-a", "sbx-haiku", time.Date(2026, 11, 11, 15, 10, 0, 0, time.Local), 90, false, false)}

	v := buildVelocity(now, recs, nil, dashVelocityLookup(nil, nil, nil))
	if v == nil {
		t.Fatal("buildVelocity = nil, want a reading")
	}
	if len(v.Hours) != 4 {
		t.Fatalf("hours = %d, want 4 (15:00 through 18:00)", len(v.Hours))
	}
	if want := time.Date(2026, 11, 11, 15, 0, 0, 0, time.Local); !v.Hours[0].Hour.Equal(want) {
		t.Errorf("first hour = %v, want %v", v.Hours[0].Hour, want)
	}
	if v.Hours[0].Points != 1 || v.Hours[0].Weighted != 1 {
		t.Errorf("first hour = %+v, want one weighted point", v.Hours[0])
	}
	if v.Hours[3].Points != 0 {
		t.Errorf("the partial hour has %d points, want 0", v.Hours[3].Points)
	}
	if len(v.Classes["all"]) != 4 {
		t.Errorf("all class has %d hours, want 4", len(v.Classes["all"]))
	}
	for _, h := range v.Hours {
		if h.D1 != nil || h.D2 != nil {
			t.Fatalf("hour %v has a difference on a four-hour series", h.Hour)
		}
	}

	f := v.Figures
	if f.Points24h != 1 {
		t.Errorf("points 24h = %d, want 1", f.Points24h)
	}
	if f.AvgPerHour == nil || !dashVelocityNear(*f.AvgPerHour, 1.0/3) {
		t.Errorf("avg per hour = %v, want 1/3 over the three complete hours", f.AvgPerHour)
	}
	if f.NowPerHour == nil || !dashVelocityNear(*f.NowPerHour, 1.0/3) {
		t.Errorf("now per hour = %v, want 1/3", f.NowPerHour)
	}
	if f.MedianPerLanding == nil || *f.MedianPerLanding != 1 {
		t.Errorf("median per landing = %v, want 1", f.MedianPerLanding)
	}
}

// A record without polecat time is counted and adds no points; a rejection
// takes its time from the polecat lookup with a zero asOf, is unweighted when
// the lookup has no reading, and sits in the seat class of the bead's landed
// record.
func TestDashboardVelocityUnweightedAndRejections(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 18, 30, 0, 0, time.Local)
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 11, hour, minute, 0, 0, time.Local)
	}
	recs := []omRecord{
		dashVelocityRec("gt-l", "sbx-haiku", at(14, 5), 120, false, false),       // 2 min -> 2 points
		dashVelocityRec("gt-m", "sbx-haiku", at(15, 10), 0, false, false),        // no time
		dashVelocityRec("gt-n", "deepseek-flash", at(14, 10), 120, false, false), // 2 min -> 2 points
	}
	rejs := []omRejection{
		{At: at(16, 0), Bead: "gt-n"},  // 1 min -> 1 rejected point, flash by its landed record
		{At: at(17, 0), Bead: "gt-p"},  // no reading: unweighted, no landed record
		{At: at(17, 10), Bead: "gt-q"}, // lower bound: unweighted, no landed record
	}
	asked := map[string]time.Time{}
	lookup := dashVelocityLookup(
		map[string]float64{"gt-n": 60, "gt-p": 0, "gt-q": 300},
		map[string]bool{"gt-q": true},
		asked,
	)

	v := buildVelocity(now, recs, rejs, lookup)
	if v == nil {
		t.Fatal("buildVelocity = nil, want a reading")
	}
	if len(v.Hours) != 5 {
		t.Fatalf("hours = %d, want 5 (14:00 through 18:00)", len(v.Hours))
	}
	for _, bead := range []string{"gt-n", "gt-p", "gt-q"} {
		if !asked[bead].IsZero() {
			t.Errorf("rejection of %s was read as of %v, want the zero asOf a rejected row uses", bead, asked[bead])
		}
	}

	h14 := v.Hours[0]
	if h14.Points != 4 || h14.Weighted != 2 || h14.Unweighted != 0 {
		t.Errorf("hour 14: points %d, weighted %d, unweighted %d; want 4, 2, 0", h14.Points, h14.Weighted, h14.Unweighted)
	}
	if h := v.Hours[1]; h.Points != 0 || h.Unweighted != 1 {
		t.Errorf("hour 15: points %d, unweighted %d; want 0 and 1", h.Points, h.Unweighted)
	}
	if h := v.Hours[2]; h.RejectedPoints != 1 || h.RejectedUnweighted != 0 {
		t.Errorf("hour 16: rejected points %d, rejected unweighted %d; want 1 and 0", h.RejectedPoints, h.RejectedUnweighted)
	}
	if h := v.Hours[3]; h.RejectedPoints != 0 || h.RejectedUnweighted != 2 {
		t.Errorf("hour 17: rejected points %d, rejected unweighted %d; want 0 and 2", h.RejectedPoints, h.RejectedUnweighted)
	}

	if got := v.Classes["flash"][2].RejectedPoints; got != 1 {
		t.Errorf("flash rejected points in hour 16 = %d, want 1 from gt-n's landed record", got)
	}
	if got := v.Classes["haiku"][1].Unweighted; got != 1 {
		t.Errorf("haiku unweighted landings in hour 15 = %d, want 1", got)
	}
	for _, class := range []string{"haiku", "flash", "sonnet"} {
		if got := v.Classes[class][3].RejectedUnweighted; got != 0 {
			t.Errorf("class %q counted %d unweighted rejections with no landed record, want 0", class, got)
		}
	}
	if got := v.Classes[storypoints.AllClass][3].RejectedUnweighted; got != 2 {
		t.Errorf("all class unweighted rejections in hour 17 = %d, want 2", got)
	}

	f := v.Figures
	if f.Points24h != 4 || f.Unweighted24h != 1 {
		t.Errorf("points/unweighted 24h = %d/%d, want 4/1", f.Points24h, f.Unweighted24h)
	}
	if f.AvgPerHour == nil || !dashVelocityNear(*f.AvgPerHour, 1) {
		t.Errorf("avg per hour = %v, want 1", f.AvgPerHour)
	}
	if f.NowPerHour == nil || *f.NowPerHour != 0 {
		t.Errorf("now per hour = %v, want a defined 0: the last three complete hours landed no points", f.NowPerHour)
	}
	// The even count [2, 2] takes the mean rounded down, and the rejected
	// landing of gt-n is not a landed point.
	if f.MedianPerLanding == nil || *f.MedianPerLanding != 2 {
		t.Errorf("median per landing = %v, want 2", f.MedianPerLanding)
	}
}

// Every storypoints state has one name in the feed, and a state outside them
// is insufficient rather than a fourth value.
func TestDashboardVelocityStateNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state storypoints.State
		want  string
	}{
		{storypoints.StateSignal, dashboard.VelocityStateSignal},
		{storypoints.StateNoise, dashboard.VelocityStateNoise},
		{storypoints.StateInsufficient, dashboard.VelocityStateInsufficient},
		{storypoints.State(99), dashboard.VelocityStateInsufficient},
	}
	for _, tc := range cases {
		if got := velocityState(tc.state); got != tc.want {
			t.Errorf("velocityState(%d) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// When the differences could not be computed the hours keep their counts and
// carry no difference, never a zero that reads as a quiet series.
func TestDashboardVelocityHourSeriesWithoutDifferences(t *testing.T) {
	t.Parallel()
	hour := time.Date(2026, 11, 11, 15, 0, 0, 0, time.Local)
	buckets := []storypoints.HourPoint{
		{Hour: hour, Points: 8, Weighted: 2, Unweighted: 1, RejectedPoints: 3, RejectedUnweighted: 1},
	}
	cases := []struct {
		name    string
		derived storypoints.Derived
	}{
		{
			// Derive answers an error with no hours. A Derived carrying both is
			// still not trusted: a refused input's hours must not be served as
			// a judged series.
			name: "Derive refused its input",
			derived: storypoints.Derived{
				Hours: []storypoints.Hour{{
					Smoothed: dashVelocityPtr(2), D1: dashVelocityPtr(-1), D2: dashVelocityPtr(0.5),
					D1State: storypoints.StateSignal, D2State: storypoints.StateNoise,
				}},
				Err: errors.New("storypoints: derive hours must be consecutive whole hours"),
			},
		},
		{name: "Derive answered a shorter series", derived: storypoints.Derived{Hours: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := velocityHourSeries(buckets, tc.derived)
			if len(got) != 1 {
				t.Fatalf("hours = %d, want 1", len(got))
			}
			h := got[0]
			if h.Points != 8 || h.Weighted != 2 || h.Unweighted != 1 || h.RejectedPoints != 3 || h.RejectedUnweighted != 1 {
				t.Errorf("counts lost without differences: %+v", h)
			}
			if h.Smoothed != nil || h.D1 != nil || h.D2 != nil {
				t.Errorf("a difference was served without one: %+v", h)
			}
			if h.D1State != dashboard.VelocityStateInsufficient || h.D2State != dashboard.VelocityStateInsufficient {
				t.Errorf("states = %q/%q, want insufficient", h.D1State, h.D2State)
			}
		})
	}
}

// The hub's trend is buildTrend's unchanged reading plus the velocity series
// built from the same records; a feed with no records has no velocity reading.
func TestDashboardVelocityTrendCarriesIt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 18, 30, 0, 0, time.Local)
	recs := []omRecord{dashVelocityRec("gt-a", "sbx-haiku", time.Date(2026, 11, 11, 17, 10, 0, 0, time.Local), 120, false, false)}
	polecat := dashVelocityLookup(nil, nil, nil)

	tr := buildTrendWithVelocity(now, recs, nil, nil, nil, polecat)
	if tr.Velocity == nil {
		t.Fatal("Trend.Velocity = nil, want the velocity series set from the records")
	}
	if len(tr.Velocity.Hours) != 2 {
		t.Errorf("velocity hours = %d, want 2 (17:00 and 18:00)", len(tr.Velocity.Hours))
	}
	plain := buildTrend(now, recs, nil, nil, nil)
	plain.Velocity = tr.Velocity
	if !reflect.DeepEqual(plain, tr) {
		t.Errorf("buildTrendWithVelocity changed the trend besides Velocity:\n got %+v\nwant %+v", tr, plain)
	}

	empty := buildTrendWithVelocity(now, nil, nil, nil, nil, nil)
	if empty.Velocity != nil {
		t.Errorf("Trend.Velocity = %+v on a feed with no records, want nil", empty.Velocity)
	}
}
