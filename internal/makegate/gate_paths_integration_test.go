//go:build integration

package makegate

import (
	"regexp"
	"strings"
	"testing"
)

// The gate failure-path cases: one test per case in the block of
// scripts/makefile-gate_test.sh from its "gate: failure paths, driven with
// stubs" heading through the Go suite case. Each runs the real gate recipe
// against a GateSandbox's stub go, golangci-lint and exec-tax preflight, so
// every red stage is seen to exit non-zero without building or linting the
// tree. Every name carries the TestIntegrationGatePaths prefix the bead's
// `-run` covers, and every comment names the mutation that turns the test red.
//
// The make calls are processes, so this file is integration tier
// (docs/standards/test-engineering.md, rule 2).

// gateWallLine matches the wall the recipe prints when it passes.
var gateWallLine = regexp.MustCompile(`gate: PASSED in [0-9]+s wall`)

// TestIntegrationGatePathsGreenStubs checks "green stubs: exit 0 with the wall
// printed, lint waited on the lock, only the Go suite ran, and it saw
// GT_TEST_DOCKER=0 despite an inherited 1".
// Mutation: drop GT_TEST_DOCKER=0 from the unit tier's two go test lines (the
// main suite and the tracker loop), and the exact `go test GT_TEST_DOCKER=0`
// call disappears while the inherited 1 shows through; add the shell tests to
// the gate, and the `shell-tests` call appears; drop the gate's and gate-lint's
// LINT_RUNNER_FLAGS assignments, and lint no longer runs with
// --allow-serial-runners.
func TestIntegrationGatePathsGreenStubs(t *testing.T) {
	r := newGateSandbox(t).Run(nil)

	if r.Code != 0 {
		t.Fatalf("the gate exited %d on green stubs, want 0\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stderr, r.callLog())
	}
	if !gateWallLine.MatchString(r.Stderr) {
		t.Fatalf("the gate printed no `gate: PASSED in <n>s wall` line\nstderr:\n%s", r.Stderr)
	}
	if !r.hasCall("go test GT_TEST_DOCKER=0") {
		t.Fatalf("the unit tier did not see GT_TEST_DOCKER=0 despite an inherited 1\ncalls:\n%s", r.callLog())
	}
	if r.hasCall("shell-tests") {
		t.Fatalf("the gate ran the shell tests, which belong to test-slow\ncalls:\n%s", r.callLog())
	}
	if !r.hasCallWithPrefix("golangci-lint run --timeout=5m --allow-serial-runners") {
		t.Fatalf("lint did not wait on the lock: no golangci-lint run call carries --allow-serial-runners\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationGatePathsTaxedHostWarnsAndPasses checks "a taxed host: the
// gate warns on the exec tax and still passes (gt-2ycne.1)".
// Mutation: drop the exec-tax-preflight prerequisite from gate-lint, and the
// preflight stub never runs, so no warning line is printed.
func TestIntegrationGatePathsTaxedHostWarnsAndPasses(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"}, "EXEC_TAX_WARN=1")

	if r.Code != 0 {
		t.Fatalf("the gate exited %d on a taxed host, want 0\nstderr:\n%s", r.Code, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "gate: WARNING exec tax 180 ms/exec") {
		t.Fatalf("the gate did not print the exec-tax warning\nstderr:\n%s", r.Stderr)
	}
	if !gateWallLine.MatchString(r.Stderr) {
		t.Fatalf("the gate printed no `gate: PASSED in <n>s wall` line\nstderr:\n%s", r.Stderr)
	}
}

// TestIntegrationGatePathsLintFailsStopsTheGate checks "lint fails: non-zero,
// nothing built or tested".
// Mutation: make lint swallow its own exit status (`|| true` on both
// golangci-lint calls), and the gate continues to build and test.
func TestIntegrationGatePathsLintFailsStopsTheGate(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "docs-lint"}, "STUB_LINT_FAIL=1")

	if r.Code == 0 {
		t.Fatalf("the gate exited 0 with a failing lint\nstderr:\n%s\ncalls:\n%s", r.Stderr, r.callLog())
	}
	if !r.hasCallWithPrefix("golangci-lint run") {
		t.Fatalf("the failing lint was never reached\ncalls:\n%s", r.callLog())
	}
	if r.hasCallWithPrefix("go build") || r.hasCallWithPrefix("go test") {
		t.Fatalf("the gate built or tested after lint failed\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationGatePathsBuildFailsStopsTheUnitTier checks "build fails:
// non-zero, names the build stage, unit tier never starts".
// Mutation: drop the exit guard from both build lines (the main module and
// the nested-module loop), and make runs on to the unit tier and exits 0.
func TestIntegrationGatePathsBuildFailsStopsTheUnitTier(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"}, "STUB_GO_FAIL=build")

	if r.Code == 0 {
		t.Fatalf("the gate exited 0 with a failing build\nstderr:\n%s\ncalls:\n%s", r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "gate: FAILED at build") {
		t.Fatalf("the gate did not name the build stage\nstderr:\n%s", r.Stderr)
	}
	if r.hasCallWithPrefix("go test") {
		t.Fatalf("the unit tier started after the build failed\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationGatePathsGoSuiteFailsNamesTheGoHalf checks "Go suite fails:
// non-zero, names the Go half".
// Mutation: flip gate-test's `$$go_rc -ne 0` test to `-eq 0`, and a failing
// suite falls through to the PASSED line.
func TestIntegrationGatePathsGoSuiteFailsNamesTheGoHalf(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"}, "STUB_GO_FAIL=test")

	if r.Code == 0 {
		t.Fatalf("the gate exited 0 with a failing Go suite\nstderr:\n%s\ncalls:\n%s", r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "gate: FAILED at unit tier (Go suite") {
		t.Fatalf("the gate did not name the Go half\nstderr:\n%s", r.Stderr)
	}
	if strings.Contains(r.Stderr, "gate: PASSED") {
		t.Fatalf("the gate printed PASSED despite the failing Go suite\nstderr:\n%s", r.Stderr)
	}
}
