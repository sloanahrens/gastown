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

// The lint-lock-wait and empty-docker-list cases: one test per case in lines
// 944 to 975 of scripts/makefile-gate_test.sh. The three lint-lock-wait cases
// run scripts/lint-lock-wait.sh itself as the thing under test, against a lint
// stub that reports the module lock held for its first STUB_CONTEND calls, so
// the retry loop, its wait bound and its refusal to retry a real finding are
// exercised without a lint run. The last case runs the real test-integration
// recipe with DOCKER_PKGS= set empty, so the recipe's own guard is what decides
// the exit status.
//
// Each test is named after its shell case and its comment names the mutation
// that turns it red. The script and make calls are processes, so this file is
// integration tier (docs/standards/test-engineering.md, rule 2).

// lintLockStub is the contended lint the shell fixture writes as
// $TMP/contended-lint: it logs every call, exits 3 with golangci-lint's lock
// line while STUB_CONTEND calls have not yet been made, and reports a finding
// when STUB_FINDING is set.
const lintLockStub = `#!/usr/bin/env bash
n=$(( $(wc -l <"$STUB_LOG") ))
echo "lint" >>"$STUB_LOG"
if (( n < STUB_CONTEND )); then
	echo "Error: parallel golangci-lint is running"
	exit 3
fi
[[ -n "${STUB_FINDING:-}" ]] && { echo "x.go:1: finding"; exit 1; }
exit 0
`

// lockWaitRun is one run of the script under test: its exit code, its stdout
// and stderr, and the stub's call log.
type lockWaitRun struct {
	Code   int
	Stdout string
	Stderr string
	Calls  []string
}

// callCount is the number of lint attempts the stub logged.
func (r lockWaitRun) callCount() int { return len(r.Calls) }

// runLintLockWait writes the contended-lint stub into a fresh sandbox and runs
// the checkout's scripts/lint-lock-wait.sh with waitS and the stub as CMD. vars
// are environment assignments for the run (STUB_CONTEND and friends), appended
// last so a case's value wins over anything inherited.
func runLintLockWait(t *testing.T, waitS string, vars ...string) lockWaitRun {
	t.Helper()
	s := newSandbox(t)
	stub := s.Path("contended-lint")
	if err := os.WriteFile(stub, []byte(lintLockStub), 0o755); err != nil {
		t.Fatalf("write the contended-lint stub %s: %v", stub, err)
	}
	calls := s.Path("calls")
	if err := os.WriteFile(calls, nil, 0o644); err != nil {
		t.Fatalf("truncate the stub call log %s: %v", calls, err)
	}
	script := filepath.Join(repoRoot(t), "scripts", "lint-lock-wait.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("stat the script under test %s: %v", script, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script, waitS, stub)
	cmd.Env = lintLockEnv(calls, vars)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %s: %v\n%s", script, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %s: %v\n%s", script, makeTestTimeout, ctx.Err(), stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read the stub call log %s: %v", calls, err)
	}
	var log []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			log = append(log, line)
		}
	}
	return lockWaitRun{Code: code, Stdout: stdout.String(), Stderr: stderr.String(), Calls: log}
}

// lintLockEnv is the environment the script under test runs in: the sandbox
// environment with every inherited STUB_* variable and LINT_LOCK_POLL dropped,
// plus this run's stub log and a zero poll, with the case's variables appended
// last so they win. The poll is zero so a retry costs no real time; neither a
// caller's poll nor a caller's stub variable decides the run.
func lintLockEnv(calls string, vars []string) []string {
	drop := map[string]bool{"LINT_LOCK_POLL": true}
	for _, kv := range vars {
		key, _, _ := strings.Cut(kv, "=")
		drop[key] = true
	}
	env := []string{"LINT_LOCK_POLL=0", "STUB_LOG=" + calls}
	for _, kv := range sandboxEnv() {
		key, _, _ := strings.Cut(kv, "=")
		if drop[key] || strings.HasPrefix(key, "STUB_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, vars...)
}

// TestIntegrationLintLockWaitRetriesAContendedLintAndPasses checks
// "lint-lock-wait: a lint contended twice is retried and passes".
// Mutation: replace the retry branch with `exit "$rc"`, and the first contended
// attempt ends the script: one call and exit 3.
func TestIntegrationLintLockWaitRetriesAContendedLintAndPasses(t *testing.T) {
	r := runLintLockWait(t, "5", "STUB_CONTEND=2")

	if r.Code != 0 {
		t.Fatalf("lint-lock-wait exited %d, want 0 after the lock was released\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stdout, r.Stderr, strings.Join(r.Calls, "\n"))
	}
	if got := r.callCount(); got != 3 {
		t.Fatalf("the lint ran %d times, want 3: two contended attempts and the pass\ncalls:\n%s", got, strings.Join(r.Calls, "\n"))
	}
	if !strings.Contains(r.Stderr, "retrying in 0s") {
		t.Fatalf("lint-lock-wait did not report the retry in 0s\nstderr:\n%s", r.Stderr)
	}
}

// TestIntegrationLintLockWaitAtTheBoundExitsWithTheLintCode checks
// "lint-lock-wait: still contended at the wait bound exits with the lint's code
// and keeps the lock line".
// Mutation: exit 0 at the bound instead of the lint's code, drop the bound's
// "nothing was linted" message, or keep retrying past the bound (more than one
// call), and this case is red.
func TestIntegrationLintLockWaitAtTheBoundExitsWithTheLintCode(t *testing.T) {
	r := runLintLockWait(t, "0", "STUB_CONTEND=99")

	if r.Code != 3 {
		t.Fatalf("lint-lock-wait exited %d at a wait bound of 0, want the lint's 3\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if got := r.callCount(); got != 1 {
		t.Fatalf("the lint ran %d times at a wait bound of 0, want 1\ncalls:\n%s", got, strings.Join(r.Calls, "\n"))
	}
	if !strings.Contains(r.Stdout, "parallel golangci-lint is running") {
		t.Fatalf("lint-lock-wait dropped the lock line, so internal/lintlock cannot read the run as \"nothing was linted\"\nstdout:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "nothing was linted") {
		t.Fatalf("lint-lock-wait did not say the bound left nothing linted\nstderr:\n%s", r.Stderr)
	}
}

// TestIntegrationLintLockWaitNeverRetriesALintWithFindings checks
// "lint-lock-wait: a lint with findings is never retried".
// Mutation: retry any non-zero attempt instead of only one whose output carries
// the lock line, and the finding is linted twice (two calls) instead of once.
func TestIntegrationLintLockWaitNeverRetriesALintWithFindings(t *testing.T) {
	r := runLintLockWait(t, "5", "STUB_CONTEND=0", "STUB_FINDING=1")

	if r.Code != 1 {
		t.Fatalf("lint-lock-wait exited %d for a lint with findings, want its 1\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	if got := r.callCount(); got != 1 {
		t.Fatalf("the lint ran %d times with a finding, want 1: a real finding is never retried\ncalls:\n%s", got, strings.Join(r.Calls, "\n"))
	}
	if !strings.Contains(r.Stdout, "finding") {
		t.Fatalf("the finding is not on the output, so the case would not see a retry it must refuse\nstdout:\n%s", r.Stdout)
	}
}

// TestIntegrationEmptyDockerListRefusesInsteadOfTestingNothing checks
// "test-integration refuses an empty docker.txt instead of testing nothing".
// The recipe runs in the checkout under test against the gate stubs, so an
// empty DOCKER_PKGS reaches the guard without any package being tested.
// Mutation: delete test-integration's `test -n "$(strip $(DOCKER_PKGS))"` guard
// line, and the recipe runs its stub go test over nothing and exits 0, naming
// no reason to refuse.
func TestIntegrationEmptyDockerListRefusesInsteadOfTestingNothing(t *testing.T) {
	s := newGateSandbox(t)
	r := s.runTarget("test-integration", []string{"DOCKER_PKGS="})

	if r.Code == 0 {
		t.Fatalf("test-integration exited 0 with an empty docker package list\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Stdout, r.Stderr, r.callLog())
	}
	if !strings.Contains(r.Stderr, "lists no package") {
		t.Fatalf("test-integration did not name the empty docker package list\nstderr:\n%s", r.Stderr)
	}
}
