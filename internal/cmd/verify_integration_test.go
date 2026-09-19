//go:build integration

package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// TestRunDefaultTestVerification guards the core gt-h9kf behavior: gt done's
// default (non-opt-in) test gate must actually run the branch's changed
// package tests and refuse when they fail, succeed when they pass, and skip
// cleanly when there is nothing to verify.
func TestRunDefaultTestVerification(t *testing.T) {
	// runDefaultTestVerification's slot.Acquire call checks `docker ps`
	// regardless of townRoot (gt-jqif): even though every subtest below
	// passes its own fresh t.TempDir() as townRoot — so the flock itself
	// never contends with anything else — the docker-container check is
	// global, and this package is guarded as container-suite (gt tap guard
	// container-suite) precisely because it shares the host with
	// Dolt/testcontainers-backed suites. Without this stub, any real
	// dolt/testcontainers/ryuk container running elsewhere on a shared Gas
	// Town host makes every subtest's slot.Acquire release-and-retry for up
	// to defaultTestVerifySlotTimeout (60m), blowing this package's own test
	// timeout. See stubNoContainers in mq_batch_slot_test.go.
	stubNoContainers(t)
	townRoot := t.TempDir()

	t.Run("no test_command configured: skips, does not run anything", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", &config.MergeQueueConfig{}, townRoot, "test/skip-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.ran {
			t.Error("ran = true, want false when no test_command is configured")
		}
		if result.skipReason == "" {
			t.Error("skipReason is empty, want an explanation")
		}
	})

	t.Run("no changed go files: skips cleanly, does not block", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "docs only")
		_ = base

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/docs-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.ran {
			t.Error("ran = true, want false for a docs-only change")
		}
	})

	t.Run("passing changed-package test: succeeds, scoped to the changed package", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "pkga", "a.go"), []byte("package pkga\n\nfunc Add(a, b int) int { return a + b }\nfunc Triple(a int) int { return a * 3 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "add Triple")

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/pass-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if result.scope != "packages" {
			t.Errorf("scope = %q, want %q", result.scope, "packages")
		}
		if len(result.packages) != 1 || !strings.HasSuffix(result.packages[0], "/pkga") {
			t.Errorf("packages = %v, want exactly one entry ending in /pkga", result.packages)
		}
		if result.logSHA256 == "" {
			t.Error("logSHA256 is empty on success")
		}
	})

	t.Run("failing changed-package test: refuses with the failure in the error", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		// Introduce a broken new test in pkga — this is exactly the bug
		// gt-h9kf describes: a polecat's own new test that was never run.
		if err := os.WriteFile(filepath.Join(dir, "pkga", "broken_test.go"), []byte("package pkga\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) {\n\tt.Fatal(\"this new test was never actually run\")\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "add broken test")

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/fail-role")
		if err == nil {
			t.Fatalf("runDefaultTestVerification: expected an error for a failing test, got result=%+v", result)
		}
		if !strings.Contains(err.Error(), "TestBroken") && !strings.Contains(err.Error(), "this new test was never actually run") {
			t.Errorf("error does not surface the failing test output: %v", err)
		}
	})

	t.Run("non-Go rig (no go.mod): runs the full test_command instead of scoping", func(t *testing.T) {
		dir := t.TempDir()
		runGitIn(t, dir, "init", "-q", "-b", "main")
		runGitIn(t, dir, "config", "user.email", "test@example.com")
		runGitIn(t, dir, "config", "user.name", "Test")
		runGitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
		base := strings.TrimSpace(runGitOut(t, dir, "rev-parse", "HEAD"))
		runGitIn(t, dir, "update-ref", "refs/remotes/origin/main", base)

		marker := filepath.Join(dir, "ran")
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "non-go change")

		g := git.NewGit(dir)
		// marker is under t.TempDir(), whose path embeds this subtest's name
		// (including the parens in "(no go.mod)") — quote it so the shell
		// doesn't choke on its own metacharacters.
		mq := &config.MergeQueueConfig{TestCommand: "echo ran >> '" + marker + "'"}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/non-go-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if result.scope != "full" {
			t.Errorf("scope = %q, want %q", result.scope, "full")
		}
		if got, readErr := os.ReadFile(marker); readErr != nil || strings.TrimSpace(string(got)) != "ran" {
			t.Errorf("full test_command did not run: got=%q err=%v", got, readErr)
		}
	})
}

// TestRunDefaultTestVerificationBudgets drives the gate with a fake slot and
// a fake runner (gt-pnkd item 4) to pin the resolved budgets, the env parity
// with the rig's test_command, and the fact that slot waiting is reported and
// counted separately from the run itself.
func TestRunDefaultTestVerificationBudgets(t *testing.T) {
	townRoot := t.TempDir()

	type runCall struct {
		script   string
		env      []string
		deadline time.Time
	}

	newRepoWithTwoChangedPackages := func(t *testing.T) string {
		t.Helper()
		dir, _ := initVerifyTestGoRepo(t)
		for _, rel := range []string{"pkga/a.go", "pkgb/b.go"} {
			path := filepath.Join(dir, rel)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, []byte("\n// touched\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch both packages")
		return dir
	}

	t.Run("run budget scales and the Makefile -timeout reaches the command", func(t *testing.T) {
		if _, err := exec.LookPath("make"); err != nil {
			t.Skip("make not available")
		}
		dir := newRepoWithTwoChangedPackages(t)
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tgo test -timeout 20m ./...\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", "Makefile")
		runGitIn(t, dir, "commit", "-q", "-m", "add Makefile")

		var gotTimeout time.Duration
		var calls []runCall
		stubVerifyGate(t,
			func(_ string, _ string, timeout time.Duration) (func(), error) {
				gotTimeout = timeout
				return func() {}, nil
			},
			func(ctx context.Context, _ string, script string, env []string, _ *os.File) error {
				deadline, _ := ctx.Deadline()
				calls = append(calls, runCall{script: script, env: env, deadline: deadline})
				return nil
			})

		includeContainers := true // slot-path assertions below need the gate to take a slot (gt-yihz)
		mq := &config.MergeQueueConfig{TestCommand: "GOFLAGS=-p=6 make test", TestVerifyIncludeContainerPackages: &includeContainers}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/budget-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if len(calls) != 1 {
			t.Fatalf("runner called %d times, want 1", len(calls))
		}

		if gotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slot timeout = %s, want the %s default", gotTimeout, defaultTestVerifySlotTimeout)
		}
		if !strings.Contains(calls[0].script, "-timeout 20m") {
			t.Errorf("script = %q, want the Makefile's -timeout 20m stated explicitly", calls[0].script)
		}
		if !strings.Contains(calls[0].script, "pkga") || !strings.Contains(calls[0].script, "pkgb") {
			t.Errorf("script = %q, want both changed packages", calls[0].script)
		}
		// Env parity (gt-fa3s): the rig's configured test_command environment
		// must reach the gate. slot.Acquire's reentrant marker may also be
		// present, so look for the assignment rather than exact equality.
		if !containsEnv(calls[0].env, "GOFLAGS=-p=6") {
			t.Errorf("env is missing GOFLAGS=-p=6 from test_command")
		}
		// 20m per package × 2 changed packages = a 40m run budget.
		gotBudget := time.Until(calls[0].deadline)
		if gotBudget < 39*time.Minute || gotBudget > 41*time.Minute {
			t.Errorf("run budget ≈ %s, want ≈40m (20m per package × 2)", gotBudget.Round(time.Minute))
		}
		if result.runBudget != 40*time.Minute {
			t.Errorf("result.runBudget = %s, want 40m", result.runBudget)
		}
		if result.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("result.slotTimeout = %s, want %s", result.slotTimeout, defaultTestVerifySlotTimeout)
		}

		// The resolve-before-you-run header is the artifact gt-pnkd's victims
		// could not read: it must name both budgets and their provenance.
		logBytes, readErr := os.ReadFile(result.logPath)
		if readErr != nil {
			t.Fatalf("reading verify log: %v", readErr)
		}
		logText := string(logBytes)
		for _, want := range []string{"run budget: 40m", "slot cap: 1h", "Makefile", "env (inherited from test_command): GOFLAGS=-p=6"} {
			if !strings.Contains(logText, want) {
				t.Errorf("verify log is missing %q:\n%s", want, logText)
			}
		}
	})

	t.Run("test_verify_command overrides the derived go test and fills {packages}", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)
		var script string
		stubVerifyGate(t,
			func(_ string, _ string, _ time.Duration) (func(), error) { return func() {}, nil },
			func(_ context.Context, _ string, s string, _ []string, _ *os.File) error {
				script = s
				return nil
			})

		mq := &config.MergeQueueConfig{
			TestCommand:       "GOFLAGS=-p=6 make test",
			TestVerifyCommand: "make test-changed PKGS='{packages}'",
		}
		g := git.NewGit(dir)
		if _, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/override-role"); err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if strings.Contains(script, "{packages}") {
			t.Errorf("script = %q, want the placeholder substituted", script)
		}
		if !strings.Contains(script, "pkga") || !strings.Contains(script, "pkgb") {
			t.Errorf("script = %q, want the resolved packages", script)
		}
	})

	t.Run("slot contention is reported as contention, not as a test failure", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)
		runnerCalled := false
		stubVerifyGate(t,
			func(_ string, _ string, _ time.Duration) (func(), error) {
				return nil, errors.New("timed out after 5m0s waiting for container-gate slot")
			},
			func(_ context.Context, _ string, _ string, _ []string, _ *os.File) error {
				runnerCalled = true
				return nil
			})

		includeContainers := true // contention only exists when the gate takes a slot (gt-yihz)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", TestVerifySlotTimeout: "5m", TestVerifyIncludeContainerPackages: &includeContainers}
		g := git.NewGit(dir)
		_, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/contention-role")
		if err == nil {
			t.Fatal("expected an error when the slot cannot be acquired")
		}
		if runnerCalled {
			t.Error("the suite ran even though the slot was never acquired")
		}
		msg := err.Error()
		for _, want := range []string{"slot contention", "NOT a test failure", "merge_queue.test_verify_slot_timeout", "5m"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
	})

	t.Run("a timing-out suite reports the budget and that slot wait is excluded", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)
		stubVerifyGate(t,
			func(_ string, _ string, _ time.Duration) (func(), error) { return func() {}, nil },
			func(ctx context.Context, _ string, _ string, _ []string, _ *os.File) error {
				<-ctx.Done()
				return ctx.Err()
			})

		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", TestVerifyRunTimeout: "1ms"}
		g := git.NewGit(dir)
		_, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/run-timeout-role")
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		msg := err.Error()
		for _, want := range []string{"timed out", "merge_queue.test_verify_run_timeout", "slot wait is not counted"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error is missing %q: %v", want, err)
			}
		}
	})

	t.Run("waiting and running both emit progress lines into the verify log", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)
		stubVerifyProgress(t, 5*time.Millisecond)
		stubVerifyGate(t,
			func(_ string, _ string, _ time.Duration) (func(), error) {
				// Long enough for at least one progress tick to fire.
				time.Sleep(30 * time.Millisecond)
				return func() {}, nil
			},
			func(_ context.Context, _ string, _ string, _ []string, _ *os.File) error {
				time.Sleep(30 * time.Millisecond)
				return nil
			})

		// The slot-wait progress path only exists when the gate takes a slot,
		// which since gt-yihz means the rig opted its container-backed
		// packages back into gt done's gate (pkga/pkgb spin nothing, so the
		// default would run them slot-free).
		includeContainers := true
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", TestVerifyIncludeContainerPackages: &includeContainers}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/progress-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		logBytes, readErr := os.ReadFile(result.logPath)
		if readErr != nil {
			t.Fatalf("reading verify log: %v", readErr)
		}
		logText := string(logBytes)
		if !strings.Contains(logText, "still waiting for the container-gate slot") {
			t.Errorf("verify log has no slot-wait progress line:\n%s", logText)
		}
		if !strings.Contains(logText, "test-verify still running") {
			t.Errorf("verify log has no run progress line:\n%s", logText)
		}
		if !strings.Contains(logText, "container-gate slot acquired after") {
			t.Errorf("verify log does not report the slot wait:\n%s", logText)
		}
	})
}

func TestChangedGoPackages(t *testing.T) {
	t.Run("no go files changed reports changedGoFiles=false", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "docs only")

		g := git.NewGit(dir)
		pkgs, changed, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if changed {
			t.Error("changedGoFiles = true, want false for a docs-only change")
		}
		if len(pkgs) != 0 {
			t.Errorf("pkgs = %v, want empty", pkgs)
		}
	})

	t.Run("resolves only the changed package, not the whole repo", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "pkga", "a.go"), []byte("package pkga\n\nfunc Add(a, b int) int { return a + b + 0 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		g := git.NewGit(dir)
		pkgs, changed, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if !changed {
			t.Fatal("changedGoFiles = false, want true")
		}
		if len(pkgs) != 1 || !strings.HasSuffix(pkgs[0], "/pkga") {
			t.Errorf("pkgs = %v, want exactly one entry ending in /pkga", pkgs)
		}
	})

	t.Run("deleted-only directory is dropped, not an error", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		if err := os.Remove(filepath.Join(dir, "pkgb", "b.go")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "pkgb", "b_test.go")); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "delete pkgb")

		g := git.NewGit(dir)
		pkgs, changed, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if !changed {
			t.Fatal("changedGoFiles = false, want true (deletion still touches .go paths)")
		}
		if len(pkgs) != 0 {
			t.Errorf("pkgs = %v, want empty — pkgb no longer exists as a package", pkgs)
		}
	})
}

// TestRunDefaultTestVerification_DefersContainerPackages covers gt-yihz: the
// Docker suite runs once per submission, in the refinery's gate, so gt done's
// own gate leaves container-backed packages alone and needs no slot for the
// rest.
func TestRunDefaultTestVerification_DefersContainerPackages(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	t.Run("container-backed package deferred, plain package runs without a slot", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga and internal/cmd")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/defer-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if len(result.packages) != 1 || !strings.HasSuffix(result.packages[0], "/pkga") {
			t.Errorf("packages = %v, want only pkga", result.packages)
		}
		if len(result.deferredPackages) != 1 || !strings.HasSuffix(result.deferredPackages[0], "/internal/cmd") {
			t.Errorf("deferredPackages = %v, want only internal/cmd", result.deferredPackages)
		}
		if result.slotUsed {
			t.Error("slotUsed = true, want false when nothing in scope spins containers")
		}
		if acquired {
			t.Error("the gate acquired a container-gate slot for a container-free package set")
		}
		log, _ := os.ReadFile(result.logPath)
		if !strings.Contains(string(log), "deferred to the refinery gate") || !strings.Contains(string(log), "container-gate slot: not needed") {
			t.Errorf("verify log does not explain the deferral / no-slot decision:\n%s", log)
		}
	})

	t.Run("only container-backed packages changed: skips with reason, takes no slot", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "only internal/cmd")

		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			t.Error("slot acquired although every changed package was deferred")
			return func() {}, nil
		}, nil)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/all-deferred-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.ran {
			t.Errorf("ran = true, want false: %+v", result)
		}
		if !strings.Contains(result.skipReason, "refinery") || !strings.Contains(result.skipReason, "internal/cmd") {
			t.Errorf("skipReason = %q, want the refinery-owns-it explanation naming the package", result.skipReason)
		}
		if len(result.deferredPackages) != 1 {
			t.Errorf("deferredPackages = %v, want the one container package", result.deferredPackages)
		}
	})

	t.Run("test_verify_include_container_packages restores the slot-gated full run", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch both")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, nil)

		include := true
		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", TestVerifyIncludeContainerPackages: &include}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/include-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success || len(result.packages) != 2 {
			t.Fatalf("result = %+v, want both packages run", result)
		}
		if len(result.deferredPackages) != 0 || !result.slotUsed || !acquired {
			t.Errorf("include flag: deferred=%v slotUsed=%v acquired=%v, want none/true/true", result.deferredPackages, result.slotUsed, acquired)
		}
	})
}
