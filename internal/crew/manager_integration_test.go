//go:build integration

package crew

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestIntegrationManagerAddUsesLocalRepoReference proves on real git that a
// rig's local repo is passed as the clone's reference: only git writes the
// alternates file it leaves.
func TestIntegrationManagerAddUsesLocalRepoReference(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "crew-test-local-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	rigPath := filepath.Join(tmpDir, "test-rig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("failed to create rig dir: %v", err)
	}

	remoteRepoPath := filepath.Join(tmpDir, "remote.git")
	if err := runCmd("git", "init", "--bare", remoteRepoPath); err != nil {
		t.Fatalf("failed to create bare repo: %v", err)
	}

	localRepoPath := filepath.Join(tmpDir, "local-repo")
	if err := runCmd("git", "init", localRepoPath); err != nil {
		t.Fatalf("failed to init local repo: %v", err)
	}
	if err := runCmd("git", "-C", localRepoPath, "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("failed to configure email: %v", err)
	}
	if err := runCmd("git", "-C", localRepoPath, "config", "user.name", "Test"); err != nil {
		t.Fatalf("failed to configure name: %v", err)
	}
	if err := runCmd("git", "-C", localRepoPath, "remote", "add", "origin", remoteRepoPath); err != nil {
		t.Fatalf("failed to add origin: %v", err)
	}

	if err := os.WriteFile(filepath.Join(localRepoPath, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if err := runCmd("git", "-C", localRepoPath, "add", "."); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	if err := runCmd("git", "-C", localRepoPath, "commit", "-m", "initial"); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	r := &rig.Rig{
		Name:      "test-rig",
		Path:      rigPath,
		GitURL:    remoteRepoPath,
		LocalRepo: localRepoPath,
	}

	mgr := NewManager(r, git.NewGit(rigPath), nil)

	worker, err := mgr.Add("dave", false)
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	alternates := filepath.Join(worker.ClonePath, ".git", "objects", "info", "alternates")
	if _, err := os.Stat(alternates); err != nil {
		t.Fatalf("expected alternates file: %v", err)
	}
}

// A rig whose repository is its shared bare .repo.git, with no mayor/rig clone
// on disk, still provisions a crew clone carrying that repository's remotes
// (gt-jmqc5). The unit tier models the bare repo; this checks real git reads it.
func TestIntegrationManagerAddSyncsRemotesFromTheBareRepo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "crew-test-bare-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// The rig's fork (origin) and the upstream it was cloned from.
	origin := bareRepoWithCommit(t, filepath.Join(tmpDir, "fork"))
	upstream := bareRepoWithCommit(t, filepath.Join(tmpDir, "upstream"))

	rigPath := filepath.Join(tmpDir, "test-rig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("failed to create rig dir: %v", err)
	}
	// The rig's shared repository, holding a remote configuration the crew
	// clone is not cloned with: origin at the rig's fork, and an upstream
	// remote beside it. Both are what the sync copies.
	bare := filepath.Join(rigPath, ".repo.git")
	if err := runCmd("git", "clone", "-q", "--bare", upstream, bare); err != nil {
		t.Fatalf("failed to clone bare repo: %v", err)
	}
	if err := runCmd("git", "--git-dir", bare, "remote", "set-url", "origin", origin); err != nil {
		t.Fatalf("failed to set the bare repo's origin: %v", err)
	}
	if err := runCmd("git", "--git-dir", bare, "remote", "add", "upstream", upstream); err != nil {
		t.Fatalf("failed to add the bare repo's upstream remote: %v", err)
	}

	r := &rig.Rig{
		Name:    "test-rig",
		Path:    rigPath,
		GitURL:  upstream,
		PushURL: origin,
	}
	mgr := NewManager(r, git.NewGit(rigPath), nil)

	worker, err := mgr.Add("bareless", false)
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	crewGit := git.NewGit(worker.ClonePath)
	if got, err := crewGit.RemoteURL("origin"); err != nil || got != origin {
		t.Errorf("crew origin = %q, %v; want the rig's fork %q", got, err, origin)
	}
	if got, err := crewGit.RemoteURL("upstream"); err != nil || got != upstream {
		t.Errorf("crew upstream = %q, %v; want %q", got, err, upstream)
	}
	if got, err := crewGit.GetPushURL("origin"); err != nil || got != origin {
		t.Errorf("crew origin push URL = %q, %v; want %q", got, err, origin)
	}
}

// bareRepoWithCommit makes a bare repo at dir with one commit on main.
func bareRepoWithCommit(t *testing.T, dir string) string {
	t.Helper()
	work := dir + "-work"
	if err := runCmd("git", "init", "-q", "-b", "main", work); err != nil {
		t.Fatalf("failed to init %s: %v", work, err)
	}
	for _, kv := range [][2]string{{"user.email", "test@test.com"}, {"user.name", "Test"}} {
		if err := runCmd("git", "-C", work, "config", kv[0], kv[1]); err != nil {
			t.Fatalf("failed to configure %s: %v", kv[0], err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# "+filepath.Base(dir)+"\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if err := runCmd("git", "-C", work, "add", "."); err != nil {
		t.Fatalf("failed to stage: %v", err)
	}
	if err := runCmd("git", "-C", work, "commit", "-q", "-m", "initial"); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
	if err := runCmd("git", "init", "-q", "--bare", "-b", "main", dir); err != nil {
		t.Fatalf("failed to init bare repo %s: %v", dir, err)
	}
	if err := runCmd("git", "-C", work, "push", "-q", dir, "main"); err != nil {
		t.Fatalf("failed to push to %s: %v", dir, err)
	}
	return dir
}

// Helper to run commands
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}
