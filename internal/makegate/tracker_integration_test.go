//go:build integration

package makegate

import (
	"path/filepath"
	"strings"
	"testing"
)

// The tracker module cases: one test per case in
// scripts/makefile-gate_test.sh from "green stubs: the tracker module is
// vetted and tested after the root suite" through "a lint finding in the
// tracker module fails the gate and names the module". Each runs the real
// gate recipe against a GateSandbox's stub go, golangci-lint and exec-tax
// preflight; the stubs log the module a call ran in (`in tracker`) and can
// fail only inside (or outside) tracker/, so a case sees the module's stages
// without building, linting or testing the tree.
//
// The make calls are processes, so this file is integration tier
// (docs/standards/test-engineering.md, rule 2).

// callIndex returns the index of the first call line equal to line, or -1.
func callIndex(r GateRun, line string) int {
	for i, call := range r.Calls {
		if call == line {
			return i
		}
	}
	return -1
}

// anyCallHolds reports whether any call line holds substr.
func anyCallHolds(r GateRun, substr string) bool {
	for _, call := range r.Calls {
		if strings.Contains(call, substr) {
			return true
		}
	}
	return false
}

// TestIntegrationTrackerModuleVettedAndTestedAfterRootSuite checks "green
// stubs: the tracker module is vetted and tested after the root suite".
// Mutation: drop the TESTED_MODULES loop from gate-test's unit tier, and no
// `go vet in tracker` or `go test in tracker` call appears; run that loop
// before the root suite with its guard forced true, and the tracker calls
// come before the root `go test GT_TEST_DOCKER=0`.
func TestIntegrationTrackerModuleVettedAndTestedAfterRootSuite(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"})

	if r.Code != 0 {
		t.Fatalf("the gate exited %d on green stubs, want 0\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stderr, r.callLog())
	}
	if !r.hasCall("go vet in tracker") {
		t.Fatalf("the gate did not vet the tracker module\ncalls:\n%s", r.callLog())
	}
	if !r.hasCall("go test in tracker") {
		t.Fatalf("the gate did not test the tracker module\ncalls:\n%s", r.callLog())
	}
	root := callIndex(r, "go test GT_TEST_DOCKER=0")
	tracker := callIndex(r, "go vet in tracker")
	if root < 0 || tracker < 0 || tracker < root {
		t.Fatalf("the tracker module ran before the root suite (root go test at call %d, tracker vet at call %d)\ncalls:\n%s", root, tracker, r.callLog())
	}
}

// TestIntegrationTrackerTestsFailNameTheGoHalf checks "tracker tests fail:
// non-zero, names the Go half, no PASSED line".
// Mutation: drop the `|| { go_rc=$$?; break; }` guard from the tracker loop,
// and a failing tracker suite is ignored: the gate exits 0 and prints
// `gate: PASSED`.
func TestIntegrationTrackerTestsFailNameTheGoHalf(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"}, "STUB_GO_FAIL=test", "STUB_GO_FAIL_DIR=tracker")

	if r.Code == 0 {
		t.Fatalf("the gate exited 0 with failing tracker tests\nstderr:\n%s\ncalls:\n%s", r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "gate: FAILED at unit tier (Go suite") {
		t.Fatalf("the gate did not name the Go half\nstderr:\n%s", r.Stderr)
	}
	if strings.Contains(r.Stderr, "gate: PASSED") {
		t.Fatalf("the gate printed PASSED despite the failing tracker tests\nstderr:\n%s", r.Stderr)
	}
	if !r.hasCall("go test in tracker") {
		t.Fatalf("the failing tracker suite never ran\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationTrackerRootSuiteFailureSkipsTheModule checks "root suite
// fails: the tracker module does not run".
// Mutation: move the TESTED_MODULES loop out of the `if [ $$go_rc -eq 0 ]`
// guard, and the tracker module is vetted and tested after the root suite
// has already failed.
func TestIntegrationTrackerRootSuiteFailureSkipsTheModule(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"}, "STUB_GO_FAIL=test", "STUB_GO_FAIL_DIR=!tracker")

	if r.Code == 0 {
		t.Fatalf("the gate exited 0 with a failing root suite\nstderr:\n%s\ncalls:\n%s", r.Stderr, r.callLog())
	}
	if r.hasCall("go vet in tracker") || r.hasCall("go test in tracker") {
		t.Fatalf("the gate ran the tracker module after the root suite failed\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationTrackerLintUsesTheRootConfig checks "lint runs
// golangci-lint in the tracker module with the root config".
// Mutation: drop `--config ../.golangci.yml` from the lint loop, and no
// golangci-lint call holds it; drop the loop itself, and no
// `golangci-lint in tracker` call appears.
func TestIntegrationTrackerLintUsesTheRootConfig(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "docs-lint"})

	if r.Code != 0 {
		t.Fatalf("the gate exited %d on green stubs, want 0\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stderr, r.callLog())
	}
	if !r.hasCall("golangci-lint in tracker") {
		t.Fatalf("lint never ran golangci-lint in the tracker module\ncalls:\n%s", r.callLog())
	}
	if !anyCallHolds(r, "--config ../.golangci.yml") {
		t.Fatalf("the tracker lint did not use the root config\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationTrackerLintFindingFailsTheGate checks "a lint finding in the
// tracker module fails the gate and names the module".
// Mutation: replace the lint loop's `|| { echo "lint failed in $$m"; exit 1; }`
// guard with `|| true`, and the failure is ignored: the gate exits 0 and
// prints `gate: PASSED`; drop the module name from that guard's echo, and no
// line names tracker.
func TestIntegrationTrackerLintFindingFailsTheGate(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "docs-lint"}, "STUB_LINT_FAIL_DIR=tracker")

	if r.Code == 0 {
		t.Fatalf("the gate exited 0 with a lint finding in the tracker module\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stdout+r.Stderr, "lint failed in tracker") {
		t.Fatalf("the gate did not name the module whose lint failed\nstdout:\n%s\nstderr:\n%s", r.Stdout, r.Stderr)
	}
	if strings.Contains(r.Stderr, "gate: PASSED") {
		t.Fatalf("the gate printed PASSED despite the tracker lint finding\nstderr:\n%s", r.Stderr)
	}
}

// callCount is how many times the run logged call.
func callCount(r GateRun, call string) int {
	n := 0
	for _, c := range r.Calls {
		if c == call {
			n++
		}
	}
	return n
}

// TestIntegrationGateStubsNameTheModuleByPathNotBasename checks that a checkout
// which is itself called tracker is not mistaken for the tracker module: the
// stubs name a module by its path under the checkout, so the module's own run
// is the only one logged `in tracker`, as in any other checkout.
// Mutation: key the stubs' module marker on the working directory's basename
// (`${PWD##*/}` = tracker), and every root run in this checkout logs `in
// tracker`, so the counts differ.
func TestIntegrationGateStubsNameTheModuleByPathNotBasename(t *testing.T) {
	ordinary := newGateSandbox(t).Run([]string{"-o", "lint"})

	g := newGateSandbox(t)
	g.repo = g.Path("elsewhere", "tracker")
	for name, body := range gateScratchFiles(t) {
		g.WriteFile(filepath.Join("elsewhere", "tracker", name), body)
	}
	named := g.Run([]string{"-o", "lint"})

	if ordinary.Code != 0 || named.Code != 0 {
		t.Fatalf("the gate exited %d (ordinary) and %d (checkout named tracker), want 0 and 0\n%s\n---\n%s", ordinary.Code, named.Code, ordinary.Stderr, named.Stderr)
	}
	for _, call := range []string{"go build in tracker", "go vet in tracker", "go test in tracker"} {
		if got, want := callCount(named, call), callCount(ordinary, call); got != want {
			t.Errorf("a checkout named tracker logged %q %d times, an ordinary one %d: the stubs read the checkout's name as a module", call, got, want)
		}
	}
	if callCount(ordinary, "go test in tracker") == 0 {
		t.Fatalf("the ordinary gate never ran the tracker module's tests\ncalls:\n%s", ordinary.callLog())
	}
}
