package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// gt-csng: the polecat-side pre-work duplicate check.

// gitRootFixture seeds a throwaway git repo whose origin/main carries a fresh
// commit, and returns its root; the caller removes it.
func gitRootFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	f := filepath.Join(root, "cmd", "gt", "hermetic_main_test.go")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("package gt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "TestRunPrimeExternalTools_BoundsSlowMailCheck: fix the slow-mail bound (gt-abc)")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	return root
}

func TestDupesRecentLog(t *testing.T) {
	root := gitRootFixture(t)
	commits, err := dupesRecentLog(root, []string{"cmd/gt/hermetic_main_test.go"})
	if err != nil {
		t.Fatalf("dupesRecentLog: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %d, want 1: %+v", len(commits), commits)
	}
	c := commits[0]
	if len(c.Hash) < 7 {
		t.Errorf("hash = %q, want a git hash", c.Hash)
	}
	if !c.Files["cmd/gt/hermetic_main_test.go"] {
		t.Errorf("Files = %v, want cmd/gt/hermetic_main_test.go", c.Files)
	}
	want := "TestRunPrimeExternalTools_BoundsSlowMailCheck: fix the slow-mail bound (gt-abc)"
	if c.Subject != want {
		t.Errorf("Subject = %q, want %q", c.Subject, want)
	}

	// A path the history never touches returns nothing, no error.
	if commits, err := dupesRecentLog(root, []string{"internal/cmd/never_touched.go"}); err != nil || len(commits) != 0 {
		t.Errorf("untouched path: commits=%v err=%v, want empty/nil", commits, err)
	}
}

func TestCheckHookedPathDupes(t *testing.T) {
	t.Run("warns when a shared file was recently changed", func(t *testing.T) {
		root := gitRootFixture(t)
		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: root},
				&beads.Issue{
					ID:    "gt-3vr",
					Title: "Fix the slow-mail bound",
					Description: "cmd/gt/hermetic_main_test.go fails in " +
						"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
				})
		})
		if !strings.Contains(out, "cmd/gt/hermetic_main_test.go") {
			t.Fatalf("output missing the shared file:\n%s", out)
		}
		if !strings.Contains(out, "test name your bead names") {
			t.Fatalf("output missing the stronger warning line:\n%s", out)
		}
		if !strings.Contains(out, "check before duplicating this work") {
			t.Fatalf("output missing the header:\n%s", out)
		}
	})

	t.Run("silent when no overlap", func(t *testing.T) {
		root := gitRootFixture(t)
		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: root},
				&beads.Issue{
					ID:          "gt-rl0",
					Title:       "Some unrelated bead",
					Description: "Touches internal/cmd/never_touched.go only.",
				})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("expected silence, got:\n%s", out)
		}
	})

	t.Run("silent when the bead names no paths", func(t *testing.T) {
		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat},
				&beads.Issue{ID: "gt-x", Title: "No paths here", Description: "Just prose."})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("expected silence, got:\n%s", out)
		}
	})

	t.Run("skipped in continuation mode", func(t *testing.T) {
		old := primeContinuationMode
		primeContinuationMode = true
		defer func() { primeContinuationMode = old }()

		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: t.TempDir()},
				&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("continuation mode must skip the check, got:\n%s", out)
		}
	})

	t.Run("non-polecat is skipped", func(t *testing.T) {
		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RoleWitness, WorkDir: t.TempDir()},
				&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("non-polecat must skip the check, got:\n%s", out)
		}
	})

	t.Run("skipped in dry-run", func(t *testing.T) {
		// The fixture is one the check WOULD warn on, so silence here is the
		// dry-run gate and not an absent overlap.
		root := gitRootFixture(t)
		old := primeDryRun
		primeDryRun = true
		defer func() { primeDryRun = old }()

		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: root},
				&beads.Issue{
					ID:          "gt-x",
					Title:       "Fix the slow-mail bound",
					Description: "cmd/gt/hermetic_main_test.go fails in TestRunPrimeExternalTools_BoundsSlowMailCheck.",
				})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("dry-run must skip the check, got:\n%s", out)
		}
	})

	t.Run("silent outside a git repo", func(t *testing.T) {
		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: t.TempDir()},
				&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("a workdir outside a repo must not be reported on, got:\n%s", out)
		}
	})

	t.Run("silent when the repo has no origin/main", func(t *testing.T) {
		// Fail-closed: a worktree whose remote branch is not fetched yet is
		// exactly the case a warning would be least trustworthy in, so the
		// check must say nothing rather than guess.
		root := gitRootFixture(t)
		drop := exec.Command("git", "update-ref", "-d", "refs/remotes/origin/main")
		drop.Dir = root
		if out, err := drop.CombinedOutput(); err != nil {
			t.Fatalf("drop origin/main: %v\n%s", err, out)
		}

		out := captureStdout(t, func() {
			checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: root},
				&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
		})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("no origin/main must fail closed, got:\n%s", out)
		}
	})
}

// TestCheckHookedPathDupes_BoundsSlowGitLog proves the check's git calls run on
// prime's external-tool deadline rather than waiting a wedged git out.
//
// The deadline is driven by a barrier — the stub git announces it has started
// on the log call, and that announcement cancels the context — not by host
// wall-clock time, for the reason recorded on
// TestRunPrimeExternalTools_BoundsSlowMailCheck (gt-v2a5). The stub stands in
// for git on PATH only after the fixture is built, so the repository the check
// reads is a real one.
func TestCheckHookedPathDupes_BoundsSlowGitLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script subprocess test")
	}
	markerDir := t.TempDir()
	startedPath := filepath.Join(markerDir, "git-started")
	survivedPath := filepath.Join(markerDir, "git-survived")

	root := gitRootFixture(t)

	binDir := filepath.Join(markerDir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  rev-parse) exit 0 ;;\n" +
		"  log)\n" +
		"    : > \"$PRIME_GIT_STARTED\"\n" +
		"    sleep " + primeTestStallSeconds + "\n" +
		"    : > \"$PRIME_GIT_SURVIVED\"\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write git stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PRIME_GIT_STARTED", startedPath)
	t.Setenv("PRIME_GIT_SURVIVED", survivedPath)

	// Prime's own budget for an external tool, so the only deadline that can
	// fire here is the barrier-driven one below.
	oldTimeout := primeExternalToolTimeout
	primeExternalToolTimeout = primeTestToolTimeout
	t.Cleanup(func() { primeExternalToolTimeout = oldTimeout })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	withPrimeExternalToolDeadline(t, func(time.Duration) (context.Context, context.CancelFunc) {
		return context.WithCancel(ctx)
	})

	barrierReached := make(chan bool, 1)
	go func() {
		reached := waitForPath(startedPath, primeTestBarrierWait)
		cancel() // never leave the check blocked, even if the barrier never came
		barrierReached <- reached
	}()

	start := time.Now()
	out := captureStdout(t, func() {
		checkHookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: root},
			&beads.Issue{
				ID:    "gt-x",
				Title: "Fix the slow-mail bound",
				Description: "cmd/gt/hermetic_main_test.go fails in " +
					"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
			})
	})
	elapsed := time.Since(start)

	if !<-barrierReached {
		t.Fatalf("git stub never announced it started within %v — nothing can be concluded about the check's bound", primeTestBarrierWait)
	}
	// The stub was mid-stall when the deadline fired, so the check must have
	// returned without it: a check that waited the tool out would have reaped
	// the child, and the child writes its survived marker before exiting.
	if _, err := os.Stat(survivedPath); err == nil {
		t.Fatalf("git log child ran to completion — the check waited past its deadline instead of abandoning the command")
	} else if !os.IsNotExist(err) {
		t.Fatalf("check survived marker: %v", err)
	}
	// Safety net rather than a bound: the stub stalls for primeTestSlowToolStall,
	// so only a check that sat out the whole stall can approach it.
	if elapsed > primeTestSlowToolStall {
		t.Fatalf("the check waited out the stalled git log: elapsed = %v", elapsed)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a git call the check abandoned must not be reported on:\n%s", out)
	}
}
