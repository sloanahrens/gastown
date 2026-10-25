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

// analysisTown lays out a fake town: a day's extract output under a date the
// clock disagrees with, a snapshot log, and a peak row. It returns the town and
// the extract's output directory.
func analysisTown(t *testing.T) (town, dayDir string) {
	t.Helper()
	town = t.TempDir()
	// The directory is named for a day the fake clock is not: the analysis must
	// follow the extract's receipt rather than derive a date of its own.
	dayDir = filepath.Join(town, ".runtime", "metrics", "2026-10-01")
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

// analysisLandings is 44 landings whose first 30 train rejected_before_landing:
// enough positives for that target's mango stage, none for the other two.
func analysisLandings() (header []string, rows []Row) {
	base := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < analyseRows; i++ {
		row := make(Row, len(landingColumns))
		for _, name := range landingColumns {
			row[name] = ""
		}
		row["bead"] = fmt.Sprintf("gt-a%03d", i)
		row["rig"] = "gastown"
		row["landed_at"] = base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		row["week"] = "2026-W43"
		row["route"] = "om"
		row["om_verdict"] = "approve"
		row["gate_result"] = "pass"
		row["model"] = "opus"
		row["dominant_model"] = "opus"
		row["type"] = "task"
		row["priority"] = "2"
		row["lines_total"] = "100"
		if i < 30 {
			row["rejected_before_landing"] = "1"
		} else {
			row["rejected_before_landing"] = "0"
			row["manual_rescue"] = "1"
		}
		rows = append(rows, row)
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

// analysisOptions is the analysis's paths for town.
func analysisOptions(town string) AnalysisOptions {
	return AnalysisOptions{
		Repo:        filepath.Join(town, "gastown"),
		MangoGitDir: filepath.Join(town, "mango", ".repo.git"),
		MangoBin:    filepath.Join(town, ".runtime", "metrics", "bin", "mango"),
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
// a successful build, the extract's receipt naming dayDir, and mango's own
// commands writing the canned result where explore asked.
func analysisAnswer(t *testing.T, dayDir string) func(recordedCall) ([]byte, error) {
	t.Helper()
	archive := tarOf(t, map[string]string{"go.mod": "module mango\n", "main.go": "package main\n"})
	return func(c recordedCall) ([]byte, error) {
		switch c.argv[0] {
		case "git":
			if hasArg(c.argv, "archive") {
				return archive, nil
			}
			return []byte(analysisHead + "\n"), nil
		case "go":
			return nil, nil
		case "python3":
			return []byte(fmt.Sprintf("extract: %d landings, 1 weekly rows -> %s\n", analyseRows, dayDir)), nil
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

	// Every command is one the sandbox's exec allowlist names: a script path is
	// never argv[0], so the run needs no interpreter it does not already name.
	for _, call := range *calls {
		switch call.argv[0] {
		case "python3", "git", "go", opts.MangoBin:
		default:
			t.Errorf("recorded argv %v starts with %q, which is neither python3, git, go nor the mango binary", call.argv, call.argv[0])
		}
	}
	extract := callWith(*calls, "python3")
	if extract == nil {
		t.Fatalf("the analysis never ran the extract: %v", *calls)
	}
	wantExtract := []string{"python3", filepath.Join(opts.Repo, "scripts", "town-metrics"), "extract"}
	if strings.Join(extract.argv, " ") != strings.Join(wantExtract, " ") {
		t.Errorf("extract argv = %v, want %v", extract.argv, wantExtract)
	}
	docs := filepath.Join(dir, docsName)
	for _, kv := range []string{"TOWN_METRICS_TOWN=" + town, "TOWN_METRICS_DOCS=" + docs} {
		if !hasArg(extract.env, kv) {
			t.Errorf("extract ran without %s in its environment (%v)", kv, extract.env)
		}
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
		name   string
		fail   string // the argv[0] or argv[1] the fake host refuses
		detail string
	}{
		{"a mango build that cannot find go", "go", "mango build"},
		{"a mango build that fails", "build-fails", "mango build"},
		{"an extract that fails", "python3", "extract"},
		{"an archive that fails", "archive-fails", "mango archive"},
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

			_, err := RunAnalysis(context.Background(), env, analysisOptions(town))
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

// TestRunEnvDropsNothingWhenTheSeamIsMissing is the same guard on the helper
// itself, because a nil seam that returned the command's output would be a run
// without the environment the caller asked for.
func TestRunEnvDropsNothingWhenTheSeamIsMissing(t *testing.T) {
	t.Parallel()
	argv := []string{"python3", "/repo/scripts/town-metrics", "extract"}
	_, err := runEnv(context.Background(), Env{}, argv, extractEnv("/town", "/town/.runtime/metrics/latest/docs"))
	if err == nil {
		t.Fatal("runEnv ran a command with no environment seam")
	}
	for _, name := range []string{"RunEnv", "TOWN_METRICS_TOWN", "TOWN_METRICS_DOCS"} {
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
