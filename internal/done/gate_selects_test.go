package done

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// gateSelectsDescription is a bead whose Gate holds one go test -run line, so
// the branch check reaches the pattern the line names.
const gateSelectsDescription = "## Goal\nx\n\n## Gate\ngo test ./internal/done -run 'TestGateSelects' -count=1\ngo vet ./internal/done\n"

// checkGateSelectsForSubmit drives the production call site: a polecat's
// branch check over a bead carrying description, with run standing in for go
// test.
func checkGateSelectsForSubmit(t *testing.T, run gateListRunner, description string) error {
	t.Helper()
	r := &doneRun{
		cwd:  t.TempDir(),
		out:  new(bytes.Buffer),
		opts: doneOptions{allowReverts: true, allowThrowawayPaths: true},
		deps: doneSubmitDeps{listTests: run},
	}
	return checkBranchForSubmit(r, doneSubmission{sourceIssue: &beads.Issue{Description: description}}, "origin/main")
}

// wantGateRefusal asserts the branch check refused and the refusal names each
// of wants.
func wantGateRefusal(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("checkBranchForSubmit() accepted the Gate")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

// TestGateSelectsAPatternThatNamesATest: a -run pattern whose list names a
// test passes the branch check.
func TestGateSelectsAPatternThatNamesATest(t *testing.T) {
	t.Parallel()
	var calls []string
	run := func(_ string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return []byte("TestGateSelectsAPatternThatNamesATest\nok  \tgithub.com/x/internal/done\t0.002s\n"), nil
	}
	if err := checkGateSelectsForSubmit(t, run, gateSelectsDescription); err != nil {
		t.Fatalf("checkBranchForSubmit() = %v, want the pattern to pass", err)
	}
	if want := "test -list TestGateSelects ./internal/done"; !strings.Contains(strings.Join(calls, "\n"), want) {
		t.Errorf("go test calls = %q, want one containing %q", calls, want)
	}
}

// TestGateSelectsRefusesAPatternThatNamesNoTest: a -run pattern whose list
// names no test refuses, naming the Gate line and the package.
func TestGateSelectsRefusesAPatternThatNamesNoTest(t *testing.T) {
	t.Parallel()
	run := func(_ string, _ ...string) ([]byte, error) {
		return []byte("ok  \tgithub.com/x/internal/done\t0.002s\n"), nil
	}
	wantGateRefusal(t, checkGateSelectsForSubmit(t, run, gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1", "./internal/done",
		"-run matches test function names; name the tests so the pattern selects them")
}

// TestGateSelectsRefusesAListThatFailsToBuild: a go test -list that fails to
// build refuses, carrying the first lines of its output.
func TestGateSelectsRefusesAListThatFailsToBuild(t *testing.T) {
	t.Parallel()
	run := func(_ string, _ ...string) ([]byte, error) {
		return []byte("./gate_selects_test.go:3:2: undefined: missing\n"), errors.New("exit status 1")
	}
	wantGateRefusal(t, checkGateSelectsForSubmit(t, run, gateSelectsDescription),
		"could not build the test binary", "./gate_selects_test.go:3:2: undefined: missing")
}

// TestGateSelectsRefusesNoiseAroundAnEmptyList: go test -list can emit noise
// (a package's init logging, a warning) around an empty list; none of it names
// a test, so the Gate line refuses (gt-hx8f5i).
func TestGateSelectsRefusesNoiseAroundAnEmptyList(t *testing.T) {
	t.Parallel()
	run := func(_ string, _ ...string) ([]byte, error) {
		return []byte("2026/11/24 12:00:00 warning: no tests to run\nPASS\nok  \tgithub.com/x/internal/done\t0.002s\n"), nil
	}
	wantGateRefusal(t, checkGateSelectsForSubmit(t, run, gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1",
		"-run matches test function names; name the tests so the pattern selects them")
}

// TestGateSelectsRefusesAMissingRunner: a check with no list runner refuses a
// Gate that has a pattern to check, instead of reaching for the real go test.
func TestGateSelectsRefusesAMissingRunner(t *testing.T) {
	t.Parallel()
	wantGateRefusal(t, checkGateSelectsForSubmit(t, nil, gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1", "no go test runner is configured")
}

// TestGateSelectsABeadWithNoCheckedLine: a Gate with no go test -run line the
// check reads (none, no -run, or ./..., which it drops) has nothing to check,
// so the branch check passes without running go test (acceptance row four).
func TestGateSelectsABeadWithNoCheckedLine(t *testing.T) {
	t.Parallel()
	for _, description := range []string{
		"## Goal\nx\n\n## Gate\nmake gate\ngo vet ./internal/done\n",
		"## Goal\nx\n\n## Gate\ngo test ./internal/done -count=1\n",
		"## Goal\nx\n\n## Gate\ngo test ./... -run 'TestGateSelects' -count=1\n",
	} {
		run := func(_ string, args ...string) ([]byte, error) {
			return nil, fmt.Errorf("go test ran for a Gate with no checked line: %q", args)
		}
		if err := checkGateSelectsForSubmit(t, run, description); err != nil {
			t.Fatalf("checkBranchForSubmit(%q) = %v, want no checked line to pass", description, err)
		}
	}
}
