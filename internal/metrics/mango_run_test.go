package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mangoCannedResult is what the fake Runner writes where explore asked for its
// output. It carries a key this package does not read, as the CLI will when it
// grows one.
const mangoCannedResult = `{
  "target": "manual_rescue",
  "positive": 1,
  "baseline": 0.1,
  "rows": 100,
  "hypercubes": [{"ranges": [{"column": "rig", "categories": ["a"]}], "n": 28, "purity": 0.4}],
  "stats": {
    "candidates": 200,
    "future_counter": {"nested": true},
    "generate": {
      "iterations": 10,
      "germsTried": 200,
      "generated": 200,
      "valid": 1,
      "significant": 1,
      "unique": 0,
      "skippedColumns": [{"column": "x", "reason": "too few distinct values"}]
    }
  }
}`

// fakeMango returns a Runner that records every argv and writes result where
// explore asked for it, standing in for the CLI without running one.
func fakeMango(t *testing.T, dir, result string) (Runner, *[][]string) {
	t.Helper()
	calls := &[][]string{}
	run := func(_ context.Context, argv []string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), argv...))
		if len(argv) > 1 && argv[1] == "explore" {
			return nil, os.WriteFile(filepath.Join(dir, "result.json"), []byte(result), 0o644)
		}
		return nil, nil
	}
	return run, calls
}

func TestRunMangoRunsIngestThenExploreWithTheExperimentFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		options MangoOptions
		explore []string
	}{
		{
			name:    "the zero options run the report's experiment",
			options: MangoOptions{},
			explore: []string{
				"--target", "manual_rescue", "--positive", "1", "--seed", "20261019",
				"--permutations", "1000", "--min-size", "30", "--min-purity", "0.15", "--widen",
			},
		},
		{
			name:    "a smaller support floor without widening",
			options: MangoOptions{MinSupport: 12, NoWiden: true},
			explore: []string{
				"--target", "manual_rescue", "--positive", "1", "--seed", "20261019",
				"--permutations", "1000", "--min-size", "12", "--min-purity", "0.15",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			run, calls := fakeMango(t, dir, mangoCannedResult)
			if _, err := RunMango(context.Background(), run, "/opt/mango", dir, "manual_rescue", tc.options); err != nil {
				t.Fatalf("RunMango: %v", err)
			}
			want := [][]string{
				{"/opt/mango", "ingest", filepath.Join(dir, "train.csv"), "--out", filepath.Join(dir, "train.parquet")},
				append([]string{"/opt/mango", "explore", filepath.Join(dir, "train.parquet")},
					append(tc.explore, "--out", filepath.Join(dir, "result.json"))...),
			}
			if !reflect.DeepEqual(*calls, want) {
				t.Errorf("argv = %q, want the ingest and explore commands %q", *calls, want)
			}
		})
	}
}

func TestRunMangoParsesTheBoxesAndSearchCounters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run, _ := fakeMango(t, dir, mangoCannedResult)
	got, err := RunMango(context.Background(), run, "/opt/mango", dir, "manual_rescue", MangoOptions{})
	if err != nil {
		t.Fatalf("RunMango: %v", err)
	}
	if got.Iterations != 10 || got.Germs != 200 || got.Candidates != 200 || got.Generated != 200 ||
		got.Valid != 1 || got.Significant != 1 {
		t.Errorf("counters = %+v, want the canned iterations 10, germs 200, candidates 200, generated 200, valid 1, significant 1", got)
	}
	if want := []SkippedColumn{{"x", "too few distinct values"}}; !reflect.DeepEqual(got.SkippedColumns, want) {
		t.Errorf("skipped columns = %v, want %v", got.SkippedColumns, want)
	}
	if len(got.Boxes) != 1 || got.Boxes[0].N != 28 || got.Boxes[0].Purity != 0.4 {
		t.Fatalf("boxes = %+v, want the canned box's n 28 and purity 0.4", got.Boxes)
	}
	if got, want := Describe(got.Boxes[0].Cube), "rig in {a}"; got != want {
		t.Errorf("box = %q, want %q", got, want)
	}
}

func TestRunMangoFailsWithTheOutputTail(t *testing.T) {
	t.Parallel()
	const output = "boom: bad parquet"
	long := strings.Repeat("x", 2000) + output
	tests := []struct {
		name      string
		failAfter int
		wantStage string
	}{
		{"ingest fails", 0, "mango ingest failed"},
		{"explore fails", 1, "mango explore failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			call := 0
			run := func(_ context.Context, argv []string) ([]byte, error) {
				if call == tc.failAfter {
					return []byte(long), errors.New("exit 2")
				}
				call++
				return nil, os.WriteFile(filepath.Join(dir, "result.json"), []byte(mangoCannedResult), 0o644)
			}
			_, err := RunMango(context.Background(), run, "mango", dir, "t", MangoOptions{})
			if err == nil {
				t.Fatal("RunMango returned no error for a failing command")
			}
			if !strings.Contains(err.Error(), tc.wantStage) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantStage)
			}
			if !strings.HasSuffix(err.Error(), tailString(long, mangoOutputTail)) {
				t.Errorf("error = %q, want it to end with the last %d bytes of the output", err, mangoOutputTail)
			}
			if len(err.Error()) >= len(long) {
				t.Errorf("error is %d bytes, want only the output's tail", len(err.Error()))
			}
		})
	}
}

// mangoCheckGolden is internal/metrics/testdata/mango_check_cases.json,
// produced by running the Python check_holdout over each case's rows and boxes.
// The Python test scripts/test_town_metrics_mango_check_golden.py reads the
// same file, so the two implementations cannot drift apart unseen.
type mangoCheckGolden struct {
	Cases []struct {
		Name       string `json:"name"`
		Target     string `json:"target"`
		MinSupport int    `json:"min_support"`
		Rows       []Row  `json:"rows"`
		Boxes      []Box  `json:"boxes"`
		Want       []struct {
			Box        string  `json:"box"`
			TrainN     int     `json:"train_n"`
			TrainRate  float64 `json:"train_rate"`
			N          int     `json:"n"`
			K          int     `json:"k"`
			Rate       float64 `json:"rate"`
			Baseline   float64 `json:"baseline"`
			P          float64 `json:"p_holm"`
			Replicates bool    `json:"replicates"`
			Label      string  `json:"label"`
		} `json:"want"`
	} `json:"cases"`
}

const mangoTol = 1e-9

func TestCheckHoldoutMatchesThePythonNumbers(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "mango_check_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden mangoCheckGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("no cases in the golden file")
	}
	belowBaseline := false
	for _, c := range golden.Cases {
		for _, w := range c.Want {
			if w.N > 0 && w.Rate < w.Baseline && !w.Replicates {
				belowBaseline = true
			}
		}
	}
	if !belowBaseline {
		t.Fatal("the golden has no box below the baseline reported as not replicating")
	}
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := CheckHoldout(c.Boxes, c.Rows, c.Target, c.MinSupport)
			if len(got) != len(c.Want) {
				t.Fatalf("checks = %d, want the Python's %d", len(got), len(c.Want))
			}
			for i, w := range c.Want {
				g := got[i]
				if g.Box != w.Box || g.N != w.N || g.K != w.K || g.TrainN != w.TrainN ||
					g.Replicates != w.Replicates || g.Label != w.Label {
					t.Errorf("check %d = %+v, want the Python's %+v", i, g, w)
				}
				for _, f := range []struct {
					name string
					got  float64
					want float64
				}{
					{"rate", g.Rate, w.Rate},
					{"train rate", g.TrainRate, w.TrainRate},
					{"baseline", g.Baseline, w.Baseline},
					{"p", g.P, w.P},
				} {
					if math.Abs(f.got-f.want) > mangoTol {
						t.Errorf("check %d %s = %v, want the Python's %v", i, f.name, f.got, f.want)
					}
				}
			}
		})
	}
}

func TestCheckHoldoutMeasuresATarget(t *testing.T) {
	t.Parallel()
	rows := make([]Row, 10)
	for i := range rows {
		rows[i] = Row{
			"landed_at": fmt.Sprintf("2026-10-%02dT00:00:00Z", i+1),
			"rig":       "a",
			"t":         "1",
		}
		if i >= 7 {
			rows[i]["rig"] = "b"
			rows[i]["t"] = "0" // the holdout's early rows are positives only in train
		}
	}
	rows[8]["t"] = "1"
	rows[9]["t"] = "0"
	p := Prepare([]string{"landed_at", "rig", "t"}, rows, nil, "t")
	boxes := []Box{{Cube: Cube{Ranges: []Range{{Column: "rig", Categories: []string{"a"}}}}, N: 40, Purity: 0.6}}
	got := measureTarget("t", p, boxes, 1)
	if got.TrainN != 7 || got.TrainPositives != 7 || got.HoldoutN != 3 || got.HoldoutPositives != 1 {
		t.Errorf("counts = train %d/%d holdout %d/%d, want train 7/7 and holdout 3/1",
			got.TrainN, got.TrainPositives, got.HoldoutN, got.HoldoutPositives)
	}
	if got.Boxes != 1 || got.Best == nil || got.Best.Box != "rig in {a}" {
		t.Errorf("boxes = %d, best = %+v, want the single box rig in {a}", got.Boxes, got.Best)
	}
}

// TestMeasureTargetSplitsTheBestBoxOverEveryPreparedRow hands the panel the
// best box's conditions, each with its share of the train and holdout rows
// together: a share taken over one side of the split would be a different
// fraction.
func TestMeasureTargetSplitsTheBestBoxOverEveryPreparedRow(t *testing.T) {
	t.Parallel()

	rows := make([]Row, 10)
	for i := range rows {
		rows[i] = Row{
			"landed_at": fmt.Sprintf("2026-10-%02dT00:00:00Z", i+1),
			"rig":       "a",
			"t":         "1",
		}
	}
	for i := 7; i < len(rows); i++ {
		rows[i]["rig"] = "b"
		rows[i]["t"] = "0"
	}
	rows[8]["t"] = "1"
	p := Prepare([]string{"landed_at", "rig", "t"}, rows, nil, "t")
	boxes := []Box{{Cube: Cube{Ranges: []Range{
		{Column: "rig", Categories: []string{"a"}},
		{Column: "t", Min: 1, Max: 1},
	}}, N: 7, Purity: 1}}

	got := measureTarget("t", p, boxes, 1)
	want := []MangoCondition{
		{Text: "rig in {a}", Share: 0.7, Narrows: true},
		{Text: "t = 1", Share: 0.8, Narrows: true},
	}
	if !reflect.DeepEqual(got.Conditions, want) {
		t.Errorf("conditions = %+v, want %+v over all ten prepared rows", got.Conditions, want)
	}

	if got := measureTarget("t", p, nil, 1); got.Conditions != nil {
		t.Errorf("a target with no boxes carries conditions %+v, want none", got.Conditions)
	}
}

func TestBestBoxPicksTheReplicatingBoxThenTheLowestP(t *testing.T) {
	t.Parallel()
	// c is shorthand for a check with only the fields BestBox reads.
	c := func(replicates bool, rate, p float64) BoxCheck {
		return BoxCheck{Replicates: replicates, Rate: rate, P: p}
	}
	tests := []struct {
		name   string
		checks []BoxCheck
		want   int // index into checks, -1 for nil
	}{
		{"no boxes", nil, -1},
		{"one replicating box", []BoxCheck{c(false, 0.5, 0.001), c(true, 0.6, 0.01)}, 1},
		{"several replicating boxes take the highest rate", []BoxCheck{c(true, 0.6, 0.01), c(true, 0.8, 0.04), c(true, 0.7, 0.001)}, 1},
		{"no replicating box, the lowest p", []BoxCheck{c(false, 0.1, 0.2), c(false, 0.2, 0.03), c(false, 0.3, 0.05)}, 1},
		{"a tiny p below the baseline loses to a replicating box", []BoxCheck{c(false, 0.0, 1e-9), c(true, 0.2, 0.04)}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BestBox(tc.checks)
			if tc.want < 0 {
				if got != nil {
					t.Errorf("BestBox = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("BestBox = nil, want check %d", tc.want)
			}
			if !reflect.DeepEqual(*got, tc.checks[tc.want]) {
				t.Errorf("BestBox = %+v, want check %d %+v", *got, tc.want, tc.checks[tc.want])
			}
		})
	}
}
