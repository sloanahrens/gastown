//go:build integration

package cmd

// Git helpers the integration tier shares. The unit tier runs no git
// (internal/testpolicy realgit.txt), so they live behind the tag.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	// An empty dir runs git in the test process's own cwd — the package
	// directory inside the real repo — so a stray test would create branches and
	// objects in the rig's shared ref store.
	if dir == "" {
		t.Fatal("testRunGit: empty dir would run git in the test process cwd")
	}
	fullArgs := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func setupGitStateRemoteRepo(t *testing.T) string {
	t.Helper()
	return cachedGitFixtureStrings(t, "setupGitStateRemoteRepo", func(dir string) []string {
		return []string{buildSetupGitStateRemoteRepo(t, dir)}
	})[0]
}

// buildSetupGitStateRemoteRepo makes setupGitStateRemoteRepo's repos under dir.
func buildSetupGitStateRemoteRepo(t *testing.T, dir string) string {
	t.Helper()
	remote := filepath.Join(dir, "remote.git")
	repo := filepath.Join(dir, "repo")
	runGitCmd(t, "", "init", "--bare", remote)
	runGitCmd(t, "", "init", repo)
	runGitCmd(t, repo, "config", "user.email", "test@example.com")
	runGitCmd(t, repo, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(repo, "README.md"), "base\n")
	runGitCmd(t, repo, "add", "README.md")
	runGitCmd(t, repo, "commit", "-m", "base")
	runGitCmd(t, repo, "branch", "-M", "main")
	runGitCmd(t, repo, "remote", "add", "origin", remote)
	runGitCmd(t, repo, "push", "-u", "origin", "main")
	runGitCmd(t, repo, "switch", "-c", "integration/test")
	runGitCmd(t, repo, "push", "-u", "origin", "integration/test")
	return repo
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func writeMQSubmitTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGitForMQSubmitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initOrphanCleanupRepo builds a clone whose origin carries a polecat branch,
// optionally squash-merged into main, so the orphan path runs against a real
// remote rather than a fake. (gt-qjp2)
func initOrphanCleanupRepo(t *testing.T, squashMerge bool) (clone, branch string) {
	t.Helper()
	root, f := cachedGitFixture(t, fmt.Sprintf("orphan-cleanup squash=%v", squashMerge), func(dir string) ([2]string, error) {
		clone, branch := buildOrphanCleanupRepo(t, dir, squashMerge)
		rel, err := filepath.Rel(dir, clone)
		return [2]string{rel, branch}, err
	})
	return filepath.Join(root, f[0]), f[1]
}

// buildOrphanCleanupRepo makes initOrphanCleanupRepo's repos under tmp.
func buildOrphanCleanupRepo(t *testing.T, tmp string, squashMerge bool) (clone, branch string) {
	t.Helper()
	originPath := filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone = filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "README.md", "seed\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	branch = testOrphanBranch
	runOrphanCleanupGit(t, clone, "checkout", "-b", branch)
	writeOrphanCleanupFile(t, clone, "work.txt", "polecat work\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "polecat work")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", branch)

	if !squashMerge {
		return clone, branch
	}
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--squash", branch)
	runOrphanCleanupGit(t, clone, "commit", "-m", "merge squash")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	return clone, branch
}

func runOrphanCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

const testOrphanBranch = "polecat/test/gt-orphan"

func writeOrphanCleanupFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
