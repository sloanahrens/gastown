package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The series' clock: the newest sample of most cases below is taken at this
// instant, so days_left is a whole number of days and the arithmetic reads off
// the page.
var claudePlanNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// planSample is one reading at at, of the weekly limit resetsAt identifies.
func planSample(at time.Time, seven float64, resetsAt int64) ClaudePlanSample {
	return ClaudePlanSample{T: at.Unix(), SevenDay: seven, SevenDayResetsAt: resetsAt}
}

// The meter's arithmetic, case by case: a flat week paces at zero, a rising one
// projects past the reset or onto the limit, and a rollover is a new window
// rather than a fall. The window the newest reading names is the only one the
// pace is measured across, so a reset never shows as a negative pace.
func TestComputeClaudePlanPaceProjectionAndDollars(t *testing.T) {
	t.Parallel()

	twoDays, day := 2*24*time.Hour, 24*time.Hour
	resets := func(d time.Duration) int64 { return claudePlanNow.Add(d).Unix() }
	zero, one, threeQuarters, aThird := 0.0, 1.0, 0.75, 0.33

	for _, tt := range []struct {
		name    string
		samples []ClaudePlanSample
		planUSD float64
		planOut float64
		weekly  float64
		pace1h  float64
		pace24h float64
		proj    float64
		hits    *float64
		weekUSD float64
		usedUSD float64
		stale   bool
	}{
		{
			name:    "no samples: the price stands, the meter is stale",
			planOut: 200, weekUSD: 46.05, stale: true,
		},
		{
			name:    "one sample: no span, so no pace and no projection to make",
			samples: []ClaudePlanSample{planSample(claudePlanNow, 31, resets(twoDays))},
			planUSD: 200, planOut: 200, weekly: 31, proj: 31, weekUSD: 46.05, usedUSD: 14.28,
		},
		{
			name: "a flat week: zero pace, no limit in sight",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 40, resets(twoDays)),
				planSample(claudePlanNow, 40, resets(twoDays)),
			},
			planUSD: 200, planOut: 200, weekly: 40, proj: 40, weekUSD: 46.05, usedUSD: 18.42,
		},
		{
			name: "a rising day: twelve points a day, the reset comes first",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 20, resets(twoDays)),
				planSample(claudePlanNow, 32, resets(twoDays)),
			},
			planUSD: 200, planOut: 200, weekly: 32, pace1h: 12, pace24h: 12, proj: 56,
			weekUSD: 46.05, usedUSD: 14.74,
		},
		{
			name: "the limit lands a day before the reset",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 79, resets(3*day)),
				planSample(claudePlanNow, 91, resets(3*day)),
			},
			planUSD: 200, planOut: 200, weekly: 91, pace1h: 12, pace24h: 12, proj: 127,
			hits: &threeQuarters, weekUSD: 46.05, usedUSD: 41.91,
		},
		{
			name: "a projected week over the limit, a third of a day short of it",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 84, resets(day)),
				planSample(claudePlanNow, 96, resets(day)),
			},
			planUSD: 200, planOut: 200, weekly: 96, pace1h: 12, pace24h: 12, proj: 108,
			hits: &aThird, weekUSD: 46.05, usedUSD: 44.21,
		},
		{
			name: "the limit lands exactly at the reset, so it is still named",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 76, resets(day)),
				planSample(claudePlanNow, 88, resets(day)),
			},
			planUSD: 200, planOut: 200, weekly: 88, pace1h: 12, pace24h: 12, proj: 100,
			hits: &one, weekUSD: 46.05, usedUSD: 40.52,
		},
		{
			name: "a rollover drops to the new window, never below a zero pace",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-2*time.Hour), 95, resets(-time.Minute)),
				planSample(claudePlanNow.Add(-time.Hour), 1, resets(twoDays)),
				planSample(claudePlanNow, 3, resets(twoDays)),
			},
			planUSD: 200, planOut: 200, weekly: 3, pace1h: 48, pace24h: 48, proj: 99,
			weekUSD: 46.05, usedUSD: 1.38,
		},
		{
			name:    "a series nothing has appended to for three quarters of an hour",
			samples: []ClaudePlanSample{planSample(claudePlanNow.Add(-45*time.Minute), 29, resets(twoDays))},
			planUSD: 200, planOut: 200, weekly: 29, proj: 29, weekUSD: 46.05, usedUSD: 13.35,
			stale: true,
		},
		{
			name:    "a week spent to ninety percent a day before the reset",
			samples: []ClaudePlanSample{planSample(claudePlanNow, 90, resets(day))},
			planUSD: 200, planOut: 200, weekly: 90, proj: 90, weekUSD: 46.05, usedUSD: 41.45,
		},
		{
			name: "a week past the limit: reached now, not in a negative time",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 96, resets(twoDays)),
				planSample(claudePlanNow, 105, resets(twoDays)),
			},
			planUSD: 200, planOut: 200, weekly: 105, pace1h: 9, pace24h: 9, proj: 123,
			hits: &zero, weekUSD: 46.05, usedUSD: 48.35,
		},
		{
			name:    "a week past the limit that has gone flat: spent, not unknown",
			samples: []ClaudePlanSample{planSample(claudePlanNow, 102, resets(twoDays))},
			planUSD: 200, planOut: 200, weekly: 102, proj: 102,
			hits: &zero, weekUSD: 46.05, usedUSD: 46.97,
		},
		{
			name:    "a window the clock has already left: a week nobody is spending against",
			samples: []ClaudePlanSample{planSample(claudePlanNow, 88, resets(-time.Hour))},
			planUSD: 200, planOut: 200, weekly: 88, proj: 88,
			weekUSD: 46.05, usedUSD: 40.52, stale: true,
		},
		{
			name: "a week with no reset instant to measure against: no end, so no projection",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 30, 0),
				planSample(claudePlanNow, 40, 0),
			},
			planUSD: 200, planOut: 200, weekly: 40, pace1h: 10, pace24h: 10, proj: 40,
			weekUSD: 46.05, usedUSD: 18.42, stale: true,
		},
		{
			name: "one window whose reset instant the API reports a few seconds apart",
			samples: []ClaudePlanSample{
				planSample(claudePlanNow.Add(-day), 20, resets(twoDays)-4),
				planSample(claudePlanNow, 32, resets(twoDays)),
			},
			planUSD: 200, planOut: 200, weekly: 32, pace1h: 12, pace24h: 12, proj: 56,
			weekUSD: 46.05, usedUSD: 14.74,
		},
		{
			name:    "the plan's price comes from the flag",
			samples: []ClaudePlanSample{planSample(claudePlanNow, 38, resets(twoDays))},
			planUSD: 100, planOut: 100, weekly: 38, proj: 38, weekUSD: 23.03, usedUSD: 8.75,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := ComputeClaudePlan(tt.samples, claudePlanNow, tt.planUSD)

			for _, f := range []struct {
				name string
				got  float64
				want float64
			}{
				{"plan_usd_month", p.PlanUSDMonth, tt.planOut},
				{"weekly_pct", p.WeeklyPct, tt.weekly},
				{"pace_per_day_1h", p.PacePerDay1h, tt.pace1h},
				{"pace_per_day_24h", p.PacePerDay24h, tt.pace24h},
				{"projected_pct_at_reset", p.ProjectedPctAtReset, tt.proj},
				{"weekly_value_usd", p.WeeklyValueUSD, tt.weekUSD},
				{"used_value_usd", p.UsedValueUSD, tt.usedUSD},
			} {
				if !near(f.got, f.want) {
					t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
				}
			}
			switch {
			case tt.hits == nil && p.HitsLimitInDays != nil:
				t.Errorf("hits_limit_in_days = %v, want null", *p.HitsLimitInDays)
			case tt.hits != nil && p.HitsLimitInDays == nil:
				t.Errorf("hits_limit_in_days = null, want %v", *tt.hits)
			case tt.hits != nil && !near(*p.HitsLimitInDays, *tt.hits):
				t.Errorf("hits_limit_in_days = %v, want %v", *p.HitsLimitInDays, *tt.hits)
			}
			if p.Stale != tt.stale {
				t.Errorf("stale = %v, want %v", p.Stale, tt.stale)
			}
			if p.Unavailable {
				t.Error("a computed payload reads as unavailable")
			}
		})
	}
}

// A price nobody set is the default rather than zero dollars a month, and the
// newest reading's own fields reach the payload as they stand: the five-hour
// meter, both reset instants and when the reading was taken.
func TestComputeClaudePlanCarriesTheNewestReadingAndTheDefaultPrice(t *testing.T) {
	t.Parallel()

	at := claudePlanNow.Add(-10 * time.Minute)
	samples := []ClaudePlanSample{{
		T: at.Unix(), FiveHour: 12.5, FiveHourResetsAt: claudePlanNow.Add(time.Hour).Unix(),
		SevenDay: 38, SevenDayResetsAt: claudePlanNow.Add(48 * time.Hour).Unix(),
	}}

	p := ComputeClaudePlan(samples, claudePlanNow, 0)

	if p.PlanUSDMonth != ClaudePlanDefaultUSD {
		t.Errorf("plan_usd_month = %v, want the default %v", p.PlanUSDMonth, ClaudePlanDefaultUSD)
	}
	if p.FiveHourPct != 12.5 || p.FiveHourResetsAt != samples[0].FiveHourResetsAt {
		t.Errorf("the five-hour reading did not reach the payload: %+v", p)
	}
	if p.WeeklyResetsAt != samples[0].SevenDayResetsAt {
		t.Errorf("weekly_resets_at = %d, want %d", p.WeeklyResetsAt, samples[0].SevenDayResetsAt)
	}
	if p.SampledAt != at.Unix() {
		t.Errorf("sampled_at = %d, want the newest reading's %d", p.SampledAt, at.Unix())
	}
}

// claudePlanFile is a temp file the reader is pointed at, as the status line's
// own would be.
func claudePlanFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// planLine is one sample as the status line writes it.
func planLine(at time.Time, fiveHour, seven float64, resetsAt int64) string {
	return fmt.Sprintf(`{"t":%d,"five_hour":%v,"five_hour_resets_at":0,"seven_day":%v,"seven_day_resets_at":%d}`,
		at.Unix(), fiveHour, seven, resetsAt)
}

// A series that is not there and one that has never been appended to are
// different readings: the first is a fault the page names, the second is a
// meter with nothing in it yet.
func TestClaudePlanReaderTellsAMissingSeriesFromAnEmptyOne(t *testing.T) {
	t.Parallel()

	missing := NewClaudePlanReader(filepath.Join(t.TempDir(), "samples.jsonl"), 200).Read(claudePlanNow)
	if !missing.Unavailable || !missing.Stale {
		t.Errorf("a missing series read as %+v, want unavailable and stale", missing)
	}
	if missing.PlanUSDMonth != ClaudePlanDefaultUSD {
		t.Errorf("an unavailable read priced the plan at %v, want the default", missing.PlanUSDMonth)
	}
	// A file the status line has not written yet is a read that found no
	// samples, not a read that failed: the Cost charts say the status line has
	// written nothing rather than that its series broke (gt-yxx98).
	if !missing.Samples.Read || missing.Samples.Failed || len(missing.Samples.Samples) != 0 {
		t.Errorf("a missing series handed the charts %+v, want a read with no samples and no failure", missing.Samples)
	}

	// The town root itself is not a series, and reading it is not a reading of
	// nothing.
	dir := NewClaudePlanReader(t.TempDir(), 200).Read(claudePlanNow)
	if !dir.Unavailable {
		t.Errorf("a directory read as %+v, want unavailable", dir)
	}
	if !dir.Samples.Read || !dir.Samples.Failed {
		t.Errorf("an unreadable series handed the charts %+v, want a read that failed", dir.Samples)
	}

	empty := NewClaudePlanReader(claudePlanFile(t), 200).Read(claudePlanNow)
	if empty.Unavailable {
		t.Errorf("an empty series read as unavailable: %+v", empty)
	}
	if !empty.Stale || empty.WeeklyPct != 0 || empty.SampledAt != 0 {
		t.Errorf("an empty series read as %+v, want a stale meter at zero", empty)
	}
	if !empty.Samples.Read || empty.Samples.Failed || len(empty.Samples.Samples) != 0 {
		t.Errorf("an empty series handed the charts %+v, want a read with no samples and no failure", empty.Samples)
	}
}

// A line that will not parse costs the meter that line and never the series:
// the status line's append is the only writer, and one cut short leaves a bad
// line behind.
func TestClaudePlanReaderSkipsTheLinesItCannotParse(t *testing.T) {
	t.Parallel()

	resets := claudePlanNow.Add(48 * time.Hour).Unix()
	path := claudePlanFile(t,
		`{"t":`,
		planLine(claudePlanNow.Add(-24*time.Hour), 4, 20, resets),
		`{"t":0,"five_hour":1,"seven_day":1,"seven_day_resets_at":1}`,
		`not json at all`,
		planLine(claudePlanNow, 9.5, 44.25, resets),
	)

	p := NewClaudePlanReader(path, 200).Read(claudePlanNow)

	if p.Unavailable {
		t.Fatalf("a series with two good lines read as unavailable: %+v", p)
	}
	if p.WeeklyPct != 44.25 || p.FiveHourPct != 9.5 {
		t.Errorf("the newest good line did not reach the payload: %+v", p)
	}
	if !near(p.PacePerDay24h, 24.25) {
		t.Errorf("pace_per_day_24h = %v, want the two good lines' 24.25", p.PacePerDay24h)
	}
}

// The newest line decides the payload whoever appended it last: two sessions
// appending at once can leave the file out of order.
func TestClaudePlanReaderTakesTheNewestSampleOfTheFile(t *testing.T) {
	t.Parallel()

	resets := claudePlanNow.Add(48 * time.Hour).Unix()
	path := claudePlanFile(t,
		planLine(claudePlanNow.Add(-time.Minute), 4, 30, resets),
		planLine(claudePlanNow, 4, 55, resets),
		planLine(claudePlanNow.Add(-2*time.Minute), 4, 20, resets),
	)

	p := NewClaudePlanReader(path, 200).Read(claudePlanNow)

	if p.WeeklyPct != 55 || p.SampledAt != claudePlanNow.Unix() {
		t.Errorf("the payload is the last line's rather than the newest: %+v", p)
	}
}

// A poll reads the tail of a file that only grows: the series runs for months,
// and the math reaches back no further than the current weekly window.
func TestClaudePlanReaderReadsTheTailOfALongSeries(t *testing.T) {
	t.Parallel()

	const written = 6000
	resets := claudePlanNow.Add(48 * time.Hour).Unix()
	first := claudePlanNow.Add(-10 * 24 * time.Hour)
	var b strings.Builder
	for i := 0; i < written; i++ {
		fmt.Fprintln(&b, planLine(first.Add(time.Duration(i)*time.Minute), 1, 0.5, 0))
	}
	b.WriteString(planLine(claudePlanNow, 4, 38, resets) + "\n")
	path := claudePlanFile(t, strings.TrimSuffix(b.String(), "\n"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= claudePlanTailBytes {
		t.Fatalf("the fixture is %d bytes, want more than the %d byte tail read", info.Size(), claudePlanTailBytes)
	}

	samples, err := readClaudePlanSamples(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 || len(samples) > written {
		t.Fatalf("the tail read gave %d samples, want the tail of the %d written", len(samples), written+1)
	}
	// The file's oldest lines are out of the read, which is what keeps a poll of
	// a months-long series cheap.
	if samples[0].T == first.Unix() {
		t.Errorf("the read began at the file's first line, not at the tail")
	}
	if newest := samples[len(samples)-1]; newest.T != claudePlanNow.Unix() || newest.SevenDay != 38 {
		t.Errorf("the tail read ended at %+v, want the file's newest line", newest)
	}

	p := NewClaudePlanReader(path, 200).Read(claudePlanNow)
	if p.WeeklyPct != 38 || p.Stale {
		t.Errorf("the payload from the long series is %+v, want the newest line at 38%%", p)
	}
}
