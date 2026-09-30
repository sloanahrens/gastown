//go:build integration

package rig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// The unit tier adds rigs through gitfake. These add them through real git:
// the bare clone, the mayor clone, and what git makes
// of remotes gitfake does not model. bd stays in process.

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// realGitManager is testManager with real git.
func realGitManager(t *testing.T) (*Manager, string, *config.RigsConfig) {
	t.Helper()
	root, rigsConfig := setupTestTown(t)
	m, _, _ := testManager(root, rigsConfig)
	m.git, m.openRepo = git.NewGit(root), nil
	return m, root, rigsConfig
}

// sourceRepo is a repository with a commit on main and one more on develop,
// HEAD on main.
func sourceRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitT(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "Initial commit")
	gitT(t, dir, "checkout", "-q", "-b", "develop")
	if err := os.WriteFile(filepath.Join(dir, "develop.txt"), []byte("develop branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "develop commit")
	gitT(t, dir, "checkout", "-q", "main")
	return dir
}

func TestIntegrationAddRig(t *testing.T) {
	m, root, _ := realGitManager(t)
	source := sourceRepo(t, "source")
	upstream := sourceRepo(t, "upstream")

	if _, err := m.AddRig(AddRigOptions{
		Name:          "testrig",
		GitURL:        source,
		UpstreamURL:   upstream,
		BeadsPrefix:   "tr",
		DefaultBranch: "develop",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig: %v", err)
	}
	rigPath := filepath.Join(root, "testrig")
	bare := filepath.Join(rigPath, ".repo.git")
	mayor := filepath.Join(rigPath, "mayor", "rig")

	if got := gitT(t, root, "--git-dir", bare, "symbolic-ref", "--short", "HEAD"); got != "develop" {
		t.Errorf("bare HEAD = %q, want develop", got)
	}
	if got := gitT(t, mayor, "rev-parse", "--abbrev-ref", "HEAD"); got != "develop" {
		t.Errorf("mayor clone on %q, want develop", got)
	}
	gitT(t, root, "--git-dir", bare, "rev-parse", "--verify", "refs/remotes/origin/develop")
	for _, dir := range []string{bare, filepath.Join(mayor, ".git")} {
		if got := gitT(t, root, "--git-dir", dir, "remote", "get-url", "upstream"); got != upstream {
			t.Errorf("%s upstream = %q, want %q", dir, got, upstream)
		}
	}

	// gt-ylpg: .beads/ in the mayor clone (credential key, backups, locks)
	// is ignored, not one git clean -fd from deletion.
	if err := os.MkdirAll(filepath.Join(mayor, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayor, ".beads", ".beads-credential-key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(gitT(t, mayor, "status", "--porcelain", "--ignored", "--", ".beads"), "\n") {
		if strings.HasPrefix(line, "??") {
			t.Errorf("mayor clone .beads/ is untracked and unignored: %q", line)
		}
	}
}

// A remote with refs but no branch its HEAD names clones to nothing usable.
// What git's clone leaves behind for such remotes is what AddRig's diagnosis
// reads, so it is checked against git itself.
func TestIntegrationAddRigDiagnosesRemotesWithoutADefaultBranch(t *testing.T) {
	t.Run("HEAD names a missing branch", func(t *testing.T) {
		m, _, _ := realGitManager(t)
		tmp := t.TempDir()
		remote := filepath.Join(tmp, "remote.git")
		work := filepath.Join(tmp, "work")
		gitT(t, tmp, "init", "-q", "--bare", "-b", "main", remote)
		gitT(t, tmp, "clone", "-q", remote, work)
		gitT(t, work, "commit", "-q", "--allow-empty", "-m", "init")
		gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
		gitT(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/missing")

		_, err := m.AddRig(AddRigOptions{Name: "badheadrepo", GitURL: remote, BeadsPrefix: "bhr", SkipDoltCheck: true})
		if err == nil {
			t.Fatal("AddRig succeeded, want bad remote HEAD error")
		}
		if strings.Contains(err.Error(), "is empty") {
			t.Fatalf("AddRig reported non-empty bad-HEAD repo as empty: %q", err)
		}
		if !strings.Contains(err.Error(), "has refs, but no default branch could be cloned") {
			t.Fatalf("AddRig error = %q, want bad remote HEAD diagnostic", err)
		}
	})

	t.Run("only tags", func(t *testing.T) {
		m, _, _ := realGitManager(t)
		repo := filepath.Join(t.TempDir(), "tag-only")
		gitT(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
		gitT(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
		gitT(t, repo, "tag", "v1")
		gitT(t, repo, "update-ref", "-d", "refs/heads/main")

		_, err := m.AddRig(AddRigOptions{Name: "tagonlyrepo", GitURL: repo, BeadsPrefix: "tor", SkipDoltCheck: true})
		if err == nil {
			t.Fatal("AddRig succeeded, want no branch error")
		}
		if strings.Contains(err.Error(), "is empty") {
			t.Fatalf("AddRig reported tag-only repo as empty: %q", err)
		}
	})
}
