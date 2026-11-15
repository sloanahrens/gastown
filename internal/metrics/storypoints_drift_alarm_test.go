package metrics

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// driftAlarmColumns is the header of the fixture's landings table: the cells the
// drift window reads and nothing else, since ReadLandings reads any header.
var driftAlarmColumns = []string{
	"bead", "landed_at", "sp_minutes", "polecat_secs",
	"polecat_lower_bound", "sp_estimated_at", "dispatched_at",
}

// driftAlarmEnv is a fixture town whose reads go to the filesystem under its
// temp directory.
func driftAlarmEnv(t *testing.T) (string, Env) {
	t.Helper()
	town := t.TempDir()
	return town, Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir}
}

// writeDriftExtract writes rows as one day's landings table under town and
// returns the day's directory.
func writeDriftExtract(t *testing.T, town, day string, rows []Row) string {
	t.Helper()
	dir := filepath.Join(town, ".runtime", "metrics", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var buf bytes.Buffer
	if err := WriteLandings(&buf, driftAlarmColumns, rows); err != nil {
		t.Fatalf("WriteLandings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, landingsName), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write landings: %v", err)
	}
	return dir
}

// TestStoryPointsDriftReadReadsTheNewestExtract holds the read to the extract
// the shadow report reads: today's day directory when it has a table, else the
// newest day that does.
func TestStoryPointsDriftReadReadsTheNewestExtract(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	drifted := func(i int) float64 { return float64(30 - i) } // reverses the estimate
	withTheEstimate := func(i int) float64 { return float64(i + 1) }
	tests := []struct {
		name    string
		days    map[string][]Row
		now     time.Time
		wantDay string
		wantRho float64
	}{
		{
			name: "today has a table",
			days: map[string][]Row{
				"2026-09-19": driftLandings(30, base, withTheEstimate),
				"2026-09-20": driftLandings(30, base, drifted),
			},
			now:     time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
			wantDay: "2026-09-20",
			wantRho: -1,
		},
		{
			name: "today has none, so the newest day answers",
			days: map[string][]Row{
				"2026-09-18": driftLandings(30, base, drifted),
				"2026-09-19": driftLandings(30, base, withTheEstimate),
			},
			now:     time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			wantDay: "2026-09-19",
			wantRho: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, env := driftAlarmEnv(t)
			for day, rows := range tc.days {
				writeDriftExtract(t, town, day, rows)
			}
			read, err := ReadStoryPointsDrift(env, tc.now)
			if err != nil {
				t.Fatalf("ReadStoryPointsDrift: %v", err)
			}
			if want := filepath.Join(town, ".runtime", "metrics", tc.wantDay); read.Dir != want {
				t.Errorf("Dir = %q, want %q", read.Dir, want)
			}
			if !read.Drift.Current.Measurable || math.Abs(read.Drift.Current.Rho-tc.wantRho) > 1e-12 {
				t.Errorf("Current = %+v, want a measurable %.2f", read.Drift.Current, tc.wantRho)
			}
			if read.Drift.N != 30 {
				t.Errorf("N = %d, want 30", read.Drift.N)
			}
			if read.Weights != storyPointsCheckedInWeights {
				t.Errorf("Weights = %q, want %q with no runtime file", read.Weights, storyPointsCheckedInWeights)
			}
		})
	}
}

// TestStoryPointsDriftReadRefusesAnUnreadableTable: every input the read cannot
// take is an error naming it, never a zero reading a caller could compare with
// the bar.
func TestStoryPointsDriftReadRefusesAnUnreadableTable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		setup     func(t *testing.T, town string)
		env       func(Env) Env
		zeroClock bool
		substr    []string
	}{
		{
			name:   "no extract at all",
			substr: []string{"no " + landingsName, filepath.Join(".runtime", "metrics")},
		},
		{
			name: "an unreadable table",
			setup: func(t *testing.T, town string) {
				// A directory where the table should be makes the read fail
				// with the platform's own error, which the notice carries.
				dir := filepath.Join(town, ".runtime", "metrics", "2026-09-20", landingsName)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			substr: []string{filepath.Join("2026-09-20", landingsName)},
		},
		{
			name: "a cell that does not parse",
			setup: func(t *testing.T, town string) {
				writeDriftExtract(t, town, "2026-09-20", []Row{
					rowWith("sp_estimated_at", "not a time")(driftRow("gt-a", base, 5, 300)),
				})
			},
			substr: []string{filepath.Join("2026-09-20", landingsName), "sp_estimated_at"},
		},
		{
			name: "a readable table beside a weights file the estimator refuses",
			setup: func(t *testing.T, town string) {
				writeDriftExtract(t, town, "2026-09-20", driftLandings(30, base, func(i int) float64 {
					return float64(i + 1)
				}))
				writeWeightsFile(t, filepath.Join(town, ".runtime", "storypoints", "weights.json"), "{not json")
			},
			substr: []string{filepath.Join(".runtime", "storypoints", "weights.json")},
		},
		{
			name: "a ragged table",
			setup: func(t *testing.T, town string) {
				dir := filepath.Join(town, ".runtime", "metrics", "2026-09-20")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				ragged := "bead,landed_at\n" + "gt-a,2026-09-19T10:00:00Z,extra\n"
				if err := os.WriteFile(filepath.Join(dir, landingsName), []byte(ragged), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			substr: []string{filepath.Join("2026-09-20", landingsName)},
		},
		{
			name:   "no town",
			env:    func(e Env) Env { e.Town = ""; return e },
			substr: []string{"Env.Town is unset"},
		},
		{
			name:   "no file seam",
			env:    func(e Env) Env { e.ReadFile = nil; return e },
			substr: []string{"Env.ReadFile is unset"},
		},
		{
			name:      "no clock",
			zeroClock: true,
			substr:    []string{"the clock is unset"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, env := driftAlarmEnv(t)
			if tc.env != nil {
				env = tc.env(env)
			}
			if tc.setup != nil {
				tc.setup(t, town)
			}
			at := now
			if tc.zeroClock {
				at = time.Time{}
			}
			read, err := ReadStoryPointsDrift(env, at)
			if err == nil {
				t.Fatalf("ReadStoryPointsDrift = %+v, want an error", read)
			}
			for _, want := range tc.substr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestStoryPointsDriftWeightsVersionTable: the item names the weights the
// estimator runs, and a runtime file it cannot read is an error rather than a
// name for a model the town is not running.
func TestStoryPointsDriftWeightsVersionTable(t *testing.T) {
	t.Parallel()
	const versionPath = ".runtime/storypoints/weights.json"
	tests := []struct {
		name   string
		setup  func(t *testing.T, town string)
		env    func(Env) Env
		want   string
		substr string
	}{
		{
			name: "no runtime file",
			want: storyPointsCheckedInWeights,
		},
		{
			name: "the runtime file's version",
			setup: func(t *testing.T, town string) {
				writeWeightsFile(t, filepath.Join(town, versionPath), trainTestBaseJSON)
			},
			want: "v1",
		},
		{
			name: "an unreadable runtime file",
			setup: func(t *testing.T, town string) {
				if err := os.MkdirAll(filepath.Join(town, versionPath), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			substr: versionPath,
		},
		{
			name: "a runtime file that is not JSON",
			setup: func(t *testing.T, town string) {
				writeWeightsFile(t, filepath.Join(town, versionPath), "{not json")
			},
			substr: versionPath,
		},
		{
			name: "a runtime file of another version",
			setup: func(t *testing.T, town string) {
				writeWeightsFile(t, filepath.Join(town, versionPath), `{"version":2}`)
			},
			substr: "version 2",
		},
		{
			name:   "no town",
			env:    func(e Env) Env { e.Town = ""; return e },
			substr: "Env.Town is unset",
		},
		{
			name:   "no file seam",
			env:    func(e Env) Env { e.ReadFile = nil; return e },
			substr: "Env.ReadFile is unset",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, env := driftAlarmEnv(t)
			if tc.env != nil {
				env = tc.env(env)
			}
			if tc.setup != nil {
				tc.setup(t, town)
			}
			got, err := StoryPointsWeightsVersion(env)
			if tc.substr != "" {
				if err == nil {
					t.Fatalf("StoryPointsWeightsVersion = %q, want an error", got)
				}
				if !strings.Contains(err.Error(), tc.substr) {
					t.Errorf("err = %q, want it to name %q", err, tc.substr)
				}
				return
			}
			if err != nil {
				t.Fatalf("StoryPointsWeightsVersion: %v", err)
			}
			if got != tc.want {
				t.Errorf("StoryPointsWeightsVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

// writeWeightsFile writes data at path, creating its directory.
func writeWeightsFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
