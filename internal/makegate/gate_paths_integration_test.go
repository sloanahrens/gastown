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
// printed, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an
// inherited 1".
// Mutation: drop GT_TEST_DOCKER=0 from the unit tier's two go test lines (the
// main suite and the tracker loop), and the exact `go test GT_TEST_DOCKER=0`
// call disappears while the inherited 1 shows through; add the shell tests to
// the gate, and the `shell-tests` call appears.
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
}

// lintRuns is how many golangci-lint run calls the run made.
func lintRuns(r GateRun) int {
	n := 0
	for _, call := range r.Calls {
		if strings.HasPrefix(call, "golangci-lint run") {
			n++
		}
	}
	return n
}

// TestIntegrationGatePathsLintWaitsOnTheLock checks "lint waited on the lock" by
// the wait: with another golangci-lint holding the module lock for the first two
// runs (the stub exits 3 with the lock line, as the real one does), the gate
// still exits 0, and it made two more lint runs than an uncontended gate, which
// is the retry the wait script does (LINT_LOCK_POLL=0 keeps the pause out).
// Mutation: run golangci-lint without scripts/lint-lock-wait.sh in lint and
// gate-lint, and the first contended run is the gate's last: it exits non-zero.
func TestIntegrationGatePathsLintWaitsOnTheLock(t *testing.T) {
	quiet := newGateSandbox(t).Run([]string{"-o", "docs-lint"})
	if quiet.Code != 0 || lintRuns(quiet) == 0 {
		t.Fatalf("the uncontended gate exited %d with %d lint runs, want 0 and at least 1\nstderr:\n%s\ncalls:\n%s", quiet.Code, lintRuns(quiet), quiet.Stderr, quiet.callLog())
	}
	r := newGateSandbox(t).Run([]string{"-o", "docs-lint"}, "STUB_LINT_CONTEND=2", "LINT_LOCK_POLL=0")

	if r.Code != 0 {
		t.Fatalf("the gate exited %d with another lint holding the lock, want it to wait and pass\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stderr, r.callLog())
	}
	if got, want := lintRuns(r), lintRuns(quiet)+2; got != want {
		t.Fatalf("the contended gate made %d lint runs, want %d (the uncontended %d plus the two it waited out)\ncalls:\n%s", got, want, lintRuns(quiet), r.callLog())
	}
}

// TestIntegrationGatePathsTaxedHostWarnsAndPasses checks "a taxed host: the
// gate warns on the exec tax and still passes (gt-2ycne.1)".
// Mutation: drop the exec-tax-preflight prerequisite from gate-lint, and the
// preflight stub is never called. The warning itself is the preflight's text,
// not the gate's, so the case asserts the gate ran the preflight and carried on.
func TestIntegrationGatePathsTaxedHostWarnsAndPasses(t *testing.T) {
	r := newGateSandbox(t).Run([]string{"-o", "lint"}, "EXEC_TAX_WARN=1")

	if r.Code != 0 {
		t.Fatalf("the gate exited %d on a taxed host, want 0\nstderr:\n%s", r.Code, r.Stderr)
	}
	if !r.hasCall("preflight") {
		t.Fatalf("the gate never ran the exec-tax preflight\ncalls:\n%s", r.callLog())
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
