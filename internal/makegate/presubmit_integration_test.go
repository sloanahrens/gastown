//go:build integration

package makegate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The presubmit, interrupt and test-slow cases: one test per case in lines 882
// to 925 of scripts/makefile-gate_test.sh. Each runs the real recipe (the gate,
// presubmit, test-slow or test-integration) in a scratch checkout against stub
// go, golangci-lint, exec-tax preflight and shell-tests, so a stage's exit
// status is seen without building, linting or testing the checkout under test.
//
// The checkout is a Sandbox repository holding the checkout under test's
// Makefile, a tracker module and the docker package list, in a two-commit
// history whose second commit changes the tracker module. The presubmit cases
// need both a base the module differs from (the first commit) and a base it
// does not (HEAD), so they build that history themselves and never read the
// depth or the first commit of the clone the tests run in.
//
// Each test is named after its shell case and its comment names the mutation
// that turns it red. The make calls are processes, so this file is integration
// tier (docs/standards/test-engineering.md, rule 2).

// gateStubGoInterrupt is the gate family's go stub: gateStubGo's call log,
// module marker and failure switch, plus the interrupt path the shell fixture's
// stub carries. A `go test` under STUB_GO_INTERRUPT logs `go test interrupt`,
// signals its parent recipe shell, and then blocks reading a fifo it opens
// read-write. A gate whose trap stops the suite kills the stub before the read
// times out, so no `survived` call is logged; the read is bash's own timer and
// starts no child, which would outlive the stub and hold the run's pipe open.
const gateStubGoInterrupt = `#!/bin/bash
# The gate's build, vet and test steps start with the CPU-class wrapper,
# "go run ./internal/slot/cmd/cpuslot" (gt-bq68w). The scratch checkout has no
# cpuslot package, and what these cases judge is which command the recipe runs,
# so the stub runs the wrapped command itself: it applies the wrapper's -dir
# and peels the env assignments the wrapper would have applied.
if [ "$1" = run ] && [ "$2" = ./internal/slot/cmd/cpuslot ]; then
	shift 2
	while [ $# -gt 0 ]; do
		case "$1" in
			-dir|--dir) cd "$2" || exit 1; shift 2 ;;
			-timeout|--timeout) shift 2 ;;
			-*) shift ;;
			*) break ;;
		esac
	done
	while [ $# -gt 0 ]; do
		case "$1" in
			*=*) export "$1"; shift ;;
			*) break ;;
		esac
	done
	[ "$1" = go ] && shift
fi
echo "go $1 GT_TEST_DOCKER=${GT_TEST_DOCKER:-unset}" >>"$STUB_LOG"
[ "${PWD##*/}" = tracker ] && echo "go $1 in tracker" >>"$STUB_LOG"
if [ "$1" = test ] && [ -n "${STUB_GO_INTERRUPT:-}" ]; then
	echo "go test interrupt" >>"$STUB_LOG"
	fifo="$STUB_LOG.fifo"
	[ -p "$fifo" ] || mkfifo "$fifo"
	if exec 3<>"$fifo"; then
		kill -TERM "$PPID"
		read -r -t 20 -u 3 _
	fi
	echo "go test survived the interrupt" >>"$STUB_LOG"
fi
dir_ok=1
if [ -n "${STUB_GO_FAIL_DIR:-}" ]; then
	if [ "${STUB_GO_FAIL_DIR#!}" != "$STUB_GO_FAIL_DIR" ]; then
		[ "${PWD##*/}" != "${STUB_GO_FAIL_DIR#!}" ] || dir_ok=0
	else
		[ "${PWD##*/}" = "$STUB_GO_FAIL_DIR" ] || dir_ok=0
	fi
fi
[ "$1" = "${STUB_GO_FAIL:-}" ] && [ "$dir_ok" = 1 ] && exit 1
exit 0
`

// gateStubShellTestsFail is the shell-tests stub: it logs its call and fails
// when STUB_SHELL_FAIL is set, as the shell fixture's stub does.
const gateStubShellTestsFail = `#!/bin/sh
echo "shell-tests" >>"$STUB_LOG"
[ -n "${STUB_SHELL_FAIL:-}" ] && exit 1
exit 0
`

// gateScratch is the checkout these cases run make in. first is the commit the
// tracker module differs from; HEAD is the commit it does not.
type gateScratch struct {
	*GateSandbox
	first string
}

// newGateScratch makes the scratch checkout. The two commits share the
// checkout under test's Makefile, so every case runs the real recipes; the
// tracker module and the docker package list are what those recipes read from
// the tree, and the second commit changes the module so presubmit has a base
// to compare against.
func newGateScratch(t *testing.T) *gateScratch {
	t.Helper()
	g := newGateSandbox(t)
	g.writeStub("go", gateStubGoInterrupt)
	g.writeStub("shell-tests", gateStubShellTestsFail)
	dir := g.Checkout("scratch")
	g.repo = dir
	first := g.Commit(dir, "first", map[string]string{
		"Makefile":                       makefileText(t, repoRoot(t)),
		"tracker/go.mod":                 "module example.invalid/tracker\n\ngo 1.21\n",
		"internal/testpolicy/docker.txt": "internal/cmd # scratch fixture\n",
	})
	g.Commit(dir, "second", map[string]string{"tracker/changed.go": "package tracker\n"})
	return &gateScratch{GateSandbox: g, first: first}
}

// runTarget runs the named make goal in the scratch checkout against the
// stubs, as the shell fixture's run_gate runs the goal its TARGET names.
// GateSandbox.Run fixes the goal to gate; this family also runs presubmit,
// test-slow and test-integration. The call log is truncated first, so a case
// reads only this run's calls.
func (g *GateSandbox) runTarget(target string, makeFlags []string, vars ...string) GateRun {
	g.t.Helper()
	calls := g.Path("calls")
	if err := os.WriteFile(calls, nil, 0o644); err != nil {
		g.t.Fatalf("truncate the stub call log %s: %v", calls, err)
	}
	argv := append([]string{"-C", g.repo, "--no-print-directory"}, makeFlags...)
	argv = append(argv, target,
		"SHELL_TESTS="+filepath.Join(g.bin, "shell-tests"),
		"EXEC_TAX_PREFLIGHT="+filepath.Join(g.bin, "preflight"))
	ctx, cancel := context.WithTimeout(context.Background(), gateRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", argv...)
	cmd.Env = g.gateEnv(vars)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			g.t.Fatalf("run make %s: %v\n%s", target, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	if ctx.Err() != nil {
		g.t.Fatalf("make %s did not finish within %s: %v\n%s", target, gateRunTimeout, ctx.Err(), stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		g.t.Fatalf("read the stub call log %s: %v", calls, err)
	}
	var log []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			log = append(log, line)
		}
	}
	return GateRun{Code: code, Stdout: stdout.String(), Stderr: stderr.String(), Calls: log}
}

// TestIntegrationPresubmitChangedTrackerModuleTestsFail checks "presubmit: a
// changed tracker module whose tests fail fails presubmit".
// Mutation: drop presubmit's TESTED_MODULES loop, and the changed module's
// suite never runs (no `go test in tracker` call); replace the loop's
// `|| { echo "presubmit: FAILED at tests ($$m)"; exit 1; }` guard with `||
// true`, and the gate exits 0 on the failing suite.
func TestIntegrationPresubmitChangedTrackerModuleTestsFail(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("presubmit", []string{"-o", "lint"},
		"PRESUBMIT_BASE="+s.first, "STUB_GO_FAIL=test", "STUB_GO_FAIL_DIR=tracker")

	if r.Code == 0 {
		t.Fatalf("presubmit exited 0 with failing tracker tests\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "presubmit: FAILED at tests (tracker)") {
		t.Fatalf("presubmit did not name the failing module\nstderr:\n%s", r.Stderr)
	}
	if !r.hasCall("go test in tracker") {
		t.Fatalf("the failing tracker suite never ran\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationPresubmitUnchangedTrackerModuleNotTested checks "presubmit: an
// unchanged tracker module is not tested".
// Mutation: drop the `git diff --quiet $(PRESUBMIT_BASE)...HEAD -- "$$m"` test
// from presubmit's loop (test every module), and the unchanged module's suite
// runs: STUB_GO_FAIL=test makes it fail, so presubmit exits non-zero and logs
// `go test in tracker`.
func TestIntegrationPresubmitUnchangedTrackerModuleNotTested(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("presubmit", []string{"-o", "lint"},
		"PRESUBMIT_BASE=HEAD", "STUB_GO_FAIL=test", "STUB_GO_FAIL_DIR=tracker")

	if r.Code != 0 {
		t.Fatalf("presubmit exited %d with an unchanged tracker module, want 0\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stdout, r.Stderr, r.callLog())
	}
	if r.hasCall("go test in tracker") {
		t.Fatalf("presubmit tested a tracker module the branch did not change\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationTestIntegrationFailingTrackerTestFailsTheTarget checks
// "test-integration: a failing tracker integration test fails the target and
// names the module".
// Mutation: replace test-integration's tracker loop guard
// `|| { echo "test-integration: FAILED in $$m"; exit 1; }` with `|| true`, and
// the failing module's suite is ignored: the target exits 0 and names no
// module.
func TestIntegrationTestIntegrationFailingTrackerTestFailsTheTarget(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("test-integration", []string{"-o", "lint"},
		"STUB_GO_FAIL=test", "STUB_GO_FAIL_DIR=tracker")

	if r.Code == 0 {
		t.Fatalf("test-integration exited 0 with a failing tracker suite\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "test-integration: FAILED in tracker") {
		t.Fatalf("test-integration did not name the failing module\nstderr:\n%s", r.Stderr)
	}
	if !r.hasCall("go test in tracker") {
		t.Fatalf("the tracker integration suite never ran\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationInterruptedGateStopsTheRunningSuite checks "interrupted gate:
// non-zero, and the trap stopped the running suite".
// Mutation: drop the trap from gate-test's unit tier, and the interrupted suite
// survives: the stub's `go test survived the interrupt` call lands in the log.
func TestIntegrationInterruptedGateStopsTheRunningSuite(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("gate", []string{"-o", "lint"}, "STUB_GO_INTERRUPT=1")

	if r.Code == 0 {
		t.Fatalf("the interrupted gate exited 0\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if strings.Contains(r.Stderr, "gate: PASSED") {
		t.Fatalf("the interrupted gate printed PASSED\nstderr:\n%s", r.Stderr)
	}
	if !r.hasCall("go test interrupt") {
		t.Fatalf("the suite never reached the interrupt path, so this case asserts nothing\ncalls:\n%s", r.callLog())
	}
	if anyCallHolds(r, "survived") {
		t.Fatalf("the running suite survived the interrupt\ncalls:\n%s", r.callLog())
	}
}

// TestIntegrationTestSlowGreenStubs checks "test-slow green stubs: exit 0, the
// Go suite and the shell tests ran with GT_TEST_DOCKER=0".
// Mutation: drop GT_TEST_DOCKER=0 from the gate's unit tier, and that suite
// runs with the inherited 1 (`go test GT_TEST_DOCKER=1` in the log, even though
// the tracker module's suite still carries 0); drop the shell-tests line from
// test-slow, and no `shell-tests` call appears; drop the target's PASSED echo,
// and no `test-slow: PASSED` line is printed.
func TestIntegrationTestSlowGreenStubs(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("test-slow", []string{"-o", "lint"})

	if r.Code != 0 {
		t.Fatalf("test-slow exited %d on green stubs, want 0\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stdout, r.Stderr, r.callLog())
	}
	if !r.hasCall("go test GT_TEST_DOCKER=0") {
		t.Fatalf("the unit tier did not run with the container opt-in off\ncalls:\n%s", r.callLog())
	}
	if anyCallHolds(r, "go test GT_TEST_DOCKER=1") {
		t.Fatalf("a Go suite ran with the inherited container opt-in on\ncalls:\n%s", r.callLog())
	}
	if !r.hasCall("shell-tests") {
		t.Fatalf("test-slow did not run the shell tests\ncalls:\n%s", r.callLog())
	}
	if !gateWallLine.MatchString(r.Stderr) {
		t.Fatalf("the gate printed no `gate: PASSED in <n>s wall` line\nstderr:\n%s", r.Stderr)
	}
	if !strings.Contains(r.Stderr, "test-slow: PASSED") {
		t.Fatalf("test-slow did not print its PASSED line\nstderr:\n%s", r.Stderr)
	}
}

// TestIntegrationTestSlowGoSuiteFails checks "test-slow Go suite fails:
// non-zero, the gate names the Go half".
// Mutation: make gate-test's `$$go_rc -ne 0` branch never run (`if false`), and
// the failing suite falls through to `gate: PASSED`: test-slow runs its shell
// tests and prints `test-slow: PASSED`.
func TestIntegrationTestSlowGoSuiteFails(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("test-slow", []string{"-o", "lint"}, "STUB_GO_FAIL=test")

	if r.Code == 0 {
		t.Fatalf("test-slow exited 0 with a failing Go suite\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "gate: FAILED at unit tier (Go suite") {
		t.Fatalf("the gate did not name the Go half\nstderr:\n%s", r.Stderr)
	}
	if strings.Contains(r.Stderr, "test-slow: PASSED") {
		t.Fatalf("test-slow printed PASSED despite the failing Go suite\nstderr:\n%s", r.Stderr)
	}
}

// TestIntegrationTestSlowShellTestsFail checks "test-slow shell tests fail:
// non-zero, names the shell tests".
// Mutation: replace test-slow's
// `GT_TEST_DOCKER=0 $(SHELL_TESTS) || { echo "test-slow: FAILED at shell
// tests"; exit 1; }` guard with `|| true`, and the failing shell tests are
// ignored: the target exits 0 and prints `test-slow: PASSED`.
func TestIntegrationTestSlowShellTestsFail(t *testing.T) {
	s := newGateScratch(t)
	r := s.runTarget("test-slow", []string{"-o", "lint"}, "STUB_SHELL_FAIL=1")

	if r.Code == 0 {
		t.Fatalf("test-slow exited 0 with failing shell tests\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "test-slow: FAILED at shell tests") {
		t.Fatalf("test-slow did not name the shell tests\nstderr:\n%s", r.Stderr)
	}
}
