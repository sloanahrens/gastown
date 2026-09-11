package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		mq := &config.MergeQueueConfig{TestCommand: "echo ran >> " + marker}
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
