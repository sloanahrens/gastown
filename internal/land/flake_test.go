package land

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	flakyPkg = "github.com/x/a"
	otherPkg = "github.com/x/b"
)

func TestParseGoTestOutputNamesFailedTestsPerPackage(t *testing.T) {
	t.Parallel()
	out := "=== RUN TestA\n--- FAIL: TestA (0.01s)\n    --- FAIL: TestA/sub (0.00s)\n--- FAIL: TestB (0.00s)\nFAIL\n" +
		"FAIL\t" + flakyPkg + "\t0.2s\n" +
		"ok  \tgithub.com/x/c\t0.1s\n" +
		"panic: boom\nFAIL\t" + otherPkg + "\t0.3s\n"
	pkgs, tests := parseGoTestOutput(out)
	if len(pkgs) != 3 || pkgs[0] != (PackageResult{Package: flakyPkg}) || !pkgs[1].Passed || pkgs[2].Passed {
		t.Errorf("packages = %+v", pkgs)
	}
	want := []FailedTest{{flakyPkg, "TestA", 10 * time.Millisecond}, {flakyPkg, "TestB", 0}}
	if len(tests) != len(want) || tests[0] != want[0] || tests[1] != want[1] {
		t.Errorf("failed tests = %+v, want %+v", tests, want)
	}
}

func TestParseBudgetOverrunsOnlyFailingLines(t *testing.T) {
	t.Parallel()
	out := "BUDGET: internal/x used 12s user CPU (limit 10s user CPU); slowest: TestX 9s\n" +
		"BUDGET (reported only, load 9 >= 8 cpus; GATE_STRICT_BUDGET=1 enforces): internal/y used 11s\n" +
		"BUDGET: internal/z passed but its CPU time was not recorded (wall 1s); the budget cannot judge it\n"
	got := parseBudgetOverruns(out)
	if len(got) != 2 || got[0].Package != "internal/x" || got[1].Package != "internal/z" || !strings.HasPrefix(got[0].Line, "BUDGET: internal/x used") {
		t.Errorf("overruns = %+v", got)
	}
}

func TestParseWarningsKeepsTheGatesOwnWarnings(t *testing.T) {
	t.Parallel()
	out := "gate: build (go build ./...)\n" +
		"gate: WARNING exec tax 180 ms/exec in this process tree; see gt-2ycne.1\n" +
		"gate: FAILED at build\n" +
		"golangci-lint: something gate: WARNING but not at the line start\n"
	got := parseWarnings(out)
	want := "exec tax 180 ms/exec in this process tree; see gt-2ycne.1"
	if len(got) != 1 || got[0] != want {
		t.Errorf("warnings = %q, want only %q", got, want)
	}
}

// A warning rides the landing record: a landing that took 12 minutes because
// its host was taxed says so in the record and in the bead's note.
func TestGateRecordCarriesTheWarnings(t *testing.T) {
	t.Parallel()
	res := Result{
		Gate: GateResult{Passed: true, Steps: []StepResult{
			{Name: "preflight", Warnings: []string{"exec tax 180 ms/exec in this process tree"}},
			{Name: "unit", Warnings: []string{"packages mostly waiting, not computing"}},
		}},
		Rerun: &GateResult{Passed: true},
	}
	got := gateRecord(res)
	for _, want := range []string{"pass (", "exec tax 180 ms/exec", "packages mostly waiting", "rerun of the failed package(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("gateRecord = %q, want it to name %q", got, want)
		}
	}
}

type fakeGateBeads struct {
	mu    sync.Mutex
	filed []GateBead
	err   error
}

func (g *fakeGateBeads) FileGateBead(b GateBead) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.filed = append(g.filed, b)
	if g.err != nil {
		return "", g.err
	}
	return "gt-flk", nil
}

// redGate fails the test step with flakyPkg's TestA failing and otherPkg ok.
func redGate(string) GateResult {
	return GateResult{Steps: []StepResult{{Name: "gate", ExitCode: 1, Tail: "FAIL\t" + flakyPkg + "\n",
		Packages:    []PackageResult{{Package: flakyPkg}, {Package: otherPkg, Passed: true}},
		FailedTests: []FailedTest{{Package: flakyPkg, Test: "TestA"}}}}}
}

// redGatePackages fails the test step with n failing tests in each named
// package, the i-th of them taking i+1 seconds.
func redGatePackages(n int, pkgs ...string) func(string) GateResult {
	return func(string) GateResult {
		var res []PackageResult
		var tests []FailedTest
		for _, p := range pkgs {
			res = append(res, PackageResult{Package: p})
			for i := 0; i < n; i++ {
				tests = append(tests, FailedTest{Package: p, Test: fmt.Sprintf("Test%02d", i), Duration: time.Duration(i+1) * time.Second})
			}
		}
		return GateResult{Steps: []StepResult{{Name: "gate", ExitCode: 1, Tail: "FAIL\n", Packages: res, FailedTests: tests}}}
	}
}

// flakeFixture is a Lander whose red gate is redGate's; rerun answers the
// rerun and records what it was asked to run. The policy is driven directly:
// with the local gate gone (gt-fn9e6.32) the landing path's verdict is CI's,
// and applyFlakePolicy is where the policy's decisions live.
func flakeFixture(t *testing.T, rerun func(pkgs []string) GateResult) (*Lander, *fakeGateBeads, *[][]string) {
	t.Helper()
	gb := &fakeGateBeads{}
	var calls [][]string
	var out bytes.Buffer
	l := &Lander{GateBeads: gb, Out: &out}
	l.Rerun = func(_ context.Context, _ string, pkgs []string) GateResult {
		calls = append(calls, pkgs)
		return rerun(pkgs)
	}
	return l, gb, &calls
}

// applyRedGate runs the flake policy on the fixture's red gate at the merged
// tree "deadbeef", the way Land does when the gate comes back red.
func applyRedGate(t *testing.T, l *Lander, gateRes GateResult) (flakeVerdict, error) {
	t.Helper()
	w := Work{BeadID: "gt-abc", Branch: fixtureBranch, Head: "cafe", Target: "main"}
	return l.applyFlakePolicy(context.Background(), "/work/tree", w, "deadbeef", gateRes)
}

func passedRerun(pkgs []string) GateResult {
	var res []PackageResult
	for _, p := range pkgs {
		res = append(res, PackageResult{Package: p, Passed: true})
	}
	return GateResult{Passed: true, Steps: []StepResult{{Name: "test", Packages: res}}}
}

func TestFlakePolicyRerunsOnlyTheFailedPackageAndLandsAFlake(t *testing.T) {
	t.Parallel()
	l, gb, calls := flakeFixture(t, passedRerun)
	v, err := applyRedGate(t, l, redGate(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(*calls) != 1 || len((*calls)[0]) != 1 || (*calls)[0][0] != flakyPkg {
		t.Fatalf("reruns = %v, want one rerun of %s alone", *calls, flakyPkg)
	}
	if len(gb.filed) != 1 || gb.filed[0].Kind != GateBeadFlake || gb.filed[0].Package != flakyPkg || gb.filed[0].Test != "TestA" ||
		!strings.Contains(gb.filed[0].Detail, "gt-abc") {
		t.Fatalf("filed = %+v, want one flake bead for %s TestA", gb.filed, flakyPkg)
	}
	if len(v.flakes) != 1 || v.flakes[0].BeadID != "gt-flk" || v.rerun == nil {
		t.Errorf("verdict flakes = %+v rerun = %v", v.flakes, v.rerun)
	}
}

func TestFlakePolicyRerunStillRedIsARejection(t *testing.T) {
	t.Parallel()
	l, gb, calls := flakeFixture(t, func([]string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestA again\n", Packages: []PackageResult{{Package: flakyPkg}}}}}
	})
	v, err := applyRedGate(t, l, redGate(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(*calls) != 1 || len(gb.filed) != 0 {
		t.Errorf("reruns %v, filed %+v; want one rerun and no bead", *calls, gb.filed)
	}
	if v.rerun == nil || len(v.flakes) != 0 {
		t.Errorf("verdict = %+v; want the rerun recorded and no flake", v)
	}
}

// A rerun that exits zero but does not report the package ok proves nothing.
func TestFlakePolicyRerunMustReportEveryPackageOK(t *testing.T) {
	t.Parallel()
	l, _, _ := flakeFixture(t, func([]string) GateResult {
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	})
	v, err := applyRedGate(t, l, redGate(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(v.flakes) != 0 {
		t.Errorf("flakes = %+v; an unproven rerun lands nothing", v.flakes)
	}
}

func TestFlakePolicyRerunThatCannotRunIsInfra(t *testing.T) {
	t.Parallel()
	l, _, _ := flakeFixture(t, func([]string) GateResult { return GateResult{Err: errors.New("slot unavailable")} })
	_, err := applyRedGate(t, l, redGate(""))
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "gate rerun" {
		t.Fatalf("applyFlakePolicy error = %T %v, want *InfraError at gate rerun", err, err)
	}
}

func TestFlakePolicyBudgetOverrunBlocksWithoutRerun(t *testing.T) {
	t.Parallel()
	l, gb, calls := flakeFixture(t, passedRerun)
	red := redGate("")
	red.Steps[0].BudgetOverruns = []BudgetOverrun{{Package: "internal/x", Line: "BUDGET: internal/x used 12s"}}
	_, err := applyRedGate(t, l, red)
	var infra *InfraError
	if !errors.As(err, &infra) || !strings.Contains(err.Error(), "test budget overrun") {
		t.Fatalf("applyFlakePolicy error = %T %v, want a budget *InfraError", err, err)
	}
	if len(*calls) != 0 {
		t.Errorf("a budget overrun was rerun: %v", *calls)
	}
	if len(gb.filed) != 1 || gb.filed[0].Kind != GateBeadBudget || gb.filed[0].Package != "internal/x" {
		t.Errorf("filed = %+v, want one budget bead", gb.filed)
	}
}

// A red gate that named no failing package (lint, build) has nothing to
// rerun: it is a rejection.
func TestFlakePolicyRedGateWithoutPackagesIsNotRerun(t *testing.T) {
	t.Parallel()
	l, _, calls := flakeFixture(t, passedRerun)
	v, err := applyRedGate(t, l, GateResult{Steps: []StepResult{{Name: "gate", ExitCode: 1, Tail: "gate: FAILED at build\n"}}})
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(*calls) != 0 || len(v.flakes) != 0 || v.rerun != nil {
		t.Errorf("verdict = %+v reruns = %v; want nothing rerun and no flake", v, *calls)
	}
}

// A flake bead that cannot be filed never changes the verdict.
func TestFlakePolicyFilingFailureStillLands(t *testing.T) {
	t.Parallel()
	l, gb, _ := flakeFixture(t, passedRerun)
	gb.err = errors.New("bd down")
	v, err := applyRedGate(t, l, redGate(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(v.flakes) != 1 || v.flakes[0].BeadID != "not filed" {
		t.Errorf("flakes = %+v", v.flakes)
	}
}

func TestFlakyTestsNamesAPackageWithoutATest(t *testing.T) {
	t.Parallel()
	step := StepResult{FailedTests: []FailedTest{{Package: flakyPkg, Test: "TestA"}}}
	got := flakyTests(step, []string{flakyPkg, otherPkg})
	if len(got) != 2 || got[1] != (FailedTest{Package: otherPkg, Test: NoTestNamed}) {
		t.Errorf("flaky = %+v", got)
	}
}

// Below the threshold nothing changes: each failed test keeps its own bead.
func TestFlakePolicyFilesOneBeadPerTestBelowThePackageThreshold(t *testing.T) {
	t.Parallel()
	l, gb, _ := flakeFixture(t, passedRerun)
	v, err := applyRedGate(t, l, redGatePackages(MinPackageFlakeTests-1, flakyPkg)(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(gb.filed) != MinPackageFlakeTests-1 || len(v.flakes) != MinPackageFlakeTests-1 {
		t.Fatalf("filed %d %+v, flakes %d; want %d beads", len(gb.filed), gb.filed, len(v.flakes), MinPackageFlakeTests-1)
	}
	for _, b := range gb.filed {
		if b.Kind != GateBeadFlake || b.Package != flakyPkg || b.Test == "" || len(b.Tests) != 0 {
			t.Errorf("bead = %+v, want one per test", b)
		}
	}
}

// At the threshold the package's tests are one bead, listing them, with the
// first failure and its duration so a setup stall can be told from a flake.
func TestFlakePolicyFilesOnePackageBeadAtTheThreshold(t *testing.T) {
	t.Parallel()
	l, gb, _ := flakeFixture(t, passedRerun)
	v, err := applyRedGate(t, l, redGatePackages(MinPackageFlakeTests, flakyPkg)(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(gb.filed) != 1 || len(v.flakes) != 1 {
		t.Fatalf("filed %+v, flakes %+v; want one bead", gb.filed, v.flakes)
	}
	b := gb.filed[0]
	if b.Kind != GateBeadFlakePackage || b.Package != flakyPkg || b.Test != "Test00" || len(b.Tests) != MinPackageFlakeTests {
		t.Fatalf("bead = %+v, want a %d-test package bead", b, MinPackageFlakeTests)
	}
	if !strings.Contains(b.Detail, "First failing test: Test00 (1s)") || !strings.Contains(b.Detail, "Test04") {
		t.Errorf("detail does not record the first failure and the tests:\n%s", b.Detail)
	}
}

// The hm-8uq sighting: 41 tests behind one container stall file one bead, with
// all 41 names listed in order.
func TestFlakePolicyFilesOnePackageBeadForFortyOneFailures(t *testing.T) {
	t.Parallel()
	l, gb, _ := flakeFixture(t, passedRerun)
	v, err := applyRedGate(t, l, redGatePackages(41, flakyPkg)(""))
	if err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(gb.filed) != 1 || gb.filed[0].Kind != GateBeadFlakePackage || len(gb.filed[0].Tests) != 41 {
		t.Fatalf("filed %+v, want one package bead listing 41 tests", gb.filed)
	}
	for i, name := range gb.filed[0].Tests {
		if want := fmt.Sprintf("Test%02d", i); name != want {
			t.Fatalf("tests[%d] = %q, want %q (all names listed in order)", i, name, want)
		}
	}
	if len(v.flakes) != 1 {
		t.Errorf("flakes = %+v, want one entry", v.flakes)
	}
}

// Two packages each over the threshold get a bead each.
func TestFlakePolicyFilesOnePackageBeadPerFailingPackage(t *testing.T) {
	t.Parallel()
	l, gb, _ := flakeFixture(t, passedRerun)
	if _, err := applyRedGate(t, l, redGatePackages(MinPackageFlakeTests, flakyPkg, otherPkg)("")); err != nil {
		t.Fatalf("applyFlakePolicy: %v", err)
	}
	if len(gb.filed) != 2 {
		t.Fatalf("filed %+v, want two package beads", gb.filed)
	}
	for _, b := range gb.filed {
		if b.Kind != GateBeadFlakePackage || len(b.Tests) != MinPackageFlakeTests {
			t.Errorf("bead = %+v, want a package bead", b)
		}
	}
}
