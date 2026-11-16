package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// fixedClaudePlanNow is the clock the plan read tests run at: mid-day and
// mid-month, so the billing month the rollup draws cannot move between two
// reads of the same series.
var fixedClaudePlanNow = time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)

// claudePlanRead is one hub-shaped plan read over town at the fixed clock,
// logging its why lines into logged, which may be nil.
func claudePlanRead(t *testing.T, town string, rows costSamplesRead, logged *[]string) func() *dashboard.ClaudePlan {
	t.Helper()
	whyLog := newDashWhyLog(func(format string, args ...any) {
		if logged != nil {
			*logged = append(*logged, fmt.Sprintf(format, args...))
		}
	})
	return newDashboardClaudePlanRead(town, rows, whyLog, newDashUsageReader(town),
		dashboardClaudePlanUSD, dashboard.ClaudePlanDefaultRenewalDay, func() time.Time { return fixedClaudePlanNow })
}

// claudePlanRow is one claude_plan row, the way gt usage sample writes it.
func claudePlanRow(at time.Time, fiveHour, sevenDay int) beads.Sample {
	five, seven := float64(fiveHour), float64(sevenDay)
	fiveResets, sevenResets := at.Add(time.Hour).UTC(), at.Add(claudePlanSeriesResets).UTC()
	return beads.Sample{
		Kind:             beads.SampleKindClaudePlan,
		At:               at.UTC(),
		FiveHourPct:      &five,
		FiveHourResetsAt: &fiveResets,
		SevenDayPct:      &seven,
		SevenDayResetsAt: &sevenResets,
	}
}

// claudePlanRowAt is claudePlanRow with the weekly window's reset named, for a
// reading from a window that has already rolled over.
func claudePlanRowAt(at, sevenResets time.Time, fiveHour, sevenDay int) beads.Sample {
	row := claudePlanRow(at, fiveHour, sevenDay)
	row.SevenDayResetsAt = &sevenResets
	return row
}

// rowsOf answers a rows read with rows, newest first as bd lists them.
func rowsOf(rows ...beads.Sample) costSamplesRead {
	return func(string, time.Time, int) ([]beads.Sample, error) { return rows, nil }
}

// TestClaudePlanReadPrefersTheCostRows: the plan meter reads the town's
// claude_plan rows before the series file, so the page draws the series the
// status line records now rather than an older copy, and the why-log names the
// rows as the source that answered. The read reaches back the weekly window the
// meter folds and leaves the count to the store, since a fixed count cannot
// name a span whose row rate the status line sets (gt-u9dsir).
func TestClaudePlanReadPrefersTheCostRows(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeClaudePlanSeries(t, town, fixedClaudePlanNow) // the file stands at 38%

	var logged []string
	var asked []string
	var sinces []time.Time
	var limits []int
	rows := func(kind string, since time.Time, limit int) ([]beads.Sample, error) {
		asked = append(asked, kind)
		sinces = append(sinces, since)
		limits = append(limits, limit)
		return []beads.Sample{claudePlanRow(fixedClaudePlanNow, 3, 7)}, nil
	}
	p := claudePlanRead(t, town, rows, &logged)()

	if p.WeeklyPct != 7 || p.FiveHourPct != 3 {
		t.Errorf("the meter reads %v%% of the week and %v%% of five hours, want the rows' 7 and 3, not the file's 38 and 8", p.WeeklyPct, p.FiveHourPct)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], claudePlanSourceRows) {
		t.Errorf("the why-log = %q, want one line naming %q", logged, claudePlanSourceRows)
	}
	if len(asked) != 1 || asked[0] != beads.SampleKindClaudePlan {
		t.Errorf("the poll read rows of kind %v, want one %s read", asked, beads.SampleKindClaudePlan)
	}
	if want := fixedClaudePlanNow.Add(-claudePlanRowScanSpan); len(sinces) != 1 || !sinces[0].Equal(want) {
		t.Errorf("the poll read rows since %v, want %v, a span that covers the weekly window", sinces, want)
	}
	if len(limits) != 1 || limits[0] != 0 {
		t.Errorf("the poll read rows with limits %v, want no count of its own (0 = the store's bound)", limits)
	}
}

// TestClaudePlanReadFallsBackToTheSeriesFile: while the town has no claude_plan
// row the series file answers, and the why-log names the file it read, so a
// town that has not started recording rows still draws its meter. The line
// carries no unusable-row reason, so a town with no row and one whose rows are
// all unusable read differently in the log (gt-u9dsir).
func TestClaudePlanReadFallsBackToTheSeriesFile(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	file := writeClaudePlanSeries(t, town, fixedClaudePlanNow)

	var logged []string
	p := claudePlanRead(t, town, rowsOf(), &logged)()

	if p.WeeklyPct != 38 {
		t.Errorf("the meter reads %v%%, want the file's 38%%", p.WeeklyPct)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], file) {
		t.Fatalf("the why-log = %q, want one line naming %s", logged, file)
	}
	if strings.Contains(logged[0], "unusable") {
		t.Errorf("the why-log = %q, want no unusable-row reason for a town with no rows", logged)
	}
}

// TestClaudePlanReadSameSeriesFromRowsAndFile: one series drawn from rows and
// from the file folds to the same payload, so the page's shape does not depend
// on which source answered.
func TestClaudePlanReadSameSeriesFromRowsAndFile(t *testing.T) {
	t.Parallel()

	rowTown := t.TempDir()
	fromRows := claudePlanRead(t, rowTown, rowsOf(claudePlanRow(fixedClaudePlanNow, 8, 38)), nil)()

	fileTown := t.TempDir()
	writeClaudePlanSeries(t, fileTown, fixedClaudePlanNow)
	fromFile := claudePlanRead(t, fileTown, rowsOf(), nil)()

	if !reflect.DeepEqual(fromRows, fromFile) {
		t.Errorf("the same series folds to %+v from rows and %+v from the file", fromRows, fromFile)
	}
}

// TestClaudePlanReadFallsBackWhenTheRowsFail: a rows read that fails answers
// the file's series and logs the reason once, so a bd trouble that leaves the
// old series in place does not blank the meter and is not silent.
func TestClaudePlanReadFallsBackWhenTheRowsFail(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	file := writeClaudePlanSeries(t, town, fixedClaudePlanNow)
	rows := func(string, time.Time, int) ([]beads.Sample, error) {
		return nil, errors.New("bd sample list: connection refused")
	}

	var logged []string
	p := claudePlanRead(t, town, rows, &logged)()

	if p.WeeklyPct != 38 {
		t.Errorf("the meter reads %v%%, want the file's 38%%", p.WeeklyPct)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], file) || !strings.Contains(logged[0], "connection refused") {
		t.Errorf("the why-log = %q, want one line naming %s and the failed rows read", logged, file)
	}
}

// TestClaudePlanReadSkipsPartialRows: a row missing a reading is not a reading,
// so it never folds as a week at 0%; with a complete row beside it the rows
// still answer, and with none the file does. Either way the why-log names the
// rows it could not use, so a fallback over unusable rows is not the same line
// as a town with no rows (gt-u9dsir).
func TestClaudePlanReadSkipsPartialRows(t *testing.T) {
	t.Parallel()

	partial := claudePlanRow(fixedClaudePlanNow, 1, 99)
	partial.SevenDayPct = nil

	town := t.TempDir()
	writeClaudePlanSeries(t, town, fixedClaudePlanNow)
	var logged []string
	p := claudePlanRead(t, town, rowsOf(partial, claudePlanRow(fixedClaudePlanNow, 3, 7)), &logged)()
	if p.WeeklyPct != 7 || p.FiveHourPct != 3 {
		t.Errorf("the meter reads %v%% of the week and %v%% of five hours, want the complete row's 7 and 3", p.WeeklyPct, p.FiveHourPct)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], claudePlanSourceRows) {
		t.Fatalf("the why-log = %q, want one line naming %q", logged, claudePlanSourceRows)
	}
	if !strings.Contains(logged[0], "unusable claude_plan rows: 1") {
		t.Errorf("the why-log = %q, want the skipped partial row named beside the source", logged)
	}

	partialOnly := t.TempDir()
	file := writeClaudePlanSeries(t, partialOnly, fixedClaudePlanNow)
	logged = nil
	p = claudePlanRead(t, partialOnly, rowsOf(partial), &logged)()
	if p.WeeklyPct != 38 {
		t.Errorf("rows of one partial row gave %v%%, want the file's 38%%", p.WeeklyPct)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], file) {
		t.Fatalf("the why-log = %q, want one line naming %s", logged, file)
	}
	if !strings.Contains(logged[0], "unusable claude_plan rows: 1") {
		t.Errorf("the why-log = %q, want the fallback to name the row it could not use, not read as no rows", logged)
	}
}

// TestClaudePlanRowsRecordTheirHistoryInAFreshTown: a town whose rows are its
// only series has no .runtime/claude-plan yet, and the closeout of a window
// that rolled over is written there anyway, so the month rollup keeps its
// history without a state error on the page.
func TestClaudePlanRowsRecordTheirHistoryInAFreshTown(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	rows := []beads.Sample{
		claudePlanRow(fixedClaudePlanNow, 8, 38),
		claudePlanRowAt(fixedClaudePlanNow.Add(-30*time.Hour), fixedClaudePlanNow.Add(-time.Hour), 5, 20),
	}
	p := claudePlanRead(t, town, rowsOf(rows...), nil)()

	if p.StateError != "" {
		t.Errorf("the rows' first poll reports %q, want no state error", p.StateError)
	}
	if _, err := os.Stat(filepath.Join(town, ".runtime", "claude-plan", "weeks.jsonl")); err != nil {
		t.Errorf("the closed window was not recorded: %v", err)
	}
}

// TestClaudePlanReadFollowsTheRowsAppearing: the source is chosen on every
// poll, so the first row the status line writes is read at the next poll and
// the why-log records the move once. A source resolved at build would keep the
// file the hub started on.
func TestClaudePlanReadFollowsTheRowsAppearing(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeClaudePlanSeries(t, town, fixedClaudePlanNow)

	var rows []beads.Sample
	var logged []string
	read := claudePlanRead(t, town, func(string, time.Time, int) ([]beads.Sample, error) { return rows, nil }, &logged)

	if p := read(); p.WeeklyPct != 38 {
		t.Fatalf("before any row exists the meter reads %v%%, want the file's 38%%", p.WeeklyPct)
	}
	rows = []beads.Sample{claudePlanRow(fixedClaudePlanNow, 3, 7)}
	if p := read(); p.WeeklyPct != 7 {
		t.Errorf("after the first row the meter reads %v%%, want the rows' 7%%", p.WeeklyPct)
	}
	read()
	if len(logged) != 2 || !strings.Contains(logged[1], claudePlanSourceRows) {
		t.Errorf("the why-log = %q, want the file named while no row existed, then one line for the rows", logged)
	}
}

// TestDashboardClaudePlanWiring checks that the plan meter's flag defaults to the
// price the status line's percentages are drawn against, and that a reader over
// the town's fallback series reads that price's reading from the file.
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
	return writeClaudePlanSampleAt(t, dashboardClaudePlanPath(root), now, 38)
}

// writeClaudePlanSampleAt writes one status line sample at path, with the weekly
// percentage sevenDay, the way the status line appends it, and returns path.
func writeClaudePlanSampleAt(t *testing.T, path string, now time.Time, sevenDay int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"t":%d,"five_hour":8,"five_hour_resets_at":%d,"seven_day":%d,"seven_day_resets_at":%d}`,
		now.Unix(), now.Add(time.Hour).Unix(), sevenDay, now.Add(claudePlanSeriesResets).Unix())
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
	// Mid-month, so the record an hour back stays inside the UTC month rollup (gt-lllti).
	now := time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)
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

	p := newDashboardClaudePlanReader(dashboardClaudePlanPath(root), newDashUsageReader(root), dashboardClaudePlanUSD).Read(now)

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
	unread := newDashboardClaudePlanReader(dashboardClaudePlanPath(noRecords), newDashUsageReader(noRecords), dashboardClaudePlanUSD).Read(now)
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
	empty := newDashboardClaudePlanReader(dashboardClaudePlanPath(emptyRoot), newDashUsageReader(emptyRoot), dashboardClaudePlanUSD).Read(now)
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
