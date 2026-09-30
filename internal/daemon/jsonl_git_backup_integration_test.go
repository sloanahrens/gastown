//go:build integration

package daemon

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realBackupRepo makes dir a git repository with one commit, as a backup
// repository that has run before.
func realBackupRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// TestIntegrationCommitAndPushJsonlBackup_RecoversFromStaleIndexLock runs
// gt-1aj2 end to end on real git: the tick after a killed `git add` used to
// fail with "index.lock: File exists" forever, and three such ticks
// escalated.
func TestIntegrationCommitAndPushJsonlBackup_RecoversFromStaleIndexLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitRepo := filepath.Join(root, "repo")
	if err := os.MkdirAll(gitRepo, 0755); err != nil {
		t.Fatal(err)
	}
	realBackupRepo(t, gitRepo)

	remote := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", gitRepo, "remote", "add", "origin", remote).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	dbDir := filepath.Join(gitRepo, "testdb")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 5)
	lockPath := ageIndexLock(t, gitRepo, 2*gitIndexLockGracePeriod)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	if err := d.commitAndPushJsonlBackup(gitRepo, []string{"testdb"}, map[string]int{"testdb": 5}, nil); err != nil {
		t.Fatalf("tick after a stale index lock should succeed, got: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("stale lock should have been cleared, stat err = %v", err)
	}

	out, err := exec.Command("git", "-C", remote, "log", "--all", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log on remote: %v", err)
	}
	if !strings.Contains(string(out), "backup") {
		t.Errorf("expected the backup commit to reach the remote, got: %q", out)
	}
}

// TestIntegrationRunGitIndexOp_RealGitRefusesAFreshLock pins what the unit
// tests script: real git refuses a held index lock with an error
// isIndexLockExistsError recognizes, and the fresh lock survives.
func TestIntegrationRunGitIndexOp_RealGitRefusesAFreshLock(t *testing.T) {
	t.Parallel()
	gitRepo := t.TempDir()
	realBackupRepo(t, gitRepo)
	lockPath := ageIndexLock(t, gitRepo, 0)

	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	err := d.runGitIndexOp(gitRepo, "git add", func() error {
		return d.backupGitAt(gitRepo, 30*time.Second).Add("-A", ".")
	})
	if err == nil || !isIndexLockExistsError(err) {
		t.Fatalf("add under a live lock = %v; want an index.lock error", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("a live owner's lock must not be removed: %v", statErr)
	}
}
