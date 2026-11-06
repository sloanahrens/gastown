package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/lintpin"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeRunner answers each call with the next entry of answers and records the
// calls it was asked to run.
func fakeRunner(answers ...string) (lintpin.Runner, *[][]string) {
	calls := &[][]string{}
	next := 0
	run := func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		if next >= len(answers) {
			return nil, errors.New("fake runner: no answer left")
		}
		answer := answers[next]
		next++
		if strings.HasPrefix(answer, "error:") {
			return nil, errors.New(strings.TrimPrefix(answer, "error:"))
		}
		return []byte(answer), nil
	}
	return run, calls
}

// runMain runs the command and returns stdout, stderr and the exit code.
func runMain(runCmd lintpin.Runner, args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	code = run(args, runCmd, &out, &errb)
	return out.String(), errb.String(), code
}

// TestRunRefusesACommandLine covers the exits 2: no mode, an unknown mode, an
// empty pin, and an argument left after the flags. Mutation: default an empty
// pin or an unknown mode, and a mistyped call checks nothing and passes.
func TestRunRefusesACommandLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no mode", nil, "name a mode"},
		{"unknown mode", []string{"bogus", "-pin", "v2.13.2"}, "unknown mode"},
		{"empty pin", []string{"check", "-pin", ""}, "-pin is empty"},
		{"missing pin", []string{"check"}, "-pin is empty"},
		{"stray argument", []string{"check", "-pin", "v2.13.2", "extra"}, "unexpected argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runCmd, calls := fakeRunner("2.13.2\n")
			_, stderr, code := runMain(runCmd, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 for %v", code, tc.args)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not name %q", stderr, tc.want)
			}
			if len(*calls) != 0 {
				t.Errorf("the refused command line still ran %q", *calls)
			}
		})
	}
}

// TestRunCheck exercises the check mode: the pin passes silently, and another
// version or a missing linter exits 1 with the pin named.
func TestRunCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer string
		code   int
		want   string
	}{
		{"pinned", "2.13.2\n", 0, ""},
		{"another version", "2.0.0\n", 1, "v2.13.2"},
		{"missing", `error:exec: "golangci-lint": executable file not found in $PATH`, 1, "v2.13.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runCmd, _ := fakeRunner(tc.answer)
			stdout, stderr, code := runMain(runCmd, "check", "-pin", "v2.13.2")
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			if tc.want != "" && !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not name %q", stderr, tc.want)
			}
		})
	}
}

// TestRunInstall exercises the install mode: the pinned linter is reported and
// left alone, another version is installed, and a failed install exits 1.
func TestRunInstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answers []string
		code    int
		want    string
	}{
		{"already pinned", []string{"2.13.2\n"}, 0, "already on PATH"},
		{"another version", []string{"2.0.0\n", ""}, 0, "installed golangci-lint v2.13.2"},
		{"install fails", []string{"2.0.0\n", "error:exit status 1"}, 1, "v2.13.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runCmd, _ := fakeRunner(tc.answers...)
			stdout, stderr, code := runMain(runCmd, "install", "-pin", "v2.13.2")
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			if out := stdout + stderr; !strings.Contains(out, tc.want) {
				t.Errorf("output %q does not name %q", out, tc.want)
			}
		})
	}
}
