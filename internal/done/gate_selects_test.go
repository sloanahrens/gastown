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
	err := checkGateSelectsForSubmit(t, run, gateSelectsDescription)
	if err == nil {
		t.Fatal("checkBranchForSubmit() accepted a -run pattern that selects no test")
	}
	for _, want := range []string{
		"go test ./internal/done -run 'TestGateSelects' -count=1",
		"./internal/done",
		"-run matches test function names; name the tests so the pattern selects them",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

// TestGateSelectsRefusesAListThatFailsToBuild: a go test -list that fails to
// build refuses, carrying the first lines of its output.
func TestGateSelectsRefusesAListThatFailsToBuild(t *testing.T) {
	t.Parallel()
	run := func(_ string, _ ...string) ([]byte, error) {
		return []byte("./gate_selects_test.go:3:2: undefined: missing\n"), errors.New("exit status 1")
	}
	err := checkGateSelectsForSubmit(t, run, gateSelectsDescription)
	if err == nil {
		t.Fatal("checkBranchForSubmit() read a failed go test -list as a pass")
	}
	for _, want := range []string{"could not build the test binary", "./gate_selects_test.go:3:2: undefined: missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

// TestGateSelectsABeadWithNoRunLine: a Gate with no go test -run line has no
// pattern to check, so the branch check passes without running go test.
func TestGateSelectsABeadWithNoRunLine(t *testing.T) {
	t.Parallel()
	run := func(_ string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("go test ran for a Gate with no -run line: %q", args)
	}
	description := "## Goal\nx\n\n## Gate\nmake gate\ngo vet ./internal/done\n"
	if err := checkGateSelectsForSubmit(t, run, description); err != nil {
		t.Fatalf("checkBranchForSubmit() = %v, want a Gate with no -run line to pass", err)
	}
}
