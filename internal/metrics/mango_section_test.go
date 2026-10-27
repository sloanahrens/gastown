package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mangoSectionTol is the tolerance the explain means are compared at.
const mangoSectionTol = 1e-9

// mangoSectionColumns is the fixture's landing header: the columns the section
// drops (identity, duplicate, the leak and the targets) plus the predictors
// and explain columns the cases read. The list is written out here rather than
// taken from landingColumns so a change to the production header cannot
// silently change what these cases cover.
var mangoSectionColumns = []string{
	"bead", "landed_at", "week", "model", "route",
	"rejected_before_landing", "manual_rescue", "reached_overrun_stop",
	"rig", "om_threshold", "dominant_model", "type", "priority", "shaped",
	"label_count", "lines_total", "files_touched", "file_entropy",
	"package_entropy", "attempts",
}

// mangoSectionResult is the canned explore result of the Python section tests:
// a search that reached 200 cubes and validated none.
const mangoSectionResult = `{
  "target": "t",
  "positive": 1,
  "baseline": 0.1,
  "rows": 28,
  "hypercubes": [],
  "stats": {
    "generate": {"iterations": 10, "germsTried": 200, "generated": 200, "valid": 0,
                 "significant": 0, "skippedColumns": []},
    "candidates": 200
  }
}`

// mangoSectionLanding is one landing as the extract writes it: every column of
// the fixture header present, and the target cells the section reads filled
// in.
func mangoSectionLanding(i, rej int, at, route string) Row {
	row := make(Row, len(mangoSectionColumns))
	for _, column := range mangoSectionColumns {
		row[column] = ""
	}
	row["bead"] = fmt.Sprintf("b%d", i)
	row["rig"] = "a"
	row["landed_at"] = at
	row["route"] = route
	row["rejected_before_landing"] = strconv.Itoa(rej)
	if route != "daemon" {
		row["manual_rescue"] = "1"
	} else {
		row["manual_rescue"] = "0"
	}
	row["reached_overrun_stop"] = "0"
	row["lines_total"] = "10"
	row["om_threshold"] = "0.7"
	row["dominant_model"] = "m"
	row["type"] = "task"
	row["priority"] = "2"
	row["shaped"] = "1"
	row["label_count"] = strconv.Itoa(i % 5)
	return row
}

// mangoSectionRows is the Python section tests' forty landings: a rejected
// landing on every other bead, dated across October so the date split is not
// the input order.
func mangoSectionRows() []Row {
	rows := make([]Row, 40)
	for i := range rows {
		rows[i] = mangoSectionLanding(i, i%2, fmt.Sprintf("2026-10-%02dT00:00:00Z", 1+i%28), "daemon")
	}
	return rows
}

// mangoSectionTown writes rows and the sidecar into a temp directory, with a
// one-row weekly table beside them.
func mangoSectionTown(t *testing.T, rows []Row) string {
	t.Helper()
	dir := t.TempDir()
	var landings strings.Builder
	if err := WriteLandings(&landings, mangoSectionColumns, rows); err != nil {
		t.Fatal(err)
	}
	mangoWriteFile(t, filepath.Join(dir, "landings.csv"), landings.String())
	mangoWriteFile(t, filepath.Join(dir, "landings.sidecar.json"),
		`{"explain_only": ["attempts", "file_entropy", "files_touched", "gate_result", "lines_added",`+
			` "lines_deleted", "lines_total", "om_verdict", "package_entropy", "unreadable"]}`)
	mangoWriteFile(t, filepath.Join(dir, "weekly.csv"), "week,rig\r\nw,ALL\r\n")
	return dir
}

// mangoWriteFile writes body at path, creating the directories above it.
func mangoWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mangoSectionRun is a fake mango: profile echoes the weekly path it was
// given, explore writes result where the command asked, and calls records
// every argv.
func mangoSectionRun(t *testing.T, result string) (Runner, *[][]string) {
	t.Helper()
	calls := &[][]string{}
	run := func(_ context.Context, argv []string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), argv...))
		switch argv[1] {
		case "profile":
			return []byte("profile of " + argv[2] + "\n"), nil
		case "explore":
			return nil, os.WriteFile(mangoFlagValue(argv, "--out"), []byte(result), 0o644)
		}
		return nil, nil
	}
	return run, calls
}

// mangoFlagValue is the value argv gives flag, and "" when it names none.
func mangoFlagValue(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// mangoSection runs the section over dir with a fake mango and returns its
// text. The town is fresh with an empty review log, so the rejection tables it
// appends find no verdicts; the cases that read refusals pass their own town.
func mangoSection(t *testing.T, dir string, run Runner, minPositives int) string {
	t.Helper()
	text, err := MangoSection(context.Background(), Env{Town: rejectionTown(t), ReadFile: os.ReadFile, ReadDir: listDir, Run: run}, dir,
		"/opt/mango", MangoSectionOptions{MinPositives: minPositives})
	if err != nil {
		t.Fatalf("MangoSection: %v", err)
	}
	return text
}

// mangoSectionSummary reads the summary the section wrote.
func mangoSectionSummary(t *testing.T, dir string) map[string]MangoSummary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "mango-summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]MangoSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestMangoSectionWithoutABinaryNamesTheCSVDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// No seams at all: a host without mango must not need them to say so.
	text, err := MangoSection(context.Background(), Env{}, dir, "", MangoSectionOptions{})
	if err != nil {
		t.Fatalf("MangoSection: %v", err)
	}
	for _, want := range []string{"mango binary not found", dir} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to name %q", text, want)
		}
	}
}

func TestMangoSectionZeroValidBoxesIsNotNothingPredictive(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	run, _ := mangoSectionRun(t, mangoSectionResult)
	text := mangoSection(t, dir, run, 1)
	for _, want := range []string{
		"### rejected_before_landing",
		"200 cubes generated",
		"0 valid",
		"not evidence that nothing is predictive",
		"profile of", // the weekly profile is embedded
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
	summary := mangoSectionSummary(t, dir)
	want := MangoSummary{Boxes: 0, Replicating: 0, HoldoutPositives: 6}
	if got := summary["rejected_before_landing"]; got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if len(summary) != 1 {
		t.Errorf("summary = %v, want only the target that ran", summary)
	}
}

func TestMangoSectionProfileDoesNotEmbedTheLocalRunPath(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	run, _ := mangoSectionRun(t, mangoSectionResult)
	text := mangoSection(t, dir, run, 1)
	start := strings.Index(text, "### rejected_before_landing")
	if start < 0 {
		t.Fatalf("text = %q, want a rejected_before_landing part", text)
	}
	profile := text[:start]
	if strings.Contains(profile, dir) {
		t.Errorf("profile = %q, want the local run directory replaced", profile)
	}
	if want := "profile of <run>/weekly.csv"; !strings.Contains(text, want) {
		t.Errorf("text = %q, want it to hold %q", text, want)
	}
}

func TestMangoSectionSkipsATargetWithTooFewPositives(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	run, calls := mangoSectionRun(t, mangoSectionResult)
	text := mangoSection(t, dir, run, 30)
	for _, want := range []string{"reached_overrun_stop", "Skipped: fewer than 30 positives in train"} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
	if len(*calls) != 1 || (*calls)[0][1] != "profile" {
		t.Errorf("calls = %q, want only the profile when every target is skipped", *calls)
	}
	if summary := mangoSectionSummary(t, dir); len(summary) != 0 {
		t.Errorf("summary = %v, want no target in it", summary)
	}
}

func TestMangoSectionHoldoutWithoutPositivesSaysSo(t *testing.T) {
	t.Parallel()
	rows := make([]Row, 40)
	for i := range rows {
		route := "daemon"
		if i < 10 {
			route = "overseer-manual"
		}
		rows[i] = mangoSectionLanding(i, i%2, "", route)
		if i == 9 {
			rows[i]["reached_overrun_stop"] = "1" // the only positive, in train, on 2026-10-10
		}
		if i < 28 {
			rows[i]["landed_at"] = fmt.Sprintf("2026-10-%02dT00:00:00Z", 1+i)
		} else {
			rows[i]["landed_at"] = fmt.Sprintf("2026-11-%02dT00:00:00Z", i-27)
		}
	}
	// The positives all land in October, so the date split puts every one of
	// them in train and the holdout has none to check a box on.
	result := `{
  "target": "reached_overrun_stop", "positive": 1, "baseline": 0.1, "rows": 28,
  "hypercubes": [{"ranges": [{"column": "rig", "categories": ["a"]}], "n": 28, "purity": 0.4}],
  "stats": {"generate": {"iterations": 10, "germsTried": 200, "generated": 50, "valid": 1,
                         "significant": 1, "skippedColumns": []}, "candidates": 1}
}`
	dir := mangoSectionTown(t, rows)
	run, _ := mangoSectionRun(t, result)
	text := mangoSection(t, dir, run, 1)
	start, end := strings.Index(text, "### reached_overrun_stop"), len(text)
	if start < 0 || end <= start {
		t.Fatalf("text = %q, want a reached_overrun_stop part at the end", text)
	}
	part := text[start:end]
	for _, want := range []string{"Holdout has no positives", "Last positive landed 2026-10-10"} {
		if !strings.Contains(part, want) {
			t.Errorf("reached_overrun_stop part = %q, want it to hold %q", part, want)
		}
	}
	if strings.Contains(part, "| replicates |") {
		t.Errorf("reached_overrun_stop part = %q, want no table when there is nothing to check", part)
	}
}

func TestMangoSectionReportsAMangoFailure(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	run := func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("exit status 2: kaboom")
	}
	text := mangoSection(t, dir, run, 1)
	if want := "mango ingest failed: exit status 2: kaboom"; !strings.Contains(text, want) {
		t.Errorf("text = %q, want it to hold %q", text, want)
	}
	// The failed target leaves the section but not the summary: a target whose
	// search did not finish has no counts to report, and the ones that did are
	// still written.
	if summary := mangoSectionSummary(t, dir); len(summary) != 0 {
		t.Errorf("summary = %v, want no target whose search failed", summary)
	}
}

func TestMangoSectionReportsAProfileFailure(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	ok, _ := mangoSectionRun(t, mangoSectionResult)
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		if argv[1] == "profile" {
			return nil, errors.New("boom: unreadable " + dir + "/weekly.csv")
		}
		return ok(ctx, argv)
	}
	text := mangoSection(t, dir, run, 1)
	// The failure is quoted like the profile itself: the local run directory
	// reads as <run>, so the published report does not name the host.
	want := "mango profile failed: boom: unreadable <run>/weekly.csv"
	if !strings.Contains(text, want) {
		t.Errorf("text = %q, want it to hold %q", text, want)
	}
	// A failed profile is context, not a finding: the targets still run and
	// the summary they compute is still written.
	if !strings.Contains(text, "### rejected_before_landing") {
		t.Errorf("text = %q, want the targets after a failed profile", text)
	}
	if summary := mangoSectionSummary(t, dir); len(summary) != 1 {
		t.Errorf("summary = %v, want the target that ran despite the profile failure", summary)
	}
}

func TestMangoSectionReportsASummaryWriteFailure(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	// A directory where mango-summary.json goes makes the write fail even for a
	// privileged test user.
	mangoWriteFile(t, filepath.Join(dir, "mango-summary.json", "occupied"), "")
	run, _ := mangoSectionRun(t, mangoSectionResult)
	text, err := MangoSection(context.Background(), Env{Town: t.TempDir(), ReadFile: os.ReadFile, Run: run}, dir,
		"/opt/mango", MangoSectionOptions{MinPositives: 30})
	if err == nil {
		t.Fatalf("MangoSection = %q, want the summary write failure", text)
	}
	if want := "mango-summary.json"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %q", err, want)
	}
}

func TestMangoSectionRendersTheBoxTableWithExplainMeans(t *testing.T) {
	t.Parallel()
	// Train is October and the holdout is November. The box is rig a, which
	// holds four of the holdout's positives in six rows; lines_total is 100
	// inside the box against 10 outside, so the explain cell reads 100 vs 55.
	rows := make([]Row, 40)
	for i := range rows {
		row := mangoSectionLanding(i, 0, "", "daemon")
		rig, lines, rej := "b", "10", "0"
		switch {
		case i < 14, i >= 28 && i < 34:
			rig = "a"
			lines = "100"
		}
		if i < 10 || (i >= 28 && i < 32) {
			rej = "1"
		}
		row["rig"], row["lines_total"], row["rejected_before_landing"] = rig, lines, rej
		if i < 28 {
			row["landed_at"] = fmt.Sprintf("2026-10-%02dT00:00:00Z", 1+i)
		} else {
			row["landed_at"] = fmt.Sprintf("2026-11-%02dT00:00:00Z", i-27)
		}
		rows[i] = row
	}
	result := `{
  "target": "rejected_before_landing", "positive": 1, "baseline": 0.1, "rows": 28,
  "hypercubes": [{"ranges": [{"column": "rig", "categories": ["a"]}], "n": 50, "purity": 0.5}],
  "stats": {"generate": {"iterations": 10, "germsTried": 200, "generated": 50, "valid": 1,
                         "significant": 1,
                         "skippedColumns": [{"column": "x", "reason": "too few distinct values"}]},
            "candidates": 1}
}`
	dir := mangoSectionTown(t, rows)
	run, _ := mangoSectionRun(t, result)
	text := mangoSection(t, dir, run, 1)
	start, end := strings.Index(text, "### rejected_before_landing"), strings.Index(text, "### reached_overrun_stop")
	if start < 0 || end <= start {
		t.Fatalf("text = %q, want a rejected_before_landing part", text)
	}
	part := text[start:end]
	header := "| # | box | label | train n | train rate | holdout n | holdout rate | baseline | p (Holm) | " +
		"replicates | attempts (box vs all) | file_entropy (box vs all) | files_touched (box vs all) | " +
		"lines_total (box vs all) | package_entropy (box vs all)"
	if !strings.Contains(part, header) {
		t.Errorf("part = %q, want the table header %q", part, header)
	}
	if want := "| 1 | rig in {a} | - | 50 | 50.0% | 6 | 66.7% | 33.3% | "; !strings.Contains(part, want) {
		t.Errorf("part = %q, want the box's row to start %q", part, want)
	}
	if want := "| no | n/a vs n/a | n/a vs n/a | n/a vs n/a | 100 vs 55 | n/a vs n/a |"; !strings.Contains(part, want) {
		t.Errorf("part = %q, want the box's row to end %q", part, want)
	}
	if want := "Columns no box constrained: x (too few distinct values)."; !strings.Contains(part, want) {
		t.Errorf("part = %q, want it to hold %q", part, want)
	}
}

// mangoBoolCell is a target cell: "1" or "0" as the extract writes it.
func mangoBoolCell(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func TestExplainMeansInsideTheBoxAgainstTheHoldout(t *testing.T) {
	t.Parallel()
	// The Python test's rows: 20 rig-a rows with 17 positives and lines_total
	// 100, and 20 rig-b rows with no positives and lines_total 10.
	var rows []Row
	for i := 0; i < 20; i++ {
		rows = append(rows, Row{"rig": "a", "om_threshold": "0.7", "t": mangoBoolCell(i < 17), "lines_total": "100"})
	}
	for i := 0; i < 20; i++ {
		rows = append(rows, Row{"rig": "b", "om_threshold": "0.7", "t": "0", "lines_total": "10"})
	}
	box := Box{Cube: Cube{Ranges: []Range{{Column: "rig", Categories: []string{"a"}}}}, N: 50, Purity: 0.5}
	checks, err := CheckHoldoutExplained([]Box{box}, rows, "t", 20, []string{"lines_total"})
	if err != nil {
		t.Fatalf("CheckHoldoutExplained: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want the one box", len(checks))
	}
	check := checks[0]
	if !check.Replicates {
		t.Errorf("Replicates = false, want the box that holds 17 of 20 in its own rows")
	}
	mean := check.Explain["lines_total"]
	if mean.Box == nil || mean.All == nil {
		t.Fatalf("means = %+v, want a mean inside the box and across the holdout", mean)
	}
	if math.Abs(*mean.Box-100) > mangoSectionTol {
		t.Errorf("box mean = %v, want 100", *mean.Box)
	}
	if math.Abs(*mean.All-55) > mangoSectionTol {
		t.Errorf("holdout mean = %v, want 55", *mean.All)
	}
}

func TestExplainMeansWithoutANumericRowIsNotANumber(t *testing.T) {
	t.Parallel()
	// rig b matches no row, and lines_total is blank everywhere; the box mean
	// and the holdout mean are both "n/a" rather than zero.
	rows := []Row{
		{"rig": "a", "t": "1", "lines_total": ""},
		{"rig": "a", "t": "0", "lines_total": ""},
		{"rig": "a", "t": "1", "lines_total": ""},
		{"rig": "a", "t": "1", "lines_total": ""},
	}
	box := Box{Cube: Cube{Ranges: []Range{{Column: "rig", Categories: []string{"b"}}}}, N: 5, Purity: 0.4}
	checks, err := CheckHoldoutExplained([]Box{box}, rows, "t", 1, []string{"lines_total"})
	if err != nil {
		t.Fatalf("CheckHoldoutExplained: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want the one box", len(checks))
	}
	mean := checks[0].Explain["lines_total"]
	if got := mangoFmt(mean.Box, false); got != "n/a" {
		t.Errorf("box mean cell = %q, want n/a for a box that matched no row", got)
	}
	if got := mangoFmt(mean.All, false); got != "n/a" {
		t.Errorf("holdout mean cell = %q, want n/a for a column with no number", got)
	}
}

func TestExplainMeansRejectACellThatIsNotANumber(t *testing.T) {
	t.Parallel()
	// The Python mean raises on a cell float() cannot read; the Go mean names
	// the column and the cell rather than dropping it and moving the mean.
	rows := []Row{
		{"rig": "a", "t": "1", "lines_total": "10"},
		{"rig": "a", "t": "0", "lines_total": "many"},
	}
	box := Box{Cube: Cube{Ranges: []Range{{Column: "rig", Categories: []string{"a"}}}}, N: 2, Purity: 0.5}
	_, err := CheckHoldoutExplained([]Box{box}, rows, "t", 1, []string{"lines_total"})
	if err == nil {
		t.Fatal("CheckHoldoutExplained = nil error, want the unreadable cell reported")
	}
	for _, want := range []string{"lines_total", `"many"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestMangoSectionFormatsCellsLikeTheReport(t *testing.T) {
	t.Parallel()
	value := 0.75
	tests := []struct {
		name string
		v    any
		pct  bool
		want string
	}{
		{"a value that is not there", nil, false, "n/a"},
		{"a mean that is not there", (*float64)(nil), false, "n/a"},
		{"a ratio as a percentage", 0.75, true, "75.0%"},
		{"a float to three significant digits", 0.4, false, "0.4"},
		{"an integer-valued float without a decimal", 55.0, false, "55"},
		{"an int as itself", 7, false, "7"},
		{"a pointer to a ratio", &value, true, "75.0%"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mangoFmt(tc.v, tc.pct); got != tc.want {
				t.Errorf("mangoFmt(%v, %v) = %q, want %q", tc.v, tc.pct, got, tc.want)
			}
		})
	}
}
