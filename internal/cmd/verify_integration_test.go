package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// TestRunDefaultTestVerification guards the core gt-h9kf behavior: gt done's
// default (non-opt-in) test gate must actually run the rig's hermetic
// test_command (gt-btw1) and refuse when it fails, succeed when it passes,
// and skip cleanly when there is nothing to verify.
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
		if result.scope != "full" {
			t.Errorf("scope = %q, want %q", result.scope, "full")
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

	t.Run("whole-package deletion that still compiles: verified by go build, not a false refusal", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		deletePkgb(t, dir)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/delete-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: a clean whole-package deletion must not refuse (gt-ytjh): %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if len(result.packages) != 1 || result.packages[0] != "." {
			t.Errorf("packages = %v, want [.] (the whole-module build stands in for the deleted package)", result.packages)
		}
	})

	// The still-refusing half of gt-ytjh. The success path above deletes a
	// package nothing imports; this one deletes a package the module root
	// imports. The changed .go files resolve to nothing — the root package that
	// needs the deleted one is not itself in the diff — so the whole-module
	// build is the only thing that can see the breakage, and the gate must
	// refuse. It runs the real goBuildWholeModule, so it also pins that the
	// build happens in the worktree under test: the host module builds, and a
	// build of it could not produce this refusal.
	t.Run("whole-package deletion with broken importers: refuses with the compiler output", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("package main\n\nimport _ \"example.test/pkgb\"\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "root imports pkgb")
		// Fold the importer into the base so the deletion is the whole diff.
		base := strings.TrimSpace(runGitOut(t, dir, "rev-parse", "HEAD"))
		runGitIn(t, dir, "update-ref", "refs/remotes/origin/main", base)
		deletePkgb(t, dir)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/delete-broken-role")
		if err == nil {
			t.Fatalf("runDefaultTestVerification: a deletion its importer still needs must refuse, got result=%+v", result)
		}
		if !strings.Contains(err.Error(), "unbuildable") {
			t.Errorf("error does not report the deletion refusal: %v", err)
		}
		if !strings.Contains(err.Error(), "example.test/pkgb") {
			t.Errorf("error does not surface the compiler's own output: %v", err)
		}
	})

	// A nested module is not this module's to refuse and not this module's to
	// build with `./...`, so the gate builds it where it lives. Both halves run
	// the real go build: with a clean nested module the gate proceeds and says
	// in the log which directory it could not scope, and with one that does not
	// compile it refuses — `go list` resolves a package without typechecking
	// it, so only the build can see that.
	t.Run("nested-module change: builds that module and proceeds", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addNestedModule(t, dir, "plugins/example-sub", "example.test/plugin")

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/nested-module-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: a change inside a nested module must not refuse: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if len(result.packages) != 1 || result.packages[0] != "." {
			t.Errorf("packages = %v, want [.] — no package of this module can be scoped to the change", result.packages)
		}
		logBytes, readErr := os.ReadFile(result.logPath)
		if readErr != nil {
			t.Fatalf("reading verify log: %v", readErr)
		}
		logText := string(logBytes)
		for _, want := range []string{"not scoped:", "plugins/example-sub", "example.test/plugin"} {
			if !strings.Contains(logText, want) {
				t.Errorf("verify log is missing %q, so the directory the gate could not scope is invisible:\n%s", want, logText)
			}
		}
	})

	t.Run("nested-module change that does not compile: refuses with the compiler output", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addNestedModule(t, dir, "plugins/example-sub", "example.test/plugin")
		// A type error, which `go list` inside the nested module still resolves
		// (it does not typecheck) and this module's `go list` never reaches.
		if err := os.WriteFile(filepath.Join(dir, "plugins", "example-sub", "nested.go"), []byte("package nestedmod\n\nfunc V() int { return \"not an int\" }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "break the nested module")

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/nested-module-broken-role")
		if err == nil {
			t.Fatalf("runDefaultTestVerification: a nested module that does not build must refuse, got result=%+v", result)
		}
		if !strings.Contains(err.Error(), "plugins/example-sub") {
			t.Errorf("the refusal does not name the nested module: %v", err)
		}
		if !strings.Contains(err.Error(), "cannot use") {
			t.Errorf("the refusal does not surface the compiler's own output: %v", err)
		}
	})

	// The -o branch of the whole-module build is what makes a single-binary
	// module verifiable at all. With pkgb gone, cmd/ is the module's only
	// package and its only main, so `go build ./...` names the executable after
	// that directory and refuses to write it over the directory itself ("build
	// output \"cmd\" already exists and is a directory"). Running the real
	// build here, this subtest fails on the plain form.
	t.Run("deletion in a single-binary module: the build links to a temp dir and the deletion verifies", func(t *testing.T) {
		dir := initSingleBinaryGoRepo(t)
		deletePkgb(t, dir)

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/single-binary-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if len(result.packages) != 1 || result.packages[0] != "." {
			t.Errorf("packages = %v, want [.] — the whole-module build stands in for the deleted package", result.packages)
		}
		// The executable went to the gate's temp dir, so cmd/ is still the
		// directory it was rather than the file the plain build would name.
		if fi, statErr := os.Stat(filepath.Join(dir, "cmd")); statErr != nil || !fi.IsDir() {
			t.Errorf("cmd/ is no longer a directory (err=%v): the whole-module build wrote its output into the worktree", statErr)
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

// initSingleBinaryGoRepo builds a Go module whose packages are a main package
// at cmd/ and one library package at pkgb/, so that deleting pkgb leaves cmd/
// as the module's only package: the layout in which `go build ./...` names its
// executable after cmd/ and will not write it over the directory.
func initSingleBinaryGoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitIn(t, dir, "init", "-q", "-b", "main")
	runGitIn(t, dir, "config", "user.email", "test@example.com")
	runGitIn(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.test\n\ngo 1.21\n")
	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "cmd", "main.go"), "package main\n\nfunc main() {}\n")
	if err := os.MkdirAll(filepath.Join(dir, "pkgb"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "pkgb", "b.go"), "package pkgb\n\nfunc Double(a int) int { return a * 2 }\n")
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(runGitOut(t, dir, "rev-parse", "HEAD"))
	runGitIn(t, dir, "update-ref", "refs/remotes/origin/main", base)
	return dir
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

	t.Run("the gate runs the rig's full hermetic test_command, slot-free, when the rig does not ask for containers", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)

		var calls []runCall
		acquired := false
		stubVerifyGate(t,
			func(_ string, _ string, _ time.Duration) (func(), error) {
				acquired = true
				return func() {}, nil
			},
			func(ctx context.Context, _ string, script string, env []string, _ *os.File) error {
				deadline, _ := ctx.Deadline()
				calls = append(calls, runCall{script: script, env: env, deadline: deadline})
				return nil
			})

		mq := &config.MergeQueueConfig{TestCommand: "GOFLAGS=-p=6 make test"}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/budget-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if len(calls) != 1 {
			t.Fatalf("runner called %d times, want 1", len(calls))
		}
		if acquired {
			t.Error("the gate took the town-wide container-gate slot for a container-free command (gt-wx53)")
		}

		// gt-btw1: the command is the rig's own hermetic test_command, not a
		// derived go test over the changed packages.
		if calls[0].script != "GOFLAGS=-p=6 make test" {
			t.Errorf("script = %q, want the rig's full test_command", calls[0].script)
		}
		// Env parity (gt-fa3s): the rig's configured test_command environment
		// must reach the gate. slot.Acquire's reentrant marker may also be
		// present, so look for the assignment rather than exact equality.
		if !containsEnv(calls[0].env, "GOFLAGS=-p=6") {
			t.Errorf("env is missing GOFLAGS=-p=6 from test_command")
		}
		// gt-wx53: the run carries an explicit opt-out so the suite's
		// container-backed tests skip instead of starting outside the slot.
		if !containsEnv(calls[0].env, dockerTestsEnv+"=0") {
			t.Errorf("env is missing %s=0, so nothing keeps the container suite from starting outside a slot:\n%v", dockerTestsEnv, calls[0].env)
		}
		// The full suite cannot be scaled by a changed-package count, so the
		// run budget is the floor (gt-btw1).
		gotBudget := time.Until(calls[0].deadline)
		if gotBudget < 29*time.Minute || gotBudget > 31*time.Minute {
			t.Errorf("run budget ≈ %s, want ≈30m (the full-suite floor)", gotBudget.Round(time.Minute))
		}
		if result.runBudget != defaultTestVerifyRunFloor {
			t.Errorf("result.runBudget = %s, want the %s floor", result.runBudget, defaultTestVerifyRunFloor)
		}
		if result.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("result.slotTimeout = %s, want %s", result.slotTimeout, defaultTestVerifySlotTimeout)
		}
		if result.slotUsed || !result.containersOptedOut {
			t.Errorf("slotUsed=%v containersOptedOut=%v, want false/true", result.slotUsed, result.containersOptedOut)
		}
		if result.scope != "full" {
			t.Errorf("scope = %q, want %q", result.scope, "full")
		}
		if len(result.packages) != 2 {
			t.Errorf("packages = %v, want both changed packages recorded for the MR bead", result.packages)
		}

		// The resolve-before-you-run header is the artifact gt-pnkd's victims
		// could not read: it must name both budgets and their provenance, and
		// (gt-wx53) say whether this gate is queueing for the town's slot.
		logBytes, readErr := os.ReadFile(result.logPath)
		if readErr != nil {
			t.Fatalf("reading verify log: %v", readErr)
		}
		logText := string(logBytes)
		for _, want := range []string{
			"run budget: 30m", "slot cap: 1h", "full test_command",
			"env (inherited from test_command): GOFLAGS=-p=6",
			"containers: opted out (" + dockerTestsEnv + "=0)",
			"running without a container-gate slot",
		} {
			if !strings.Contains(logText, want) {
				t.Errorf("verify log is missing %q:\n%s", want, logText)
			}
		}
	})

	t.Run("a rig whose command asks for containers runs it inside a slot, unchanged", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)

		var calls []runCall
		var gotTimeout time.Duration
		stubVerifyGate(t,
			func(_ string, _ string, timeout time.Duration) (func(), error) {
				gotTimeout = timeout
				return func() {}, nil
			},
			func(_ context.Context, _ string, script string, env []string, _ *os.File) error {
				calls = append(calls, runCall{script: script, env: env})
				return nil
			})

		// A rig that wants its container suite verified here says so in its own
		// command; the gate must honour it, and hold a slot while it runs.
		mq := &config.MergeQueueConfig{TestCommand: dockerTestsEnv + "=1 go test ./..."}
		g := git.NewGit(dir)
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/containers-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if gotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slot timeout = %s, want the %s default", gotTimeout, defaultTestVerifySlotTimeout)
		}
		if len(calls) != 1 {
			t.Fatalf("runner called %d times, want 1", len(calls))
		}
		if !containsEnv(calls[0].env, dockerTestsEnv+"=1") {
			t.Errorf("env lost the rig's %s=1 opt-in", dockerTestsEnv)
		}
		if containsEnv(calls[0].env, dockerTestsEnv+"=0") {
			t.Errorf("the gate opted out containers a rig had asked for")
		}
		if !result.slotUsed || result.containersOptedOut {
			t.Errorf("slotUsed=%v containersOptedOut=%v, want true/false", result.slotUsed, result.containersOptedOut)
		}
		logBytes, readErr := os.ReadFile(result.logPath)
		if readErr != nil {
			t.Fatalf("reading verify log: %v", readErr)
		}
		for _, want := range []string{"containers: enabled", "container-gate slot acquired after"} {
			if !strings.Contains(string(logBytes), want) {
				t.Errorf("verify log is missing %q:\n%s", want, logBytes)
			}
		}
	})

	t.Run("test_verify_command replaces the full test_command and fills {packages}", func(t *testing.T) {
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

		mq := &config.MergeQueueConfig{TestCommand: dockerTestsEnv + "=1 go test ./...", TestVerifySlotTimeout: "5m"}
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

		mq := &config.MergeQueueConfig{TestCommand: dockerTestsEnv + "=1 go test ./...", TestVerifyRunTimeout: "1ms"}
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

	t.Run("a slot-free timing-out suite says no slot was taken", func(t *testing.T) {
		dir := newRepoWithTwoChangedPackages(t)
		acquired := false
		stubVerifyGate(t,
			func(_ string, _ string, _ time.Duration) (func(), error) {
				acquired = true
				return func() {}, nil
			},
			func(ctx context.Context, _ string, _ string, _ []string, _ *os.File) error {
				<-ctx.Done()
				return ctx.Err()
			})

		mq := &config.MergeQueueConfig{TestCommand: "go test ./...", TestVerifyRunTimeout: "1ms"}
		g := git.NewGit(dir)
		_, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/run-timeout-noslot-role")
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		// The "slot wait is not counted" clause exists to explain a wait that
		// happened; there is no wait to explain on the slot-free path, and
		// saying "the 0s slot wait is not counted" would invent one.
		if acquired {
			t.Error("the gate took a slot on the slot-free path")
		}
		if !strings.Contains(err.Error(), "no container-gate slot was taken") {
			t.Errorf("error does not say the run took no slot: %v", err)
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

		mq := &config.MergeQueueConfig{TestCommand: dockerTestsEnv + "=1 go test ./..."}
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
		got, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if got.changedGoFiles {
			t.Error("changedGoFiles = true, want false for a docs-only change")
		}
		if len(got.packages) != 0 {
			t.Errorf("packages = %v, want empty", got.packages)
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
		got, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if !got.changedGoFiles {
			t.Fatal("changedGoFiles = false, want true")
		}
		if len(got.packages) != 1 || !strings.HasSuffix(got.packages[0], "/pkga") {
			t.Errorf("packages = %v, want exactly one entry ending in /pkga", got.packages)
		}
		if len(got.deletedDirs) != 0 || len(got.unresolvable) != 0 {
			t.Errorf("deletedDirs = %v, unresolvable = %v, want both empty for a modification to an existing package", got.deletedDirs, got.unresolvable)
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
		got, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if !got.changedGoFiles {
			t.Fatal("changedGoFiles = false, want true (deletion still touches .go paths)")
		}
		if len(got.packages) != 0 {
			t.Errorf("packages = %v, want empty — pkgb no longer exists as a package", got.packages)
		}
		if len(got.deletedDirs) != 1 || got.deletedDirs[0] != "pkgb" {
			t.Errorf("deletedDirs = %v, want exactly [pkgb] — the directory is gone, so an empty package list is the deletion talking", got.deletedDirs)
		}
		if len(got.unresolvable) != 0 {
			t.Errorf("unresolvable = %v, want empty — nothing is left in pkgb to fail to resolve", got.unresolvable)
		}
	})

	// A directory the repo's module does not contain is not a broken change:
	// it is another module's file, and resolving it there is what tells the two
	// apart. Refusing every root-unresolvable directory locked out every change
	// to a nested module (plugins/dolt-snapshots in this repo).
	t.Run("a nested module's directory resolves within its own module", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		addNestedModule(t, dir, "plugins/example-sub", "example.test/plugin")

		g := git.NewGit(dir)
		got, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if !got.changedGoFiles {
			t.Fatal("changedGoFiles = false, want true")
		}
		if len(got.packages) != 0 {
			t.Errorf("packages = %v, want empty — the nested module's files are not packages of this module", got.packages)
		}
		if len(got.nestedModules) != 1 {
			t.Fatalf("nestedModules = %v, want exactly one entry", got.nestedModules)
		}
		nested := got.nestedModules[0]
		if nested.dir != "plugins/example-sub" || nested.moduleRoot != "plugins/example-sub" || nested.importPath != "example.test/plugin" {
			t.Errorf("nested = %+v, want the dir, its own module root, and the import path that module gives it", nested)
		}
		if len(got.unresolvable) != 0 || len(got.deletedDirs) != 0 {
			t.Errorf("unresolvable = %v, deletedDirs = %v, want both empty: nothing here is broken or gone", got.unresolvable, got.deletedDirs)
		}
	})

	// Failing closed inside a nested module: a directory the owning module will
	// not make a package of either leaves nothing anywhere to build or test, so
	// the classification must not become a way to skip a broken file.
	t.Run("a directory that resolves to nothing in its own nested module stays unresolvable", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		// The nested module's only .go file is one its build constraints
		// exclude, so the module that owns it resolves no package for the
		// directory either.
		if err := os.MkdirAll(filepath.Join(dir, "sub", "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "sub", "go.mod"), "module example.test/sub\n\ngo 1.21\n")
		writeFile(t, filepath.Join(dir, "sub", "inner", "x.go"), "//go:build never\n\npackage inner\n\nfunc X() {}\n")
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "add a nested module whose only file is excluded")

		g := git.NewGit(dir)
		got, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if len(got.nestedModules) != 0 {
			t.Errorf("nestedModules = %v, want empty — the module that owns sub/inner resolves no package for it", got.nestedModules)
		}
		if len(got.unresolvable) != 1 || got.unresolvable[0].dir != "sub/inner" {
			t.Fatalf("unresolvable = %v, want exactly one entry for sub/inner", got.unresolvable)
		}
		// The refusal quotes the owning module's attempt, not this module's:
		// this module was never going to name a file of another module.
		if got.unresolvable[0].listErr == nil || !strings.Contains(got.unresolvable[0].listErr.Error(), "nested module sub") {
			t.Errorf("listErr = %v, want the nested module's own refusal quoted", got.unresolvable[0].listErr)
		}
	})

	// A .go file that is still in the worktree but that go list will not
	// resolve is NOT a deletion, and must not be reported as one: a whole-module
	// build has nothing to build for a directory its constraints exclude, so
	// treating the empty package list as a deletion would wave the change
	// through unverified.
	t.Run("a .go file excluded by build constraints is unresolvable, not deleted", func(t *testing.T) {
		dir, base := initVerifyTestGoRepo(t)
		if err := os.MkdirAll(filepath.Join(dir, "pkgexcluded"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pkgexcluded", "x.go"), []byte("//go:build never\n\npackage pkgexcluded\n\nfunc X() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "add a build-constraint-excluded package")

		g := git.NewGit(dir)
		got, err := changedGoPackages(g, dir, base)
		if err != nil {
			t.Fatalf("changedGoPackages: %v", err)
		}
		if !got.changedGoFiles {
			t.Fatal("changedGoFiles = false, want true")
		}
		if len(got.packages) != 0 {
			t.Errorf("packages = %v, want empty — go list cannot resolve the directory", got.packages)
		}
		if len(got.deletedDirs) != 0 {
			t.Errorf("deletedDirs = %v, want empty — x.go is still in the worktree", got.deletedDirs)
		}
		if len(got.unresolvable) != 1 || got.unresolvable[0].dir != "pkgexcluded" {
			t.Fatalf("unresolvable = %v, want exactly one entry for pkgexcluded", got.unresolvable)
		}
		if got.unresolvable[0].listErr == nil || !strings.Contains(got.unresolvable[0].listErr.Error(), "build constraints exclude") {
			t.Errorf("listErr = %v, want the go list failure naming the excluded build constraints", got.unresolvable[0].listErr)
		}
	})
}

// TestRunDefaultTestVerification_SlotOnlyForContainerRuns covers gt-wx53: gt
// done runs the rig's full hermetic test_command (gt-btw1), but it takes the
// town-wide container-gate slot only when that command can actually start a
// container-backed suite. Everything else — including a change to a
// container-backed package, since this rig's command does not ask for
// containers — runs slot-free with the container opt-in forced off, so a
// polecat's submission no longer queues behind the daemon's main-branch patrol
// and the refinery's batch gate. There is no deferral to the refinery either
// way (gt-btw1): the command is the rig's full one, run whole.
func TestRunDefaultTestVerification_SlotOnlyForContainerRuns(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	t.Run("container-backed package changed, rig does not ask for containers: slot-free", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga and internal/cmd")

		acquired := false
		var env []string
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, func(_ context.Context, _ string, _ string, e []string, _ *os.File) error {
			env = e
			return nil
		})

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/slot-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true", result)
		}
		if len(result.packages) != 2 {
			t.Errorf("packages = %v, want both changed packages recorded for the MR bead", result.packages)
		}
		if result.slotUsed || acquired {
			t.Errorf("slotUsed=%v acquired=%v, want false/false: the rig's command never asked for containers", result.slotUsed, acquired)
		}
		if !result.containersOptedOut {
			t.Error("containersOptedOut = false, want true: the run must keep the container suite from starting outside a slot")
		}
		// The container-backed package in the diff is what makes this case
		// interesting: the suite still runs whole, and the opt-out is what
		// keeps its Docker tests from starting with no slot held.
		if !containsEnv(env, dockerTestsEnv+"=0") {
			t.Errorf("child env is missing %s=0:\n%v", dockerTestsEnv, env)
		}
	})

	t.Run("only container-backed packages changed: still slot-free, and the whole suite still runs", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		addContainerBackedPackage(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "only internal/cmd")

		acquired := false
		var scripts []string
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, func(_ context.Context, _ string, script string, _ []string, _ *os.File) error {
			scripts = append(scripts, script)
			return nil
		})

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/slot-all-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want ran=true success=true (no deferral under gt-btw1)", result)
		}
		if len(scripts) != 1 || scripts[0] != "go test ./..." {
			t.Errorf("scripts = %v, want the rig's whole test_command run once", scripts)
		}
		if result.slotUsed || acquired {
			t.Errorf("slotUsed=%v acquired=%v, want false/false", result.slotUsed, acquired)
		}
	})

	t.Run("an inherited container opt-in is written off, and the slot with it", func(t *testing.T) {
		dir, _ := initVerifyTestGoRepo(t)
		changePkga(t, dir)
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

		// The integration harness itself sets this (the package's TestMain
		// needs Docker), so this is also the case that would otherwise make the
		// gate behave two different ways inside one test binary.
		t.Setenv(dockerTestsEnv, "1")

		acquired := false
		var env []string
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, func(_ context.Context, _ string, _ string, e []string, _ *os.File) error {
			env = e
			return nil
		})

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "go test ./..."}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/inherited-optin-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if result.slotUsed || acquired {
			t.Errorf("slotUsed=%v acquired=%v, want false/false: the rig's command never asked for containers", result.slotUsed, acquired)
		}
		if !containsEnv(env, dockerTestsEnv+"=0") {
			t.Errorf("child env is missing %s=0:\n%v", dockerTestsEnv, env)
		}
		seen := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, dockerTestsEnv+"=") {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("child env carries %s %d times, want exactly once (a duplicate resolves differently per reader)", dockerTestsEnv, seen)
		}
		// gt-0hbm: this is the environment the run reads, so it is what the
		// slot decision was made for — a gate that held no slot must not hand
		// the suite a switch that says containers are on.
		if result.slotUsed != (containsEnv(env, dockerTestsEnv+"=1")) {
			t.Errorf("slotUsed=%v with the run's %s=%v: the decision and the environment disagree", result.slotUsed, dockerTestsEnv, containsEnv(env, dockerTestsEnv+"=1"))
		}
	})

	t.Run("a non-Go rig's command is opaque: it keeps the slot", func(t *testing.T) {
		dir := t.TempDir()
		runGitIn(t, dir, "init", "-q", "-b", "main")
		runGitIn(t, dir, "config", "user.email", "test@example.com")
		runGitIn(t, dir, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "base")
		runGitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\nmore\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "touch readme")

		acquired := false
		stubVerifyGate(t, func(townRoot, role string, timeout time.Duration) (func(), error) {
			acquired = true
			return func() {}, nil
		}, func(_ context.Context, _ string, _ string, _ []string, _ *os.File) error { return nil })

		g := git.NewGit(dir)
		mq := &config.MergeQueueConfig{TestCommand: "make test"}
		result, err := runDefaultTestVerification(g, dir, "main", "main", mq, townRoot, "test/non-go-slot-role")
		if err != nil {
			t.Fatalf("runDefaultTestVerification: %v", err)
		}
		if !result.slotUsed || !acquired {
			t.Errorf("slotUsed=%v acquired=%v, want true/true: nothing here can tell us a non-Go command never touches Docker", result.slotUsed, acquired)
		}
		if result.containersOptedOut {
			t.Error("containersOptedOut = true: a non-Go rig is never opted out")
		}
	})
}
