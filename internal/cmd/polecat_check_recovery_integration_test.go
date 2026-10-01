//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegrationHasSubmittableWorkForRecovery runs the recovery MQ verdict
// against real repositories: whether a branch's work is already on its upstream
// or an explicit target (ancestor, cherry-pick, squash merge) is git's patch
// equivalence. The unit tests cover hasSubmittableWork's decision over a fake.
func TestIntegrationHasSubmittableWorkForRecovery(t *testing.T) {
	t.Parallel()
	t.Run("UsesUpstream", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)

		if got := hasSubmittableWorkForRecovery(repo, nil, &GitState{UnpushedCommits: 99}, nil); got {
			t.Fatal("branch with no commits ahead of its upstream should not require MQ submission")
		}

		writeRecoveryFile(t, filepath.Join(repo, "change.txt"), "change")
		runGit(t, repo, "add", "change.txt")
		runGit(t, repo, "commit", "-m", "change")

		if got := hasSubmittableWorkForRecovery(repo, nil, &GitState{}, nil); !got {
			t.Fatal("branch with commits ahead of its upstream should require MQ submission")
		}
	})

	t.Run("IgnoresSelfUpstream", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)
		runGit(t, repo, "switch", "-c", "polecat/test")
		writeRecoveryFile(t, filepath.Join(repo, "feature.txt"), "feature")
		runGit(t, repo, "add", "feature.txt")
		runGit(t, repo, "commit", "-m", "feature")
		runGit(t, repo, "push", "-u", "origin", "polecat/test")

		if got := hasSubmittableWorkForRecovery(repo, nil, &GitState{UnpushedCommits: 1}, nil); !got {
			t.Fatal("self-upstream feature branch should fall back and preserve MQ requirement")
		}
	})

	t.Run("IgnoresPatchEquivalentBranch", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)
		runGit(t, repo, "switch", "-c", "polecat/equivalent")
		writeRecoveryFile(t, filepath.Join(repo, "equiv.txt"), "equiv")
		runGit(t, repo, "add", "equiv.txt")
		runGit(t, repo, "commit", "-m", "equiv")
		runGit(t, repo, "switch", "integration/test")
		writeRecoveryFile(t, filepath.Join(repo, "other.txt"), "other")
		runGit(t, repo, "add", "other.txt")
		runGit(t, repo, "commit", "-m", "other")
		runGit(t, repo, "cherry-pick", "polecat/equivalent")
		runGit(t, repo, "push", "origin", "integration/test")
		runGit(t, repo, "switch", "polecat/equivalent")
		runGit(t, repo, "branch", "--set-upstream-to=origin/integration/test")

		if got := hasSubmittableWorkForRecovery(repo, nil, &GitState{UnpushedCommits: 99}, nil); got {
			t.Fatal("patch-equivalent branch should not require MQ submission")
		}
	})

	t.Run("UsesExplicitTargetAncestor", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)
		runGit(t, repo, "switch", "-c", "polecat/contained")
		writeRecoveryFile(t, filepath.Join(repo, "contained.txt"), "contained")
		runGit(t, repo, "add", "contained.txt")
		runGit(t, repo, "commit", "-m", "contained")
		runGit(t, repo, "switch", "integration/test")
		runGit(t, repo, "merge", "--ff-only", "polecat/contained")
		runGit(t, repo, "push", "origin", "integration/test")
		runGit(t, repo, "switch", "polecat/contained")

		if got := hasSubmittableWorkForRecovery(repo, []string{"integration/test"}, &GitState{UnpushedCommits: 99}, nil); got {
			t.Fatal("branch whose HEAD is contained by explicit target should not require MQ submission")
		}
	})

	t.Run("UsesExplicitTargetCherry", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)
		runGit(t, repo, "switch", "-c", "polecat/cherry")
		writeRecoveryFile(t, filepath.Join(repo, "cherry.txt"), "cherry")
		runGit(t, repo, "add", "cherry.txt")
		runGit(t, repo, "commit", "-m", "cherry")
		runGit(t, repo, "switch", "integration/test")
		writeRecoveryFile(t, filepath.Join(repo, "target.txt"), "target")
		runGit(t, repo, "add", "target.txt")
		runGit(t, repo, "commit", "-m", "advance target")
		runGit(t, repo, "cherry-pick", "polecat/cherry")
		runGit(t, repo, "push", "origin", "integration/test")
		runGit(t, repo, "switch", "polecat/cherry")

		if got := hasSubmittableWorkForRecovery(repo, []string{"integration/test"}, &GitState{UnpushedCommits: 99}, nil); got {
			t.Fatal("patch-equivalent branch on advanced explicit target should not require MQ submission")
		}
	})

	t.Run("UsesExplicitTargetSquashNoop", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)
		if err := exec.Command("git", "-C", repo, "merge-tree", "--write-tree", "HEAD", "HEAD").Run(); err != nil {
			t.Fatalf("git merge-tree --write-tree unsupported: %v", err)
		}
		runGit(t, repo, "switch", "-c", "polecat/squash")
		writeRecoveryFile(t, filepath.Join(repo, "squash.txt"), "one\n")
		runGit(t, repo, "add", "squash.txt")
		runGit(t, repo, "commit", "-m", "checkpoint one")
		writeRecoveryFile(t, filepath.Join(repo, "squash.txt"), "one\ntwo\n")
		runGit(t, repo, "add", "squash.txt")
		runGit(t, repo, "commit", "-m", "checkpoint two")

		runGit(t, repo, "switch", "integration/test")
		runGit(t, repo, "merge", "--squash", "polecat/squash")
		runGit(t, repo, "commit", "-m", "squash polecat work")
		writeRecoveryFile(t, filepath.Join(repo, "target.txt"), "target advanced\n")
		runGit(t, repo, "add", "target.txt")
		runGit(t, repo, "commit", "-m", "advance target")
		runGit(t, repo, "push", "origin", "integration/test")
		runGit(t, repo, "switch", "polecat/squash")

		if got := hasSubmittableWorkForRecovery(repo, []string{"integration/test"}, &GitState{UnpushedCommits: 99}, nil); got {
			t.Fatal("squash-preserved branch on advanced explicit target should not require MQ submission")
		}
	})

	t.Run("KeepsExplicitTargetUniquePatch", func(t *testing.T) {
		t.Parallel()
		repo := setupRecoveryGitRepo(t)
		runGit(t, repo, "switch", "-c", "polecat/unique")
		writeRecoveryFile(t, filepath.Join(repo, "unique.txt"), "unique")
		runGit(t, repo, "add", "unique.txt")
		runGit(t, repo, "commit", "-m", "unique")

		if got := hasSubmittableWorkForRecovery(repo, []string{"integration/test"}, &GitState{}, nil); !got {
			t.Fatal("unique patch absent from explicit target should require MQ submission")
		}
	})
}

func setupRecoveryGitRepo(t *testing.T) string {
	t.Helper()
	return cachedGitFixtureStrings(t, "setupRecoveryGitRepo", func(root string) []string {
		return []string{buildSetupRecoveryGitRepo(t, root)}
	})[0]
}

// buildSetupRecoveryGitRepo makes setupRecoveryGitRepo's repos under root.
func buildSetupRecoveryGitRepo(t *testing.T, root string) string {
	t.Helper()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	writeRecoveryFile(t, filepath.Join(repo, "README.md"), "base")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	runGit(t, repo, "switch", "-c", "integration/test")
	runGit(t, repo, "push", "-u", "origin", "integration/test")
	return repo
}

func writeRecoveryFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
