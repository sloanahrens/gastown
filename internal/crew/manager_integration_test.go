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

	mgr := NewManager(r, git.NewGit(rigPath))

	worker, err := mgr.Add("dave", false)
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	alternates := filepath.Join(worker.ClonePath, ".git", "objects", "info", "alternates")
	if _, err := os.Stat(alternates); err != nil {
		t.Fatalf("expected alternates file: %v", err)
	}
}

// Helper to run commands
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}
