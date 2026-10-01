//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegrationPolecatStopPendingWork runs the Stop hook's pending-work
// check against real repositories; TestPolecatStopPendingWork covers the
// decision over canned git state.
func TestIntegrationPolecatStopPendingWork(t *testing.T) {
	t.Parallel()
	t.Run("clean feature branch has no pending work", func(t *testing.T) {
		t.Parallel()
		repo := initPolecatStopTestRepo(t)

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})

	t.Run("non-runtime dirty work is pending", func(t *testing.T) {
		t.Parallel()
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "internal/cmd/work.go", "package cmd\n")

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if !pending {
			t.Fatal("pending = false, want true")
		}
		if !strings.Contains(reason, "non-runtime dirty") {
			t.Fatalf("reason = %q, want non-runtime dirty work", reason)
		}
	})

	t.Run("runtime-only dirty work is ignored", func(t *testing.T) {
		t.Parallel()
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, ".opencode/state.json", "{}\n")

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})

	t.Run("branch stash is pending", func(t *testing.T) {
		t.Parallel()
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "stash-work.txt", "saved work\n")
		runPolecatStopTestGit(t, repo, "stash", "push", "-u", "-m", "branch stash")

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if !pending {
			t.Fatal("pending = false, want true")
		}
		if !strings.Contains(reason, "branch stash") {
			t.Fatalf("reason = %q, want branch stash", reason)
		}
	})

	t.Run("pushed source branch still pending until target contains it", func(t *testing.T) {
		t.Parallel()
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "submitted.go", "package main\n")
		runPolecatStopTestGit(t, repo, "add", "submitted.go")
		runPolecatStopTestGit(t, repo, "commit", "-m", "add submitted work")
		runPolecatStopTestGit(t, repo, "push", "origin", "HEAD:"+polecatStopTestBranch)

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if !pending {
			t.Fatal("pending = false, want true")
		}
		if !strings.Contains(reason, "unsubmitted commit") {
			t.Fatalf("reason = %q, want unsubmitted commit", reason)
		}
	})

	t.Run("target-contained commit has no pending work", func(t *testing.T) {
		t.Parallel()
		repo := initPolecatStopTestRepo(t)
		writePolecatStopTestFile(t, repo, "merged.go", "package main\n")
		runPolecatStopTestGit(t, repo, "add", "merged.go")
		runPolecatStopTestGit(t, repo, "commit", "-m", "add merged work")
		runPolecatStopTestGit(t, repo, "checkout", "main")
		runPolecatStopTestGit(t, repo, "merge", "--ff-only", polecatStopTestBranch)
		runPolecatStopTestGit(t, repo, "push", "origin", "main")
		runPolecatStopTestGit(t, repo, "checkout", polecatStopTestBranch)

		pending, reason, err := polecatStopPendingWork(repo, polecatStopTestBranch)
		if err != nil {
			t.Fatalf("polecatStopPendingWork: %v", err)
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})

	t.Run("invalid repo fails closed", func(t *testing.T) {
		t.Parallel()
		pending, reason, err := polecatStopPendingWork(t.TempDir(), polecatStopTestBranch)
		if err == nil {
			t.Fatal("polecatStopPendingWork error = nil, want error")
		}
		if pending {
			t.Fatalf("pending = true (%s), want false", reason)
		}
	})
}

// amendPolecatStopCommitDate re-dates repo's HEAD commit, author and
// committer, to when.
func amendPolecatStopCommitDate(t *testing.T, repo string, when time.Time) {
	t.Helper()
	date := when.Format(time.RFC3339)
	cmd := exec.Command("git", "commit", "--amend", "--no-edit", "--date", date)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend: %v\n%s", err, out)
	}
}

// TestIntegrationPolecatStopCommittedWithinGrace guards the second turn-boundary signal
// for gt-couv: a commit that just landed means the polecat is very likely
// still mid-formula (about to build/lint/test), while an old commit carries
// no such signal.
func TestIntegrationPolecatStopCommittedWithinGrace(t *testing.T) {
	t.Parallel()
	t.Run("fresh commit is within grace", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		// The repo is a copy of a template built once per test binary, so
		// its commit is as old as the template. Re-date it to now: this case
		// is about a commit that just landed.
		amendPolecatStopCommitDate(t, repo, time.Now())

		recent, err := polecatStopCommittedWithinGrace(repo)
		if err != nil {
			t.Fatalf("polecatStopCommittedWithinGrace: %v", err)
		}
		if !recent {
			t.Fatal("recent = false, want true for a commit made moments ago")
		}
	})

	t.Run("old commit is outside grace", func(t *testing.T) {
		repo := initPolecatStopTestRepo(t)
		amendPolecatStopCommitDate(t, repo, time.Now().Add(-10*time.Minute))

		recent, err := polecatStopCommittedWithinGrace(repo)
		if err != nil {
			t.Fatalf("polecatStopCommittedWithinGrace: %v", err)
		}
		if recent {
			t.Fatal("recent = true, want false for a 10-minute-old commit")
		}
	})

	t.Run("invalid repo fails closed", func(t *testing.T) {
		recent, err := polecatStopCommittedWithinGrace(t.TempDir())
		if err == nil {
			t.Fatal("polecatStopCommittedWithinGrace error = nil, want error")
		}
		if recent {
			t.Fatal("recent = true, want false on error")
		}
	})
}

func initPolecatStopTestRepo(t *testing.T) string {
	t.Helper()
	root, _ := cachedGitFixture(t, "polecat-stop", func(dir string) (struct{}, error) {
		buildPolecatStopTestRepo(t, dir)
		return struct{}{}, nil
	})
	return filepath.Join(root, "repo")
}

// buildPolecatStopTestRepo makes initPolecatStopTestRepo's repo at tmp/repo,
// with its origin at tmp/origin.git.
func buildPolecatStopTestRepo(t *testing.T, tmp string) {
	t.Helper()
	repo := filepath.Join(tmp, "repo")
	origin := filepath.Join(tmp, "origin.git")

	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runPolecatStopTestGit(t, repo, "init")
	runPolecatStopTestGit(t, repo, "branch", "-M", "main")
	runPolecatStopTestGit(t, repo, "config", "user.email", "test@test.com")
	runPolecatStopTestGit(t, repo, "config", "user.name", "Test User")
	writePolecatStopTestFile(t, repo, "README.md", "# Test\n")
	runPolecatStopTestGit(t, repo, "add", "README.md")
	runPolecatStopTestGit(t, repo, "commit", "-m", "initial")

	runPolecatStopTestGit(t, tmp, "init", "--bare", origin)
	runPolecatStopTestGit(t, repo, "remote", "add", "origin", origin)
	runPolecatStopTestGit(t, repo, "push", "-u", "origin", "main")
	runPolecatStopTestGit(t, repo, "checkout", "-b", polecatStopTestBranch)

}

func writePolecatStopTestFile(t *testing.T, repo, rel, contents string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func runPolecatStopTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	fullArgs := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
