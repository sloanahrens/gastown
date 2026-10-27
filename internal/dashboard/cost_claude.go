package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The Claude series of the Cost panel: the plan's dollars on the same buckets
// the DeepSeek charts draw, so one chart carries both spends instead of the plan
// meter standing alone beside them (gt-yxx98).
//
// The dollars are an estimate, not a bill: the weekly limit is worth the
// month's price spread over a week, and a rise in the seven-day percentage of
// that limit is worth its share of that week. The status line is the only
// source, the same samples the plan meter is folded from.

// claudePlanSeriesName names the series wherever the page draws it: the legend
// entry, the chart's aria-label and every bar's tooltip.
const claudePlanSeriesName = "Claude, plan-equivalent estimate"

// PlanSamples is the status line's series as the Cost panel reads it: the
// samples, whether a poll has read the file at all, and whether that read
// failed. The zero value is a series no poll has read yet, which draws nothing
// and says so: "not read" and "the status line has written nothing" are
// different facts, and only the second one is about the status line.
type PlanSamples struct {
	Samples []ClaudePlanSample
	Read    bool
	Failed  bool
}

// costBucket is one bucket of a Cost chart: the short label under the bars, the
// name the tooltips use, and when the bucket starts.
type costBucket struct {
	Label string
	Name  string
	Start time.Time
}

// claudeCostSeries folds the samples into plan-equivalent dollars on the
// chart's buckets, or says why it has no series to draw.
//
// A step between two consecutive samples that fall in the same bucket is what
// that bucket spent (see claudeStepDollars). A step that leaves the bucket is
// not priced at all: the readings on either side of it straddle a stretch the
// series did not sample, and charging the whole rise to the bucket that ended
// the stretch would bill the missing time to the hour that happened to close it.
// A bucket the series has no sample for is a gap, hatched by the page, because
// "no reading" and "no spend" are different facts. A series with no sample in
// the window draws nothing at all.
func claudeCostSeries(buckets []costBucket, lastEnd time.Time, samples PlanSamples, weeklyUSD float64) (*CostClaudeSeries, string) {
	switch {
	case !samples.Read:
		return nil, "the status line's samples have not been read yet"
	case samples.Failed:
		return nil, "the status line's samples could not be read"
	case len(samples.Samples) == 0:
		return nil, "the status line has written no sample yet"
	}
	starts := make([]time.Time, len(buckets))
	for i, b := range buckets {
		starts[i] = b.Start
	}
	// The file is appended in time order, and sorting a series that is already
	// ordered costs nothing; a step between two readings out of order would
	// otherwise read as a reset.
	ordered := make([]ClaudePlanSample, len(samples.Samples))
	copy(ordered, samples.Samples)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].T < ordered[j].T })

	dollars := make([]float64, len(buckets))
	seen := make([]bool, len(buckets))
	inWindow := false
	prevBucket := -1
	for i, s := range ordered {
		b := bucketOf(starts, lastEnd, time.Unix(s.T, 0).In(time.Local))
		if b >= 0 {
			seen[b] = true
			inWindow = true
			// The step is priced only inside one bucket: a step out of a bucket
			// spans time the series did not read.
			if i > 0 && prevBucket == b {
				dollars[b] += claudeStepDollars(ordered[i-1], s, weeklyUSD)
			}
		}
		prevBucket = b
	}
	if !inWindow {
		return nil, "no sample falls in this window"
	}
	series := &CostClaudeSeries{Name: claudePlanSeriesName, Bars: make([]CostBar, len(buckets))}
	for i, b := range buckets {
		if !seen[i] {
			series.Bars[i] = CostBar{Label: b.Label, Gap: true, Title: b.Name + " " + claudePlanSeriesName + ": no sample"}
			continue
		}
		v := roundTo2(dollars[i])
		series.Bars[i] = CostBar{Label: b.Label, Value: v, Title: b.Name + " " + money(v) + " " + claudePlanSeriesName}
	}
	return series, ""
}

// claudeStepDollars is what the step between two readings spent, in dollars: the
// rise in the seven-day percentage of the limit, and nothing more.
//
// A percentage that falls is a week renewing or the API correcting itself, and
// neither is money coming back: the drop contributes 0, never a negative figure,
// and the new week's spending is counted from its own readings from there on. A
// rise is spend, and the instant the week resets carries no dollars of its own.
func claudeStepDollars(prev, s ClaudePlanSample, weeklyUSD float64) float64 {
	if s.SevenDay <= prev.SevenDay {
		return 0
	}
	return (s.SevenDay - prev.SevenDay) / claudePlanLimitPct * weeklyUSD
}

// claudeWeekUSD is what a week of the plan's limit is worth in dollars: the
// month's price spread over a mean month, the figure the plan meter draws
// against. A plan block that has not reported prices the week at the default.
func claudeWeekUSD(plan *ClaudePlan) float64 {
	var usd float64
	if plan != nil {
		usd = plan.PlanUSDMonth
	}
	return roundTo2(planUSD(usd) * claudePlanWeekDays / claudePlanMonthDays)
}

// bucketOf is the index of the bucket that holds at, or -1 when at falls outside
// them. Each bucket ends where the next begins, and the last ends at lastEnd.
func bucketOf(starts []time.Time, lastEnd time.Time, at time.Time) int {
	if len(starts) == 0 || at.Before(starts[0]) || !at.Before(lastEnd) {
		return -1
	}
	// The first start after at bounds the bucket that holds it; a start exactly
	// at is not after it.
	return sort.Search(len(starts), func(i int) bool { return starts[i].After(at) }) - 1
}

// claudeSeriesAria is what a chart's aria-label adds for its Claude series: the
// name, every figure, and how many buckets the series has no sample for.
func claudeSeriesAria(s *CostClaudeSeries) string {
	var words []string
	gaps := 0
	for _, b := range s.Bars {
		if b.Gap {
			gaps++
			continue
		}
		words = append(words, b.Label+" "+money(b.Value))
	}
	out := ". " + s.Name
	if len(words) > 0 {
		out += ": " + strings.Join(words, ", ")
	}
	if gaps > 0 {
		out += fmt.Sprintf("; %d %s no sample", gaps, plural(gaps, "bucket has", "buckets have"))
	}
	return out
}
