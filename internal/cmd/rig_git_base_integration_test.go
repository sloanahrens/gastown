//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// The rig git base against real git: a rig's shared .repo.git is the repo the
// cmd package runs branch and worktree operations in, and a mayor/rig clone is
// never one. The refusal rows are unit-tested in rig_git_base_test.go.

// seedRigBareRepo builds a rig's shared bare repo under rigPath, with one
// commit on main, the shape a real .repo.git has.
func seedRigBareRepo(t *testing.T, rigPath string) string {
	t.Helper()
	seed := t.TempDir()
	runGit(t, seed, "init", "--initial-branch", "main")
	runGit(t, seed, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "--allow-empty", "-m", "initial")
	bare := filepath.Join(rigPath, ".repo.git")
	runGit(t, rigPath, "clone", "--bare", seed, bare)
	return bare
}

// seedMayorClone builds the mayor/rig clone a rig without .repo.git keeps,
// with one commit on main.
func seedMayorClone(t *testing.T, rigPath string) string {
	t.Helper()
	seed := t.TempDir()
	runGit(t, seed, "init", "--initial-branch", "main")
	runGit(t, seed, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "--allow-empty", "-m", "initial")
	clone := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, rigPath, "clone", seed, clone)
	return clone
}

// TestIntegrationStartCleanupDeletesThePolecatBranchFromTheBareRepo pins where
// gt shutdown deletes a polecat branch: from the rig's shared bare repo.
func TestIntegrationStartCleanupDeletesThePolecatBranchFromTheBareRepo(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	bare := seedRigBareRepo(t, rigPath)
	const branch = "polecat/max"
	runGit(t, bare, "branch", branch, "main")

	if err := deleteRigPolecatBranch(rigPath, branch); err != nil {
		t.Fatalf("deleteRigPolecatBranch = %v, want nil", err)
	}
	exists, err := git.NewGitWithDir(bare, "").BranchExists(branch)
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Errorf("branch %s survived in the bare repo", branch)
	}
}

// TestIntegrationCrewRemoveRunsTheWorktreeRemoveFromTheBareRepo pins that a
// crew worktree is removed through the rig's bare repo, which is where git
// registers it.
func TestIntegrationCrewRemoveRunsTheWorktreeRemoveFromTheBareRepo(t *testing.T) {
	t.Parallel()
	rigPath, crewPath, _ := crewRemoveTown(t)
	bare := seedRigBareRepo(t, rigPath)
	runGit(t, bare, "worktree", "add", crewPath, "-b", "crew-max", "main")

	var warnings []string
	revoker := &crewRevoker{}
	if err := runCrewRemoveWith([]string{"max"}, "gastown", true, false, crewRemoveEnvFor(rigPath, revoker, &warnings)); err != nil {
		t.Fatalf("crew remove = %v, want nil", err)
	}
	if _, err := os.Stat(crewPath); !os.IsNotExist(err) {
		t.Errorf("crew worktree stat = %v, want it removed", err)
	}
}

// TestIntegrationCrewRemoveRefusesAWorktreeOfAMayorCloneOnlyRig pins that the
// worktree is not removed through a mayor/rig clone: a rig with no .repo.git
// fails the removal and keeps the workspace.
func TestIntegrationCrewRemoveRefusesAWorktreeOfAMayorCloneOnlyRig(t *testing.T) {
	t.Parallel()
	rigPath, crewPath, _ := crewRemoveTown(t)
	clone := seedMayorClone(t, rigPath)
	runGit(t, clone, "worktree", "add", crewPath, "-b", "crew-max", "main")

	var warnings []string
	revoker := &crewRevoker{}
	err := runCrewRemoveWith([]string{"max"}, "gastown", true, false, crewRemoveEnvFor(rigPath, revoker, &warnings))
	if err == nil {
		t.Fatal("crew remove = nil, want the removal refused")
	}
	if _, statErr := os.Stat(crewPath); statErr != nil {
		t.Errorf("crew worktree stat = %v, want it kept", statErr)
	}
}
