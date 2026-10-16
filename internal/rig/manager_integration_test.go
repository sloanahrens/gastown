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

// A mayor clone git refuses leaves a rig, not a rolled-back directory: the
// rig's repository is the shared bare .repo.git, and git agrees that is a
// repository without a working tree (gt-jmqc5). The fork and the upstream the
// clone would have carried are on .repo.git, where AddRig configures them
// before it makes any clone.
func TestIntegrationAddRigWithoutAMayorClone(t *testing.T) {
	m, root, _ := realGitManager(t)
	m.git = mayorCloneRefused{m.git}
	source := sourceRepo(t, "source")
	fork := sourceRepo(t, "fork")
	upstream := sourceRepo(t, "upstream")

	if _, err := m.AddRig(AddRigOptions{
		Name:          "cloneless",
		GitURL:        source,
		PushURL:       fork,
		UpstreamURL:   upstream,
		BeadsPrefix:   "cl",
		SkipDoltCheck: true,
	}); err != nil {
		t.Fatalf("AddRig with a refused mayor clone: %v", err)
	}

	rigPath := filepath.Join(root, "cloneless")
	bare := filepath.Join(rigPath, ".repo.git")
	if got := gitT(t, root, "--git-dir", bare, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("bare HEAD = %q, want main", got)
	}
	gitT(t, root, "--git-dir", bare, "rev-parse", "--verify", "refs/heads/main")
	if _, err := os.Stat(filepath.Join(rigPath, "mayor", "rig", ".git")); !os.IsNotExist(err) {
		t.Errorf("mayor clone = %v, want the rig to have none", err)
	}
	if got := gitT(t, root, "--git-dir", bare, "remote", "get-url", "--push", "origin"); got != fork {
		t.Errorf("bare origin push URL = %q, want the fork %q", got, fork)
	}
	if got := gitT(t, root, "--git-dir", bare, "remote", "get-url", "upstream"); got != upstream {
		t.Errorf("bare upstream = %q, want %q", got, upstream)
	}
}

// The mayor clone is not optional for a repo that tracks .beads/: the rig
// adopts that database through the clone's working tree, so a clone real git
// refuses fails the add instead of leaving a rig that runs its own beside it
// (gt-jmqc5).
func TestIntegrationAddRigRefusesATrackedBeadsRepoWithoutAMayorClone(t *testing.T) {
	m, root, _ := realGitManager(t)
	m.git = mayorCloneRefused{m.git}
	source := sourceRepo(t, "tracked-source")
	commitFiles(t, source, map[string]string{
		".beads/config.yaml":   "prefix: tb\n",
		".beads/metadata.json": `{"database":"beads_tb"}`,
	})

	_, err := m.AddRig(AddRigOptions{
		Name:          "tracked",
		GitURL:        source,
		BeadsPrefix:   "tb",
		SkipDoltCheck: true,
	})
	if err == nil {
		t.Fatal("AddRig succeeded, want a failure: the tracked .beads/ is read through the clone")
	}
	if !strings.Contains(err.Error(), ".beads/") {
		t.Errorf("error = %q, want it to name .beads/", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tracked")); !os.IsNotExist(err) {
		t.Errorf("rig directory = %v, want the failed add rolled back", err)
	}
}

// commitFiles writes files under dir (paths may name directories) and commits
// them on the checked-out branch.
func commitFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "tracked beads")
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
