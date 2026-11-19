package dashboard

import (
	"slices"
	"testing"
	"time"
)

// TestParseJobs is the section's table: the array a good read gives, the reads
// that are refuse, and the worst state each case computes.
func TestParseJobs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		out        string
		wantErr    bool
		wantStates []string
		wantWorst  string
	}{
		{
			name:       "a good array",
			out:        `[{"name":"dolt_gc","state":"ok","outcome":"ok"},{"name":"cache_trim","state":"overdue"}]`,
			wantStates: []string{"ok", "overdue"},
			wantWorst:  "overdue",
		},
		{
			name:    "an object",
			out:     `{"name":"dolt_gc","state":"ok"}`,
			wantErr: true,
		},
		{
			name:    "empty output",
			out:     "",
			wantErr: true,
		},
		{
			name:       "a row with an unknown state",
			out:        `[{"name":"dolt_gc","state":"wedged"}]`,
			wantStates: []string{"unknown"},
			wantWorst:  "unknown",
		},
		{
			name:       "a row with no state",
			out:        `[{"name":"dolt_gc","outcome":"ok"}]`,
			wantStates: []string{"unknown"},
			wantWorst:  "unknown",
		},
		{
			name:       "an outcome the section does not know",
			out:        `[{"name":"dolt_gc","state":"ok","outcome":"wedged"}]`,
			wantStates: []string{"unknown"},
			wantWorst:  "unknown",
		},
		{
			name:    "null",
			out:     "null",
			wantErr: true,
		},
		{
			name:    "text that is not JSON",
			out:     "jobs: none",
			wantErr: true,
		},
		{
			name:    "an element that is not an object",
			out:     `["dolt_gc"]`,
			wantErr: true,
		},
		{
			name:    "a row without a name",
			out:     `[{"name":"dolt_gc","state":"ok"},{"name":" ","state":"ok"}]`,
			wantErr: true,
		},
		{
			name:       "an empty array",
			out:        `[]`,
			wantStates: []string{},
			wantWorst:  "",
		},
		{
			name:       "every job ok",
			out:        `[{"name":"a","state":"ok"},{"name":"b","state":"ok","outcome":"ok"}]`,
			wantStates: []string{"ok", "ok"},
			wantWorst:  "ok",
		},
		{
			name:       "unknown is worse than overdue",
			out:        `[{"name":"a","state":"overdue"},{"name":"b","state":"unknown"}]`,
			wantStates: []string{"overdue", "unknown"},
			wantWorst:  "unknown",
		},
		{
			name:       "overdue is worse than a failed outcome",
			out:        `[{"name":"a","state":"overdue"},{"name":"b","state":"ok","outcome":"failed"}]`,
			wantStates: []string{"overdue", "ok"},
			wantWorst:  "overdue",
		},
		{
			name:       "a failed outcome is worse than never_run",
			out:        `[{"name":"a","state":"never_run"},{"name":"b","state":"ok","outcome":"failed"}]`,
			wantStates: []string{"never_run", "ok"},
			wantWorst:  "failed",
		},
		{
			name:       "never_run is worse than ok",
			out:        `[{"name":"a","state":"ok"},{"name":"b","state":"never_run"}]`,
			wantStates: []string{"ok", "never_run"},
			wantWorst:  "never_run",
		},
		{
			name:       "no_cadence is worse than ok",
			out:        `[{"name":"a","state":"ok"},{"name":"b","state":"no_cadence"}]`,
			wantStates: []string{"ok", "no_cadence"},
			wantWorst:  "no_cadence",
		},
		{
			name:       "a refused run is not ok",
			out:        `[{"name":"a","state":"ok"},{"name":"b","state":"ok","outcome":"refused"}]`,
			wantStates: []string{"ok", "ok"},
			wantWorst:  "refused",
		},
		{
			name:       "a deferred run is not ok",
			out:        `[{"name":"a","state":"ok"},{"name":"b","state":"ok","outcome":"deferred"}]`,
			wantStates: []string{"ok", "ok"},
			wantWorst:  "deferred",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseJobs([]byte(tc.out))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseJobs(%q) = %+v, want an error", tc.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJobs(%q): %v", tc.out, err)
			}
			states := make([]string, 0, len(got.Rows))
			for _, r := range got.Rows {
				states = append(states, r.State)
			}
			if !slices.Equal(states, tc.wantStates) {
				t.Errorf("states = %v, want %v", states, tc.wantStates)
			}
			if got.Worst != tc.wantWorst {
				t.Errorf("worst = %q, want %q", got.Worst, tc.wantWorst)
			}
		})
	}
}

// TestParseJobsRowFields is what a row carries: its name trimmed, its last run,
// its headline stat, and its newest runs with the wall time of the newest one.
func TestParseJobsRowFields(t *testing.T) {
	t.Parallel()

	out := `[{"name":" dolt_gc ","state":"ok","outcome":"ok","last_run":"2026-11-19T09:00:00Z",
	  "stats":{"build_cache_bytes":9,"reclaimed_bytes":42,"databases":2},
	  "runs":[
	    {"started_at":"2026-11-19T08:00:00Z","ended_at":"2026-11-19T08:00:02Z","outcome":"ok"},
	    {"started_at":"2026-11-19T07:00:00Z","ended_at":"2026-11-19T07:00:01Z","outcome":"ok"},
	    {"started_at":"2026-11-19T06:00:00Z","ended_at":"2026-11-19T06:00:03Z","outcome":"failed"},
	    {"started_at":"2026-11-19T05:00:00Z","ended_at":"2026-11-19T05:00:04Z","outcome":"ok"},
	    {"started_at":"2026-11-19T04:00:00Z","ended_at":"2026-11-19T04:00:05Z","outcome":"ok"},
	    {"started_at":"2026-11-19T03:00:00Z","ended_at":"2026-11-19T03:00:06Z","outcome":"ok"}
	  ]}]`
	got, err := ParseJobs([]byte(out))
	if err != nil {
		t.Fatalf("ParseJobs: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(got.Rows))
	}
	row := got.Rows[0]
	if row.Name != "dolt_gc" {
		t.Errorf("name = %q, want %q", row.Name, "dolt_gc")
	}
	if want := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC); row.LastRun == nil || !row.LastRun.Equal(want) {
		t.Errorf("last run = %v, want %v", row.LastRun, want)
	}
	if want := (JobStat{Name: "reclaimed_bytes", Value: 42}); row.Headline == nil || *row.Headline != want {
		t.Errorf("headline = %+v, want %+v", row.Headline, want)
	}
	if len(row.Runs) != jobsRunCap {
		t.Fatalf("runs = %d, want %d", len(row.Runs), jobsRunCap)
	}
	for i, want := range []string{"08:00:02", "07:00:01", "06:00:03", "05:00:04", "04:00:05"} {
		at := row.Runs[i].EndedAt.Format("15:04:05")
		if at != want {
			t.Errorf("run %d ended %s, want %s: the newest %d must lead", i, at, want, jobsRunCap)
		}
	}
	if row.DurationMS == nil || *row.DurationMS != 2000 {
		t.Errorf("duration = %v, want 2000 ms", row.DurationMS)
	}
}

// TestParseJobsHeadlineOrder is which stat a row leads with when it holds
// several, and that a row holding none of them has no headline.
func TestParseJobsHeadlineOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		stats string
		want  *JobStat
	}{
		{
			name:  "used percent first",
			stats: `{"used_percent":80,"reclaimed_bytes":1}`,
			want:  &JobStat{Name: "used_percent", Value: 80},
		},
		{
			name:  "reclaimed bytes next",
			stats: `{"reclaimed_bytes":5,"build_cache_bytes":9,"bytes_freed":3,"duration_ms":7}`,
			want:  &JobStat{Name: "reclaimed_bytes", Value: 5},
		},
		{
			name:  "build cache bytes next",
			stats: `{"build_cache_bytes":9,"bytes_freed":3,"duration_ms":7}`,
			want:  &JobStat{Name: "build_cache_bytes", Value: 9},
		},
		{
			name:  "bytes freed next",
			stats: `{"bytes_freed":3,"duration_ms":7}`,
			want:  &JobStat{Name: "bytes_freed", Value: 3},
		},
		{
			name:  "duration last",
			stats: `{"duration_ms":7}`,
			want:  &JobStat{Name: "duration_ms", Value: 7},
		},
		{
			name:  "no headline stat",
			stats: `{"databases":2,"containers":4}`,
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseJobs([]byte(`[{"name":"a","state":"ok","stats":` + tc.stats + `}]`))
			if err != nil {
				t.Fatalf("ParseJobs: %v", err)
			}
			headline := got.Rows[0].Headline
			if tc.want == nil {
				if headline != nil {
					t.Fatalf("headline = %+v, want none", headline)
				}
				return
			}
			if headline == nil || *headline != *tc.want {
				t.Errorf("headline = %+v, want %+v", headline, tc.want)
			}
		})
	}
}

// TestParseJobsDuration is which run the duration column measures: the newest
// finished one, since a run still in flight has no wall time yet.
func TestParseJobsDuration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		runs string
		want *int64
	}{
		{
			name: "newest run still in flight",
			runs: `[{"started_at":"2026-11-19T08:00:00Z"},
			        {"started_at":"2026-11-19T07:00:00Z","ended_at":"2026-11-19T07:00:04Z"}]`,
			want: ptrInt64(4000),
		},
		{
			name: "a zero length run is zero, not absent",
			runs: `[{"started_at":"2026-11-19T08:00:00Z","ended_at":"2026-11-19T08:00:00Z"}]`,
			want: ptrInt64(0),
		},
		{
			name: "an end before its start is no duration",
			runs: `[{"started_at":"2026-11-19T08:00:05Z","ended_at":"2026-11-19T08:00:00Z"}]`,
			want: nil,
		},
		{
			name: "no runs",
			runs: `[]`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseJobs([]byte(`[{"name":"a","state":"ok","runs":` + tc.runs + `}]`))
			if err != nil {
				t.Fatalf("ParseJobs: %v", err)
			}
			ms := got.Rows[0].DurationMS
			switch {
			case tc.want == nil && ms != nil:
				t.Errorf("duration = %d, want none", *ms)
			case tc.want != nil && (ms == nil || *ms != *tc.want):
				t.Errorf("duration = %v, want %d", ms, *tc.want)
			}
		})
	}
}

// TestParseJobsLastRun is when a row has a last run: null and the zero time
// both mean none is recorded, and the pane draws no age for a job that never
// ran.
func TestParseJobsLastRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  string
		want *time.Time
	}{
		{name: "no field", row: `{"name":"a","state":"ok"}`, want: nil},
		{name: "null", row: `{"name":"a","state":"ok","last_run":null}`, want: nil},
		{name: "the zero time", row: `{"name":"a","state":"ok","last_run":"0001-01-01T00:00:00Z"}`, want: nil},
		{
			name: "a time",
			row:  `{"name":"a","state":"ok","last_run":"2026-11-19T09:00:00Z"}`,
			want: ptrTime(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseJobs([]byte(`[` + tc.row + `]`))
			if err != nil {
				t.Fatalf("ParseJobs: %v", err)
			}
			at := got.Rows[0].LastRun
			switch {
			case tc.want == nil && at != nil:
				t.Errorf("last run = %v, want none", at)
			case tc.want != nil && (at == nil || !at.Equal(*tc.want)):
				t.Errorf("last run = %v, want %v", at, *tc.want)
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }

func ptrTime(v time.Time) *time.Time { return &v }
