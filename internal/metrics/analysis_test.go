package metrics

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// analyseNow is the clock of a test that runs the daily analysis: a day after
// the snapshots and landings the fake town holds, so the first run is due.
var analyseNow = time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)

// analyseRows is enough landings that the training split (70%) holds the 30
// positives the mango stage needs: 30 of 44.
const analyseRows = 44

func TestDue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		stamp time.Time
		every time.Duration
		want  bool
	}{
		{"no stamp is due", time.Time{}, DailyEvery, true},
		{"a stamp a minute old is not due", analyseNow.Add(-time.Minute), DailyEvery, false},
		{"a stamp exactly every old is due", analyseNow.Add(-DailyEvery), DailyEvery, true},
		{"a stamp older than every is due", analyseNow.Add(-25 * time.Hour), DailyEvery, true},
		{"a future stamp is due, so a clock set back cannot wedge the run", analyseNow.Add(48 * time.Hour), DailyEvery, true},
		{"a non-positive every is always due", analyseNow, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Due(tc.stamp, analyseNow, tc.every); got != tc.want {
				t.Fatalf("Due(%s, %s, %s) = %v, want %v", tc.stamp, analyseNow, tc.every, got, tc.want)
			}
		})
	}
}

func TestNeedRebuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		built, head string
		want        bool
	}{
		{"no recorded build", "", "abc", true},
		{"the same head", "abc", "abc", false},
		{"a moved head", "abc", "def", true},
		{"a head git did not answer with", "abc", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NeedRebuild(tc.built, tc.head); got != tc.want {
				t.Fatalf("NeedRebuild(%q, %q) = %v, want %v", tc.built, tc.head, got, tc.want)
			}
		})
	}
}

// recordedCall is one command the analysis ran, with the environment it was
// given.
type recordedCall struct {
	argv []string
	env  []string
}

// analysisDay is the day directory analysisTown writes the extract's landings
// into, and the day the fake extract reports.
const analysisDay = "2026-10-01"

// analysisTown lays out a fake town: a day's extract output under a date the
// clock disagrees with, a snapshot log, and a peak row. It returns the town and
// the extract's output directory.
func analysisTown(t *testing.T) (town, dayDir string) {
	t.Helper()
	town = t.TempDir()
	// The directory is named for a day the fake clock is not: the analysis must
	// follow the day the extract reports rather than derive a date of its own.
	dayDir = filepath.Join(town, ".runtime", "metrics", analysisDay)
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	header, rows := analysisLandings()
	var csv bytes.Buffer
	if err := WriteLandings(&csv, header, rows); err != nil {
		t.Fatal(err)
	}
	writeTownFile(t, filepath.Join(dayDir, landingsName), csv.String())
	writeTownFile(t, filepath.Join(dayDir, sidecarName), `{"explain_only": ["lines_total"]}`)
	writeTownFile(t, filepath.Join(town, ".runtime", "metrics", "snapshots.jsonl"),
		`{"v":1,"ts":"2026-10-24T10:00:00Z","placement":null,"errors":[],"queue":{"open":3}}
{"v":1,"ts":"2026-10-24T12:00:00Z","placement":null,"errors":[],"queue":{"open":4}}
`)
	writeTownFile(t, filepath.Join(town, ".runtime", "metrics", "resources", peaksFile),
		`{"bead":"gt-a000","ended_at":"2026-10-24T00:30:00Z","peak_mem_bytes":1000,"peak_cpu_pct":12.5,"mem_limit_bytes":2000}
`)
	return town, dayDir
}

// analysisWeeks is how many landings each of five consecutive weeks from
// 2026-09-14 holds: the first three are thick enough for the weekly series and
// the last two are thin.
var analysisWeeks = []int{12, 12, 12, 4, 4}

// analysisLandings is 44 landings whose first 30 train rejected_before_landing:
// enough positives for that target's mango stage, none for the other two. Each
// landing's week is its landed_at's Monday, so the panel's trend holds the
// three thick weeks and leaves the two thin ones out.
func analysisLandings() (header []string, rows []Row) {
	base := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	for week, count := range analysisWeeks {
		monday := base.AddDate(0, 0, 7*week)
		for n := 0; n < count; n++ {
			i := len(rows)
			row := make(Row, len(landingColumns))
			for _, name := range landingColumns {
				row[name] = ""
			}
			row["bead"] = fmt.Sprintf("gt-a%03d", i)
			row["rig"] = "gastown"
			row["landed_at"] = monday.Add(time.Duration(n) * time.Hour).Format(time.RFC3339)
			row["week"] = monday.Format("2006-01-02")
			row["route"] = "om"
			row["om_verdict"] = "approve"
			row["gate_result"] = "pass"
			row["model"] = "opus"
			row["dominant_model"] = "opus"
			row["type"] = "task"
			row["priority"] = "2"
			row["lines_total"] = "100"
			row["attempts"] = "1"
			row["unreadable"] = "0"
			row["rejected_before_landing"] = "0"
			row["manual_rescue"] = "0"
			row["reached_overrun_stop"] = "0"
			if i < 30 {
				row["rejected_before_landing"] = "1"
			} else {
				row["manual_rescue"] = "1"
			}
			// The overrun guard stopped one late landing, so the target skipped
			// for too few positives still has a day to report.
			if i == analyseRows-4 {
				row["reached_overrun_stop"] = "1"
			}
			rows = append(rows, row)
		}
	}
	return append([]string(nil), landingColumns...), rows
}

// writeTownFile writes body at path, creating the directories above it.
func writeTownFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// analysisOptions is the analysis's paths for town, with the extract answered
// by the day directory analysisTown wrote. Repo is left empty, as the daily run
// leaves it now that the extract runs in process.
func analysisOptions(town string) AnalysisOptions {
	return AnalysisOptions{
		MangoGitDir: filepath.Join(town, "mango", ".repo.git"),
		MangoBin:    filepath.Join(town, ".runtime", "metrics", "bin", "mango"),
		Extract:     analysisExtract(filepath.Join(town, ".runtime", "metrics", analysisDay), new([]extractCall)),
	}
}

// extractCall is the town and docs directory the analysis gave its extract.
type extractCall struct{ town, docs string }

// analysisExtract is the extract seam for a test: it records the town and docs
// directory it was given and reports dayDir as the day it wrote.
func analysisExtract(dayDir string, calls *[]extractCall) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, town, docs string) (string, error) {
		*calls = append(*calls, extractCall{town: town, docs: docs})
		return dayDir, nil
	}
}

// analysisEnv is an Env over town whose commands are all recorded and answered
// by answer, so a test reads the order and the environment the analysis gave.
func analysisEnv(t *testing.T, town, dayDir string, answer func(recordedCall) ([]byte, error)) (Env, *[]recordedCall) {
	t.Helper()
	calls := &[]recordedCall{}
	if answer == nil {
		answer = analysisAnswer(t, dayDir)
	}
	record := func(extra []string, argv []string) ([]byte, error) {
		call := recordedCall{argv: argv, env: extra}
		*calls = append(*calls, call)
		return answer(call)
	}
	return Env{
		Town:     town,
		Now:      func() time.Time { return analyseNow },
		ReadFile: os.ReadFile,
		Run:      func(_ context.Context, argv []string) ([]byte, error) { return record(nil, argv) },
		RunEnv:   func(_ context.Context, argv, extra []string) ([]byte, error) { return record(extra, argv) },
	}, calls
}

// analysisAnswer is the fake host's answer: the mango head, a one-file archive,
// a successful build, and mango's own commands writing the canned result where
// explore asked.
func analysisAnswer(t *testing.T, dayDir string) func(recordedCall) ([]byte, error) {
	t.Helper()
	archive := tarOf(t, map[string]string{
		"go-api/go.mod":            "module github.com/sloan/mango/go-api\n",
		"go-api/cmd/mango/main.go": "package main\n",
	})
	return func(c recordedCall) ([]byte, error) {
		switch c.argv[0] {
		case "git":
			if hasArg(c.argv, "archive") {
				return archive, nil
			}
			return []byte(analysisHead + "\n"), nil
		case "go":
			return nil, nil
		}
		if len(c.argv) > 1 && c.argv[1] == "explore" {
			dir := filepath.Dir(c.argv[2])
			return nil, os.WriteFile(filepath.Join(dir, "result.json"), []byte(mangoCannedResult), 0o644)
		}
		return nil, nil
	}
}

// analysisHead is the head the fake mango repository reports.
const analysisHead = "0123456789abcdef0123456789abcdef01234567"

// hasArg reports whether argv holds arg.
func hasArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

// tarOf builds the archive git archive writes holding files.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAnalysePublishesThePanelOverTheRunners(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	opts := analysisOptions(town)
	var extracts []extractCall
	opts.Extract = analysisExtract(dayDir, &extracts)
	env, calls := analysisEnv(t, town, dayDir, nil)

	res, err := RunAnalysis(context.Background(), env, opts)
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	if !res.Ran || res.Landings != analyseRows {
		t.Fatalf("result = %+v, want a run over %d landings", res, analyseRows)
	}
	if len(res.Panel.Targets) != len(mangoTargets) {
		t.Fatalf("panel has %d targets, want %d", len(res.Panel.Targets), len(mangoTargets))
	}
	if res.Panel.SnapshotHours != 2 {
		t.Errorf("SnapshotHours = %d, want 2 from the two snapshots", res.Panel.SnapshotHours)
	}
	if got := res.Panel.Targets[0]; got.Skipped || got.Verdict != VerdictNotYet {
		t.Errorf("rejected_before_landing = %+v, want a target that ran with no holdout positives", got)
	}
	if got := res.Panel.Targets[1]; !got.Skipped {
		t.Errorf("manual_rescue = %+v, want it skipped for too few training positives", got)
	}

	dir := filepath.Join(town, ".runtime", "metrics", analysisDirName)
	panelRaw, err := os.ReadFile(filepath.Join(dir, panelName))
	if err != nil {
		t.Fatalf("panel.json: %v", err)
	}
	var published Panel
	if err := json.Unmarshal(panelRaw, &published); err != nil {
		t.Fatalf("panel.json does not parse: %v", err)
	}
	if published.GeneratedAt != analyseNow.Format(time.RFC3339) || published.Items != analyseRows {
		t.Errorf("published panel = %+v, want generated_at %s over %d landings", published, analyseNow.Format(time.RFC3339), analyseRows)
	}
	// The fixture's three thick weeks, oldest first; the two four-landing weeks
	// are thin and the panel leaves them out.
	wantWeekly := []PanelWeek{
		{Week: "2026-09-14", Landings: 12, RejectedShare: 1},
		{Week: "2026-09-21", Landings: 12, RejectedShare: 1},
		{Week: "2026-09-28", Landings: 12, RejectedShare: 0.5},
	}
	if !reflect.DeepEqual(published.Weekly, wantWeekly) {
		t.Errorf("panel.json weekly = %+v, want the thick weeks in order: %+v", published.Weekly, wantWeekly)
	}
	if published.Events == nil || len(published.Events) != 0 {
		t.Errorf("panel.json events = %+v with no events file, want an empty list", published.Events)
	}
	// The day each target's last positive landed comes from its split, so it is
	// on the panel whether or not the target was run.
	if got := published.Targets[0].LastPositive; got != "2026-09-28" {
		t.Errorf("the run target's last positive = %q, want 2026-09-28", got)
	}
	if got := published.Targets[1].LastPositive; got != "2026-10-12" {
		t.Errorf("the skipped target's last positive = %q, want 2026-10-12", got)
	}
	stamp, err := os.ReadFile(filepath.Join(dir, stampName))
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if got := strings.TrimSpace(string(stamp)); got != analyseNow.Format(time.RFC3339) {
		t.Errorf("stamp = %q, want %s", got, analyseNow.Format(time.RFC3339))
	}
	joined, err := os.ReadFile(filepath.Join(dir, joinedName))
	if err != nil {
		t.Fatalf("joined landings: %v", err)
	}
	for _, column := range []string{"snap_queue_open", "sandbox_peak_mem_bytes"} {
		if !bytes.Contains(joined, []byte(column)) {
			t.Errorf("joined landings have no %s column:\n%s", column, joined)
		}
	}
	sha, err := os.ReadFile(filepath.Join(filepath.Dir(opts.MangoBin), mangoShaName))
	if err != nil {
		t.Fatalf("mango.sha: %v", err)
	}
	if got := strings.TrimSpace(string(sha)); got != analysisHead {
		t.Errorf("mango.sha = %q, want the built head %s", got, analysisHead)
	}

	// Every command is one the sandbox's exec allowlist names: the run starts no
	// interpreter, so no argv[0] is python3 or a script.
	for _, call := range *calls {
		switch call.argv[0] {
		case "git", "go", opts.MangoBin:
		default:
			t.Errorf("recorded argv %v starts with %q, which is neither git, go nor the mango binary", call.argv, call.argv[0])
		}
	}
	docs := filepath.Join(dir, docsName)
	if want := []extractCall{{town: town, docs: docs}}; !reflect.DeepEqual(extracts, want) {
		t.Errorf("extract seam calls = %+v, want one call over the town and the panel docs %+v", extracts, want)
	}
	// A build is only recorded after it succeeded, so the archive comes first
	// and the next run finds the head current.
	if indexOf(*calls, "go") < indexOf(*calls, "archive") {
		t.Errorf("go build came before git archive: %v", *calls)
	}

	before := len(*calls)
	if again, err := RunAnalysis(context.Background(), env, opts); err != nil || again.Ran {
		t.Fatalf("second RunAnalysis = %+v, %v, want a run that is not due", again, err)
	}
	if len(*calls) != before {
		t.Errorf("the not-due run ran %d commands, want none", len(*calls)-before)
	}
}

// TestAnalysisNeverStartsPython is the daily run with no checkout named: the
// extract runs in process through its seam, so no command the analysis starts
// is python3 or a town-metrics script, and the panel still publishes.
func TestAnalysisNeverStartsPython(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	opts := analysisOptions(town)
	var extracts []extractCall
	opts.Extract = analysisExtract(dayDir, &extracts)
	env, calls := analysisEnv(t, town, dayDir, nil)

	res, err := RunAnalysis(context.Background(), env, opts)
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	if !res.Ran {
		t.Fatal("RunAnalysis was not due on a town with no stamp")
	}
	for _, call := range *calls {
		if call.argv[0] == "python3" || strings.Contains(strings.Join(call.argv, " "), "town-metrics") {
			t.Errorf("the analysis started %v, want no python3 and no town-metrics script", call.argv)
		}
	}
	if len(extracts) != 1 {
		t.Errorf("the extract seam ran %d times, want once", len(extracts))
	}
	if _, err := os.Stat(filepath.Join(town, ".runtime", "metrics", analysisDirName, panelName)); err != nil {
		t.Errorf("panel.json was not published: %v", err)
	}
}

// emptyRun answers every command with no output, so the town's sources read as
// empty and the extract has nothing to report.
func emptyRun(context.Context, string, []string) ([]byte, error) { return nil, nil }

// failingGitRun refuses every git command, which is the first source RealEnv
// reads, so the in-process extract stops before it writes anything.
func failingGitRun(_ context.Context, _ string, argv []string) ([]byte, error) {
	if hasArg(argv, "git") {
		return nil, errors.New("git: no repository")
	}
	return nil, nil
}

// dirRunCall is one command the in-process extract ran, in its directory.
type dirRunCall struct {
	dir  string
	argv []string
}

// recordingDirRun answers every command with no output and records it, so a
// test reads what the default extract ran.
func recordingDirRun(calls *[]dirRunCall) RunnerDir {
	return func(_ context.Context, dir string, argv []string) ([]byte, error) {
		*calls = append(*calls, dirRunCall{dir: dir, argv: argv})
		return nil, nil
	}
}

// TestAnalyseRunsTheExtractInProcess is the default extract, driven through
// RunAnalysis with no Extract seam: the run writes the day's landings itself,
// saves the caches, publishes the panel, and runs every git and bd command
// through Env.RunDir rather than starting python3.
func TestAnalyseRunsTheExtractInProcess(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	opts := analysisOptions(town)
	opts.Extract = nil
	// The in-process extract joins its home-level bead listing to Home; a
	// temporary one keeps this run off the host's.
	opts.Home = t.TempDir()
	var ran []dirRunCall
	env, _ := analysisEnv(t, town, "", nil)
	env.RunDir = recordingDirRun(&ran)

	res, err := RunAnalysis(context.Background(), env, opts)
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	if !res.Ran {
		t.Fatal("RunAnalysis was not due on a town with no stamp")
	}
	if len(ran) == 0 {
		t.Fatal("the in-process extract ran no command through Env.RunDir")
	}
	for _, call := range ran {
		if call.argv[0] == "python3" || strings.Contains(strings.Join(call.argv, " "), "town-metrics") {
			t.Errorf("the extract started %v in %q, want no python3 and no town-metrics script", call.argv, call.dir)
		}
	}
	dayDir := filepath.Join(town, ".runtime", "metrics", analyseNow.Local().Format("2006-01-02"))
	for _, name := range []string{landingsName, sidecarName} {
		if _, err := os.Stat(filepath.Join(dayDir, name)); err != nil {
			t.Errorf("the extract wrote no %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(town, ".runtime", "metrics", "cache", diffstatsCacheName)); err != nil {
		t.Errorf("the caches were not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, panelName)); err != nil {
		t.Errorf("panel.json was not published: %v", err)
	}
}

// TestAnalyseInProcessExtractFailureStopsTheStep gives each step of the default
// extract a failure, driven through RunAnalysis, and requires the run to fail
// with no panel and no stamp published.
func TestAnalyseInProcessExtractFailureStopsTheStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		run     RunnerDir
		setup   func(t *testing.T, town string)
		want    string // the step's own name in the error
		wantDay bool   // the error also names the day directory
	}{
		{
			name: "the sources cannot be read",
			run:  failingGitRun,
			want: "real env",
		},
		{
			name: "the day directory cannot be made",
			run:  emptyRun,
			setup: func(t *testing.T, town string) {
				// A file where the day's directory goes: MkdirAll cannot make it.
				day := analyseNow.Local().Format("2006-01-02")
				writeTownFile(t, filepath.Join(town, ".runtime", "metrics", day), "not a directory")
			},
			wantDay: true,
		},
		{
			name: "the caches cannot be saved",
			run:  emptyRun,
			setup: func(t *testing.T, town string) {
				// A dangling link where the cache directory goes: reads of it find
				// nothing, and MkdirAll cannot make it.
				metrics := filepath.Join(town, ".runtime", "metrics")
				if err := os.MkdirAll(metrics, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(town, "missing"), filepath.Join(metrics, "cache")); err != nil {
					t.Fatal(err)
				}
			},
			want: "save caches",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, town)
			}
			opts := analysisOptions(town)
			opts.Extract = nil
			// A temporary home keeps the in-process extract off the host's.
			opts.Home = t.TempDir()
			env, _ := analysisEnv(t, town, "", nil)
			env.RunDir = tc.run

			_, err := RunAnalysis(context.Background(), env, opts)
			if err == nil {
				t.Fatalf("RunAnalysis ran with %s", tc.name)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			if tc.wantDay {
				day := filepath.Join(town, ".runtime", "metrics", analyseNow.Local().Format("2006-01-02"))
				if !strings.Contains(err.Error(), day) {
					t.Errorf("error = %q, want it to name the day directory %q", err, day)
				}
			}
			for _, name := range []string{panelName, stampName} {
				if _, err := os.Stat(filepath.Join(town, ".runtime", "metrics", analysisDirName, name)); !os.IsNotExist(err) {
					t.Errorf("%s was published by a run whose extract failed: %v", name, err)
				}
			}
		})
	}
}

// TestAnalyseInProcessExtractNeedsRunDir pins the seam the default extract
// depends on: with no Extract seam and no Env.RunDir, the run stops rather than
// running the extract's git and bd commands somewhere else.
func TestAnalyseInProcessExtractNeedsRunDir(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	opts := analysisOptions(town)
	opts.Extract = nil
	env, calls := analysisEnv(t, town, dayDir, nil)

	_, err := RunAnalysis(context.Background(), env, opts)
	if err == nil {
		t.Fatal("RunAnalysis ran the in-process extract with no Env.RunDir")
	}
	if !strings.Contains(err.Error(), "RunDir") {
		t.Errorf("error = %q, want it to name the missing seam", err)
	}
	if len(*calls) != 0 {
		t.Errorf("the run gave %d commands a runner rather than stopping: %v", len(*calls), *calls)
	}
}

// TestAnalyseMarksTheRegimeEvents writes the town's events file out of date
// order and requires the panel's events in it, oldest first.
func TestAnalyseMarksTheRegimeEvents(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	writeTownFile(t, filepath.Join(town, regimeEventsFile),
		`{"v":1,"events":[{"date":"2026-10-10","what":"sonnet seats replaced flash seats"},`+
			`{"date":"2026-09-28","what":"gate raised"}]}`)
	env, _ := analysisEnv(t, town, dayDir, nil)

	res, err := RunAnalysis(context.Background(), env, analysisOptions(town))
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	want := []PanelEvent{
		{Date: "2026-09-28", What: "gate raised"},
		{Date: "2026-10-10", What: "sonnet seats replaced flash seats"},
	}
	if !reflect.DeepEqual(res.Panel.Events, want) {
		t.Errorf("panel events = %+v, want the file's events in date order: %+v", res.Panel.Events, want)
	}
}

// TestAnalyseWritesThePanelWhenTheEventsFileIsUnreadable keeps a failed events
// read off the run's critical path: the panel still publishes, with no events,
// and the report line names the file the reader could not use.
func TestAnalyseWritesThePanelWhenTheEventsFileIsUnreadable(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	writeTownFile(t, filepath.Join(town, regimeEventsFile), "not json\n")
	env, _ := analysisEnv(t, town, dayDir, nil)
	opts := analysisOptions(town)
	var reported []string
	opts.Reportf = func(format string, args ...any) { reported = append(reported, fmt.Sprintf(format, args...)) }

	res, err := RunAnalysis(context.Background(), env, opts)
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(res.Dir, panelName))
	if err != nil {
		t.Fatalf("panel.json: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"events": []`)) {
		t.Errorf("panel.json = %s, want it written with an empty events list", raw)
	}
	named := false
	for _, line := range reported {
		if strings.Contains(line, regimeEventsFile) {
			named = true
		}
	}
	if !named {
		t.Errorf("the run reported %v, want a line naming %s", reported, regimeEventsFile)
	}
}

// TestPanelWeeklyReportsARowItCannotRead keeps a bad landings row off the run's
// critical path: the trend is empty and the report line names it, so the panel
// publishes without a trend rather than not at all, the same as a failed events
// read.
func TestPanelWeeklyReportsARowItCannotRead(t *testing.T) {
	t.Parallel()

	var reported []string
	got := panelWeekly([]Row{{"week": "2026-10-05", "rig": "gastown", "attempts": "not a number"}},
		func(format string, args ...any) { reported = append(reported, fmt.Sprintf(format, args...)) })
	if len(got) != 0 {
		t.Errorf("panelWeekly = %+v, want no trend from a row it cannot read", got)
	}
	if len(reported) == 0 || !strings.Contains(reported[0], "weekly") {
		t.Errorf("panelWeekly reported %v, want one line naming the weekly trend's failure", reported)
	}
}

// TestAnalysePublishesThePanelWhenTheTrendCannotBeBuilt is the weekly twin of
// the unreadable events file: a landings row the weekly builder cannot read
// leaves weekly [] on the published panel, names the row on the report line, and
// does not fail the run.
func TestAnalysePublishesThePanelWhenTheTrendCannotBeBuilt(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	header, rows := analysisLandings()
	rows[3]["attempts"] = "not a number"
	var csv bytes.Buffer
	if err := WriteLandings(&csv, header, rows); err != nil {
		t.Fatal(err)
	}
	writeTownFile(t, filepath.Join(dayDir, landingsName), csv.String())
	env, _ := analysisEnv(t, town, dayDir, nil)
	opts := analysisOptions(town)
	var reported []string
	opts.Reportf = func(format string, args ...any) { reported = append(reported, fmt.Sprintf(format, args...)) }

	res, err := RunAnalysis(context.Background(), env, opts)
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(res.Dir, panelName))
	if err != nil {
		t.Fatalf("panel.json: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"weekly": []`)) {
		t.Errorf("panel.json = %s, want it written with an empty weekly series", raw)
	}
	named := false
	for _, line := range reported {
		if strings.Contains(line, "landings row") {
			named = true
		}
	}
	if !named {
		t.Errorf("the run reported %v, want a line naming the landings row", reported)
	}
}

// callWith is the first recorded call whose argv[0] is name.
func callWith(calls []recordedCall, name string) *recordedCall {
	for i, call := range calls {
		if call.argv[0] == name {
			return &calls[i]
		}
	}
	return nil
}

// indexOf is the index of the first call whose argv names arg, or -1.
func indexOf(calls []recordedCall, arg string) int {
	for i, call := range calls {
		if hasArg(call.argv, arg) {
			return i
		}
	}
	return -1
}

func TestAnalyseFailedStepKeepsTheOldPanelAndStamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		fail        string // the argv[0] or argv[1] the fake host refuses
		failExtract bool   // the extract seam refuses
		detail      string
	}{
		{"a mango build that cannot find go", "go", false, "mango build"},
		{"a mango build that fails", "build-fails", false, "mango build"},
		{"an extract that fails", "", true, "extract"},
		{"an archive that fails", "archive-fails", false, "mango archive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, dayDir := analysisTown(t)
			dir := filepath.Join(town, ".runtime", "metrics", analysisDirName)
			writeTownFile(t, filepath.Join(dir, panelName), "old panel")
			oldStamp := analyseNow.Add(-25 * time.Hour).Format(time.RFC3339)
			writeTownFile(t, filepath.Join(dir, stampName), oldStamp+"\n")
			answer := analysisAnswer(t, dayDir)
			env, _ := analysisEnv(t, town, dayDir, func(c recordedCall) ([]byte, error) {
				switch {
				case tc.fail == c.argv[0]:
					return nil, errors.New("no such command")
				case tc.fail == "build-fails" && c.argv[0] == "go":
					return nil, errors.New("compile error")
				case tc.fail == "archive-fails" && hasArg(c.argv, "archive"):
					return nil, errors.New("bad object")
				}
				return answer(c)
			})

			opts := analysisOptions(town)
			if tc.failExtract {
				opts.Extract = func(context.Context, string, string) (string, error) {
					return "", errors.New("no such extract")
				}
			}
			_, err := RunAnalysis(context.Background(), env, opts)
			if err == nil {
				t.Fatal("RunAnalysis returned no error for a step that failed")
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("error = %q, want it to name %q", err, tc.detail)
			}
			if got, err := os.ReadFile(filepath.Join(dir, panelName)); err != nil || string(got) != "old panel" {
				t.Errorf("panel.json = %q, %v, want the old panel untouched", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, stampName)); err != nil || strings.TrimSpace(string(got)) != oldStamp {
				t.Errorf("stamp = %q, %v, want the old stamp %s, so the next run retries", got, err, oldStamp)
			}
			if tc.detail == "mango build" || tc.detail == "mango archive" {
				shaPath := filepath.Join(filepath.Dir(analysisOptions(town).MangoBin), mangoShaName)
				if _, err := os.Stat(shaPath); err == nil {
					t.Errorf("mango.sha was written by a run whose build did not finish, so the next run would skip the rebuild")
				}
			}
		})
	}
}

// TestAnalyseWithoutRunEnvIsAnError pins the seam the extract depends on: a
// run with no way to pass TOWN_METRICS_TOWN must stop, not run the extract in
// whatever environment it happens to inherit and read another town.
func TestAnalyseWithoutRunEnvIsAnError(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	env, calls := analysisEnv(t, town, dayDir, nil)
	env.RunEnv = nil

	_, err := RunAnalysis(context.Background(), env, analysisOptions(town))
	if err == nil {
		t.Fatal("RunAnalysis ran with no environment seam for the extract")
	}
	if !strings.Contains(err.Error(), "RunEnv") {
		t.Errorf("error = %q, want it to name the missing seam", err)
	}
	if len(*calls) != 0 {
		t.Errorf("the run gave %d commands no environment rather than stopping: %v", len(*calls), *calls)
	}
}

// TestRunEnvNeedsItsSeam is the guard on the helper itself: a nil RunEnv is an
// error that names the seam and the command, not a command that silently did
// not run.
func TestRunEnvNeedsItsSeam(t *testing.T) {
	t.Parallel()
	_, err := runEnv(context.Background(), Env{}, []string{"git", "rev-parse", "origin/main"})
	if err == nil {
		t.Fatal("runEnv returned no error with no RunEnv seam")
	}
	for _, name := range []string{"RunEnv", "git"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %q, want it to name %s", err, name)
		}
	}
}

func TestAnalyseRebuildsMangoOnlyWhenItsHeadMoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		recorded  string
		binary    bool
		wantBuild bool
	}{
		{"the recorded head is current", analysisHead, true, false},
		{"the head moved", "fedcba9876543210fedcba9876543210fedcba98", true, true},
		{"no build was recorded", "", true, true},
		{"the sha names a binary that is gone", analysisHead, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, dayDir := analysisTown(t)
			opts := analysisOptions(town)
			writeTownFile(t, filepath.Join(filepath.Dir(opts.MangoBin), mangoShaName), tc.recorded+"\n")
			writeTownFile(t, opts.MangoBin, "binary\n")
			if !tc.binary {
				if err := os.Remove(opts.MangoBin); err != nil {
					t.Fatal(err)
				}
			}
			env, calls := analysisEnv(t, town, dayDir, nil)

			if _, err := RunAnalysis(context.Background(), env, opts); err != nil {
				t.Fatalf("RunAnalysis: %v", err)
			}
			if got := indexOf(*calls, "archive") >= 0; got != tc.wantBuild {
				t.Fatalf("archive ran = %v, want %v (calls: %v)", got, tc.wantBuild, *calls)
			}
			sha, err := os.ReadFile(filepath.Join(filepath.Dir(opts.MangoBin), mangoShaName))
			if err != nil {
				t.Fatalf("mango.sha: %v", err)
			}
			if got := strings.TrimSpace(string(sha)); got != analysisHead {
				t.Errorf("mango.sha = %q, want the head %s the analysis ran against", got, analysisHead)
			}
		})
	}
}

// TestAnalyseBuildsMangoInItsModuleDirectory pins the build's working directory
// and package: mango's go.mod and CLI live under go-api/, so a build at the
// archive root finds no module (gt-m2im9).
func TestAnalyseBuildsMangoInItsModuleDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tweak  func(*AnalysisOptions)
		dir    string
		target string
	}{
		{"the defaults", nil, "go-api", "./cmd/mango"},
		{"a set module directory", func(o *AnalysisOptions) { o.BuildModuleDir = "src" }, "src", "./cmd/mango"},
		{"a set target", func(o *AnalysisOptions) { o.BuildTarget = "./cmd/other" }, "go-api", "./cmd/other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, dayDir := analysisTown(t)
			opts := analysisOptions(town)
			if tc.tweak != nil {
				tc.tweak(&opts)
			}
			env, calls := analysisEnv(t, town, dayDir, nil)

			if _, err := RunAnalysis(context.Background(), env, opts); err != nil {
				t.Fatalf("RunAnalysis: %v", err)
			}
			build := callWith(*calls, "go")
			if build == nil {
				t.Fatalf("the analysis never built mango: %v", *calls)
			}
			got := build.argv
			if len(got) != 7 || got[1] != "build" || got[2] != "-C" || got[4] != "-o" {
				t.Fatalf("mango build argv = %v, want go build -C <dir> -o <bin> <target>", got)
			}
			if dir := filepath.Base(got[3]); dir != tc.dir {
				t.Errorf("go build ran in %q, want the %q module directory below the archive", got[3], tc.dir)
			}
			if got[6] != tc.target {
				t.Errorf("go build target = %q, want %q", got[6], tc.target)
			}
		})
	}
}

func TestAnalyseSkipsTargetsUnderMinimumSupport(t *testing.T) {
	t.Parallel()
	town, dayDir := analysisTown(t)
	env, calls := analysisEnv(t, town, dayDir, nil)

	res, err := RunAnalysis(context.Background(), env, analysisOptions(town))
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	mangoTargetsRun := map[string]bool{}
	for _, call := range *calls {
		if i := indexOfCallArg(call, "--target"); i >= 0 {
			mangoTargetsRun[call.argv[i+1]] = true
		}
	}
	if !mangoTargetsRun["rejected_before_landing"] {
		t.Errorf("mango did not run for the target with 30 training positives: %v", *calls)
	}
	for _, target := range []string{"manual_rescue", "reached_overrun_stop"} {
		if mangoTargetsRun[target] {
			t.Errorf("mango ran for %s, which the training split has too few positives for", target)
		}
	}
	if len(res.Panel.Targets) != len(mangoTargets) {
		t.Errorf("the panel lists %d targets, want every target, run or skipped", len(res.Panel.Targets))
	}
}

// indexOfCallArg is the index of arg in call.argv, or -1.
func indexOfCallArg(call recordedCall, arg string) int {
	for i, a := range call.argv {
		if a == arg {
			return i
		}
	}
	return -1
}

func TestAnalyseReadsTheDirectoryTheExtractReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{"the receipt's directory", "extract: 44 landings, 1 weekly rows -> /town/.runtime/metrics/2026-10-01\n", "/town/.runtime/metrics/2026-10-01", false},
		{"the last directory in a multi-line receipt", "warn: nothing\n extract: 1 landings, 0 weekly rows -> /town/.runtime/metrics/x\n", "/town/.runtime/metrics/x", false},
		{"no directory at all", "extract: done\n", "", true},
		{"an empty directory", "extract: 1 landings -> \n", "", true},
		{"no output", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := extractDir([]byte(tc.out))
			if (err != nil) != tc.wantErr {
				t.Fatalf("extractDir(%q) error = %v, want error = %v", tc.out, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("extractDir(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

func TestReadSnapshotsSkipsOnlyAnUnfinishedLastLine(t *testing.T) {
	t.Parallel()
	record := `{"v":1,"ts":"2026-10-24T10:00:00Z","placement":null,"errors":[],"queue":{"open":3}}`
	tests := []struct {
		name    string
		log     string
		want    int
		wantErr bool
	}{
		{"two records and a final newline", record + "\n" + record + "\n", 2, false},
		{"a last line an append is still writing", record + "\n" + `{"v":1,"ts":"20`, 1, false},
		{"no trailing newline, whole record", record, 1, false},
		{"a blank line names no record", "\n" + record + "\n\n", 1, false},
		{"an empty log", "", 0, false},
		{"a broken line before the end is an error", record + "\nnot json\n" + record + "\n", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadSnapshots(strings.NewReader(tc.log))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReadSnapshots error = %v, want error = %v", err, tc.wantErr)
			}
			if len(got) != tc.want {
				t.Fatalf("ReadSnapshots read %d records, want %d", len(got), tc.want)
			}
		})
	}
}

func TestExtractTarRefusesAnEntryOutsideItsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := tarOf(t, map[string]string{"../escaped.go": "package main\n"})
	if err := extractTar(dir, data); err == nil {
		t.Fatal("extractTar wrote an entry outside its directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escaped.go")); err == nil {
		t.Fatal("the escaping file was written")
	}
}
