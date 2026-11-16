package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// claudePlanRowScanSpan is how far back one poll reads the town's claude_plan
// rows: one weekly window, the span the meter folds, plus a day, so the reading
// that closed the window before this one is still in the series. A count of
// rows cannot bound that span: the status line appends a row on every changed
// reading and holds only an unchanged one to the ten-minute heartbeat, so a
// busy town writes far more rows in a week than a count sized for a quiet one
// holds, and the window's start would fall off the read (gt-u9dsir).
const claudePlanRowScanSpan = 8 * 24 * time.Hour

// claudePlanSourceRows names the row series in the plan meter's log line.
const claudePlanSourceRows = "the town's claude_plan rows"

// costSamplesRead is the cost samples read the dashboard's beads handle
// answers: beads.Client's ReadCostSamples without the context, since the
// dashboard's pollers carry none.
type costSamplesRead func(kind string, since time.Time, limit int) ([]beads.Sample, error)

// dashboardClaudePlanPath is the town's own Claude plan series: one JSON line
// per reading, under the town's runtime directory. It is the fallback the plan
// meter reads while the town has no claude_plan row.
func dashboardClaudePlanPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "claude-plan", "samples.jsonl")
}

// newDashboardClaudePlanReader builds the Cost panel's plan meter over the
// series file at path, priced by the flag, with the usage scan's records
// behind it so the plan line can lay what the town's Claude tokens would have
// cost on the API beside the flat plan price (gt-d8sem). The records come
// through the panel's own reader, whose cache the two share, so a month of
// files is decoded once for both panels.
func newDashboardClaudePlanReader(path string, usage *dashUsageReader, planUSDMonth float64) *dashboard.ClaudePlanReader {
	return dashboard.NewClaudePlanReader(path, planUSDMonth).
		WithUsage(usage.monthRecords).
		WithUsageAbsent(usage.absent)
}

// claudePlanRowSamples reads the town's claude_plan rows since the start of
// the span the meter folds, as the plan series. A row missing one of its four
// readings is not a reading and is skipped, never folded as a week at 0%, so a
// partial row another writer left cannot pull the meter down (gt-j3ya0x). The
// skipped count comes back beside the samples so the caller can name the rows
// it could not use; without it a fallback over unusable rows reads the same as
// a town with no rows (gt-u9dsir). A failed read is the caller's error, never
// an empty series.
func claudePlanRowSamples(read costSamplesRead, now time.Time) ([]dashboard.ClaudePlanSample, int, error) {
	// The span is this read's bound, so the count is left to the store's own:
	// no fixed count names a span whose row rate the writer sets.
	rows, err := read(beads.SampleKindClaudePlan, now.Add(-claudePlanRowScanSpan), 0)
	if err != nil {
		return nil, 0, err
	}
	unusable := 0
	samples := make([]dashboard.ClaudePlanSample, 0, len(rows))
	for _, row := range rows {
		p, err := claudePlanOfSample(row)
		if err != nil {
			unusable++
			continue
		}
		samples = append(samples, dashboard.ClaudePlanSample{
			T:                p.At.Unix(),
			FiveHour:         p.FiveHourPct,
			SevenDay:         p.SevenDayPct,
			FiveHourResetsAt: p.FiveHourResetsAt.Unix(),
			SevenDayResetsAt: p.SevenDayResetsAt.Unix(),
		})
	}
	return samples, unusable, nil
}

// newDashboardClaudePlanRead is the plan meter's read for the hub. It chooses
// the series on every poll, never once at build: the town's claude_plan rows
// answer while it has any, and the series file answers only while no row
// exists, so the first row the status line writes is read at the next poll. The
// why-log names the source that answered, once per change. A rows read that
// fails falls back to the file with the reason logged, so a bd hiccup does not
// blank a meter whose old series is still there (gt-hutsr6). Rows that carry no
// complete reading are named in the log beside whichever source answered, so a
// fallback over unusable rows is not read as a town that has no rows
// (gt-u9dsir).
func newDashboardClaudePlanRead(townRoot string, rows costSamplesRead, whyLog *dashWhyLog, usage *dashUsageReader, planUSDMonth float64, renewalDay int, now func() time.Time) func() *dashboard.ClaudePlan {
	file := dashboardClaudePlanPath(townRoot)
	return func() *dashboard.ClaudePlan {
		at := now()
		read := newDashboardClaudePlanReader(file, usage, planUSDMonth).WithRenewalDay(renewalDay)
		samples, unusable, err := claudePlanRowSamples(rows, at)
		why := ""
		if unusable > 0 {
			why = fmt.Sprintf("unusable claude_plan rows: %d", unusable)
		}
		switch {
		case err != nil:
			whyLog.claudePlanSeries(file, fmt.Sprintf("reading the claude_plan rows: %v", err))
		case len(samples) > 0:
			whyLog.claudePlanSeries(claudePlanSourceRows, why)
			read = read.WithSamples(samples)
		default:
			whyLog.claudePlanSeries(file, why)
		}
		return read.Read(at)
	}
}
