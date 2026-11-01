package shelltier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeTimingFiles records each line appended, and answers with err.
type fakeTimingFiles struct {
	err   error
	paths []string
	lines []string
}

func (f *fakeTimingFiles) Append(path string, line []byte) error {
	f.paths = append(f.paths, path)
	f.lines = append(f.lines, string(line))
	return f.err
}

// clockReadings is a clock that reads each instant in turn, then keeps reading
// the last one.
func clockReadings(instants ...time.Time) func() time.Time {
	next := 0
	return func() time.Time {
		if next >= len(instants) {
			return instants[len(instants)-1]
		}
		read := instants[next]
		next++
		return read
	}
}

// expiredTimeout stands in for WithTimeout with a context whose Budget has
// already run out.
func expiredTimeout(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx, cancel
}

// timingRun is the run the timing cases share: two scripts on a clock that
// spends 67 seconds on the first and 67 on the second, and a fake Exec that
// exits 3 on the second.
func timingRun(ctx context.Context, files TimingFiles, path string, errw *bytes.Buffer) Result {
	start := time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)
	mid, end := start.Add(67*time.Second), start.Add(134*time.Second)
	var cmds []Command
	r := Run(ctx, Config{
		Entries:     entriesAt("scripts/a_test.sh", "scripts/b_test.sh"),
		Root:        "/repo",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        fakeExec(map[string]answer{"scripts/b_test.sh": {code: 3}}, &cmds),
		Now:         clockReadings(start, start, mid, mid, end, end),
		WithTimeout: liveTimeout,
		Timing:      files,
		TimingPath:  path,
		Err:         errw,
	})
	return r
}

func TestTimingLineIsOneJSONObjectPerScript(t *testing.T) {
	t.Parallel()
	files := &fakeTimingFiles{}
	var errs bytes.Buffer
	r := timingRun(t.Context(), files, TimingPath("/town"), &errs)
	if r.Passed != 1 || r.Failed != 1 {
		t.Fatalf("counts passed=%d failed=%d, want 1 1", r.Passed, r.Failed)
	}
	wantPath := filepath.Join("/town", ".runtime", "tiertiming", "shell.jsonl")
	if want := []string{wantPath, wantPath}; !slices.Equal(files.paths, want) {
		t.Errorf("appended to %v, want %v", files.paths, want)
	}
	want := []string{
		`{"script":"scripts/a_test.sh","seconds":67,"exit":0,"at":"2026-10-29T12:01:07Z"}` + "\n",
		`{"script":"scripts/b_test.sh","seconds":67,"exit":3,"at":"2026-10-29T12:02:14Z"}` + "\n",
	}
	if !slices.Equal(files.lines, want) {
		t.Fatalf("lines = %q, want %q", files.lines, want)
	}
	if errs.Len() != 0 {
		t.Errorf("error stream = %q, want nothing when every line is written", errs.String())
	}
}

func TestTimingAppendFailureIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	files := &fakeTimingFiles{err: errors.New("no space left on device")}
	var errs bytes.Buffer
	r := timingRun(t.Context(), files, TimingPath("/town"), &errs)
	if r.Passed != 1 || r.Failed != 1 || r.TimedOut != 0 || r.Elapsed != 134*time.Second {
		t.Fatalf("result = %+v, want the run without a timing log: 1 passed, 1 failed, 0 timed out, 2m14s", r)
	}
	if want := []string{"scripts/b_test.sh"}; !slices.Equal(r.FailedNames, want) {
		t.Errorf("FailedNames = %v, want %v", r.FailedNames, want)
	}
	if got := r.Exit(); got != ExitRed {
		t.Errorf("exit = %d, want ExitRed", got)
	}
	if len(files.lines) != 2 {
		t.Fatalf("appended %d lines, want 2: a failed append must still be tried per script", len(files.lines))
	}
	for _, want := range []string{TimingPath("/town"), "scripts/a_test.sh", "no space left on device"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("error stream %q does not name %q", errs.String(), want)
		}
	}
}

func TestTimingPathIsUnderTheTownRuntime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		town string
		want string
	}{
		{"a town root", "/town", filepath.Join("/town", ".runtime", "tiertiming", "shell.jsonl")},
		{"a town root with a trailing separator", "/town/", filepath.Join("/town", ".runtime", "tiertiming", "shell.jsonl")},
		{"no town root", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := TimingPath(tc.town); got != tc.want {
				t.Errorf("TimingPath(%q) = %q, want %q", tc.town, got, tc.want)
			}
		})
	}
}

func TestTimingRecordsScriptsThatCouldNotStartOrWereCutShort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		exec        func(context.Context, Command) (int, error)
		withTimeout func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
	}{
		{
			name:        "a script that could not start",
			exec:        func(context.Context, Command) (int, error) { return 0, errors.New("cannot start") },
			withTimeout: liveTimeout,
		},
		{
			name:        "a script its budget killed",
			exec:        func(context.Context, Command) (int, error) { return -1, errors.New("signal: killed") },
			withTimeout: expiredTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := &fakeTimingFiles{}
			var errs bytes.Buffer
			start := time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)
			r := Run(t.Context(), Config{
				Entries:     entriesAt("scripts/a_test.sh"),
				Root:        "/repo",
				LogDir:      "/logs",
				Budget:      time.Minute,
				Exec:        tc.exec,
				Now:         clockReadings(start, start, start.Add(2*time.Second), start.Add(2*time.Second)),
				WithTimeout: tc.withTimeout,
				Timing:      files,
				TimingPath:  TimingPath("/town"),
				Err:         &errs,
			})
			if r.Passed != 0 || r.Failed != 1 {
				t.Fatalf("counts passed=%d failed=%d, want 0 1", r.Passed, r.Failed)
			}
			if len(files.lines) != 1 {
				t.Fatalf("appended %d lines, want 1", len(files.lines))
			}
			var rec struct {
				Script  string  `json:"script"`
				Seconds float64 `json:"seconds"`
				Exit    int     `json:"exit"`
				At      string  `json:"at"`
			}
			if err := json.Unmarshal([]byte(files.lines[0]), &rec); err != nil {
				t.Fatalf("line %q is not one JSON object: %v", files.lines[0], err)
			}
			if rec.Exit != -1 {
				t.Errorf("exit = %d, want -1 for a script with no exit code", rec.Exit)
			}
			if rec.Script != "scripts/a_test.sh" || rec.Seconds != 2 {
				t.Errorf("line = %+v, want the script's path and 2 seconds", rec)
			}
		})
	}
}

func TestTimingRecordsNothingWithoutALog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		timing TimingFiles
		path   string
	}{
		{"no appender", nil, TimingPath("/town")},
		{"no path", &fakeTimingFiles{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errs bytes.Buffer
			timingRun(t.Context(), tc.timing, tc.path, &errs)
			files, ok := tc.timing.(*fakeTimingFiles)
			if ok && len(files.lines) != 0 {
				t.Errorf("appended %q, want nothing without a log", files.lines)
			}
			if errs.Len() != 0 {
				t.Errorf("error stream = %q, want nothing when no log is configured", errs.String())
			}
		})
	}
}

func TestTimingOSAppendCreatesTheLogAndAppends(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".runtime", "tiertiming", "shell.jsonl")
	files := OSTimingFiles{}
	for _, line := range []string{"{\"script\":\"a\"}\n", "{\"script\":\"b\"}\n"} {
		if err := files.Append(path, []byte(line)); err != nil {
			t.Fatalf("append %q: %v", line, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if want := "{\"script\":\"a\"}\n{\"script\":\"b\"}\n"; string(got) != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestTimingOSAppendRefusesAPathUnderAFile(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := OSTimingFiles{}.Append(filepath.Join(blocker, "shell.jsonl"), []byte("{}\n"))
	if err == nil {
		t.Fatal("Append under a regular file = nil, want an error")
	}
}
