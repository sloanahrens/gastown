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
// so the fixture is judged, and returns its exit code and its stdout and
// stderr combined.
func runBudget(t *testing.T, budget, fixture string) (int, string) {
	t.Helper()
	return runBudgetArgs(t, budget, nil, "-count=1", fixturePkg(fixture))
}

// fixturePkg is the package pattern of a fixture under testdata/budget.
func fixturePkg(fixture string) string {
	return "./internal/testpolicy/testdata/budget/" + fixture
}

// runBudgetArgs runs the budget runner from the repo root with args as its
// go test arguments, an empty overbudget list, and an unconverted list
// naming the given fixtures, and returns its exit code and its stdout and
// stderr combined.
func runBudgetArgs(t *testing.T, budget string, unconverted []string, args ...string) (int, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var list strings.Builder
	for _, f := range unconverted {
		list.WriteString("internal/testpolicy/testdata/budget/" + f + "\n")
	}
	listFile := filepath.Join(dir, "unconverted.txt")
	if err := os.WriteFile(listFile, []byte(list.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", append([]string{"run", "./internal/testpolicy/cmd/budget",
		"-budget", budget, "-unconverted", listFile, "-overbudget", empty, "--"}, args...)...)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), out.String()
	default:
		t.Fatalf("go run budget: %v\n%s", err, out.String())
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

// TestIntegrationBudgetReportsSignal checks that a test binary killed by a
// signal is reported the way plain go test reports it ("signal: killed"),
// although the binary runs under the runner's exec wrapper.
func TestIntegrationBudgetReportsSignal(t *testing.T) {
	t.Parallel()
	code, out := runBudget(t, "10s", "sigkill")
	if code == 0 {
		t.Fatalf("exit code = 0, want a failure; output:\n%s", out)
	}
	if !strings.Contains(out, "signal: killed") {
		t.Fatalf("output has no \"signal: killed\" line:\n%s", out)
	}
}

// TestIntegrationBudgetCachesUnconverted checks that the runner leaves go
// test's result cache on for unconverted packages: run twice on an
// unchanged tree, the second run reports the unconverted fixture
// "(cached)", while the judged fixture runs again under the exec wrapper
// (a cached package runs no binary, so the budget would have nothing to
// measure).
func TestIntegrationBudgetCachesUnconverted(t *testing.T) {
	t.Parallel()
	args := []string{"-timeout", "5m", fixturePkg("cached"), fixturePkg("quick")}
	for run := 1; run <= 2; run++ {
		code, out := runBudgetArgs(t, "10s", []string{"cached"}, args...)
		if code != 0 {
			t.Fatalf("run %d: exit code = %d, want 0; output:\n%s", run, code, out)
		}
		if run == 1 {
			continue
		}
		if !regexp.MustCompile(`(?m)^ok\s+\S+/testdata/budget/cached\s+\(cached\)`).MatchString(out) {
			t.Fatalf("second run did not reuse the cached result of the unconverted fixture:\n%s", out)
		}
		if !regexp.MustCompile(`(?m)^ok\s+\S+/testdata/budget/quick\s`).MatchString(out) ||
			regexp.MustCompile(`(?m)^ok\s+\S+/testdata/budget/quick\s+\(cached\)`).MatchString(out) {
			t.Fatalf("second run did not run the judged fixture again:\n%s", out)
		}
	}
}

// TestIntegrationBudgetUnconvertedFailureFails checks that a failure in the
// unconverted half fails the run and is shown the way plain go test shows
// it, and that an empty judged half runs nothing (go test with no packages
// would test the current directory instead).
func TestIntegrationBudgetUnconvertedFailureFails(t *testing.T) {
	t.Parallel()
	code, out := runBudgetArgs(t, "10s", []string{"sigkill"}, "-count=1", fixturePkg("sigkill"))
	if code == 0 {
		t.Fatalf("exit code = 0, want a failure; output:\n%s", out)
	}
	if !strings.Contains(out, "signal: killed") || !regexp.MustCompile(`(?m)^FAIL\s+\S+/testdata/budget/sigkill\s`).MatchString(out) {
		t.Fatalf("output lacks the sigkill fixture's signal and FAIL lines:\n%s", out)
	}
	if strings.Contains(out, "no Go files") {
		t.Fatalf("the empty judged half ran go test on the current directory:\n%s", out)
	}
}

// TestIntegrationBudgetJudgedFailureWithCachedPass checks that a budget
// failure in the judged half fails the run although the unconverted half
// passes, and that the BUDGET report comes after both halves' output.
func TestIntegrationBudgetJudgedFailureWithCachedPass(t *testing.T) {
	t.Parallel()
	code, out := runBudgetArgs(t, "1s", []string{"cached"}, "-count=1", fixturePkg("childcpu"), fixturePkg("cached"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (childcpu over budget); output:\n%s", code, out)
	}
	budgetAt := strings.Index(out, "BUDGET: internal/testpolicy/testdata/budget/childcpu")
	cachedAt := regexp.MustCompile(`(?m)^ok\s+\S+/testdata/budget/cached\s`).FindStringIndex(out)
	if budgetAt < 0 || cachedAt == nil || budgetAt < cachedAt[0] {
		t.Fatalf("want the unconverted fixture's ok line, then the BUDGET line for childcpu:\n%s", out)
	}
}
