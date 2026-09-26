package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
}
