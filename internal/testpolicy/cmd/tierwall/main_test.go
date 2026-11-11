package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedHost is the seam run reads outside the stream: one fixed clock, no
// load, no town, real files under the test's own temporary directory, and a
// fixed 16 CPUs with no cgroup quota file, so no test reads the host.
func fixedHost() host {
	return host{
		now:      func() time.Time { return time.Date(2026, 11, 1, 14, 42, 0, 0, time.UTC) },
		load1:    func() (float64, bool) { return 0, false },
		town:     func() string { return "" },
		mkdir:    func(string) error { return nil },
		open:     openAppend,
		numCPU:   func() int { return 16 },
		maxProcs: func() int { return 16 },
		readFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
	}
}

// runFile feeds a canned go test -json stream from testdata through run, the
// way the Makefile target feeds a real one through stdin, and returns the
// exit code with what run wrote on both streams.
func runFile(t *testing.T, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runFileWith(t, name, fixedHost(), args...)
}

// runFileWith runs runFile's stream against a test's own host seam, for a case
// that carries a load reading or stamps a stream with no timestamps.
func runFileWith(t *testing.T, name string, h host, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var out, errOut bytes.Buffer
	code = run(args, f, &out, &errOut, h)
	return code, out.String(), errOut.String()
}

// TestReport_PrintsEveryPackageSlowestFirstThenTheTier pins the report's
// shape on a stream where nothing failed: one line per package that reported
// pass or fail, slowest first, then the tier's own wall against the bar. The
// packages that only skipped — a test skipped by name, a package with no test
// files — earn no line, and no passing test's output reaches stdout.
func TestReport_PrintsEveryPackageSlowestFirstThenTheTier(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runFile(t, "pass.jsonl")
	if code != 0 {
		t.Errorf("exit code = %d, want 0: an all-pass stream is go test's verdict even over the bar", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	want := strings.Join([]string{
		"tierwall: github.com/steveyegge/gastown/internal/cmd 203.4s",
		"tierwall: github.com/steveyegge/gastown/internal/git 22.7s",
		"tierwall: github.com/steveyegge/gastown/internal/tmux 19s",
		"tierwall: github.com/steveyegge/gastown/internal/slot 0.1s",
		"tierwall: tier 245.6s bar 90s OVER",
		"tierwall: cpus: NumCPU=16 GOMAXPROCS=16 cgroup=unreadable",
		"",
	}, "\n")
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestReport_FailingStreamKeepsTheFailureAndExitsOne pins what a red tier
// prints: the failing test's own lines and the failing package's, the
// compiler errors of a package that did not build, and nothing from the tests
// that passed. The exit code is go test's verdict, so the caller's pipefail
// and make both see red.
func TestReport_FailingStreamKeepsTheFailureAndExitsOne(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runFile(t, "fail.jsonl")
	if code != 1 {
		t.Errorf("exit code = %d, want 1: a FAIL event is go test's verdict", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	want := strings.Join([]string{
		"=== RUN   TestLandFails",
		"    land_test.go:44: the landing record has no gate result",
		"    land_test.go:45: want ok, got red",
		"--- FAIL: TestLandFails (0.00s)",
		"FAIL",
		"FAIL\tgithub.com/steveyegge/gastown/internal/daemon\t40.000s",
		"FAIL\tgithub.com/steveyegge/gastown/internal/broken [build failed]",
		"# github.com/steveyegge/gastown/internal/broken [github.com/steveyegge/gastown/internal/broken.test]",
		"internal/broken/broken_test.go:8:2: undefined: landRecord",
		"tierwall: github.com/steveyegge/gastown/internal/daemon 40s",
		"tierwall: github.com/steveyegge/gastown/internal/notify 39.2s",
		"tierwall: github.com/steveyegge/gastown/internal/broken 0s",
		"tierwall: tier 40.6s bar 90s OK",
		"tierwall: cpus: NumCPU=16 GOMAXPROCS=16 cgroup=unreadable",
		"",
	}, "\n")
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestReport_BarIsALabelAndMaxWallIsTheFailure pins the two knobs apart:
// -bar moves the OK/OVER label and fails nothing, and -max-wall is what turns
// an over-bar run red, with exit 3 so a caller can tell the wall from a test
// failure.
func TestReport_BarIsALabelAndMaxWallIsTheFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want int
		line string
	}{
		{name: "bar under the tier only relabels", args: []string{"-bar", "45s"}, want: 0, line: "tierwall: tier 245.6s bar 45s OVER"},
		{name: "bar over the tier relabels the other way", args: []string{"-bar", "300s"}, want: 0, line: "tierwall: tier 245.6s bar 300s OK"},
		{name: "no max-wall fails nothing", args: nil, want: 0, line: "tierwall: tier 245.6s bar 90s OVER"},
		{name: "max-wall under the tier exits 3", args: []string{"-max-wall", "90s"}, want: 3, line: "tierwall: tier 245.6s bar 90s OVER"},
		{name: "max-wall over the tier exits 0", args: []string{"-max-wall", "300s"}, want: 0, line: "tierwall: tier 245.6s bar 90s OVER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, _ := runFile(t, "pass.jsonl", tc.args...)
			if code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
			if !strings.Contains(stdout, tc.line+"\n") {
				t.Errorf("stdout = %q, want it to hold %q", stdout, tc.line)
			}
		})
	}
}

// TestReport_MaxWallDoesNotOutrankATestFailure pins the precedence: -max-wall
// is a request about wall time, and a run whose tests failed is red for the
// reason go test gave, not for the wall.
func TestReport_MaxWallDoesNotOutrankATestFailure(t *testing.T) {
	t.Parallel()
	code, _, _ := runFile(t, "fail.jsonl", "-max-wall", "10s")
	if code != 1 {
		t.Errorf("exit code = %d, want 1: a FAIL event outranks -max-wall's 3", code)
	}
}

// TestReport_PassesUnknownLinesThrough covers a runner that writes something
// other than a go test event, such as go's own line on stdout: it reaches the
// log unchanged instead of being dropped.
func TestReport_PassesUnknownLinesThrough(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run(nil, strings.NewReader("go: downloading something\n\nnot json at all\n"),
		&out, &errOut, fixedHost())
	if code != 0 {
		t.Errorf("exit code = %d, want 0: no event failed", code)
	}
	want := "go: downloading something\nnot json at all\ntierwall: tier 0s bar 90s OK\ntierwall: cpus: NumCPU=16 GOMAXPROCS=16 cgroup=unreadable\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// TestReport_BadFlagIsARunnerFailure checks that a flag the report cannot read
// is exit 2, the code that means tierwall itself failed, and not 1, which
// means the tier's tests did. -h asks for the usage and is not a failure.
func TestReport_BadFlagIsARunnerFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want int
	}{
		{args: []string{"-nope"}, want: 2},
		{args: []string{"-h"}, want: 0},
	} {
		var out, errOut bytes.Buffer
		code := run(tc.args, strings.NewReader(""), &out, &errOut, fixedHost())
		if code != tc.want {
			t.Errorf("run(%v) exit code = %d, want %d", tc.args, code, tc.want)
		}
		if errOut.Len() == 0 {
			t.Errorf("run(%v) stderr is empty, want the usage there", tc.args)
		}
	}
}

// TestJSONLAppendsOneLinePerPackage pins the series -jsonl writes: one line
// per package the stream reported a verdict for, sorted by package, carrying
// the package's own wall and verdict, the run's id, the run's start, and the
// load it ran beside. A package that ran no tests is still a package the run
// covered, so it earns a line at its zero wall.
// Mutation: ignore -jsonl, and the file the case reads never exists.
func TestJSONLAppendsOneLinePerPackage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	h := fixedHost()
	h.load1 = func() (float64, bool) { return 11.3, true }

	code, _, stderr := runFileWith(t, "pass.jsonl", h, "-jsonl", path, "-run", "gate-1")
	if code != 0 {
		t.Errorf("exit code = %d, want 0: recording a series fails nothing", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the run wrote no series: %v", err)
	}
	want := strings.Join([]string{
		`{"ts":"2026-10-02T11:00:00-05:00","run":"gate-1","pkg":"github.com/steveyegge/gastown/internal/cmd","wall_s":203.4,"result":"pass","load1":11.3}`,
		`{"ts":"2026-10-02T11:00:00-05:00","run":"gate-1","pkg":"github.com/steveyegge/gastown/internal/git","wall_s":22.7,"result":"pass","load1":11.3}`,
		`{"ts":"2026-10-02T11:00:00-05:00","run":"gate-1","pkg":"github.com/steveyegge/gastown/internal/none","wall_s":0,"result":"skip","load1":11.3}`,
		`{"ts":"2026-10-02T11:00:00-05:00","run":"gate-1","pkg":"github.com/steveyegge/gastown/internal/slot","wall_s":0.1,"result":"pass","load1":11.3}`,
		`{"ts":"2026-10-02T11:00:00-05:00","run":"gate-1","pkg":"github.com/steveyegge/gastown/internal/tmux","wall_s":19,"result":"pass","load1":11.3}`,
		"",
	}, "\n")
	if string(data) != want {
		t.Errorf("series = %q, want %q", string(data), want)
	}
}

// TestJSONLCachedPackageShipsNoWall pins how the series reads go test's result
// cache: a cached package keeps its line and its verdict, marked cached, and
// carries no wall_s, while a package that really ran keeps its wall_s and has
// no cached key. A reader takes wall_s only from lines without cached.
// Mutation: write the elapsed time for a cached package, and the cached line
// grows a wall_s key.
func TestJSONLCachedPackageShipsNoWall(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	const stream = `{"Time":"2026-10-02T11:00:00.000000Z","Action":"start","Package":"example.com/cached"}
{"Time":"2026-10-02T11:00:00.100000Z","Action":"output","Package":"example.com/cached","Output":"ok  \texample.com/cached\t(cached)\n"}
{"Time":"2026-10-02T11:00:00.200000Z","Action":"pass","Package":"example.com/cached","Elapsed":0}
{"Time":"2026-10-02T11:00:00.300000Z","Action":"start","Package":"example.com/ran"}
{"Time":"2026-10-02T11:00:00.400000Z","Action":"output","Package":"example.com/ran","Output":"ok  \texample.com/ran\t1.500s\n"}
{"Time":"2026-10-02T11:00:00.500000Z","Action":"pass","Package":"example.com/ran","Elapsed":1.5}
`
	var out, errOut bytes.Buffer
	code := run([]string{"-jsonl", path, "-run", "gate-1"}, strings.NewReader(stream), &out, &errOut, fixedHost())
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: stderr = %q", code, errOut.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the run wrote no series: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("series = %q, want two lines", data)
	}
	for i, tc := range []struct {
		pkg    string
		keys   []string
		absent []string
	}{
		{"example.com/cached", []string{"cached"}, []string{"wall_s"}},
		{"example.com/ran", []string{"wall_s"}, []string{"cached"}},
	} {
		var line map[string]json.RawMessage
		if err := json.Unmarshal([]byte(lines[i]), &line); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if got := string(line["pkg"]); got != `"`+tc.pkg+`"` {
			t.Errorf("line %d pkg = %s, want %q", i, got, tc.pkg)
		}
		for _, k := range tc.keys {
			if _, ok := line[k]; !ok {
				t.Errorf("line %d (%s) has no %q key: %s", i, tc.pkg, k, lines[i])
			}
		}
		for _, k := range tc.absent {
			if _, ok := line[k]; ok {
				t.Errorf("line %d (%s) has a %q key: %s", i, tc.pkg, k, lines[i])
			}
		}
	}
}

// TestJSONLEmptyStreamAppendsNothing checks that a stream with no package
// verdicts writes no file and says so on stderr: an empty run must not read as
// a run with nothing slow to report.
func TestJSONLEmptyStreamAppendsNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	var out, errOut bytes.Buffer
	code := run([]string{"-jsonl", path, "-run", "gate-1"},
		strings.NewReader("go: downloading something\n"), &out, &errOut, fixedHost())
	if code != 0 {
		t.Errorf("exit code = %d, want 0: an empty stream fails nothing", code)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want no file: nothing was appended, so nothing was created", path, err)
	}
	if !strings.Contains(errOut.String(), "no package events") {
		t.Errorf("stderr = %q, want it to say nothing was appended", errOut.String())
	}
}

// TestJSONLUntimedStreamIsStampedFromTheClock covers a stream whose events
// carry no Time: the run is stamped from the clock rather than written with a
// zero time.
func TestJSONLUntimedStreamIsStampedFromTheClock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	const stream = `{"Action":"start","Package":"example.com/a"}
{"Action":"pass","Package":"example.com/a","Elapsed":1.5}
`
	var out, errOut bytes.Buffer
	code := run([]string{"-jsonl", path, "-run", "gate-1"}, strings.NewReader(stream), &out, &errOut, fixedHost())
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: stderr = %q", code, errOut.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the run wrote no series: %v", err)
	}
	want := `{"ts":"2026-11-01T14:42:00Z","run":"gate-1","pkg":"example.com/a","wall_s":1.5,"result":"pass"}` + "\n"
	if string(data) != want {
		t.Errorf("series = %q, want %q", string(data), want)
	}
}

// TestJSONLNeedsARun checks that -jsonl without -run is the runner's own
// failure and writes nothing: a series line with no run cannot be placed.
func TestJSONLNeedsARun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	var out, errOut bytes.Buffer
	code := run([]string{"-jsonl", path}, strings.NewReader(""), &out, &errOut, fixedHost())
	if code != 2 {
		t.Errorf("exit code = %d, want 2: the flag set is the runner's failure", code)
	}
	if !strings.Contains(errOut.String(), "-run") {
		t.Errorf("stderr = %q, want it to name the missing -run", errOut.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want no file: a refused run writes no series", path, err)
	}
}

// TestJSONLSeriesFailuresLeaveTheVerdictAlone pins the contract the gate is
// wired on: every way the series can fail is reported on stderr naming the
// file and by exit code 4, which the gate reads as its "series incomplete"
// alarm. The report still prints, and a failed package (1) or an over-wall tier
// (3) keeps its own code, because wall time never fails a run.
func TestJSONLSeriesFailuresLeaveTheVerdictAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(h *host)
	}{
		{"the directory cannot be made", func(h *host) {
			h.mkdir = func(string) error { return errors.New("read-only file system") }
		}},
		{"the file cannot be opened", func(h *host) {
			h.open = func(string) (io.WriteCloser, error) { return nil, errors.New("no such device") }
		}},
		{"the write is rejected", func(h *host) {
			h.open = func(string) (io.WriteCloser, error) {
				return failingFile{writeErr: errors.New("disk full")}, nil
			}
		}},
		{"the close is rejected", func(h *host) {
			h.open = func(string) (io.WriteCloser, error) {
				return failingFile{closeErr: errors.New("stale NFS handle")}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
			h := fixedHost()
			tc.setup(&h)

			code, stdout, stderr := runFileWith(t, "pass.jsonl", h, "-jsonl", path, "-run", "gate-1")
			if code != 4 {
				t.Errorf("exit code = %d, want 4: a series that could not be written is the gate's alarm", code)
			}
			if !strings.Contains(stderr, path) {
				t.Errorf("stderr = %q, want it to name %s", stderr, path)
			}
			if !strings.Contains(stdout, "tierwall: tier ") {
				t.Errorf("stdout = %q, want the report still printed", stdout)
			}
		})
	}
}

// failingFile is a series file whose write or close fails: the failures a real
// file on a healthy host cannot be made to reach.
type failingFile struct {
	writeErr error
	closeErr error
}

func (f failingFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f failingFile) Close() error { return f.closeErr }

// TestJSONLSeriesRelativePathIsTakenFromTheTown pins the wiring's one
// assumption: the Makefile names the series with a relative path, and the tool
// places it under the town it found, creating the runtime directory.
func TestJSONLSeriesRelativePathIsTakenFromTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	h := fixedHost()
	h.town = func() string { return town }
	h.mkdir = func(dir string) error { return os.MkdirAll(dir, 0o755) }

	code, _, stderr := runFileWith(t, "pass.jsonl", h, "-jsonl", ".runtime/metrics/gate-walls.jsonl", "-run", "gate-1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	data, err := os.ReadFile(filepath.Join(town, ".runtime", "metrics", "gate-walls.jsonl"))
	if err != nil {
		t.Fatalf("the run wrote no series under the town: %v", err)
	}
	if got := strings.Count(string(data), "\n"); got != 5 {
		t.Errorf("series has %d lines, want one per package (5): %q", got, data)
	}
}

// TestJSONLSeriesRelativePathWithoutATownWritesNothing checks that a relative
// path the tool cannot place is reported, not silently read as a clean run.
func TestJSONLSeriesRelativePathWithoutATownWritesNothing(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runFileWith(t, "pass.jsonl", fixedHost(),
		"-jsonl", ".runtime/metrics/gate-walls.jsonl", "-run", "gate-1")
	if code != 4 {
		t.Errorf("exit code = %d, want 4: a series with no town to go in is the gate's alarm", code)
	}
	if !strings.Contains(stderr, ".runtime/metrics/gate-walls.jsonl") {
		t.Errorf("stderr = %q, want it to name the path it could not place", stderr)
	}
	if !strings.Contains(stderr, "no town root") {
		t.Errorf("stderr = %q, want it to say why nothing was appended", stderr)
	}
	if !strings.Contains(stdout, "tierwall: tier ") {
		t.Errorf("stdout = %q, want the report still printed", stdout)
	}
}

// TestTownRootAtReadsTheOutermostTown checks the two places townRoot looks:
// the working directory, for a gate run inside the town, and the repository's
// shared git dir, for a landing's throwaway worktree outside it. A rig carries
// a mayor/ of its own, so only the outermost mayor/town.json is the town.
func TestTownRootAtReadsTheOutermostTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rig := filepath.Join(town, "gastown", "mayor", "rig")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	noRepo := func() (string, error) { return "", errors.New("not a repository") }

	for _, tc := range []struct {
		name   string
		getwd  func() (string, error)
		common func() (string, error)
		want   string
	}{
		{"the working directory is in the town", func() (string, error) { return rig, nil }, noRepo, town},
		{"the repository is in the town", func() (string, error) { return outside, nil }, func() (string, error) { return filepath.Join(rig, ".git"), nil }, town},
		{"neither is in a town", func() (string, error) { return outside, nil }, noRepo, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := townRootAt(tc.getwd, tc.common); got != tc.want {
				t.Errorf("townRootAt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAppendSeries_UnencodableLineIsAnError checks that a line the encoder
// cannot render is refused by the appending call site, not written without its
// time and not reported as a series it recorded.
func TestAppendSeries_UnencodableLineIsAnError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	r := newReport(io.Discard)
	r.results["example.com/a"] = "pass"
	r.first = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := appendSeries(path, "gate-1", r, fixedHost(), io.Discard); err == nil {
		t.Fatal("appendSeries returned no error for a timestamp json cannot encode")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want no file: a refused series writes nothing", path, err)
	}
}

// TestGitCommonDirFromReadsTheCommand checks the one git call the town lookup
// makes: its output names the common dir, and a command that fails is an error
// the caller sees rather than an empty path read as "no repository".
func TestGitCommonDirFromReadsTheCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		run     func() ([]byte, error)
		want    string
		wantErr bool
	}{
		{"the command answers", func() ([]byte, error) { return []byte(" /town/rig/.git\n"), nil }, "/town/rig/.git", false},
		{"the command fails", func() ([]byte, error) { return nil, errors.New("exit status 128") }, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := gitCommonDirFrom(tc.run)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error: %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("common dir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTierwallReportAndExitCodeUnchanged checks that -jsonl and -run add a
// series and change nothing the report prints or the code it exits.
func TestTierwallReportAndExitCodeUnchanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	plainCode, plainOut, plainErr := runFile(t, "pass.jsonl")
	seriesCode, seriesOut, seriesErr := runFile(t, "pass.jsonl", "-jsonl", path, "-run", "gate-1")
	if seriesCode != plainCode {
		t.Errorf("exit code with -jsonl = %d, want %d", seriesCode, plainCode)
	}
	if seriesOut != plainOut {
		t.Errorf("stdout with -jsonl = %q, want the report unchanged: %q", seriesOut, plainOut)
	}
	if seriesErr != plainErr {
		t.Errorf("stderr with -jsonl = %q, want %q", seriesErr, plainErr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the flags wrote no series: %v", err)
	}
}

// TestSecs_RendersTenthsOfASecond pins the report's one number format, which
// both the package lines and the tier line are read at.
func TestSecs_RendersTenthsOfASecond(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"90s", "90s"},
		{"203.4s", "203.4s"},
		{"0.784s", "0.8s"},
		{"0.017s", "0s"},
		{"0s", "0s"},
		{"0.95s", "1s"},
	} {
		d, err := time.ParseDuration(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := secs(d); got != tc.want {
			t.Errorf("secs(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A series failure never hides a verdict: a failed package still exits 1, and
// an over-wall tier still exits 3, with the series unwritable as well.
func TestJSONLSeriesFailureDoesNotMaskTheVerdict(t *testing.T) {
	t.Parallel()
	h := fixedHost()
	h.mkdir = func(string) error { return errors.New("read-only file system") }
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	if code, _, _ := runFileWith(t, "fail.jsonl", h, "-jsonl", path, "-run", "gate-1"); code != 1 {
		t.Errorf("failed package with an unwritable series: exit code = %d, want 1", code)
	}
}

// TestSlowTestsListedFromTheStream pins -slow-tests on a fixed stream with a
// test of 1s, one of 5s, one of 31s and a subtest of 30s: the 5s and 31s tests
// print under the package that ran them, slowest first. The 1s test and the
// subtest, whose time sits inside its parent's, print nothing. Mutation: raise
// tiersweep.SlowTest above 5s and the 5s line goes; drop the threshold and the
// 1s line appears.
func TestSlowTestsListedFromTheStream(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runFile(t, "slow.jsonl", "-slow-tests")
	if code != 0 {
		t.Errorf("exit code = %d, want 0: a passing stream is go test's verdict", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	want := strings.Join([]string{
		"tierwall: github.com/steveyegge/gastown/internal/daemon 37s",
		"tierwall: slow tests",
		"tierwall:   github.com/steveyegge/gastown/internal/daemon TestLong 31s",
		"tierwall:   github.com/steveyegge/gastown/internal/daemon TestFive 5s",
		"tierwall: phases github.com/steveyegge/gastown/internal/daemon serial 37s parallel 0s",
		"tierwall: tier 37s bar 90s OK",
		"tierwall: cpus: NumCPU=16 GOMAXPROCS=16 cgroup=unreadable",
		"",
	}, "\n")
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestSlowTestsOffByDefault pins the flag's default: without -slow-tests the
// pass and fail fixtures print no slow-test header and no test line. Their
// exact reports are pinned byte for byte by the two TestReport cases above.
// Mutation: default the flag on, and the header appears in both reports.
func TestSlowTestsOffByDefault(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pass.jsonl", "fail.jsonl"} {
		_, stdout, _ := runFile(t, name)
		if strings.Contains(stdout, "tierwall: slow tests") {
			t.Errorf("%s without -slow-tests printed the slow-test header: %q", name, stdout)
		}
		for _, line := range strings.Split(stdout, "\n") {
			if strings.HasPrefix(line, "tierwall:   ") {
				t.Errorf("%s without -slow-tests printed a test line %q", name, line)
			}
		}
	}
}

// TestSlowTestsSeriesTagsTheTests pins what -jsonl appends with -slow-tests:
// the package's line, then one timings line per slow test in the sweep's
// format, kind "test", tier "unit", elapsed_ms, slowest first. Mutation: stop
// appending the test lines, and the file ends after the package line.
func TestSlowTestsSeriesTagsTheTests(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gate-walls.jsonl")
	code, _, stderr := runFileWith(t, "slow.jsonl", fixedHost(), "-slow-tests", "-jsonl", path, "-run", "gate-1")
	if code != 0 {
		t.Errorf("exit code = %d, want 0: recording a series fails nothing", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the run wrote no series: %v", err)
	}
	want := strings.Join([]string{
		`{"ts":"2026-10-02T11:00:00-05:00","run":"gate-1","pkg":"github.com/steveyegge/gastown/internal/daemon","wall_s":37,"result":"pass"}`,
		`{"kind":"test","tier":"unit","package":"github.com/steveyegge/gastown/internal/daemon","test":"TestLong","elapsed_ms":31000}`,
		`{"kind":"test","tier":"unit","package":"github.com/steveyegge/gastown/internal/daemon","test":"TestFive","elapsed_ms":5000}`,
		"",
	}, "\n")
	if string(data) != want {
		t.Errorf("series = %q, want %q", string(data), want)
	}
}

// TestSlowTestsMalformedEventIsARunnerFailure pins the refusal: a JSON object
// line that is not a go test event stops -slow-tests with exit 2, names the
// line number on stderr, and prints no report, so no list is short of tests
// and no event text is echoed. Mutation: fall back to the tolerant parse on a
// parse error, and the run exits 0 with a list missing the tests after it.
func TestSlowTestsMalformedEventIsARunnerFailure(t *testing.T) {
	t.Parallel()
	stream := `{"Action":"pass","Package":"example.com/fx/a","Test":"TestA","Elapsed":6}` + "\n" +
		`{"Action":5,"Package":"example.com/fx/a","Output":"secret-ish text"}` + "\n"
	var out, errOut bytes.Buffer
	code := run([]string{"-slow-tests"}, strings.NewReader(stream), &out, &errOut, fixedHost())
	if code != 2 {
		t.Errorf("exit code = %d, want 2: a malformed event is the runner's failure", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want no report after a malformed event", out.String())
	}
	if want := "tierwall: slow tests: line 2: malformed go test -json event\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
}

// TestSlowTestsTiesGoByPackageThenName pins the order of equal walls: the
// package first, then the test name, so one stream always prints one list.
// Mutation: sort by elapsed alone, and the tie prints in stream order.
func TestSlowTestsTiesGoByPackageThenName(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"Action":"pass","Package":"example.com/fx/b","Test":"TestZ","Elapsed":6}`,
		`{"Action":"pass","Package":"example.com/fx/a","Test":"TestB","Elapsed":6}`,
		`{"Action":"pass","Package":"example.com/fx/a","Test":"TestA","Elapsed":6}`,
		"",
	}, "\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"-slow-tests"}, strings.NewReader(stream), &out, &errOut, fixedHost()); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr %q", code, errOut.String())
	}
	want := strings.Join([]string{
		"tierwall: slow tests",
		"tierwall:   example.com/fx/a TestA 6s",
		"tierwall:   example.com/fx/a TestB 6s",
		"tierwall:   example.com/fx/b TestZ 6s",
		"tierwall: tier 0s bar 90s OK",
		"tierwall: cpus: NumCPU=16 GOMAXPROCS=16 cgroup=unreadable",
		"",
	}, "\n")
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// phaseStream is one package's go test -json stream for the phases tests: the
// serial lines given, then three tests that pause, run and pass, then the
// package's own pass with a 25s wall.
func phaseStream(serial ...string) string {
	const p = `"Package":"example.com/fx/a"`
	lines := append([]string(nil), serial...)
	lines = append(lines,
		`{"Action":"pause",`+p+`,"Test":"TestParA"}`,
		`{"Action":"pause",`+p+`,"Test":"TestParB"}`,
		`{"Action":"pause",`+p+`,"Test":"TestParC"}`,
		`{"Action":"cont",`+p+`,"Test":"TestParA"}`,
		`{"Action":"cont",`+p+`,"Test":"TestParB"}`,
		`{"Action":"cont",`+p+`,"Test":"TestParC"}`,
		`{"Action":"pass",`+p+`,"Test":"TestParA","Elapsed":8}`,
		`{"Action":"pass",`+p+`,"Test":"TestParB","Elapsed":9}`,
		`{"Action":"pass",`+p+`,"Test":"TestParC","Elapsed":7}`,
		`{"Action":"pass",`+p+`,"Elapsed":25}`,
		"",
	)
	return strings.Join(lines, "\n")
}

// TestPhasesSplitSerialFromParallel pins the phases line on fixed streams. In
// the first, two top-level tests that never pause run 4s and 6s and three
// tests pause, so serial is the 10s of the two and parallel the 15s the rest of
// the 25s wall holds. A subtest's time sits inside its parent's, so the
// subtest does not add to serial. A package whose serial tests outrun its wall
// prints parallel 0s, never a negative. Mutation: count the paused tests as
// serial and the first row reads serial 34s; ignore the pause event and every
// test reads serial; count the subtest and the second row reads serial 13s;
// drop the floor and the third row prints a negative parallel.
func TestPhasesSplitSerialFromParallel(t *testing.T) {
	t.Parallel()
	const p = `"Package":"example.com/fx/a"`
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{
			name: "serial tests split from the parallel rest",
			stream: phaseStream(
				`{"Action":"pass",`+p+`,"Test":"TestSerialA","Elapsed":4}`,
				`{"Action":"pass",`+p+`,"Test":"TestSerialB","Elapsed":6}`,
			),
			want: "tierwall: phases example.com/fx/a serial 10s parallel 15s",
		},
		{
			name: "a subtest is counted inside its parent",
			stream: phaseStream(
				`{"Action":"pass",`+p+`,"Test":"TestSerialA/child","Elapsed":3}`,
				`{"Action":"pass",`+p+`,"Test":"TestSerialA","Elapsed":4}`,
				`{"Action":"pass",`+p+`,"Test":"TestSerialB","Elapsed":6}`,
			),
			want: "tierwall: phases example.com/fx/a serial 10s parallel 15s",
		},
		{
			name: "serial past the wall leaves no parallel phase",
			stream: phaseStream(
				`{"Action":"pass",`+p+`,"Test":"TestSerialA","Elapsed":20}`,
				`{"Action":"pass",`+p+`,"Test":"TestSerialB","Elapsed":15}`,
			),
			want: "tierwall: phases example.com/fx/a serial 35s parallel 0s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := run([]string{"-slow-tests"}, strings.NewReader(tc.stream), &out, &errOut, fixedHost())
			if code != 0 {
				t.Errorf("exit code = %d, want 0: a passing stream is go test's verdict", code)
			}
			if !strings.Contains(out.String(), tc.want+"\n") {
				t.Errorf("stdout = %q, want it to hold %q", out.String(), tc.want)
			}
		})
	}
}

// TestPhasesNoParallelWithoutTestEvents pins that a package whose stream holds
// no test event has no parallel phase, while its wall still counts as a
// package. The fixed stream is one package's own pass with a 25s wall and
// nothing else. A package with a test event keeps its wall as the phase split
// (the phaseStream cases above). Mutation: drop the tested check and the line
// reads parallel 25s.
func TestPhasesNoParallelWithoutTestEvents(t *testing.T) {
	t.Parallel()
	stream := `{"Action":"pass","Package":"example.com/fx/a","Elapsed":25}` + "\n"
	var out, errOut bytes.Buffer
	code := run([]string{"-slow-tests"}, strings.NewReader(stream), &out, &errOut, fixedHost())
	if code != 0 {
		t.Errorf("exit code = %d, want 0: a passing stream is go test's verdict", code)
	}
	want := "tierwall: phases example.com/fx/a serial 0s parallel 0s\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("stdout = %q, want it to hold %q", out.String(), want)
	}
}

// TestPhasesTestOutputMarksPackageAsTested pins that a test event without a
// verdict still marks its package as having run tests: the stream holds only a
// test's output line, then the package's 25s pass, so the package keeps its
// wall as parallel. Mutation: mark the package tested only on a verdict and the
// line reads parallel 0s.
func TestPhasesTestOutputMarksPackageAsTested(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"Action":"output","Package":"example.com/fx/a","Test":"TestRun","Output":"=== RUN   TestRun\n"}`,
		`{"Action":"pass","Package":"example.com/fx/a","Elapsed":25}`,
		"",
	}, "\n")
	var out, errOut bytes.Buffer
	code := run([]string{"-slow-tests"}, strings.NewReader(stream), &out, &errOut, fixedHost())
	if code != 0 {
		t.Errorf("exit code = %d, want 0: a passing stream is go test's verdict", code)
	}
	want := "tierwall: phases example.com/fx/a serial 0s parallel 25s\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("stdout = %q, want it to hold %q", out.String(), want)
	}
}

// TestPhasesOnlyForTheSlowestThree pins which packages get a phases line: the
// three slowest of a five-package stream, and none without -slow-tests. The
// packages run no tests, so each one's serial and parallel are both 0s: a
// package with no test events has no parallel phase. Mutation: drop the cap
// and all five packages get a line; drop the flag check and the three lines
// print without -slow-tests.
func TestPhasesOnlyForTheSlowestThree(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"Action":"pass","Package":"example.com/fx/a","Elapsed":50}`,
		`{"Action":"pass","Package":"example.com/fx/b","Elapsed":40}`,
		`{"Action":"pass","Package":"example.com/fx/c","Elapsed":30}`,
		`{"Action":"pass","Package":"example.com/fx/d","Elapsed":20}`,
		`{"Action":"pass","Package":"example.com/fx/e","Elapsed":10}`,
		"",
	}, "\n")
	phaseLines := func(stdout string) string {
		var got []string
		for _, line := range strings.Split(stdout, "\n") {
			if strings.HasPrefix(line, "tierwall: phases ") {
				got = append(got, line)
			}
		}
		return strings.Join(got, "\n")
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"-slow-tests"}, strings.NewReader(stream), &out, &errOut, fixedHost()); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr %q", code, errOut.String())
	}
	want := strings.Join([]string{
		"tierwall: phases example.com/fx/a serial 0s parallel 0s",
		"tierwall: phases example.com/fx/b serial 0s parallel 0s",
		"tierwall: phases example.com/fx/c serial 0s parallel 0s",
	}, "\n")
	if got := phaseLines(out.String()); got != want {
		t.Errorf("phase lines = %q, want %q", got, want)
	}

	out.Reset()
	if code := run(nil, strings.NewReader(stream), &out, &errOut, fixedHost()); code != 0 {
		t.Fatalf("exit code = %d, want 0 without -slow-tests; stderr %q", code, errOut.String())
	}
	if got := phaseLines(out.String()); got != "" {
		t.Errorf("phase lines without -slow-tests = %q, want none", got)
	}
}

// fileReader serves the files a test names and reports every other path as
// absent, so the cgroup reads never touch the host's /sys.
func fileReader(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if s, ok := files[path]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

// TestCPULineReportsTheQuota pins the cpus line on a passing run: NumCPU and
// GOMAXPROCS as the host reports them, and the cgroup quota as its file states
// it. A v2 cpu.max is reported as given, max included. Without it the v1 quota
// and period are joined in that order, and a v2 file outranks v1. Mutation:
// drop the v1 read and the third row reads unreadable; swap the two runtime
// fields in cpuLine and the fourth row prints NumCPU=16 GOMAXPROCS=4.
func TestCPULineReportsTheQuota(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cpus  int
		procs int
		files map[string]string
		want  string
	}{
		{name: "v2 quota", cpus: 16, procs: 16, files: map[string]string{cgroupV2Max: "400000 100000\n"}, want: "400000 100000"},
		{name: "v2 without a quota", cpus: 16, procs: 16, files: map[string]string{cgroupV2Max: "max 100000\n"}, want: "max 100000"},
		{name: "v1 quota and period", cpus: 16, procs: 16, files: map[string]string{cgroupV1Quota: "400000\n", cgroupV1Period: "100000\n"}, want: "400000 100000"},
		{name: "GOMAXPROCS is its own field", cpus: 4, procs: 16, files: map[string]string{cgroupV2Max: "max 100000"}, want: "max 100000"},
		{name: "v2 outranks v1", cpus: 16, procs: 16, files: map[string]string{cgroupV2Max: "200000 100000", cgroupV1Quota: "400000", cgroupV1Period: "100000"}, want: "200000 100000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := fixedHost()
			h.numCPU = func() int { return tc.cpus }
			h.maxProcs = func() int { return tc.procs }
			h.readFile = fileReader(tc.files)
			code, stdout, stderr := runFileWith(t, "pass.jsonl", h)
			if code != 0 {
				t.Errorf("exit code = %d, want 0: the cpus line fails nothing", code)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
			want := fmt.Sprintf("tierwall: cpus: NumCPU=%d GOMAXPROCS=%d cgroup=%s\n", tc.cpus, tc.procs, tc.want)
			if !strings.HasSuffix(stdout, want) {
				t.Errorf("stdout = %q, want it to end with %q", stdout, want)
			}
		})
	}
}

// TestCPULineWhenTheQuotaIsUnreadable pins the refusal: a quota that cannot be
// read prints cgroup=unreadable and fails nothing, so the exit code stays go
// test's verdict, 0 on a pass and 1 on a fail. A failed read, an empty file and
// a v1 quota without its period all count as unreadable. Mutation: let an
// empty v2 file through and the line reads cgroup= with nothing after it; take
// the v1 quota alone and the half-v1 row prints a quota with no period.
func TestCPULineWhenTheQuotaIsUnreadable(t *testing.T) {
	t.Parallel()
	denied := func(string) ([]byte, error) { return nil, errors.New("permission denied") }
	for _, tc := range []struct {
		name string
		read func(string) ([]byte, error)
	}{
		{name: "every read fails", read: denied},
		{name: "no files", read: fileReader(nil)},
		{name: "empty v2 file", read: fileReader(map[string]string{cgroupV2Max: " \n"})},
		{name: "v1 quota without a period", read: fileReader(map[string]string{cgroupV1Quota: "400000\n"})},
		{name: "v1 period with an empty quota", read: fileReader(map[string]string{cgroupV1Quota: "\n", cgroupV1Period: "100000\n"})},
	} {
		for _, fx := range []struct {
			file string
			want int
		}{{"pass.jsonl", 0}, {"fail.jsonl", 1}} {
			t.Run(tc.name+"/"+fx.file, func(t *testing.T) {
				t.Parallel()
				h := fixedHost()
				h.readFile = tc.read
				code, stdout, stderr := runFileWith(t, fx.file, h)
				if code != fx.want {
					t.Errorf("exit code = %d, want %d: the cpus line leaves go test's verdict alone", code, fx.want)
				}
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing: an unreadable quota fails nothing", stderr)
				}
				if !strings.HasSuffix(stdout, "tierwall: cpus: NumCPU=16 GOMAXPROCS=16 cgroup=unreadable\n") {
					t.Errorf("stdout = %q, want it to end with the unreadable cpus line", stdout)
				}
			})
		}
	}
}
