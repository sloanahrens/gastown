package metrics

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These cases are the Python tests' cases: TestRenderReport* are
// scripts/test_town_metrics_render.py, TestMovedSinceLast* are the moved cases
// of scripts/test_town_metrics_weekly.py, and TestReport*/TestPreviousWeekly*
// are the report cases of scripts/test_town_metrics_cli.py. The two report
// cases the load_result port already carries stay there.

// reportFixtureRow is one row of the golden's fixture: every column present, a
// value only where set gives one, as the Python's row() leaves the rest None.
func reportFixtureRow(week, rig string, thin bool, set map[string]float64) ReportRow {
	r := ReportRow{Week: week, Rig: rig, Thin: thin, Values: make(map[string]*float64, len(WeeklyColumns))}
	for _, column := range WeeklyColumns {
		switch column {
		case "week", "rig", "thin", "dominant_model":
			continue
		}
		r.Values[column] = nil
	}
	for column, v := range set {
		value := v
		r.Values[column] = &value
	}
	return r
}

// renderGoldenWeekly is the golden report's fixture weekly table: two town
// weeks, the second thin, and one per-rig row the town tables must leave out.
func renderGoldenWeekly() []ReportRow {
	return []ReportRow{
		reportFixtureRow("2026-10-05", "ALL", false, map[string]float64{
			"landings": 12, "unreadable": 0, "size_median": 40, "size_p90": 120,
			"file_entropy_mean": 1.2345, "package_entropy_mean": 0.5, "rejected_share": 0.1, "rho": 0.5,
		}),
		reportFixtureRow("2026-10-12", "ALL", true, map[string]float64{
			"landings": 8, "unreadable": 1, "size_median": 50, "size_p90": 130,
			"file_entropy_mean": 1.5, "package_entropy_mean": 0.75, "rejected_share": 0.3, "rho": 0.25,
			"mode_shift": 1.8,
		}),
		reportFixtureRow("2026-10-12", "gastown", false, map[string]float64{"size_median": 999}),
	}
}

// renderGoldenFlags is the golden report's one flag.
func renderGoldenFlags() []MovedFlag {
	return []MovedFlag{{Rig: "ALL", Week: "2026-10-12", Column: "size_median", Value: 50, Mean: 40, SD: 2}}
}

// renderGoldenFooter is the golden report's data-quality footer.
func renderGoldenFooter() Footer {
	return Footer{
		RowsRead:          map[string]int{"reviews": 5, "ledger": 3},
		RowsDropped:       [][]string{{"ledger:gastown", "unparseable json"}},
		SourcesUnreadable: []string{"bd:/x"},
	}
}

// renderGolden renders the golden's fixture; a case replaces the part it is
// about.
func renderGolden(t *testing.T, weekly []ReportRow, flags []MovedFlag, footer Footer, backtest string) string {
	t.Helper()
	text, err := RenderReport("2026-10-23", weekly, flags, footer, reportNotRun,
		[]RegimeEvent{{Date: "2026-10-10", What: "flash to Sonnet"}}, backtest)
	if err != nil {
		t.Fatalf("RenderReport: %v", err)
	}
	return text
}

// firstLineDifference names the first line where got and want differ, so a
// golden mismatch points at the line instead of repeating the whole report.
func firstLineDifference(got, want string) string {
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < min(len(gotLines), len(wantLines)); i++ {
		if gotLines[i] != wantLines[i] {
			return fmt.Sprintf("line %d: got %q, want %q", i+1, gotLines[i], wantLines[i])
		}
	}
	return fmt.Sprintf("%d lines, want %d", len(gotLines), len(wantLines))
}

// TestRenderReportMatchesTheGolden is test_matches_golden: the digest of the
// fixture is byte for byte the hand-written golden.
func TestRenderReportMatchesTheGolden(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "testdata", "town_metrics", "report_golden.md"))
	if err != nil {
		t.Fatalf("read the golden: %v", err)
	}
	got := renderGolden(t, renderGoldenWeekly(), renderGoldenFlags(), renderGoldenFooter(), reportNotRun)
	if got != string(data) {
		t.Errorf("report differs from the golden at %s", firstLineDifference(got, string(data)))
	}
}

// TestRenderReportPrintsEachMetricHeadingOnce is
// test_every_metric_heading_appears_once.
func TestRenderReportPrintsEachMetricHeadingOnce(t *testing.T) {
	t.Parallel()
	text := renderGolden(t, renderGoldenWeekly(), renderGoldenFlags(), renderGoldenFooter(), reportNotRun)
	for n := 1; n <= len(metricSections); n++ {
		heading := fmt.Sprintf("## Metric %d:", n)
		if got := strings.Count(text, heading); got != 1 {
			t.Errorf("%q appears %d times, want 1", heading, got)
		}
	}
}

// TestRenderReportQuietRunAndCleanFooter is test_quiet_run_and_clean_footer.
func TestRenderReportQuietRunAndCleanFooter(t *testing.T) {
	t.Parallel()
	clean := Footer{RowsRead: map[string]int{}, RowsDropped: [][]string{}, SourcesUnreadable: []string{}}
	text := renderGolden(t, renderGoldenWeekly(), nil, clean, reportNotRun)
	for _, want := range []string{
		"Nothing moved more than two standard deviations.",
		"Rows dropped: 0",
		"Sources unreadable: none",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
}

// TestRenderReportBacktestReplacesTheDefault is
// test_backtest_text_replaces_the_default.
func TestRenderReportBacktestReplacesTheDefault(t *testing.T) {
	t.Parallel()
	text := renderGolden(t, renderGoldenWeekly(), renderGoldenFlags(), renderGoldenFooter(),
		"| Metric | verdict |\n| rho | keep |")
	if !strings.Contains(text, "## Usefulness backtest\n\n| Metric | verdict |") {
		t.Errorf("report does not embed the backtest section")
	}
	if strings.Contains(text, "## Usefulness backtest\n\nNot run.") {
		t.Errorf("report still prints the default backtest text")
	}
}

// TestRenderReportListsGapsApartFromDroppedRows is
// test_other_gaps_are_listed_apart_from_dropped_rows.
func TestRenderReportListsGapsApartFromDroppedRows(t *testing.T) {
	t.Parallel()
	footer := renderGoldenFooter()
	footer.Gaps = [][]string{{"git:gastown", "unreadable diff for gt-1"}}
	text := renderGolden(t, renderGoldenWeekly(), renderGoldenFlags(), footer, reportNotRun)
	for _, want := range []string{
		"Other gaps: 1\n- git:gastown: unreadable diff for gt-1",
		"Rows dropped: 1\n- ledger:gastown: unparseable json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
}

// TestRenderReportPrintsTheDroppedReason is test_dropped_reason_is_printed.
func TestRenderReportPrintsTheDroppedReason(t *testing.T) {
	t.Parallel()
	text := renderGolden(t, renderGoldenWeekly(), renderGoldenFlags(), renderGoldenFooter(), reportNotRun)
	if want := "ledger:gastown: unparseable json"; !strings.Contains(text, want) {
		t.Errorf("report does not contain %q", want)
	}
}

// TestRenderReportRefusesAColumnItsRowsLack drives the refusal the Python's
// KeyError gives: a row without a column the digest prints stops the report
// instead of printing a table with a hole in it.
func TestRenderReportRefusesAColumnItsRowsLack(t *testing.T) {
	t.Parallel()
	weekly := renderGoldenWeekly()
	delete(weekly[0].Values, "size_median")
	_, err := RenderReport("2026-10-23", weekly, renderGoldenFlags(), renderGoldenFooter(), reportNotRun,
		nil, reportNotRun)
	if err == nil {
		t.Fatal("RenderReport accepted a row with no size_median column")
	}
	if !strings.Contains(err.Error(), "size_median") {
		t.Errorf("error does not name the column: %v", err)
	}
}

// TestRenderReportRefusesASectionColumnItsRowsLack drives the metric tables'
// lookup: a row without a column a section prints stops the report.
func TestRenderReportRefusesASectionColumnItsRowsLack(t *testing.T) {
	t.Parallel()
	weekly := renderGoldenWeekly()
	for i := range weekly {
		delete(weekly[i].Values, "landings")
	}
	_, err := RenderReport("2026-10-23", weekly, renderGoldenFlags(), renderGoldenFooter(), reportNotRun,
		nil, reportNotRun)
	if err == nil {
		t.Fatal("RenderReport accepted a row with no landings column")
	}
	if !strings.Contains(err.Error(), "landings") {
		t.Errorf("error does not name the column: %v", err)
	}
}

// TestRenderReportRefusesAColumnOnlyTheEarlierWeeksLack drives the comparison's
// second window: eight weeks split into the last four and the four before, with
// the missing column in a week the headline reads as history.
func TestRenderReportRefusesAColumnOnlyTheEarlierWeeksLack(t *testing.T) {
	t.Parallel()
	weekly := make([]ReportRow, 8)
	for i := range weekly {
		weekly[i] = reportFixtureRow(fmt.Sprintf("2026-09-%02d", i+1), "ALL", false,
			map[string]float64{"size_median": float64(10 + i)})
	}
	delete(weekly[0].Values, "size_median")
	_, err := RenderReport("2026-10-23", weekly, nil, renderGoldenFooter(), reportNotRun, nil, reportNotRun)
	if err == nil {
		t.Fatal("RenderReport accepted a week the headline reads without a size_median column")
	}
}

// TestRenderReportRefusesAMalformedFooterEntry stops a dropped row or a gap
// that is not a (source, reason) pair, which the Python's % formatting fails on.
func TestRenderReportRefusesAMalformedFooterEntry(t *testing.T) {
	t.Parallel()
	for name, damage := range map[string]func(*Footer){
		"dropped row": func(f *Footer) { f.RowsDropped = [][]string{{"ledger:gastown"}} },
		"gap":         func(f *Footer) { f.Gaps = [][]string{{"git:gastown"}} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			footer := renderGoldenFooter()
			damage(&footer)
			if _, err := RenderReport("2026-10-23", renderGoldenWeekly(), renderGoldenFlags(), footer,
				reportNotRun, nil, reportNotRun); err == nil {
				t.Fatalf("RenderReport accepted a malformed %s", name)
			}
		})
	}
}

// TestRenderReportFormatsACountWithoutADecimalPoint pins the digest's two
// number formats: a count prints as itself, and handoff_bytes_total prints as a
// float because the Python's typed rows leave it one.
func TestRenderReportFormatsACountWithoutADecimalPoint(t *testing.T) {
	t.Parallel()
	weekly := renderGoldenWeekly()
	weekly[0].Values["landings"] = floatPtr(1234)
	weekly[0].Values["handoff_bytes_total"] = floatPtr(1234)
	text := renderGolden(t, weekly, renderGoldenFlags(), renderGoldenFooter(), reportNotRun)
	if want := "| 2026-10-05 | 1234 | 0 |"; !strings.Contains(text, want) {
		t.Errorf("the landing-size table does not print the count as itself")
	}
	if want := "| 2026-10-05 | 1.23e+03 |"; !strings.Contains(text, want) {
		t.Errorf("the handoff table does not print the total as a three-digit float")
	}
}

// movedRow is one row of the moved fixture: the column under test and nothing
// else, as the Python's W() builds it.
func movedRow(week string, sizeMedian *float64, thin bool) ReportRow {
	return ReportRow{Week: week, Rig: "ALL", Thin: thin, Values: map[string]*float64{"size_median": sizeMedian}}
}

// movedHistory is the eight-week history the moved cases compare against.
func movedHistory() []ReportRow {
	values := []float64{10, 11, 9, 10, 11, 9, 10, 10}
	rows := make([]ReportRow, len(values))
	for i, v := range values {
		rows[i] = movedRow(fmt.Sprintf("w%d", i), floatPtr(v), false)
	}
	return rows
}

// movedCur is the history with one newest week appended.
func movedCur(history []ReportRow, value float64, thin bool) []ReportRow {
	return append(append([]ReportRow{}, history...), movedRow("w9", floatPtr(value), thin))
}

// TestMovedSinceLastFlagsATwoSigmaMove is test_flags_two_sigma_move.
func TestMovedSinceLastFlagsATwoSigmaMove(t *testing.T) {
	t.Parallel()
	history := movedHistory()
	flags := MovedSinceLast(history, movedCur(history, 30, false), []string{"size_median"})
	if len(flags) != 1 {
		t.Fatalf("flags = %v, want one flag", flags)
	}
	got := flags[0]
	if got.Week != "w9" || got.Column != "size_median" || got.Rig != "ALL" {
		t.Errorf("flag = %+v, want the w9 size_median move", got)
	}
	wantSD := math.Sqrt(0.5)
	if math.Abs(got.Mean-10) > 1e-9 || math.Abs(got.SD-wantSD) > 1e-9 || got.Value != 30 {
		t.Errorf("flag = %+v, want value 30 at mean 10 sd %v", got, wantSD)
	}
}

// TestMovedSinceLastQuietWeekDoesNotFlag is test_quiet_week_does_not_flag.
func TestMovedSinceLastQuietWeekDoesNotFlag(t *testing.T) {
	t.Parallel()
	history := movedHistory()
	flags := MovedSinceLast(history, movedCur(history, 10.5, false), []string{"size_median"})
	if len(flags) != 0 {
		t.Errorf("flags = %v, want none", flags)
	}
}

// TestMovedSinceLastThinLatestNeverFlags is test_thin_latest_never_flags.
func TestMovedSinceLastThinLatestNeverFlags(t *testing.T) {
	t.Parallel()
	history := movedHistory()
	flags := MovedSinceLast(history, movedCur(history, 30, true), []string{"size_median"})
	if len(flags) != 0 {
		t.Errorf("flags = %v, want none for a thin latest week", flags)
	}
}

// TestMovedSinceLastShortHistoryNeverFlags is test_short_history_never_flags.
func TestMovedSinceLastShortHistoryNeverFlags(t *testing.T) {
	t.Parallel()
	short := movedHistory()[:4]
	flags := MovedSinceLast(short, movedCur(short, 30, false), []string{"size_median"})
	if len(flags) != 0 {
		t.Errorf("flags = %v, want none for four weeks of history", flags)
	}
}

// TestMovedSinceLastFlagsAMoveBetweenTwoAndThreeSigma pins the threshold the
// function's name promises: 12 is two and a bit standard deviations from a
// history whose sd is 0.707, so a three-sigma rule would miss it.
func TestMovedSinceLastFlagsAMoveBetweenTwoAndThreeSigma(t *testing.T) {
	t.Parallel()
	history := movedHistory()
	flags := MovedSinceLast(history, movedCur(history, 12, false), []string{"size_median"})
	if len(flags) != 1 || flags[0].Value != 12 {
		t.Errorf("flags = %v, want the w9 size_median move to 12", flags)
	}
}

// TestMovedSinceLastConstantHistoryNeverFlags drives the sd > 0 guard: a flat
// history gives every newest week an infinite z-score, so the comparison must
// not divide by a zero spread.
func TestMovedSinceLastConstantHistoryNeverFlags(t *testing.T) {
	t.Parallel()
	history := make([]ReportRow, 8)
	for i := range history {
		history[i] = movedRow(fmt.Sprintf("w%d", i), floatPtr(10), false)
	}
	flags := MovedSinceLast(history, movedCur(history, 30, false), []string{"size_median"})
	if len(flags) != 0 {
		t.Errorf("flags = %v, want none against a flat history", flags)
	}
}

// TestReportColumnsAreWeeklyColumns is the guard on the report's own column
// tables: a column that is not a weekly column would be absent from every row,
// so the headline would print n/a and the comparison would report a quiet run.
func TestReportColumnsAreWeeklyColumns(t *testing.T) {
	t.Parallel()
	columns := append([]string{}, headlineColumns...)
	columns = append(columns, flaggedColumns...)
	for _, section := range metricSections {
		columns = append(columns, section.columns...)
	}
	for _, column := range columns {
		if !slices.Contains(WeeklyColumns, column) {
			t.Errorf("%q is in the report's column tables but not in WeeklyColumns", column)
		}
	}
}

// reportTown is an in-memory town for the report's reads and its one write, so
// no case touches the host.
type reportTown struct {
	files    map[string]string
	readErrs map[string]error
	dirs     map[string][]DirEntry
	writes   map[string]string
	made     []string
}

// newReportTown is an empty town.
func newReportTown() *reportTown {
	return &reportTown{
		files:    map[string]string{},
		readErrs: map[string]error{},
		dirs:     map[string][]DirEntry{},
		writes:   map[string]string{},
	}
}

func (r *reportTown) readFile(path string) ([]byte, error) {
	if err, ok := r.readErrs[path]; ok {
		return nil, err
	}
	data, ok := r.files[path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return []byte(data), nil
}

func (r *reportTown) readDir(path string) ([]DirEntry, error) {
	entries, ok := r.dirs[path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return entries, nil
}

func (r *reportTown) writeFile(path string, data []byte) error {
	r.writes[path] = string(data)
	return nil
}

func (r *reportTown) mkdirAll(dir string) error {
	r.made = append(r.made, dir)
	return nil
}

func (r *reportTown) env() Env {
	return Env{ReadFile: r.readFile, ReadDir: r.readDir}
}

const (
	reportOutDir  = "town/.runtime/metrics/2026-10-23"
	reportParent  = "town/.runtime/metrics"
	reportDocsDir = "town/docs/metrics"
)

// reportCase is the report's fixture: one town week to publish, and an output
// directory with no earlier run beside it.
func reportCase() (*reportTown, ReportInput) {
	town := newReportTown()
	town.dirs[reportParent] = []DirEntry{{Name: "2026-10-23", IsDir: true}}
	return town, ReportInput{
		OutDir:    reportOutDir,
		DocsDir:   reportDocsDir,
		Today:     "2026-10-23",
		Weekly:    []WeeklyMetrics{{Week: "2026-10-12", Rig: "ALL", Landings: 2, SizeMedian: floatPtr(10), Rho: floatPtr(2)}},
		Footer:    Footer{RowsRead: map[string]int{"ledger": 2}, SourcesUnreadable: []string{}},
		MkdirAll:  town.mkdirAll,
		WriteFile: town.writeFile,
	}
}

// TestReportIsWrittenUnderDocsDir is test_report_is_written_under_docs_dir.
func TestReportIsWrittenUnderDocsDir(t *testing.T) {
	t.Parallel()
	town, in := reportCase()
	path, err := Report(town.env(), in)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	want := filepath.Join(reportDocsDir, "2026-10-23-town-metrics.md")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	text := town.writes[path]
	if !strings.HasPrefix(text, "# Town metrics 2026-10-23\n") {
		t.Errorf("digest does not start with its title:\n%s", text)
	}
	if !strings.Contains(text, "Rows dropped: 0") {
		t.Errorf("digest does not report its clean footer")
	}
	if len(town.made) != 1 || town.made[0] != reportDocsDir {
		t.Errorf("made %v, want [%s]", town.made, reportDocsDir)
	}
}

// TestReportEmbedsTheMangoSection is
// test_report_embeds_the_mango_section_when_one_was_written.
func TestReportEmbedsTheMangoSection(t *testing.T) {
	t.Parallel()
	town, in := reportCase()
	town.files[filepath.Join(reportOutDir, reportMangoName)] = "### rejected_before_landing\n\nNo boxes: marker-text\n"
	path, err := Report(town.env(), in)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if text := town.writes[path]; !strings.Contains(text, "No boxes: marker-text") {
		t.Errorf("digest does not embed the mango section")
	}
}

// TestReportSaysNotRunWithoutASection is
// test_report_says_not_run_without_a_section.
func TestReportSaysNotRunWithoutASection(t *testing.T) {
	t.Parallel()
	town, in := reportCase()
	path, err := Report(town.env(), in)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if text := town.writes[path]; !strings.Contains(text, "## Mango findings\n\nNot run.") {
		t.Errorf("digest does not say the mango section was not run")
	}
}

// TestReportEmbedsTheBacktestSection is the report half of
// test_backtest_section_is_written_and_embedded_in_the_report: the section an
// earlier step wrote is embedded, and the regime events of the docs directory
// reach the digest.
func TestReportEmbedsTheBacktestSection(t *testing.T) {
	t.Parallel()
	town, in := reportCase()
	section, err := BacktestSection(weeklyRows("size_median", 10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1),
		[]RegimeEvent{{Date: "2026-10-14", What: "gate raised"}}, nil, nil)
	if err != nil {
		t.Fatalf("BacktestSection: %v", err)
	}
	town.files[filepath.Join(reportOutDir, reportBacktestName)] = section + "\n"
	town.files[filepath.Join(reportDocsDir, "regime-events.json")] = `[{"date": "2026-10-14", "what": "gate raised", "confirmed": false}]`
	path, err := Report(town.env(), in)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	text := town.writes[path]
	if !strings.Contains(text, "## Usefulness backtest\n\nEvents (from") {
		t.Errorf("digest does not embed the backtest section")
	}
	if !strings.Contains(text, "## Regime events\n\n- 2026-10-14: gate raised") {
		t.Errorf("digest does not list the regime events")
	}
}

// reportCSV is the fixture run's weekly.csv as its header line and its one data
// line.
func reportCSV(t *testing.T) (header, line string) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteCSV(&buf, []Row{loadResultRow(t, loadResultWeekly())}, WeeklyColumns); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("weekly.csv has %d lines, want a header and one record", len(lines))
	}
	return lines[0], strings.TrimSuffix(lines[1], "\r")
}

// reportRun writes a run's weekly.csv in dir and returns dir.
func reportRun(t *testing.T, town *reportTown, dir string) string {
	t.Helper()
	header, line := reportCSV(t)
	town.files[filepath.Join(dir, weeklyName)] = header + "\n" + line + "\n"
	return dir
}

// replaceCell returns a CSV record with its index-th cell replaced by value.
func replaceCell(cells []string, index int, value string) string {
	out := append([]string{}, cells...)
	out[index] = value
	return strings.Join(out, ",")
}

// TestPreviousRunIsTheNewestEarlierDirectory is
// test_previous_run_is_the_newest_earlier_directory.
func TestPreviousRunIsTheNewestEarlierDirectory(t *testing.T) {
	t.Parallel()
	town, in := reportCase()
	first := in
	first.OutDir = reportRun(t, town, filepath.Join(reportParent, "2026-10-16"))
	town.dirs[reportParent] = []DirEntry{
		{Name: "2026-10-16", IsDir: true},
		{Name: "2026-10-23", IsDir: true},
	}

	earlier, err := PreviousWeekly(town.env(), in.OutDir)
	if err != nil {
		t.Fatalf("PreviousWeekly: %v", err)
	}
	if len(earlier) == 0 {
		t.Errorf("PreviousWeekly(%s) found no earlier run", in.OutDir)
	}
	none, err := PreviousWeekly(town.env(), first.OutDir)
	if err != nil {
		t.Fatalf("PreviousWeekly: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("PreviousWeekly(%s) = %v, want none", first.OutDir, none)
	}
}

// TestPreviousWeeklySkipsARunWithoutAWeeklyCSV drives the newest-first walk: a
// directory with no weekly.csv is not a run, so the comparison reaches the one
// before it.
func TestPreviousWeeklySkipsARunWithoutAWeeklyCSV(t *testing.T) {
	t.Parallel()
	town, _ := reportCase()
	reportRun(t, town, filepath.Join(reportParent, "2026-10-16"))
	town.dirs[reportParent] = []DirEntry{
		{Name: "2026-10-16", IsDir: true},
		{Name: "2026-10-19", IsDir: true},
		{Name: "2026-10-23", IsDir: true},
	}
	weekly, err := PreviousWeekly(town.env(), reportOutDir)
	if err != nil {
		t.Fatalf("PreviousWeekly: %v", err)
	}
	if len(weekly) == 0 {
		t.Fatalf("PreviousWeekly skipped past the run that has a weekly.csv")
	}
	if want := "2026-10-12"; weekly[0].Week != want {
		t.Errorf("previous run's week = %q, want %q", weekly[0].Week, want)
	}
}

// TestPreviousWeeklyRefusesAnUnreadableWeeklyCSV keeps a run that cannot be read
// from reading as no earlier run at all.
func TestPreviousWeeklyRefusesAnUnreadableWeeklyCSV(t *testing.T) {
	t.Parallel()
	town, _ := reportCase()
	run := filepath.Join(reportParent, "2026-10-16")
	town.readErrs[filepath.Join(run, weeklyName)] = errors.New("permission denied")
	town.dirs[reportParent] = []DirEntry{
		{Name: "2026-10-16", IsDir: true},
		{Name: "2026-10-23", IsDir: true},
	}
	if _, err := PreviousWeekly(town.env(), reportOutDir); err == nil {
		t.Fatal("PreviousWeekly accepted an unreadable weekly.csv")
	}
}

// TestPreviousWeeklyRefusesAnUnsetSeam: the comparison lists a directory and
// reads a file, so a caller that wires neither is refused rather than answered
// with no earlier run.
func TestPreviousWeeklyRefusesAnUnsetSeam(t *testing.T) {
	t.Parallel()
	town, _ := reportCase()
	for name, env := range map[string]Env{
		"no read file": {ReadDir: town.readDir},
		"no read dir":  {ReadFile: town.readFile},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := PreviousWeekly(env, reportOutDir); err == nil {
				t.Fatalf("PreviousWeekly accepted an env with %s", name)
			}
		})
	}
}

// TestPreviousWeeklyRefusesAnUnlistableDirectory keeps a parent directory the
// caller cannot list from reading as no earlier run.
func TestPreviousWeeklyRefusesAnUnlistableDirectory(t *testing.T) {
	t.Parallel()
	if _, err := PreviousWeekly(newReportTown().env(), filepath.Join("elsewhere", "2026-10-23")); err == nil {
		t.Fatal("PreviousWeekly accepted a run whose parent cannot be listed")
	}
}

// TestPreviousWeeklyRefusesADamagedWeeklyCSV drives the two reads of an earlier
// run's table: a record whose width differs from its header, and a cell the
// column cannot hold.
func TestPreviousWeeklyRefusesADamagedWeeklyCSV(t *testing.T) {
	t.Parallel()
	header, line := reportCSV(t)
	cells := strings.Split(line, ",")
	index := slices.Index(WeeklyColumns, "size_median")
	if index < 0 {
		t.Fatalf("WeeklyColumns has no size_median column")
	}
	for name, record := range map[string]string{
		"a record narrower than its header": strings.Join(cells[:3], ","),
		"a cell the column cannot hold":     replaceCell(cells, index, "abc"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			town, _ := reportCase()
			town.files[filepath.Join(reportParent, "2026-10-16", weeklyName)] = header + "\n" + record + "\n"
			town.dirs[reportParent] = []DirEntry{
				{Name: "2026-10-16", IsDir: true},
				{Name: "2026-10-23", IsDir: true},
			}
			if _, err := PreviousWeekly(town.env(), reportOutDir); err == nil {
				t.Fatalf("PreviousWeekly accepted %s", name)
			}
		})
	}
}

// TestReportRefusesADamagedInput drives the report's refusals: an unreadable or
// unparseable input and a seam the caller left unset must stop the digest
// rather than publish one that silently lacks what it could not read.
func TestReportRefusesADamagedInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		damage func(*reportTown, *ReportInput)
		env    func(*reportTown) Env
	}{
		{
			name: "unreadable mango section",
			damage: func(town *reportTown, _ *ReportInput) {
				town.readErrs[filepath.Join(reportOutDir, reportMangoName)] = errors.New("permission denied")
			},
		},
		{
			name: "unreadable backtest section",
			damage: func(town *reportTown, _ *ReportInput) {
				town.readErrs[filepath.Join(reportOutDir, reportBacktestName)] = errors.New("permission denied")
			},
		},
		{
			name: "unreadable regime events",
			damage: func(town *reportTown, in *ReportInput) {
				town.readErrs[filepath.Join(in.DocsDir, "regime-events.json")] = errors.New("permission denied")
			},
		},
		{
			name: "regime events that do not parse",
			damage: func(town *reportTown, in *ReportInput) {
				town.files[filepath.Join(in.DocsDir, "regime-events.json")] = "{not json"
			},
		},
		{
			name: "no earlier-run directory to list",
			damage: func(town *reportTown, _ *ReportInput) {
				delete(town.dirs, reportParent)
			},
		},
		{
			name: "a footer the digest cannot print",
			damage: func(_ *reportTown, in *ReportInput) {
				in.Footer.RowsDropped = [][]string{{"ledger:gastown"}}
			},
		},
		{
			name: "a directory the report cannot make",
			damage: func(_ *reportTown, in *ReportInput) {
				in.MkdirAll = func(string) error { return errors.New("read-only file system") }
			},
		},
		{
			name: "a digest the report cannot write",
			damage: func(_ *reportTown, in *ReportInput) {
				in.WriteFile = func(string, []byte) error { return errors.New("no space left") }
			},
		},
		{
			name: "no read seam",
			env:  func(*reportTown) Env { return Env{} },
		},
		{
			name:   "no output directory",
			damage: func(_ *reportTown, in *ReportInput) { in.OutDir = "" },
		},
		{
			name:   "no docs directory",
			damage: func(_ *reportTown, in *ReportInput) { in.DocsDir = "" },
		},
		{
			name:   "no day to report",
			damage: func(_ *reportTown, in *ReportInput) { in.Today = "" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, in := reportCase()
			if tc.damage != nil {
				tc.damage(town, &in)
			}
			env := town.env()
			if tc.env != nil {
				env = tc.env(town)
			}
			if _, err := Report(env, in); err == nil {
				t.Fatalf("Report accepted %s", tc.name)
			}
			if len(town.writes) != 0 {
				t.Errorf("Report wrote %v after refusing %s", town.writes, tc.name)
			}
		})
	}
}

// TestReportRowCarriesEveryWeeklyColumn is reportRows' guard: a weekly column
// the report can print must reach the row it renders, or the digest prints n/a
// for a figure the extract published.
func TestReportRowCarriesEveryWeeklyColumn(t *testing.T) {
	t.Parallel()
	m := WeeklyMetrics{
		Week: "2026-10-12", Rig: "ALL",
		Landings: 1, Unreadable: 2, ManualRescueCount: 3, HandoffBytesTotal: 4,
		RhoNumerator: 5, RhoDenominator: 6, Rejections: 7,
		SizeMedian: floatPtr(8), SizeP90: floatPtr(9), FileEntropyMean: floatPtr(10),
		PackageEntropyMean: floatPtr(11), AttemptsMean: floatPtr(12), RejectedShare: floatPtr(13),
		FirstTryShare: floatPtr(14), HLabels: floatPtr(15), HRigs: floatPtr(16), HTypes: floatPtr(17),
		HandoffBytesMedian: floatPtr(18), SessionsMedian: floatPtr(19), Rho: floatPtr(20),
		DependencyDensity: floatPtr(21), ConflictGateShare: floatPtr(22), UnknownShare: floatPtr(23),
		ModeShift: floatPtr(24), OMThreshold: floatPtr(25),
	}
	rows := reportRows([]WeeklyMetrics{m})
	if len(rows) != 1 {
		t.Fatalf("reportRows returned %d rows, want 1", len(rows))
	}
	row := rows[0]
	want := map[string]float64{
		"landings": 1, "unreadable": 2, "manual_rescue_count": 3, "handoff_bytes_total": 4,
		"rho_numerator": 5, "rho_denominator": 6, "rejections": 7,
		"size_median": 8, "size_p90": 9, "file_entropy_mean": 10, "package_entropy_mean": 11,
		"attempts_mean": 12, "rejected_share": 13, "first_try_share": 14, "h_labels": 15,
		"h_rigs": 16, "h_types": 17, "handoff_bytes_median": 18, "sessions_median": 19,
		"rho": 20, "dependency_density": 21, "conflict_gate_share": 22, "unknown_share": 23,
		"mode_shift": 24, "om_threshold": 25,
	}
	for _, column := range WeeklyColumns {
		switch column {
		case "week", "rig", "thin", "dominant_model":
			continue
		}
		value, wanted := want[column]
		if !wanted {
			t.Errorf("the fixture has no value for the weekly column %q", column)
			continue
		}
		got, ok := row.Values[column]
		if !ok || got == nil {
			t.Errorf("report row does not carry %q", column)
			continue
		}
		if *got != value {
			t.Errorf("report row %q = %v, want %v", column, *got, value)
		}
	}
	if len(row.Values) != len(want) {
		t.Errorf("report row carries %d columns, want %d", len(row.Values), len(want))
	}
	if row.Week != m.Week || row.Rig != m.Rig || row.Thin != m.Thin {
		t.Errorf("report row lost its week, rig or thin flag: %+v", row)
	}
}
