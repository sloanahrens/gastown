package metrics

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// resourcesTestTown is the town the resources tests read, so every path is the
// one the source joins.
const resourcesTestTown = "/town"

// resourcesTestPath is the sampler file name under the test town's resources
// directory.
func resourcesTestPath(name string) string {
	return filepath.Join(resourcesTestTown, ".runtime", "metrics", "resources", name)
}

// resourcesTestEnv serves the files, read errors and write times a case names.
// A path with no entry is a file the town does not have; a served file with no
// write time was written at the snapshot's own instant, which is fresh.
func resourcesTestEnv(now time.Time, files map[string][]byte, errs map[string]error, mods map[string]time.Time) Env {
	return Env{
		Town: resourcesTestTown,
		Now:  func() time.Time { return now },
		ReadFile: func(path string) ([]byte, error) {
			if err := errs[path]; err != nil {
				return nil, err
			}
			if data, ok := files[path]; ok {
				return data, nil
			}
			return nil, fs.ErrNotExist
		},
		ModTime: func(path string) (time.Time, error) {
			if at, ok := mods[path]; ok {
				return at, nil
			}
			if _, ok := files[path]; ok {
				return now, nil
			}
			if err := errs[path]; err != nil && !errors.Is(err, fs.ErrNotExist) {
				return now, nil
			}

			return time.Time{}, fs.ErrNotExist
		},
	}
}

// resourcesRow is one sample in the shape the sampler writes, with the ids and
// names replaced. cpu and mem are the JSON values as written, so a case can
// pass null or a string.
func resourcesRow(ts, class, component, cpu, mem string) string {
	return `{"v":1,"ts":"` + ts + `","interval_s":30,"source":"docker","class":"` + class +
		`","component":"` + component + `","name":"sandbox-a","rig":null,"polecat":null,` +
		`"bead":null,"cpu_pct":` + cpu + `,"mem_bytes":` + mem + `,"mem_limit_bytes":null}`
}

// resourcesLines joins rows into a file: each row ends with a newline, as the
// sampler writes it.
func resourcesLines(rows ...string) []byte {
	var buf bytes.Buffer
	for _, row := range rows {
		buf.WriteString(row + "\n")
	}
	return buf.Bytes()
}

func resourcesGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestSourceResourcesCollect(t *testing.T) {
	t.Parallel()
	// The hour ending at 17:45 UTC on 2026-10-24.
	midday := time.Date(2026, 10, 24, 17, 45, 0, 0, time.UTC)
	// Twenty past midnight: the hour starts in yesterday's file.
	afterMidnight := time.Date(2026, 10, 24, 0, 20, 0, 0, time.UTC)

	tests := []struct {
		name  string
		now   time.Time
		files map[string][]byte
		errs  map[string]error
		mods  map[string]time.Time
		want  map[string]any
		// wantErrHas names substrings the error Collect returns must contain.
		wantErrHas []string
		wantErr    bool
	}{
		{
			name: "rolls up the hour across midnight from a plain file and a gzipped one",
			now:  afterMidnight,
			files: map[string][]byte{
				resourcesTestPath("2026-10-23.jsonl.gz"): resourcesGzip(t, resourcesLines(
					resourcesRow("2026-10-23T23:10:00Z", "sandbox", "supervisor", "10", "100"),
					resourcesRow("2026-10-23T23:30:00Z", "sandbox", "supervisor", "20", "300"),
					resourcesRow("2026-10-23T23:45:00Z", "control-plane", "dolt", "2", "500"),
				)),
				resourcesTestPath("2026-10-24.jsonl"): append(resourcesLines(
					resourcesRow("2026-10-24T00:00:00Z", "sandbox", "supervisor", "40", "500"),
					resourcesRow("2026-10-24T00:19:30Z", "sandbox", "supervisor", "60", "700"),
					resourcesRow("2026-10-24T00:21:00Z", "sandbox", "supervisor", "90", "900"),
				), `{"v":1,"ts":"2026-10-24T00:19:`...),
			},
			want: map[string]any{
				"res_rows_1h":                     4,
				"res_skipped_rows":                0,
				"res_unmeasured_values":           0,
				"res_unreadable_files":            0,
				"res_sandbox_supervisor_cpu_mean": 40.0,
				"res_sandbox_supervisor_cpu_max":  60.0,
				"res_sandbox_supervisor_mem_mean": 500.0,
				"res_sandbox_supervisor_mem_max":  700.0,
				"res_control-plane_dolt_cpu_mean": 2.0,
				"res_control-plane_dolt_cpu_max":  2.0,
				"res_control-plane_dolt_mem_mean": 500.0,
				"res_control-plane_dolt_mem_max":  500.0,
			},
		},
		{
			name: "skips another version and malformed rows, and counts them",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): append(resourcesLines(
					`{"v":2,"ts":"2026-10-24T17:40:00Z","class":"sandbox","component":"supervisor","cpu_pct":1,"mem_bytes":1}`,
					`not json`,
					`{"v":1,"ts":"2026-10-24T17:41:00Z","component":"supervisor","cpu_pct":1,"mem_bytes":1}`,
					`{"v":1,"ts":"yesterday","class":"sandbox","component":"supervisor","cpu_pct":1,"mem_bytes":1}`,
					resourcesRow("2026-10-24T17:42:00Z", "sandbox", "supervisor", "5", "10"),
				), `{"v":1,"ts":"2026-10-24T17:4`...),
			},
			want: map[string]any{
				"res_rows_1h":                     1,
				"res_skipped_rows":                4,
				"res_unmeasured_values":           0,
				"res_unreadable_files":            0,
				"res_sandbox_supervisor_cpu_mean": 5.0,
				"res_sandbox_supervisor_cpu_max":  5.0,
				"res_sandbox_supervisor_mem_mean": 10.0,
				"res_sandbox_supervisor_mem_max":  10.0,
			},
		},
		{
			name: "null and non-numeric values are unmeasured, not zero",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					resourcesRow("2026-10-24T17:10:00Z", "sandbox", "supervisor", "null", "100"),
					resourcesRow("2026-10-24T17:20:00Z", "sandbox", "supervisor", "8", "300"),
					resourcesRow("2026-10-24T17:30:00Z", "other", "av", `"4.2"`, "50"),
				),
			},
			want: map[string]any{
				"res_rows_1h":                     3,
				"res_skipped_rows":                0,
				"res_unmeasured_values":           2,
				"res_unreadable_files":            0,
				"res_sandbox_supervisor_cpu_mean": 8.0,
				"res_sandbox_supervisor_cpu_max":  8.0,
				"res_sandbox_supervisor_mem_mean": 200.0,
				"res_sandbox_supervisor_mem_max":  300.0,
				"res_other_av_cpu_mean":           nil,
				"res_other_av_cpu_max":            nil,
				"res_other_av_mem_mean":           50.0,
				"res_other_av_mem_max":            50.0,
			},
		},
		{
			name: "a cluster is part of the column name",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					`{"v":1,"ts":"2026-10-24T17:20:00Z","cluster":"gt-sbx","class":"sandbox","component":"supervisor","cpu_pct":3,"mem_bytes":4}`,
					resourcesRow("2026-10-24T17:21:00Z", "sandbox", "supervisor", "5", "6"),
				),
			},
			want: map[string]any{
				"res_rows_1h":                            2,
				"res_skipped_rows":                       0,
				"res_unmeasured_values":                  0,
				"res_unreadable_files":                   0,
				"res_gt-sbx_sandbox_supervisor_cpu_mean": 3.0,
				"res_gt-sbx_sandbox_supervisor_cpu_max":  3.0,
				"res_gt-sbx_sandbox_supervisor_mem_mean": 4.0,
				"res_gt-sbx_sandbox_supervisor_mem_max":  4.0,
				"res_sandbox_supervisor_cpu_mean":        5.0,
				"res_sandbox_supervisor_cpu_max":         5.0,
				"res_sandbox_supervisor_mem_mean":        6.0,
				"res_sandbox_supervisor_mem_max":         6.0,
			},
		},
		{
			name: "a file that exists but cannot be read is counted and the rest fills",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					resourcesRow("2026-10-24T17:42:00Z", "sandbox", "supervisor", "5", "10"),
				),
			},
			errs: map[string]error{
				resourcesTestPath("2026-10-23.jsonl"): errors.New("permission denied"),
			},
			want: map[string]any{
				"res_rows_1h":                     1,
				"res_skipped_rows":                0,
				"res_unmeasured_values":           0,
				"res_unreadable_files":            1,
				"res_sandbox_supervisor_cpu_mean": 5.0,
				"res_sandbox_supervisor_cpu_max":  5.0,
				"res_sandbox_supervisor_mem_mean": 10.0,
				"res_sandbox_supervisor_mem_max":  10.0,
			},
		},
		{
			name: "a gzipped file that does not decompress is counted",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-23.jsonl.gz"): []byte("not gzip"),
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					resourcesRow("2026-10-24T17:42:00Z", "sandbox", "supervisor", "5", "10"),
				),
			},
			want: map[string]any{
				"res_rows_1h":                     1,
				"res_skipped_rows":                0,
				"res_unmeasured_values":           0,
				"res_unreadable_files":            1,
				"res_sandbox_supervisor_cpu_mean": 5.0,
				"res_sandbox_supervisor_cpu_max":  5.0,
				"res_sandbox_supervisor_mem_mean": 10.0,
				"res_sandbox_supervisor_mem_max":  10.0,
			},
		},
		{
			name: "an hour with no rows gives the counts and no columns",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					resourcesRow("2026-10-24T09:00:00Z", "sandbox", "supervisor", "5", "10"),
				),
			},
			want: map[string]any{
				"res_rows_1h":           0,
				"res_skipped_rows":      0,
				"res_unmeasured_values": 0,
				"res_unreadable_files":  0,
			},
		},
		{
			name: "a sampler file written within the snapshot's hour is fresh",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					resourcesRow("2026-10-24T17:10:00Z", "sandbox", "supervisor", "5", "10"),
					resourcesRow("2026-10-24T17:20:00Z", "sandbox", "supervisor", "7", "20"),
				),
			},
			mods: map[string]time.Time{
				resourcesTestPath("2026-10-24.jsonl"): time.Date(2026, 10, 24, 17, 40, 0, 0, time.UTC),
			},
			want: map[string]any{
				"res_rows_1h":                     2,
				"res_skipped_rows":                0,
				"res_unmeasured_values":           0,
				"res_unreadable_files":            0,
				"res_sandbox_supervisor_cpu_mean": 6.0,
				"res_sandbox_supervisor_cpu_max":  7.0,
				"res_sandbox_supervisor_mem_mean": 15.0,
				"res_sandbox_supervisor_mem_max":  20.0,
			},
		},
		{
			name: "a file last written before the snapshot's hour is an error naming it, not zero rows",
			now:  midday,
			files: map[string][]byte{
				resourcesTestPath("2026-10-24.jsonl"): resourcesLines(
					resourcesRow("2026-10-24T16:50:00Z", "sandbox", "supervisor", "5", "10"),
				),
			},
			mods: map[string]time.Time{
				resourcesTestPath("2026-10-24.jsonl"): time.Date(2026, 10, 24, 16, 45, 0, 0, time.UTC),
			},
			wantErr:    true,
			wantErrHas: []string{resourcesTestPath("2026-10-24.jsonl"), "2026-10-24T16:45:00Z"},
		},
		{
			name:    "no sampler file for either day is an error, not an empty group",
			now:     midday,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := resourcesTestEnv(tt.now, tt.files, tt.errs, tt.mods)
			got, err := resourcesSource{}.Collect(context.Background(), env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Collect error = %v, want error %v", err, tt.wantErr)
			}
			for _, want := range tt.wantErrHas {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Collect error = %v, want it to contain %q", err, want)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Collect =\n%v\nwant\n%v", got, tt.want)
			}
		})
	}
}

// A sampler file that exists but whose write time cannot be read is not judged
// healthy: the snapshot cannot tell whether the writer stopped, so the group is
// an error and no columns are reported.
func TestSourceResourcesCollectRefusesAFileWhoseWriteTimeIsUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 16, 30, 0, 0, time.UTC)
	path := resourcesTestPath("2026-10-25.jsonl")
	files := map[string][]byte{path: []byte(resourcesLines(
		resourcesRow("2026-10-25T16:20:00Z", "app", "web", "10", "1000"),
	))}
	env := resourcesTestEnv(now, files, nil, nil)
	modTime := env.ModTime
	env.ModTime = func(p string) (time.Time, error) {
		if p == path {
			return time.Time{}, errors.New("stat: permission denied")
		}
		return modTime(p)
	}
	cols, err := resourcesSource{}.Collect(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "last write time unknown") {
		t.Fatalf("Collect = (%v, %v), want an error naming the unknown write time", cols, err)
	}
	if cols != nil {
		t.Errorf("columns = %v, want none for a file whose write time is unknown", cols)
	}
}

// The staleness rule is silence, not the clock hour: a live sampler writes about
// every thirty seconds, so a snapshot just past an hour or midnight boundary
// still reads its newest file as fresh, and only a writer silent for longer than
// resourcesStaleAfter is an error (gt-vfsi6).
func TestSourceResourcesCollectJudgesSilenceNotTheHour(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		now     time.Time
		file    string
		row     string
		written time.Time
		stale   bool
	}{
		{
			name:    "a live sampler just past the hour is fresh",
			now:     time.Date(2026, 10, 25, 16, 0, 10, 0, time.UTC),
			file:    "2026-10-25.jsonl",
			row:     resourcesRow("2026-10-25T15:45:00Z", "sandbox", "supervisor", "5", "10"),
			written: time.Date(2026, 10, 25, 15, 59, 40, 0, time.UTC),
		},
		{
			name:    "a live sampler just past midnight is fresh",
			now:     time.Date(2026, 10, 25, 0, 5, 0, 0, time.UTC),
			file:    "2026-10-24.jsonl",
			row:     resourcesRow("2026-10-24T23:50:00Z", "sandbox", "supervisor", "5", "10"),
			written: time.Date(2026, 10, 24, 23, 58, 0, 0, time.UTC),
		},
		{
			name:    "a sampler silent for twenty minutes is stale",
			now:     time.Date(2026, 10, 25, 16, 30, 0, 0, time.UTC),
			file:    "2026-10-25.jsonl",
			row:     resourcesRow("2026-10-25T16:05:00Z", "sandbox", "supervisor", "5", "10"),
			written: time.Date(2026, 10, 25, 16, 10, 0, 0, time.UTC),
			stale:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := resourcesTestPath(tc.file)
			files := map[string][]byte{path: resourcesLines(tc.row)}
			mods := map[string]time.Time{path: tc.written}
			env := resourcesTestEnv(tc.now, files, nil, mods)
			_, err := resourcesSource{}.Collect(context.Background(), env)
			if tc.stale != (err != nil) {
				t.Errorf("Collect error = %v, want stale %v", err, tc.stale)
			}
		})
	}
}
