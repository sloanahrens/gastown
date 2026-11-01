package shelltier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/shelltier/shelltiertest"
)

// fakeClock is a run's clock. The fake Exec advances it by the wall time its
// script takes, so a case's timings do not depend on how often Run reads the
// clock.
type fakeClock struct{ now time.Time }

// Now reads the clock.
func (c *fakeClock) Now() time.Time { return c.now }

// spend moves the clock forward.
func (c *fakeClock) spend(d time.Duration) { c.now = c.now.Add(d) }

// expiredTimeout stands in for WithTimeout with a context whose Budget has
// already run out.
func expiredTimeout(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx, cancel
}

// timingRun is the run the timing cases share: two scripts that take 67
// seconds each on the fake clock, the second exiting 3.
func timingRun(ctx context.Context, files TimingFiles, path string, errw *bytes.Buffer) Result {
	clock := &fakeClock{now: time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)}
	var cmds []Command
	answer := fakeExec(map[string]answer{"scripts/b_test.sh": {code: 3}}, &cmds)
	return Run(ctx, Config{
		Entries: entriesAt("scripts/a_test.sh", "scripts/b_test.sh"),
		Root:    "/repo",
		LogDir:  "/logs",
		Budget:  time.Minute,
		Exec: func(ctx context.Context, c Command) (int, error) {
			code, err := answer(ctx, c)
			clock.spend(67 * time.Second)
			return code, err
		},
		Now:         clock.Now,
		WithTimeout: liveTimeout,
		Timing:      files,
		TimingPath:  path,
		Err:         errw,
	})
}

func TestTimingLineIsOneJSONObjectPerScript(t *testing.T) {
	t.Parallel()
	files := &shelltiertest.Timing{}
	var errs bytes.Buffer
	r := timingRun(t.Context(), files, TimingPath("/town"), &errs)
	if r.Passed != 1 || r.Failed != 1 {
		t.Fatalf("counts passed=%d failed=%d, want 1 1", r.Passed, r.Failed)
	}
	wantPath := filepath.Join("/town", ".runtime", "tiertiming", "shell.jsonl")
	if want := []string{wantPath, wantPath}; !slices.Equal(files.Paths, want) {
		t.Errorf("appended to %v, want %v", files.Paths, want)
	}
	want := []string{
		`{"script":"scripts/a_test.sh","seconds":67,"exit":0,"at":"2026-10-29T12:01:07Z"}` + "\n",
		`{"script":"scripts/b_test.sh","seconds":67,"exit":3,"at":"2026-10-29T12:02:14Z"}` + "\n",
	}
	if !slices.Equal(files.Lines, want) {
		t.Fatalf("lines = %q, want %q", files.Lines, want)
	}
	if errs.Len() != 0 {
		t.Errorf("error stream = %q, want nothing when every line is written", errs.String())
	}
}

func TestTimingAppendFailureIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	files := &shelltiertest.Timing{Err: errors.New("no space left on device")}
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
	if len(files.Lines) != 2 {
		t.Fatalf("appended %d lines, want 2: a failed append must still be tried per script", len(files.Lines))
	}
	for _, want := range []string{TimingPath("/town"), "scripts/a_test.sh", "no space left on device"} {
		if !strings.Contains(errs.String(), want) {
			t.Errorf("error stream %q does not name %q", errs.String(), want)
		}
	}
}

func TestTimingAppendFailureFallsBackToTheProcessErrorStream(t *testing.T) {
	t.Parallel()
	if got := errorStream(nil); got != os.Stderr {
		t.Errorf("errorStream(nil) = %v, want os.Stderr: an unset stream must not drop the failure", got)
	}
	var buf bytes.Buffer
	if got := errorStream(&buf); got != &buf {
		t.Errorf("errorStream(set) = %v, want the configured stream", got)
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
		{
			name:        "a script its budget cut short that Exec reported as success",
			exec:        func(context.Context, Command) (int, error) { return 0, nil },
			withTimeout: expiredTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := &shelltiertest.Timing{}
			var errs bytes.Buffer
			clock := &fakeClock{now: time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)}
			r := Run(t.Context(), Config{
				Entries: entriesAt("scripts/a_test.sh"),
				Root:    "/repo",
				LogDir:  "/logs",
				Budget:  time.Minute,
				Exec: func(ctx context.Context, c Command) (int, error) {
					code, err := tc.exec(ctx, c)
					clock.spend(2 * time.Second)
					return code, err
				},
				Now:         clock.Now,
				WithTimeout: tc.withTimeout,
				Timing:      files,
				TimingPath:  TimingPath("/town"),
				Err:         &errs,
			})
			if r.Passed != 0 || r.Failed != 1 {
				t.Fatalf("counts passed=%d failed=%d, want 0 1", r.Passed, r.Failed)
			}
			if len(files.Lines) != 1 {
				t.Fatalf("appended %d lines, want 1", len(files.Lines))
			}
			var rec struct {
				Script  string  `json:"script"`
				Seconds float64 `json:"seconds"`
				Exit    int     `json:"exit"`
				At      string  `json:"at"`
			}
			if err := json.Unmarshal([]byte(files.Lines[0]), &rec); err != nil {
				t.Fatalf("line %q is not one JSON object: %v", files.Lines[0], err)
			}
			if rec.Exit != -1 {
				t.Errorf("exit = %d, want -1 for a script that never finished on its own", rec.Exit)
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
		{"no path", &shelltiertest.Timing{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errs bytes.Buffer
			timingRun(t.Context(), tc.timing, tc.path, &errs)
			files, ok := tc.timing.(*shelltiertest.Timing)
			if ok && len(files.Lines) != 0 {
				t.Errorf("appended %q, want nothing without a log", files.Lines)
			}
			if errs.Len() != 0 {
				t.Errorf("error stream = %q, want nothing when no log is configured", errs.String())
			}
		})
	}
}

// fakeTimingOS is an OSTimingFiles whose filesystem the case decides.
type fakeTimingOS struct {
	mkdirErr, openErr error
	mkdirPath         string
	mkdirPerm         os.FileMode
	openPath          string
	openFlag          int
	openPerm          os.FileMode
	opened            bool
	target            *fakeAppendTarget
}

// files is the appender under test, writing through the case's fakes.
func (f *fakeTimingOS) files() OSTimingFiles {
	return OSTimingFiles{
		mkdirAll: func(path string, perm os.FileMode) error {
			f.mkdirPath, f.mkdirPerm = path, perm
			return f.mkdirErr
		},
		openFile: func(path string, flag int, perm os.FileMode) (appendTarget, error) {
			f.opened = true
			f.openPath, f.openFlag, f.openPerm = path, flag, perm
			if f.openErr != nil {
				return nil, f.openErr
			}
			return f.target, nil
		},
	}
}

func TestTimingOSAppendWritesOneLineToAnAppendOnlyFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join("/town", ".runtime", "tiertiming", "shell.jsonl")
	target := &fakeAppendTarget{}
	var fake fakeTimingOS
	fake.target = target
	if err := fake.files().Append(path, []byte("{\"script\":\"a\"}\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if want := filepath.Join("/town", ".runtime", "tiertiming"); fake.mkdirPath != want || fake.mkdirPerm != 0o755 {
		t.Errorf("made %q with %v, want the log's directory %q with 0755", fake.mkdirPath, fake.mkdirPerm, want)
	}
	if fake.openPath != path {
		t.Errorf("opened %q, want %q", fake.openPath, path)
	}
	if want := os.O_WRONLY | os.O_APPEND | os.O_CREATE; fake.openFlag != want {
		t.Errorf("open flags = %d, want %d: the log is only ever appended to", fake.openFlag, want)
	}
	if fake.openPerm != 0o644 {
		t.Errorf("open perm = %v, want 0644", fake.openPerm)
	}
	if got := string(target.wrote); got != "{\"script\":\"a\"}\n" {
		t.Errorf("wrote %q, want the line", got)
	}
}

func TestTimingOSAppendReportsAMissingDirectory(t *testing.T) {
	t.Parallel()
	var fake fakeTimingOS
	fake.mkdirErr = errors.New("permission denied")
	fake.target = &fakeAppendTarget{}
	err := fake.files().Append("/town/.runtime/tiertiming/shell.jsonl", []byte("{}\n"))
	if err == nil || !strings.Contains(err.Error(), "make the log's directory: permission denied") {
		t.Fatalf("Append = %v, want the directory failure", err)
	}
	if fake.opened {
		t.Error("opened the log although its directory was not made")
	}
}

func TestTimingOSAppendReportsAFailedOpen(t *testing.T) {
	t.Parallel()
	var fake fakeTimingOS
	fake.openErr = errors.New("permission denied")
	err := fake.files().Append("/town/.runtime/tiertiming/shell.jsonl", []byte("{}\n"))
	want := "open /town/.runtime/tiertiming/shell.jsonl: permission denied"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Append = %v, want an error saying %q", err, want)
	}
}

// fakeAppendTarget is an open log whose write and close the case decides.
type fakeAppendTarget struct {
	writeErr, closeErr error
	wrote              []byte
}

func (f *fakeAppendTarget) Write(p []byte) (int, error) {
	f.wrote = append(f.wrote, p...)
	return len(p), f.writeErr
}

func (f *fakeAppendTarget) Close() error { return f.closeErr }

func TestTimingOSAppendReportsAFailedWriteOrClose(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target *fakeAppendTarget
		want   string
	}{
		{"a failed write", &fakeAppendTarget{writeErr: errors.New("disk full")}, "write /log: disk full"},
		{"a failed close", &fakeAppendTarget{closeErr: errors.New("input/output error")}, "close /log: input/output error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := appendLine(tc.target, "/log", []byte("{}\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("appendLine = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

func TestTimingRefusesALineItCannotEncode(t *testing.T) {
	t.Parallel()
	_, err := timingLine(timingRecord{Script: "scripts/a_test.sh", Seconds: math.NaN()})
	if err == nil {
		t.Fatal("timingLine with an unencodable duration = nil, want an error")
	}
}
