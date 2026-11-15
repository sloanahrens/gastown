package metrics

import (
	"bytes"
	"errors"
	"io/fs"
	"math"
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

// driftTown is the town root the fixture reads under. Nothing is written under
// it: the fake filesystem holds the files.
const driftTown = "/town"

// driftFS is an in-memory filesystem behind Env.ReadFile and Env.ReadDir. files
// holds what a path reads as, dirs what a directory lists, and errs the error a
// path answers with instead.
type driftFS struct {
	files map[string][]byte
	dirs  map[string][]DirEntry
	errs  map[string]error
}

func newDriftFS() *driftFS {
	return &driftFS{files: map[string][]byte{}, dirs: map[string][]DirEntry{}, errs: map[string]error{}}
}

// ReadFile answers the path's error, then its contents, then a missing file.
func (f *driftFS) ReadFile(path string) ([]byte, error) {
	if err, ok := f.errs[path]; ok {
		return nil, err
	}
	data, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return data, nil
}

// ReadDir lists the directory, or reports it missing.
func (f *driftFS) ReadDir(path string) ([]DirEntry, error) {
	entries, ok := f.dirs[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return entries, nil
}

// driftAlarmEnv is the fixture town and the Env whose file reads go to the fake
// filesystem.
func driftAlarmEnv(t *testing.T) (*driftFS, Env) {
	t.Helper()
	fsys := newDriftFS()
	return fsys, Env{Town: driftTown, ReadFile: fsys.ReadFile, ReadDir: fsys.ReadDir}
}

// writeDriftExtract writes rows as one day's landings table under the town and
// returns the day's directory.
func writeDriftExtract(t *testing.T, fsys *driftFS, day string, rows []Row) string {
	t.Helper()
	root := filepath.Join(driftTown, ".runtime", "metrics")
	dir := filepath.Join(root, day)
	fsys.dirs[root] = append(fsys.dirs[root], DirEntry{Name: day, IsDir: true})
	var buf bytes.Buffer
	if err := WriteLandings(&buf, driftAlarmColumns, rows); err != nil {
		t.Fatalf("WriteLandings: %v", err)
	}
	fsys.files[filepath.Join(dir, landingsName)] = buf.Bytes()
	return dir
}

// writeWeightsFile writes data as the runtime weights file at path.
func writeWeightsFile(fsys *driftFS, path, data string) {
	fsys.files[path] = []byte(data)
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
			fsys, env := driftAlarmEnv(t)
			for day, rows := range tc.days {
				writeDriftExtract(t, fsys, day, rows)
			}
			read, err := ReadStoryPointsDrift(env, tc.now)
			if err != nil {
				t.Fatalf("ReadStoryPointsDrift: %v", err)
			}
			if want := filepath.Join(driftTown, ".runtime", "metrics", tc.wantDay); read.Dir != want {
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
	todayTable := filepath.Join(driftTown, ".runtime", "metrics", "2026-09-20", landingsName)
	weightsPath := filepath.Join(driftTown, ".runtime", "storypoints", "weights.json")
	tests := []struct {
		name      string
		setup     func(t *testing.T, fsys *driftFS)
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
			setup: func(t *testing.T, fsys *driftFS) {
				// A read that fails with a platform error names the table.
				fsys.errs[todayTable] = errors.New("read: is a directory")
			},
			substr: []string{filepath.Join("2026-09-20", landingsName)},
		},
		{
			name: "a cell that does not parse",
			setup: func(t *testing.T, fsys *driftFS) {
				writeDriftExtract(t, fsys, "2026-09-20", []Row{
					rowWith("sp_estimated_at", "not a time")(driftRow("gt-a", base, 5, 300)),
				})
			},
			substr: []string{filepath.Join("2026-09-20", landingsName), "sp_estimated_at"},
		},
		{
			name: "a readable table beside a weights file the estimator refuses",
			setup: func(t *testing.T, fsys *driftFS) {
				writeDriftExtract(t, fsys, "2026-09-20", driftLandings(30, base, func(i int) float64 {
					return float64(i + 1)
				}))
				writeWeightsFile(fsys, weightsPath, "{not json")
			},
			substr: []string{filepath.Join(".runtime", "storypoints", "weights.json")},
		},
		{
			name: "a ragged table",
			setup: func(t *testing.T, fsys *driftFS) {
				fsys.files[todayTable] = []byte("bead,landed_at\n" + "gt-a,2026-09-19T10:00:00Z,extra\n")
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
			fsys, env := driftAlarmEnv(t)
			if tc.env != nil {
				env = tc.env(env)
			}
			if tc.setup != nil {
				tc.setup(t, fsys)
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
	versionPath := filepath.Join(driftTown, ".runtime", "storypoints", "weights.json")
	tests := []struct {
		name   string
		setup  func(t *testing.T, fsys *driftFS)
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
			setup: func(t *testing.T, fsys *driftFS) {
				writeWeightsFile(fsys, versionPath, trainTestBaseJSON)
			},
			want: "v1",
		},
		{
			name: "an unreadable runtime file",
			setup: func(t *testing.T, fsys *driftFS) {
				fsys.errs[versionPath] = errors.New("permission denied")
			},
			substr: versionPath,
		},
		{
			name: "a runtime file that is not JSON",
			setup: func(t *testing.T, fsys *driftFS) {
				writeWeightsFile(fsys, versionPath, "{not json")
			},
			substr: versionPath,
		},
		{
			name: "a runtime file of another version",
			setup: func(t *testing.T, fsys *driftFS) {
				writeWeightsFile(fsys, versionPath, `{"version":2}`)
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
			fsys, env := driftAlarmEnv(t)
			if tc.env != nil {
				env = tc.env(env)
			}
			if tc.setup != nil {
				tc.setup(t, fsys)
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
