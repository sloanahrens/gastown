package lintpin

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// call is one command the fake Runner was asked to run.
type call struct {
	name string
	args []string
}

// fakeRunner answers every call the case lists in answers, in order, and
// records the calls so a test can read what the code asked for.
func fakeRunner(answers ...string) (Runner, *[]call) {
	calls := &[]call{}
	next := 0
	run := func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name: name, args: args})
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

func TestShort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pin  string
		want string
	}{
		{"v2.13.2", "2.13.2"},
		{"2.13.2", "2.13.2"},
		{"v2.13.2-rc.1", "2.13.2-rc.1"},
	} {
		if got := Short(tc.pin); got != tc.want {
			t.Errorf("Short(%q) = %q, want %q", tc.pin, got, tc.want)
		}
	}
}

// TestVerifyRefusesEveryAnswerButThePin drives the refusal branches: a runner
// error, an empty answer, and another version must each be an error naming the
// pin, never a pass. Mutation: treat the empty answer or "2.0.0" as the pin
// and the lint runs with a linter nobody pinned.
func TestVerifyRefusesEveryAnswerButThePin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answer  string
		wantErr string
	}{
		{"missing", "error:exec: \"golangci-lint\": executable file not found in $PATH", "v2.13.2"},
		{"empty", "", "v2.13.2"},
		{"spaces", "  \n", "v2.13.2"},
		{"another version", "2.0.0\n", "2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run, _ := fakeRunner(tc.answer)
			got, err := Verify(run, "v2.13.2")
			if err == nil {
				t.Fatalf("Verify accepted %q (reported %q), want a refusal naming the pin", tc.answer, got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Verify error %q does not name %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "v2.13.2") {
				t.Errorf("Verify error %q does not name the pin v2.13.2", err)
			}
		})
	}
}

// TestVerifyAcceptsThePin checks the pass branch and the question asked: the
// linter is asked for version --short, and only its version equals the pin.
func TestVerifyAcceptsThePin(t *testing.T) {
	t.Parallel()
	run, calls := fakeRunner("2.13.2\n")
	got, err := Verify(run, "v2.13.2")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != "2.13.2" {
		t.Errorf("Verify returned %q, want 2.13.2", got)
	}
	if len(*calls) != 1 || (*calls)[0].name != Command || strings.Join((*calls)[0].args, " ") != "version --short" {
		t.Errorf("Verify ran %q, want golangci-lint version --short", *calls)
	}
}

// TestInstallSkipsAnAlreadyPinnedLinter checks the gate runner's path: when
// PATH carries the pin, nothing is installed. Mutation: install before
// checking, and every gate run compiles the linter again (gt-zzo1g2).
func TestInstallSkipsAnAlreadyPinnedLinter(t *testing.T) {
	t.Parallel()
	run, calls := fakeRunner("2.13.2\n")
	installed, err := Install(run, "v2.13.2")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installed {
		t.Error("Install installed the pin while PATH already carried it")
	}
	if len(*calls) != 1 {
		t.Errorf("Install ran %q, want only the version check", *calls)
	}
}

// TestInstallInstallsAnotherVersion checks the developer path: a linter at
// another version is replaced by the pin, from the Makefile's own version.
func TestInstallInstallsAnotherVersion(t *testing.T) {
	t.Parallel()
	run, calls := fakeRunner("2.0.0\n", "")
	installed, err := Install(run, "v2.13.2")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !installed {
		t.Error("Install did not install the pin over golangci-lint 2.0.0")
	}
	if len(*calls) != 2 {
		t.Fatalf("Install ran %q, want the version check and one install", *calls)
	}
	want := "install " + Module + "@v2.13.2"
	if got := (*calls)[1]; got.name != "go" || strings.Join(got.args, " ") != want {
		t.Errorf("Install ran %s %s, want go %s", got.name, strings.Join(got.args, " "), want)
	}
}

// TestInstallReportsAFailedInstall checks that a refused install is an error to
// the caller, never a silent success. Mutation: drop the error and make
// lint-tools passes with no pinned linter on PATH.
func TestInstallReportsAFailedInstall(t *testing.T) {
	t.Parallel()
	run, _ := fakeRunner("2.0.0\n", "error:exit status 1")
	installed, err := Install(run, "v2.13.2")
	if err == nil {
		t.Fatal("Install returned nil after the go install failed")
	}
	if installed {
		t.Error("Install reported an install that failed")
	}
	for _, want := range []string{Module, "v2.13.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install error %q does not name %q", err, want)
		}
	}
}
