package land

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// The flake policy (gt-v4ssj.5, deep review D2 Q5): a red gate on the merged
// tree is never waved through by judgment. Land decides it here, in one place:
//
//   - A test budget overrun (a "BUDGET:" line from the testpolicy budget
//     runner) is an infrastructure class. Nothing is rerun, the landing stops
//     with an *InfraError, and a bead is filed per package over budget.
//   - Otherwise, when the failed step named failing Go packages, those
//     packages alone are rerun once in the same merged tree. If every one
//     passes, the landing goes on and each test that failed gets a flake bead
//     naming its package and test. If any fails again, the gate rejection
//     stands.
//   - A red gate that named no failing package (lint, build) is a rejection,
//     with no rerun.
//
// Nothing lands on a red final gate: the rerun replaces only the verdict of
// the packages it reran, and only when all of them pass.

// NoTestNamed stands in for the test when a failing package printed no
// "--- FAIL" line (a panic, a timeout, a TestMain failure).
const NoTestNamed = "(no test named)"

// GateBeadKind says what a gate bead records.
type GateBeadKind string

const (
	// GateBeadFlake is a test that failed on the merged tree and passed when
	// its package was rerun.
	GateBeadFlake GateBeadKind = "flake"
	// GateBeadBudget is a package over the test budget; it blocked a landing.
	GateBeadBudget GateBeadKind = "budget"
)

// GateBead is one bead the flake policy files.
type GateBead struct {
	Kind    GateBeadKind
	Package string
	// Test is the flaky test's name (NoTestNamed when the package named
	// none); empty for a budget overrun.
	Test string
	// Detail says where it was seen: the work, the merged commit, the runs.
	Detail string
}

// GateBeads files gate beads. Implementations comment on the open bead for
// the same package and test instead of filing a duplicate.
type GateBeads interface {
	FileGateBead(b GateBead) (id string, err error)
}

// Flake is a test that passed on the rerun of its package, and the bead it
// was filed under ("" when filing failed or no GateBeads is set).
type Flake struct {
	FailedTest
	BeadID string
}

// flakeVerdict is what the policy decided about a red gate.
type flakeVerdict struct {
	// rerun is the rerun's result; nil when nothing was rerun.
	rerun *GateResult
	// flakes is non-empty only when the rerun passed and the landing goes on.
	flakes []Flake
}

// applyFlakePolicy decides the red gate gateRes on the merged tree in dir.
// An error stops the landing as infrastructure (nothing written to the work
// bead). A verdict with no flakes means the gate rejection stands.
func (l *Lander) applyFlakePolicy(ctx context.Context, dir string, w Work, merged string, gateRes GateResult) (flakeVerdict, error) {
	if len(gateRes.Steps) == 0 {
		return flakeVerdict{}, nil
	}
	step := gateRes.Steps[len(gateRes.Steps)-1]
	if len(step.BudgetOverruns) > 0 {
		var pkgs []string
		for _, o := range step.BudgetOverruns {
			pkgs = append(pkgs, o.Package)
			id := l.fileGateBead(GateBead{Kind: GateBeadBudget, Package: o.Package, Detail: fmt.Sprintf(
				"The gate on %s's merged tree %s failed the test budget, which blocks the landing (an infrastructure class: no rerun). Budget runner:\n%s",
				w.BeadID, merged, o.Line)})
			l.logf("%s: test budget overrun in %s [%s]", w.BeadID, o.Package, id)
		}
		return flakeVerdict{}, &InfraError{Stage: "gate", Err: fmt.Errorf(
			"test budget overrun on the merged tree (%s): an infrastructure failure, not rerun; nothing lands until the budget passes", strings.Join(pkgs, ", "))}
	}
	failed := failedPackages(step)
	if len(failed) == 0 || l.Rerun == nil {
		return flakeVerdict{}, nil
	}
	if err := ctx.Err(); err != nil {
		return flakeVerdict{}, &InfraError{Stage: "gate rerun", Err: err}
	}
	l.logf("%s: gate red in %s; rerunning only those package(s) once", w.BeadID, strings.Join(failed, ", "))
	rr := l.Rerun(ctx, dir, failed)
	v := flakeVerdict{rerun: &rr}
	if rr.Err != nil {
		return v, &InfraError{Stage: "gate rerun", Err: rr.Err}
	}
	if !rr.Passed || !allPassed(rr, failed) {
		return v, nil
	}
	for _, ft := range flakyTests(step, failed) {
		f := Flake{FailedTest: ft}
		f.BeadID = l.fileGateBead(GateBead{Kind: GateBeadFlake, Package: ft.Package, Test: ft.Test, Detail: fmt.Sprintf(
			"%s failed in the gate on %s's merged tree %s and passed when its package was rerun once; the landing went on. Rerun: %s",
			ft.Test, w.BeadID, merged, rr.Summary())})
		v.flakes = append(v.flakes, f)
	}
	return v, nil
}

// fileGateBead files b and returns its id, or a note why there is none. A
// failure to file never changes the landing's verdict.
func (l *Lander) fileGateBead(b GateBead) string {
	if l.GateBeads == nil {
		return "not filed"
	}
	id, err := l.GateBeads.FileGateBead(b)
	if err != nil {
		l.logf("WARNING filing the %s bead for %s %s: %v", b.Kind, b.Package, b.Test, err)
		return "not filed"
	}
	return id
}

// failedPackages is each package step reported as FAIL, once, in order.
func failedPackages(step StepResult) []string {
	var out []string
	for _, p := range step.Packages {
		if !p.Passed && !slices.Contains(out, p.Package) {
			out = append(out, p.Package)
		}
	}
	return out
}

// allPassed reports whether the rerun reported every one of pkgs ok and none
// FAIL: a package the rerun did not report is not proven green.
func allPassed(rr GateResult, pkgs []string) bool {
	ok := map[string]bool{}
	for _, s := range rr.Steps {
		for _, p := range s.Packages {
			if !p.Passed {
				return false
			}
			ok[p.Package] = true
		}
	}
	for _, p := range pkgs {
		if !ok[p] {
			return false
		}
	}
	return true
}

// flakyTests is every failed test in pkgs, and a NoTestNamed entry for a
// package that failed without naming one.
func flakyTests(step StepResult, pkgs []string) []FailedTest {
	var out []FailedTest
	for _, pkg := range pkgs {
		named := false
		for _, ft := range step.FailedTests {
			if ft.Package == pkg {
				out = append(out, ft)
				named = true
			}
		}
		if !named {
			out = append(out, FailedTest{Package: pkg, Test: NoTestNamed})
		}
	}
	return out
}

// gateRecord is the gate line of a landing record: the gate's summary, the
// warnings it printed about the host it ran on (a slow landing is explained
// where it is recorded), and for a landing the flake policy let through, the
// rerun and the flakes.
func gateRecord(res Result) string {
	parts := append([]string{res.Gate.Summary()}, res.Gate.Warnings()...)
	if res.Rerun == nil {
		return strings.Join(parts, "; ")
	}
	names := make([]string, 0, len(res.Flaky))
	for _, f := range res.Flaky {
		names = append(names, fmt.Sprintf("%s %s [%s]", f.Package, f.Test, f.BeadID))
	}
	return fmt.Sprintf("%s; rerun of the failed package(s) %s; flaky: %s", strings.Join(parts, "; "), res.Rerun.Summary(), strings.Join(names, ", "))
}
