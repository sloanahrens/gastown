package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runFile feeds a canned go test -json stream from testdata through run, the
// way the Makefile target feeds a real one through stdin, and returns the
// exit code with what run wrote on both streams.
func runFile(t *testing.T, name string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var out, errOut bytes.Buffer
	code = run(args, f, &out, &errOut)
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
		&out, &errOut)
	if code != 0 {
		t.Errorf("exit code = %d, want 0: no event failed", code)
	}
	want := "go: downloading something\nnot json at all\ntierwall: tier 0s bar 90s OK\n"
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
		code := run(tc.args, strings.NewReader(""), &out, &errOut)
		if code != tc.want {
			t.Errorf("run(%v) exit code = %d, want %d", tc.args, code, tc.want)
		}
		if errOut.Len() == 0 {
			t.Errorf("run(%v) stderr is empty, want the usage there", tc.args)
		}
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
