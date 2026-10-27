package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The plan meter's flag defaults to the price the status line's percentages are
// drawn against, and the reader the command wires reads the town's own series.
func TestDashboardClaudePlanWiring(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("claude-plan-usd")
	if f == nil {
		t.Fatal("gt dashboard has no --claude-plan-usd flag")
	}
	if want := fmt.Sprint(dashboard.ClaudePlanDefaultUSD); f.DefValue != want {
		t.Errorf("--claude-plan-usd defaults to %q, want %q", f.DefValue, want)
	}

	root := t.TempDir()
	path := dashboardClaudePlanPath(root)
	if want := filepath.Join(root, ".runtime", "claude-plan", "samples.jsonl"); path != want {
		t.Errorf("the plan series is read from %q, want %q", path, want)
	}

	now := time.Now()
	path = writeClaudePlanSeries(t, root, now)

	p := dashboard.NewClaudePlanReader(path, dashboardClaudePlanUSD).Read(now)

	if p.WeeklyPct != 38 || p.Stale || p.Unavailable {
		t.Errorf("the reader wired to the town's series read %+v, want a fresh 38%%", p)
	}
	if p.UsedValueUSD <= 0 {
		t.Errorf("the flag's price did not reach the reading: %+v", p)
	}
}

// claudePlanSeriesResets is how far ahead of the reading the sample below says
// the plan's weekly limit resets, which is what places the week the
// API-equivalent figure covers.
const claudePlanSeriesResets = 48 * time.Hour

// writeClaudePlanSeries writes one status line sample into root's town, the way
// the status line appends it, and returns the series path.
func writeClaudePlanSeries(t *testing.T, root string, now time.Time) string {
	t.Helper()
	path := dashboardClaudePlanPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"t":%d,"five_hour":8,"five_hour_resets_at":%d,"seven_day":38,"seven_day_resets_at":%d}`,
		now.Unix(), now.Add(time.Hour).Unix(), now.Add(claudePlanSeriesResets).Unix())
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The plan line's API-equivalent dollars are what the town's own Claude tokens
// would have cost on the API. They come off the usage scan's records, read by
// the panel's own reader - so this exercises the reader the command builds, not
// a second one assembled to match it (gt-d8sem).
func TestDashboardClaudePlanReaderCarriesTheAPIEquivalentDollars(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Now()
	writeClaudePlanSeries(t, root, now)

	// One complete record ending an hour ago, of a model the price file knows.
	records := filepath.Join(root, ".runtime", "usage", usageRecordsSubdir)
	if err := os.MkdirAll(records, 0o755); err != nil {
		t.Fatal(err)
	}
	end := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"bead":"gt-a","kind":"work","class":"polecat","rig":"gastown","complete":true,`+
		`"start":%q,"end":%q,"wall_s":300,`+
		`"models":{"claude-sonnet-5-5":{"calls":1,"input":1000000,"output":1000000,"cache_creation":0,"cache_read":0,"est_cost_usd":0}}}`,
		now.Add(-time.Hour-time.Minute).UTC().Format(time.RFC3339), end)
	if err := os.WriteFile(filepath.Join(records, "gt-a.work.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newDashboardClaudePlanReader(root, newDashUsageReader(root), dashboardClaudePlanUSD).Read(now)

	prices, err := dashboard.EmbeddedClaudePrices()
	if err != nil {
		t.Fatal(err)
	}
	want := dashboard.APIEquivalent([]dashboard.UsageRecord{{
		Complete: true, End: now.Add(-time.Hour),
		Models: map[string]dashboard.UsageModel{"claude-sonnet-5-5": {Input: 1e6, Output: 1e6}},
	}}, prices, now, now.Add(claudePlanSeriesResets).Unix()).WeekUSD
	if p.APIEquivalentWeekUSD == nil || p.APIEquivalentMonthUSD == nil {
		t.Fatalf("the reader the command builds carries no API-equivalent figures: %+v", p)
	}
	if want <= 0 {
		t.Fatalf("the price file prices the town's own model at nothing: %v", want)
	}
	if *p.APIEquivalentWeekUSD != want || *p.APIEquivalentMonthUSD != want {
		t.Errorf("week = $%v, month = $%v, want the records priced at $%v", *p.APIEquivalentWeekUSD, *p.APIEquivalentMonthUSD, want)
	}
	if len(p.Unpriced) != 0 {
		t.Errorf("unpriced = %v, want none for a model the price file names", p.Unpriced)
	}

	// A town whose records directory is not there has no figures rather than a
	// month at $0.00. Its series is there, so the plan line is still drawn.
	noRecords := t.TempDir()
	writeClaudePlanSeries(t, noRecords, now)
	unread := newDashboardClaudePlanReader(noRecords, newDashUsageReader(noRecords), dashboardClaudePlanUSD).Read(now)
	if unread.APIEquivalentWeekUSD != nil || unread.APIEquivalentMonthUSD != nil {
		t.Errorf("records that could not be read gave %+v, want no figures", unread)
	}

	// A records directory that exists and is empty is a town that spent nothing:
	// a figure of zero, which is a different reading from none at all.
	emptyRoot := t.TempDir()
	writeClaudePlanSeries(t, emptyRoot, now)
	if err := os.MkdirAll(filepath.Join(emptyRoot, ".runtime", "usage", usageRecordsSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := newDashboardClaudePlanReader(emptyRoot, newDashUsageReader(emptyRoot), dashboardClaudePlanUSD).Read(now)
	if empty.APIEquivalentWeekUSD == nil || *empty.APIEquivalentWeekUSD != 0 {
		t.Errorf("an empty records directory gave %v, want a figure of zero", empty.APIEquivalentWeekUSD)
	}
}

// The month rollup's renewal day defaults to the day the reader's billing month
// starts on, and the flag that sets it is on gt dashboard: a plan renewed on
// another day draws its month from that day, and the two cannot drift apart
// without the help saying so.
func TestDashboardClaudePlanRenewalDayFlag(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("claude-plan-renewal-day")
	if f == nil {
		t.Fatal("gt dashboard has no --claude-plan-renewal-day flag")
	}
	if want := fmt.Sprint(dashboard.ClaudePlanDefaultRenewalDay); f.DefValue != want {
		t.Errorf("--claude-plan-renewal-day defaults to %q, want %q", f.DefValue, want)
	}
	if !strings.Contains(dashboardCmd.Long, "--claude-plan-renewal-day") {
		t.Error("gt dashboard's help does not name the flag the month rollup reads")
	}

	// The flag's value is the day the reader's month starts on, which is the
	// plan's own month rather than the calendar's.
	now := time.Now()
	reader := dashboard.NewClaudePlanReader(filepath.Join(t.TempDir(), "samples.jsonl"), 200).
		WithRenewalDay(1)
	if p := reader.Read(now); p.StartAt != time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Unix() {
		t.Errorf("a reader renewing on the 1st drew its month from %d, want the 1st of this month", p.StartAt)
	}
}

// The poll's read of the plan hands the Cost charts its series, so the charts
// are drawn from the same single read of the file the meter is: a town whose
// status line has written a sample reads it, and one whose file is not there yet
// is no sample rather than a failed read, so the charts say the status line has
// written nothing instead of that the series broke (gt-yxx98).
func TestDashboardClaudePlanReaderHandsTheSamplesToTheCharts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	now := time.Now()
	path := writeClaudePlanSeries(t, root, now)
	got := dashboard.NewClaudePlanReader(path, dashboardClaudePlanUSD).Read(now).Samples
	if !got.Read || got.Failed || len(got.Samples) != 1 || got.Samples[0].SevenDay != 38 {
		t.Errorf("the series the charts read = %+v, want the one written sample at 38%%", got)
	}

	missing := dashboard.NewClaudePlanReader(filepath.Join(root, "absent.jsonl"), dashboardClaudePlanUSD).Read(now).Samples
	if !missing.Read || missing.Failed || len(missing.Samples) != 0 {
		t.Errorf("a series the status line has not written yet = %+v, want a read with no samples and no failure", missing)
	}
}
