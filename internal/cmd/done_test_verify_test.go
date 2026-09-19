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

// initVerifyTestGoRepo builds a tiny Go module with two packages (pkga,
// pkgb) at HEAD "base", so tests can add commits on top and diff against
// that base to exercise changedGoPackages / runDefaultTestVerification.
func initVerifyTestGoRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	mustWrite("go.mod", "module example.test\n\ngo 1.21\n")
	mustWrite("pkga/a.go", "package pkga\n\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite("pkga/a_test.go", "package pkga\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n")
	mustWrite("pkgb/b.go", "package pkgb\n\nfunc Double(a int) int { return a * 2 }\n")
	mustWrite("pkgb/b_test.go", "package pkgb\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"bad double\")\n\t}\n}\n")

	runGit("add", ".")
	runGit("commit", "-q", "-m", "base")
	base = runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", base)
	return dir, base
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

// stubVerifyGate replaces the gate's slot-acquire and suite-runner hooks for
// the duration of a test, so the gate can be driven without the real
// container-gate slot (which would contend with every other suite on a shared
// Gas Town host) and without running `go test` over a real package tree.
func stubVerifyGate(
	t *testing.T,
	acquire func(townRoot, role string, timeout time.Duration) (func(), error),
	run func(ctx context.Context, worktree, script string, env []string, logFile *os.File) error,
) {
	t.Helper()
	if acquire != nil {
		prev := acquireVerifySlot
		acquireVerifySlot = acquire
		t.Cleanup(func() { acquireVerifySlot = prev })
	}
	if run != nil {
		prev := runVerifySuite
		runVerifySuite = run
		t.Cleanup(func() { runVerifySuite = prev })
	}
}

// stubLintLockRetryDelay shortens the waits between lint-lock retries so a
// test can drive the retry path (gt-xsty) without sleeping the real tens of
// seconds.
func stubLintLockRetryDelay(t *testing.T, delays ...time.Duration) {
	t.Helper()
	prev := lintLockRetryDelay
	lintLockRetryDelay = delays
	t.Cleanup(func() { lintLockRetryDelay = prev })
}

// stubVerifyProgress shortens the gate's progress interval so a test can
// observe progress lines without waiting out the real one.
func stubVerifyProgress(t *testing.T, interval time.Duration) {
	t.Helper()
	prev := testVerifyProgressInterval
	testVerifyProgressInterval = interval
	t.Cleanup(func() { testVerifyProgressInterval = prev })
}

func TestResolveTestVerifyBudgets(t *testing.T) {
	t.Parallel()

	writeMakefile := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	makefileBody := "test:\n\tgo test -timeout 20m ./...\n"

	t.Run("defaults scale with the changed-package count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir() // no Makefile
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test ./..."}, dir, "go test ./...", 2)
		if b.perPackage != defaultTestVerifyPerPackageTimeout {
			t.Errorf("perPackage = %s, want the %s default", b.perPackage, defaultTestVerifyPerPackageTimeout)
		}
		// 20m × 2 packages = 40m, above the 30m floor.
		if b.runTimeout != 40*time.Minute {
			t.Errorf("runTimeout = %s, want 40m (20m per package × 2)", b.runTimeout)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want %s", b.slotTimeout, defaultTestVerifySlotTimeout)
		}
		if !strings.Contains(b.runSource, "2 changed package(s)") {
			t.Errorf("runSource = %q, want it to explain the scaling", b.runSource)
		}
	})

	t.Run("the 30m floor applies to a single cheap package", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test ./..."}, dir, "go test ./...", 1)
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the %s floor (gt-pnkd: a 10m budget was the bug)", b.runTimeout, defaultTestVerifyRunFloor)
		}
	})

	t.Run("the Makefile test target's -timeout wins over the default", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("make"); err != nil {
			t.Skip("make not available")
		}
		dir := writeMakefile(t, makefileBody)
		mq := &config.MergeQueueConfig{TestCommand: "GOFLAGS=-p=6 make test"}
		b := resolveTestVerifyBudgets(mq, dir, mq.TestCommand, 2)
		if b.perPackage != 20*time.Minute {
			t.Errorf("perPackage = %s, want 20m read from the Makefile", b.perPackage)
		}
		if !strings.Contains(b.perSource, "Makefile") {
			t.Errorf("perSource = %q, want it to name the Makefile", b.perSource)
		}
		if b.runTimeout != 40*time.Minute {
			t.Errorf("runTimeout = %s, want 40m", b.runTimeout)
		}
	})

	t.Run("a -timeout in test_command is used when there is no Makefile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test -timeout 25m ./..."}, dir, "go test -timeout 25m ./...", 2)
		if b.perPackage != 25*time.Minute {
			t.Errorf("perPackage = %s, want 25m from test_command", b.perPackage)
		}
		if b.runTimeout != 50*time.Minute {
			t.Errorf("runTimeout = %s, want 50m", b.runTimeout)
		}
	})

	t.Run("rig config overrides both budgets", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "90m",
			TestVerifySlotTimeout: "15m",
		}
		b := resolveTestVerifyBudgets(mq, dir, mq.TestCommand, 3)
		if b.runTimeout != 90*time.Minute {
			t.Errorf("runTimeout = %s, want 90m", b.runTimeout)
		}
		if b.slotSource != "merge_queue.test_verify_slot_timeout" {
			t.Errorf("slotSource = %q", b.slotSource)
		}
		if b.slotTimeout != 15*time.Minute {
			t.Errorf("slotTimeout = %s, want 15m", b.slotTimeout)
		}
	})

	t.Run("invalid durations fall back and say so", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "banana",
			TestVerifySlotTimeout: "-5m",
		}
		b := resolveTestVerifyBudgets(mq, dir, mq.TestCommand, 1)
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the derived floor", b.runTimeout)
		}
		if !strings.Contains(b.runSource, "banana") {
			t.Errorf("runSource = %q, want it to flag the rejected override", b.runSource)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want the default", b.slotTimeout)
		}
		if !strings.Contains(b.slotSource, "-5m") {
			t.Errorf("slotSource = %q, want it to flag the rejected override", b.slotSource)
		}
	})
}

func TestSplitCommandEnvPrefixAndMakeTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		command  string
		wantEnv  []string
		wantArgv []string
		wantMake string
		wantOK   bool
	}{
		{"GOFLAGS=-p=6 make test", []string{"GOFLAGS=-p=6"}, []string{"make", "test"}, "test", true},
		{"FOO=1 BAR=2 make -j4 build", []string{"FOO=1", "BAR=2"}, []string{"make", "-j4", "build"}, "build", true},
		{"make", nil, []string{"make"}, "test", true},
		{"go test ./...", nil, []string{"go", "test", "./..."}, "", false},
		{"sh -c 'FOO=1 make test'", nil, []string{"sh", "-c", "'FOO=1", "make", "test'"}, "", false},
		{"1=1 make test", nil, []string{"1=1", "make", "test"}, "", false},
		{"make FOO=bar test", nil, []string{"make", "FOO=bar", "test"}, "test", true},
	}

	for _, tc := range tests {
		env, argv := splitCommandEnvPrefix(tc.command)
		if strings.Join(env, ",") != strings.Join(tc.wantEnv, ",") {
			t.Errorf("splitCommandEnvPrefix(%q) env = %v, want %v", tc.command, env, tc.wantEnv)
		}
		if strings.Join(argv, ",") != strings.Join(tc.wantArgv, ",") {
			t.Errorf("splitCommandEnvPrefix(%q) argv = %v, want %v", tc.command, argv, tc.wantArgv)
		}
		target, ok := makeTarget(tc.command)
		if ok != tc.wantOK || target != tc.wantMake {
			t.Errorf("makeTarget(%q) = (%q, %v), want (%q, %v)", tc.command, target, ok, tc.wantMake, tc.wantOK)
		}
	}
}

func TestTimeoutFromCommandText(t *testing.T) {
	t.Parallel()

	t.Run("reads both flag spellings and keeps the largest", func(t *testing.T) {
		t.Parallel()
		d, ok := timeoutFromCommandText("go test -timeout=5m ./a\ngo test -timeout 20m ./b\n")
		if !ok || d != 20*time.Minute {
			t.Errorf("= (%s, %v), want (20m, true) — the budget must cover the slowest invocation", d, ok)
		}
	})

	t.Run("no flag is not an error", func(t *testing.T) {
		t.Parallel()
		if d, ok := timeoutFromCommandText("pytest -q"); ok {
			t.Errorf("= (%s, true), want ok=false", d)
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

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
