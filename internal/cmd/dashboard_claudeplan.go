package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// claudePlanRowScan bounds how many claude_plan rows one poll reads: about the
// number of sample lines the series file's tail read holds, so the meter
// reaches back as far from the rows as it did from the file.
const claudePlanRowScan = 2000

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

// claudePlanRowSamples reads the town's claude_plan rows as the plan series.
// A row missing one of its four readings is not a reading and is skipped, never
// folded as a week at 0%, so a partial row another writer left cannot pull the
// meter down (gt-j3ya0x). A failed read is the caller's error, never an empty
// series.
func claudePlanRowSamples(read costSamplesRead) ([]dashboard.ClaudePlanSample, error) {
	rows, err := read(beads.SampleKindClaudePlan, time.Time{}, claudePlanRowScan)
	if err != nil {
		return nil, err
	}
	samples := make([]dashboard.ClaudePlanSample, 0, len(rows))
	for _, row := range rows {
		p, err := claudePlanOfSample(row)
		if err != nil {
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
	return samples, nil
}

// newDashboardClaudePlanRead is the plan meter's read for the hub. It chooses
// the series on every poll, never once at build: the town's claude_plan rows
// answer while it has any, and the series file answers only while no row
// exists, so the first row the status line writes is read at the next poll. The
// why-log names the source that answered, once per change. A rows read that
// fails falls back to the file with the reason logged, so a bd hiccup does not
// blank a meter whose old series is still there (gt-hutsr6).
func newDashboardClaudePlanRead(townRoot string, rows costSamplesRead, whyLog *dashWhyLog, usage *dashUsageReader, planUSDMonth float64, renewalDay int, now func() time.Time) func() *dashboard.ClaudePlan {
	file := dashboardClaudePlanPath(townRoot)
	return func() *dashboard.ClaudePlan {
		read := newDashboardClaudePlanReader(file, usage, planUSDMonth).WithRenewalDay(renewalDay)
		samples, err := claudePlanRowSamples(rows)
		switch {
		case err != nil:
			whyLog.claudePlanSeries(file, fmt.Sprintf("reading the claude_plan rows: %v", err))
		case len(samples) > 0:
			whyLog.claudePlanSeries(claudePlanSourceRows, "")
			read = read.WithSamples(samples)
		default:
			whyLog.claudePlanSeries(file, "")
		}
		return read.Read(now())
	}
}
