package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
)

// gt-csng: the polecat-side pre-work duplicate check.

// gitRootFixture seeds a throwaway git repo whose origin/main carries a fresh
// commit, and returns its root; the caller removes it.
func gitRootFixture(t *testing.T) string {
	t.Helper()
	return cachedGitFixtureStrings(t, "gitRootFixture", func(root string) []string {
		return []string{buildGitRootFixture(t, root)}
	})[0]
}

// buildGitRootFixture makes gitRootFixture's repos under root.
func buildGitRootFixture(t *testing.T, root string) string {
	t.Helper()
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

// realGitPrimeTools runs prime's git calls for real but on a fake clock that
// is never advanced, so primeExternalToolTimeout cannot fire on a slow host:
// what these tests check is the check's reading of git, not its deadline
// (TestCheckHookedPathDupes_BoundsSlowGitLog covers that). Output collects in
// the returned buffer.
func realGitPrimeTools() (primeTools, *bytes.Buffer) {
	var out bytes.Buffer
	return primeTools{clock: clockwork.NewFakeClockAt(primeTestEpoch), out: &out}, &out
}

// hookedPathDupesOutput runs the check on real git and returns what it printed.
func hookedPathDupesOutput(ctx RoleContext, bead *beads.Issue) string {
	p, out := realGitPrimeTools()
	p.hookedPathDupes(ctx, bead)
	return out.String()
}

func TestDupesRecentLog(t *testing.T) {
	t.Parallel()
	root := gitRootFixture(t)
	p, _ := realGitPrimeTools()
	commits, err := p.dupesRecentLog(root, []string{"cmd/gt/hermetic_main_test.go"})
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
	if commits, err := p.dupesRecentLog(root, []string{"internal/cmd/never_touched.go"}); err != nil || len(commits) != 0 {
		t.Errorf("untouched path: commits=%v err=%v, want empty/nil", commits, err)
	}
}

func TestCheckHookedPathDupes(t *testing.T) {
	t.Run("warns when a shared file was recently changed", func(t *testing.T) {
		root := gitRootFixture(t)
		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat, WorkDir: root},
			&beads.Issue{
				ID:    "gt-3vr",
				Title: "Fix the slow-mail bound",
				Description: "cmd/gt/hermetic_main_test.go fails in " +
					"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
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
		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat, WorkDir: root},
			&beads.Issue{
				ID:          "gt-rl0",
				Title:       "Some unrelated bead",
				Description: "Touches internal/cmd/never_touched.go only.",
			})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("expected silence, got:\n%s", out)
		}
	})

	t.Run("silent when the bead names no paths", func(t *testing.T) {
		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat},
			&beads.Issue{ID: "gt-x", Title: "No paths here", Description: "Just prose."})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("expected silence, got:\n%s", out)
		}
	})

	t.Run("skipped in continuation mode", func(t *testing.T) {
		old := primeContinuationMode
		primeContinuationMode = true
		defer func() { primeContinuationMode = old }()

		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat, WorkDir: t.TempDir()},
			&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("continuation mode must skip the check, got:\n%s", out)
		}
	})

	t.Run("non-polecat is skipped", func(t *testing.T) {
		out := hookedPathDupesOutput(RoleContext{Role: RoleCrew, WorkDir: t.TempDir()},
			&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
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

		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat, WorkDir: root},
			&beads.Issue{
				ID:          "gt-x",
				Title:       "Fix the slow-mail bound",
				Description: "cmd/gt/hermetic_main_test.go fails in TestRunPrimeExternalTools_BoundsSlowMailCheck.",
			})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("dry-run must skip the check, got:\n%s", out)
		}
	})

	t.Run("silent outside a git repo", func(t *testing.T) {
		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat, WorkDir: t.TempDir()},
			&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
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

		out := hookedPathDupesOutput(RoleContext{Role: RolePolecat, WorkDir: root},
			&beads.Issue{ID: "gt-x", Title: "x", Description: "cmd/gt/hermetic_main_test.go."})
		if strings.TrimSpace(out) != "" {
			t.Fatalf("no origin/main must fail closed, got:\n%s", out)
		}
	})
}

// TestCheckHookedPathDupes_BoundsSlowGitLog proves the check's git calls run on
// prime's external-tool deadline rather than waiting a wedged git out, and
// that an abandoned call is not reported on. The deadline is asserted on the
// fake clock (see waitOutWedgedTool) rather than timed.
func TestCheckHookedPathDupes_BoundsSlowGitLog(t *testing.T) {
	t.Parallel()
	const logCall = "git:log origin/main --since=1 day --name-only --pretty=format:%h%x09%s -- cmd/gt/hermetic_main_test.go"
	f := &fakePrimeRunner{
		answers: map[string]string{"git:rev-parse --verify --quiet origin/main": ""},
		block:   map[string]bool{logCall: true},
	}
	p, clk, out := newFakePrimeTools(f)

	call := waitOutWedgedTool(t, f, clk, func() {
		p.hookedPathDupes(RoleContext{Role: RolePolecat, WorkDir: t.TempDir()},
			&beads.Issue{
				ID:    "gt-x",
				Title: "Fix the slow-mail bound",
				Description: "cmd/gt/hermetic_main_test.go fails in " +
					"TestRunPrimeExternalTools_BoundsSlowMailCheck.",
			})
	})

	if call.line != logCall {
		t.Fatalf("wedged call = %q, want %q", call.line, logCall)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("a git call the check abandoned must not be reported on:\n%s", out.String())
	}
}
