//go:build integration

package bdskew

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realGitRun is the Run seam backed by the real git binary, started in the
// directory it is given. GIT_ variables and the global and system config are
// dropped, so the temporary repository is the only input git reads.
func realGitRun(t *testing.T) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	env := []string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=bdskew", "GIT_AUTHOR_EMAIL=bdskew@example.invalid",
		"GIT_COMMITTER_NAME=bdskew", "GIT_COMMITTER_EMAIL=bdskew@example.invalid",
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd.Output()
	}
}

// TestIntegrationLevelAtCommitReadsARealGitTree: a commit of a real repository
// holding bd's migrations and its three non-migration names reads level 67
// through GitLister and LevelAtCommit.
func TestIntegrationLevelAtCommitReadsARealGitTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := t.TempDir()
	run := realGitRun(t)
	gitCall := func(args ...string) string {
		t.Helper()
		out, err := run(ctx, repo, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	gitCall("init", "-q")
	migrations := filepath.Join(repo, MigrationsDir)
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatalf("create %s: %v", migrations, err)
	}
	for _, name := range []string{
		"0066_add_events_journal_actor.up.sql",
		"0066_add_events_journal_actor.down.sql",
		"0067_b.up.sql",
		"0067_b.down.sql",
		"README.md",
		"nondeterminism-allowlist.txt",
	} {
		if err := os.WriteFile(filepath.Join(migrations, name), []byte("-- test\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(migrations, "ignored"), 0o755); err != nil {
		t.Fatalf("create ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "ignored", "keep"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write ignored/keep: %v", err)
	}
	gitCall("add", "-A")
	gitCall("commit", "-q", "-m", "migrations")
	commit := gitCall("rev-parse", "HEAD")

	l := GitLister{Repo: repo, Run: run}
	got, err := LevelAtCommit(ctx, l, commit)
	if err != nil {
		t.Fatalf("LevelAtCommit(%s) returned error %v", commit, err)
	}
	if got != 67 {
		t.Errorf("LevelAtCommit(%s) = %d, want 67", commit, got)
	}
}
