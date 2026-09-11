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

// TestRunPreVerificationGates guards the om-gate T8 fix: `gt done
// --pre-verified` must perform the verification it stamps, not merely trust
// the flag. A red gate must report which gate failed and produce no
// stamp-worthy log hash; a green run over multiple gates must run all of
// them, in order, and produce a stable log hash.
func TestRunPreVerificationGates(t *testing.T) {
	t.Run("failing gate reports its name and exit code, no success", func(t *testing.T) {
		dir := t.TempDir()
		mq := &config.MergeQueueConfig{TestCommand: "false"}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if result.success {
			t.Error("success = true, want false for a failing gate command")
		}
		if result.failedGate != "test" {
			t.Errorf("failedGate = %q, want %q", result.failedGate, "test")
		}
		if result.exitCode != 1 {
			t.Errorf("exitCode = %d, want 1", result.exitCode)
		}
		if result.logSHA256 != "" {
			t.Error("logSHA256 should be empty when the run did not succeed")
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".runtime", "gt-preverify.log")); statErr != nil {
			t.Errorf(".runtime/gt-preverify.log should exist after a failed run: %v", statErr)
		}
	})

	t.Run("all-zero exit runs every configured gate and produces a log hash", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		mq := &config.MergeQueueConfig{
			SetupCommand: "echo setup >> " + marker,
			LintCommand:  "echo lint >> " + marker,
			TestCommand:  "echo test >> " + marker,
		}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if !result.success {
			t.Fatalf("success = false, want true: failedGate=%q exitCode=%d", result.failedGate, result.exitCode)
		}
		if result.exitCode != 0 {
			t.Errorf("exitCode = %d, want 0", result.exitCode)
		}
		if result.logSHA256 == "" {
			t.Error("logSHA256 should be non-empty on success")
		}

		got, readErr := os.ReadFile(marker)
		if readErr != nil {
			t.Fatalf("reading marker file: %v", readErr)
		}
		want := "setup\nlint\ntest\n"
		if string(got) != want {
			t.Errorf("gates ran in wrong order or not all ran: got %q want %q", string(got), want)
		}
	})

	t.Run("stops at the first failing gate, later gates never run", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		mq := &config.MergeQueueConfig{
			SetupCommand: "echo setup >> " + marker,
			LintCommand:  "false",
			TestCommand:  "echo test >> " + marker,
		}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if result.success {
			t.Fatal("success = true, want false")
		}
		if result.failedGate != "lint" {
			t.Errorf("failedGate = %q, want %q", result.failedGate, "lint")
		}

		got, readErr := os.ReadFile(marker)
		if readErr != nil {
			t.Fatalf("reading marker file: %v", readErr)
		}
		if string(got) != "setup\n" {
			t.Errorf("expected only setup to have run, got %q", string(got))
		}
	})

	t.Run("no configured gate commands is a no-op success", func(t *testing.T) {
		dir := t.TempDir()
		result, err := runPreVerificationGates(dir, &config.MergeQueueConfig{})
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if !result.success {
			t.Error("success = false, want true when there are no gates to run")
		}
	})

	// om-gate T8 review: .gt-preverify.log at the worktree root was not a
	// recognized runtime artifact, so CleanExcludingRuntime() went false
	// after a pre-verified run — a re-run of gt done then failed at the
	// uncommitted-work check. Guard that the log's new home under .runtime/
	// actually keeps the worktree clean-excluding-runtime, not just that the
	// file moved.
	t.Run("log file does not dirty the worktree gate", func(t *testing.T) {
		dir := t.TempDir()
		initPreVerifyTestGitRepo(t, dir)
		mq := &config.MergeQueueConfig{TestCommand: "true"}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if !result.success {
			t.Fatalf("success = false, want true: failedGate=%q exitCode=%d", result.failedGate, result.exitCode)
		}

		status, err := git.NewGit(dir).CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !status.CleanExcludingRuntime() {
			t.Errorf("CleanExcludingRuntime() = false after a pre-verification run; modified=%v untracked=%v",
				status.ModifiedFiles, status.UntrackedFiles)
		}
	})
}

func initPreVerifyTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("commit", "-q", "--allow-empty", "-m", "initial")
}

// TestResolvePreVerification guards the om-gate T8 review's first major
// finding: --pre-verified disables auto-rebase, so gates can run on a
// worktree HEAD that never actually contains the target base being
// stamped. resolvePreVerification must refuse to stamp in that case.
func TestResolvePreVerification(t *testing.T) {
	dir := t.TempDir()
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
	runGit("commit", "-q", "--allow-empty", "-m", "c1")
	c1 := runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", c1)

	g := git.NewGit(dir)
	mq := &config.MergeQueueConfig{TestCommand: "true"}

	t.Run("HEAD contains the resolved target base: stamps", func(t *testing.T) {
		stamp, ok, warning := resolvePreVerification(g, dir, "main", "main", mq, config.GateSetSHA(mq))
		if !ok {
			t.Fatalf("expected ok=true, got warning=%q", warning)
		}
		if stamp.verifiedBase != c1 {
			t.Errorf("verifiedBase = %q, want %q", stamp.verifiedBase, c1)
		}
	})

	t.Run("target moved ahead of HEAD: refuses to stamp", func(t *testing.T) {
		// Build a commit descending from c1 WITHOUT touching the checked-out
		// worktree, then point origin/main at it — simulating a branch that
		// fell behind target while --pre-verified skipped the rebase.
		tree := runGit("write-tree")
		c2 := runGit("commit-tree", tree, "-p", c1, "-m", "c2")
		runGit("update-ref", "refs/remotes/origin/main", c2)
		t.Cleanup(func() { runGit("update-ref", "refs/remotes/origin/main", c1) })

		stamp, ok, warning := resolvePreVerification(g, dir, "main", "main", mq, config.GateSetSHA(mq))
		if ok {
			t.Fatalf("expected ok=false when HEAD does not contain the target base, got stamp=%+v", stamp)
		}
		if !strings.Contains(warning, "does not contain") {
			t.Errorf("warning = %q, want it to explain HEAD does not contain the target base", warning)
		}
	})
}
