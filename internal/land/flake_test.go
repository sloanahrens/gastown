package land

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
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
	want := []FailedTest{{flakyPkg, "TestA"}, {flakyPkg, "TestB"}}
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
		FailedTests: []FailedTest{{flakyPkg, "TestA"}}}}}
}

// flakeFixture is a landing whose gate is red in flakyPkg; rerun answers
// the rerun and records what it was asked to run.
func flakeFixture(t *testing.T, rerun func(pkgs []string) GateResult) (*landFixture, *Lander, *fakeGateBeads, *[][]string) {
	t.Helper()
	f := newLandFixture(t)
	f.gate.fn = redGate
	gb := &fakeGateBeads{}
	var calls [][]string
	l := f.lander()
	l.GateBeads = gb
	l.Rerun = func(_ context.Context, dir string, pkgs []string) GateResult {
		if dir != f.gate.dirs[0] {
			t.Errorf("rerun in %s, want the gated tree %s", dir, f.gate.dirs[0])
		}
		calls = append(calls, pkgs)
		return rerun(pkgs)
	}
	return f, l, gb, &calls
}

func passedRerun(pkgs []string) GateResult {
	var res []PackageResult
	for _, p := range pkgs {
		res = append(res, PackageResult{Package: p, Passed: true})
	}
	return GateResult{Passed: true, Steps: []StepResult{{Name: "test", Packages: res}}}
}

func TestLandRerunsOnlyTheFailedPackageAndLandsAFlake(t *testing.T) {
	t.Parallel()
	f, l, gb, calls := flakeFixture(t, passedRerun)
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(*calls) != 1 || len((*calls)[0]) != 1 || (*calls)[0][0] != flakyPkg {
		t.Fatalf("reruns = %v, want one rerun of %s alone", *calls, flakyPkg)
	}
	if f.originMain() != res.LandedCommit {
		t.Fatalf("origin/main = %s, want landed %s", f.originMain(), res.LandedCommit)
	}
	if len(gb.filed) != 1 || gb.filed[0].Kind != GateBeadFlake || gb.filed[0].Package != flakyPkg || gb.filed[0].Test != "TestA" ||
		!strings.Contains(gb.filed[0].Detail, "gt-abc") {
		t.Fatalf("filed = %+v, want one flake bead for %s TestA", gb.filed, flakyPkg)
	}
	if len(res.Flaky) != 1 || res.Flaky[0].BeadID != "gt-flk" || res.Rerun == nil {
		t.Errorf("result flaky = %+v rerun = %v", res.Flaky, res.Rerun)
	}
	if notes := f.bead().Notes; !strings.Contains(notes, "flaky: "+flakyPkg+" TestA [gt-flk]") {
		t.Errorf("landing record does not name the flake:\n%s", notes)
	}
}

func TestLandRerunStillRedIsAGateRejection(t *testing.T) {
	t.Parallel()
	f, l, gb, calls := flakeFixture(t, func([]string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestA again\n", Packages: []PackageResult{{Package: flakyPkg}}}}}
	})
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if len(*calls) != 1 || len(gb.filed) != 0 {
		t.Errorf("reruns %v, filed %+v; want one rerun and no bead", *calls, gb.filed)
	}
	if !strings.Contains(rej.Reason, "rerun of the failed package(s) failed too") || !strings.Contains(rej.GateTail, "TestA again") {
		t.Errorf("rejection = %q tail %q", rej.Reason, rej.GateTail)
	}
}

// A rerun that exits zero but does not report the package ok proves nothing.
func TestLandRerunMustReportEveryPackageOK(t *testing.T) {
	t.Parallel()
	f, l, _, _ := flakeFixture(t, func([]string) GateResult {
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	})
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)
}

func TestLandRerunThatCannotRunIsInfra(t *testing.T) {
	t.Parallel()
	f, l, _, _ := flakeFixture(t, func([]string) GateResult { return GateResult{Err: errors.New("slot unavailable")} })
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "gate rerun" {
		t.Fatalf("Land error = %T %v, want *InfraError at gate rerun", err, err)
	}
	f.assertUntouched(t)
}

func TestLandBudgetOverrunBlocksWithoutRerun(t *testing.T) {
	t.Parallel()
	f, l, gb, calls := flakeFixture(t, passedRerun)
	f.gate.fn = func(string) GateResult {
		res := redGate("")
		res.Steps[0].BudgetOverruns = []BudgetOverrun{{Package: "internal/x", Line: "BUDGET: internal/x used 12s"}}
		return res
	}
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || !strings.Contains(err.Error(), "test budget overrun") {
		t.Fatalf("Land error = %T %v, want a budget *InfraError", err, err)
	}
	if len(*calls) != 0 {
		t.Errorf("a budget overrun was rerun: %v", *calls)
	}
	if len(gb.filed) != 1 || gb.filed[0].Kind != GateBeadBudget || gb.filed[0].Package != "internal/x" {
		t.Errorf("filed = %+v, want one budget bead", gb.filed)
	}
	f.assertUntouched(t)
	if f.originMain() != f.base {
		t.Error("origin/main moved on a budget overrun")
	}
}

// A red gate that named no failing package (lint, build) has nothing to
// rerun: it is a rejection.
func TestLandRedGateWithoutPackagesIsNotRerun(t *testing.T) {
	t.Parallel()
	f, l, _, calls := flakeFixture(t, passedRerun)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "gate", ExitCode: 1, Tail: "gate: FAILED at build\n"}}}
	}
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)
	if len(*calls) != 0 {
		t.Errorf("reruns = %v, want none", *calls)
	}
}

// A flake bead that cannot be filed never changes the verdict.
func TestLandFlakeFilingFailureStillLands(t *testing.T) {
	t.Parallel()
	f, l, gb, _ := flakeFixture(t, passedRerun)
	gb.err = errors.New("bd down")
	res, err := l.Land(context.Background(), f.work)
	if err != nil || f.originMain() != res.LandedCommit {
		t.Fatalf("Land = %v, origin/main %s", err, f.originMain())
	}
	if len(res.Flaky) != 1 || res.Flaky[0].BeadID != "not filed" {
		t.Errorf("flaky = %+v", res.Flaky)
	}
}

func TestFlakyTestsNamesAPackageWithoutATest(t *testing.T) {
	t.Parallel()
	step := StepResult{FailedTests: []FailedTest{{flakyPkg, "TestA"}}}
	got := flakyTests(step, []string{flakyPkg, otherPkg})
	if len(got) != 2 || got[1] != (FailedTest{otherPkg, NoTestNamed}) {
		t.Errorf("flaky = %+v", got)
	}
}
