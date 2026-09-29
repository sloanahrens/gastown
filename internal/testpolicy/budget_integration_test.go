//go:build integration

package testpolicy

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runBudget runs the budget runner from the repo root over one fixture
// package under testdata/budget, with empty unconverted and overbudget lists
// so the fixture is judged, and returns its exit code and stderr.
func runBudget(t *testing.T, budget, fixture string) (int, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./internal/testpolicy/cmd/budget",
		"-budget", budget, "-unconverted", empty, "-overbudget", empty,
		"--", "-count=1", "./internal/testpolicy/testdata/budget/"+fixture)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), stderr.String()
	default:
		t.Fatalf("go run budget: %v\n%s", err, stderr.String())
		return 0, ""
	}
}

var budgetLine = regexp.MustCompile(`BUDGET: internal/testpolicy/testdata/budget/childcpu used ([0-9.]+)(ms|s) user CPU`)

// TestIntegrationBudgetCountsChildCPU checks that the runner charges a
// package for the user CPU of the processes its test binary waited for:
// childcpu's test binary burns 1.5 s in a child and almost nothing itself.
func TestIntegrationBudgetCountsChildCPU(t *testing.T) {
	t.Parallel()
	code, stderr := runBudget(t, "1s", "childcpu")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (over budget); stderr:\n%s", code, stderr)
	}
	m := budgetLine.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("stderr has no BUDGET line for childcpu:\n%s", stderr)
	}
	d, err := time.ParseDuration(m[1] + m[2])
	if err != nil {
		t.Fatal(err)
	}
	if d < 1500*time.Millisecond {
		t.Fatalf("childcpu used %s user CPU, want at least the child's 1.5s; stderr:\n%s", d, stderr)
	}
}

// TestIntegrationBudgetIgnoresWallTime checks that a package that takes more
// wall time than the budget but little CPU passes: idle's test waits 2 s.
func TestIntegrationBudgetIgnoresWallTime(t *testing.T) {
	t.Parallel()
	code, stderr := runBudget(t, "1s", "idle")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (2 s of wall, almost no CPU); stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "BUDGET:") {
		t.Fatalf("stderr reports a budget failure:\n%s", stderr)
	}
}
