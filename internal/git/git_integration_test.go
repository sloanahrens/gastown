//go:build integration

package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo returns a repo (the returned dir itself) with a test identity
// and one commit adding README.md on main, copied from testRepoFixture.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testRepoFixture.copyInto(t, dir)
	return dir
}

func TestIntegrationCheckConflicts_NoConflict(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	mainBranch, _ := g.CurrentBranch()

	// Create feature branch with non-conflicting change
	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}

	// Add a new file (won't conflict with main)
	newFile := filepath.Join(dir, "feature.txt")
	if err := os.WriteFile(newFile, []byte("feature content"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("add feature file"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Go back to main
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}

	// Check for conflicts - should be none
	conflicts, err := g.CheckConflicts("feature", mainBranch)
	if err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if len(conflicts) > 0 {
		t.Errorf("expected no conflicts, got %v", conflicts)
	}

	// Verify we're still on main and clean
	branch, _ := g.CurrentBranch()
	if branch != mainBranch {
		t.Errorf("branch = %q, want %q", branch, mainBranch)
	}
	status, _ := g.Status()
	if !status.Clean {
		t.Error("expected clean working directory after CheckConflicts")
	}
}

func TestIntegrationCheckConflicts_WithConflict(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	mainBranch, _ := g.CurrentBranch()

	// Create feature branch
	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}

	// Modify README.md on feature branch
	readmeFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Feature changes\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := g.Add("README.md"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("modify readme on feature"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Go back to main and make conflicting change
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := os.WriteFile(readmeFile, []byte("# Main changes\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := g.Add("README.md"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("modify readme on main"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Check for conflicts - should find README.md
	conflicts, err := g.CheckConflicts("feature", mainBranch)
	if err != nil {
		t.Fatalf("CheckConflicts: %v", err)
	}
	if len(conflicts) == 0 {
		t.Error("expected conflicts, got none")
	}

	foundReadme := false
	for _, f := range conflicts {
		if f == "README.md" {
			foundReadme = true
			break
		}
	}
	if !foundReadme {
		t.Errorf("expected README.md in conflicts, got %v", conflicts)
	}

	// Verify we're still on main and clean
	branch, _ := g.CurrentBranch()
	if branch != mainBranch {
		t.Errorf("branch = %q, want %q", branch, mainBranch)
	}
	status, _ := g.Status()
	if !status.Clean {
		t.Error("expected clean working directory after CheckConflicts")
	}
}

// TestCloneBareHasOriginRefs verifies that after CloneBare, origin/* refs
// are available for worktree creation. This was broken before the fix:
// bare clones had refspec configured but no fetch was run, so origin/main
// didn't exist and WorktreeAddFromRef("origin/main") failed.
//
// Related: GitHub issue #286
func TestIntegrationCloneBareHasOriginRefs(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	// Create a "remote" repo with a commit on main
	remoteDir := filepath.Join(tmp, "remote")
	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = remoteDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create initial commit
	readmeFile := filepath.Join(remoteDir, "README.md")
	if err := os.WriteFile(readmeFile, []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cmd = exec.Command("git", "add", ".")
	cmd.Dir = remoteDir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = remoteDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	// Get the main branch name (main or master depending on git version)
	cmd = exec.Command("git", "branch", "--show-current")
	cmd.Dir = remoteDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --show-current: %v", err)
	}
	mainBranch := strings.TrimSpace(string(out))

	// Clone as bare repo using our CloneBare function
	bareDir := filepath.Join(tmp, "bare.git")
	g := NewGit(tmp)
	if err := g.CloneBare(remoteDir, bareDir); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	// Verify origin/main exists (this was the bug - it didn't exist before the fix)
	bareGit := NewGitWithDir(bareDir, "")
	cmd = exec.Command("git", "--git-dir", bareDir, "branch", "-r")
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git branch -r: %v", err)
	}

	originMain := "origin/" + mainBranch
	if !strings.Contains(string(out), originMain) {
		t.Errorf("expected %q in remote branches, got: %s", originMain, out)
	}

	// Verify WorktreeAddFromRef succeeds with origin/main
	// This is what polecat creation does
	worktreePath := filepath.Join(tmp, "worktree")
	if err := bareGit.WorktreeAddFromRef(worktreePath, "test-branch", originMain); err != nil {
		t.Errorf("WorktreeAddFromRef(%q) failed: %v", originMain, err)
	}

	// Verify the worktree was created and has the expected file
	worktreeReadme := filepath.Join(worktreePath, "README.md")
	if _, err := os.Stat(worktreeReadme); err != nil {
		t.Errorf("expected README.md in worktree: %v", err)
	}
}

// initTestRepoWithRemote sets up a local repo with a bare remote and initial push.
// Returns (localDir, remoteDir, mainBranch). It is a copy of remoteFixture.
func initTestRepoWithRemote(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	remoteFixture.copyInto(t, tmp)
	return filepath.Join(tmp, "local"), filepath.Join(tmp, "remote.git"), fixtureMainBranch
}

func TestIntegrationPruneStaleBranches_MergedBranch(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	// Create a polecat branch, commit, and merge it to main
	if err := g.CreateBranch("polecat/test-merged"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/test-merged"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("feature"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("add feature"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Push polecat branch to origin
	cmd := exec.Command("git", "push", "origin", "polecat/test-merged")
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("push polecat branch: %v", err)
	}

	// Merge to main
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := g.Merge("polecat/test-merged"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Push main
	cmd = exec.Command("git", "push", "origin", mainBranch)
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("push main: %v", err)
	}

	// Delete remote polecat branch (simulating refinery cleanup)
	cmd = exec.Command("git", "push", "origin", "--delete", "polecat/test-merged")
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("delete remote branch: %v", err)
	}

	// Fetch --prune to remove remote tracking ref
	if err := g.FetchPrune("origin"); err != nil {
		t.Fatalf("FetchPrune: %v", err)
	}

	// Verify polecat branch still exists locally
	branches, err := g.ListBranches("polecat/*")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 1 {
		t.Fatalf("expected 1 local polecat branch, got %d", len(branches))
	}

	// Prune should remove it
	pruned, err := g.PruneStaleBranches("polecat/*", false)
	if err != nil {
		t.Fatalf("PruneStaleBranches: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("expected 1 pruned branch, got %d", len(pruned))
	}
	if pruned[0].Name != "polecat/test-merged" {
		t.Errorf("pruned name = %q, want polecat/test-merged", pruned[0].Name)
	}
	if pruned[0].Reason != "no-remote-merged" {
		t.Errorf("pruned reason = %q, want no-remote-merged", pruned[0].Reason)
	}

	// Verify branch is gone
	branches, err = g.ListBranches("polecat/*")
	if err != nil {
		t.Fatalf("ListBranches after prune: %v", err)
	}
	if len(branches) != 0 {
		t.Errorf("expected 0 branches after prune, got %d: %v", len(branches), branches)
	}
}

func TestIntegrationPruneStaleBranches_DryRun(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	// Create and merge a polecat branch (same as above)
	if err := g.CreateBranch("polecat/test-dryrun"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/test-dryrun"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "dry.txt"), []byte("dry"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("dry.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("dry run test"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := g.Merge("polecat/test-dryrun"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Push main to update origin/main
	cmd := exec.Command("git", "push", "origin", mainBranch)
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("push main: %v", err)
	}

	// Dry run should report but not delete
	pruned, err := g.PruneStaleBranches("polecat/*", true)
	if err != nil {
		t.Fatalf("PruneStaleBranches dry-run: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("expected 1 branch in dry-run, got %d", len(pruned))
	}

	// Branch should still exist
	branches, err := g.ListBranches("polecat/*")
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 1 {
		t.Errorf("expected branch to still exist after dry-run, got %d branches", len(branches))
	}
}

func TestIntegrationPruneStaleBranches_SkipsCurrentBranch(t *testing.T) {
	t.Parallel()
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	// Create and checkout a polecat branch (making it the current branch)
	if err := g.CreateBranch("polecat/current"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/current"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// Prune should not delete the current branch
	pruned, err := g.PruneStaleBranches("polecat/*", false)
	if err != nil {
		t.Fatalf("PruneStaleBranches: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("expected 0 pruned (current branch should be skipped), got %d", len(pruned))
	}
}

func TestIntegrationPruneStaleBranches_SkipsUnmerged(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	// Create a polecat branch with a commit NOT merged to main
	if err := g.CreateBranch("polecat/unmerged"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/unmerged"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "unmerged.txt"), []byte("unmerged"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("unmerged.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("unmerged work"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Push to remote so it has a remote tracking branch
	cmd := exec.Command("git", "push", "origin", "polecat/unmerged")
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("push: %v", err)
	}

	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}

	// Prune should NOT delete unmerged branch that still has remote
	pruned, err := g.PruneStaleBranches("polecat/*", false)
	if err != nil {
		t.Fatalf("PruneStaleBranches: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("expected 0 pruned (unmerged with remote should be kept), got %d", len(pruned))
	}
}

// initTestRepoWithSubmodule creates a parent repo with a submodule for testing.
// Returns parentDir, submoduleRemoteDir (bare).
func initTestRepoWithSubmodule(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()

	// Create a "remote" bare repo for the submodule
	subRemote := filepath.Join(tmp, "sub-remote.git")
	runGit(t, tmp, "init", "--bare", "--initial-branch", "main", subRemote)

	// Create a working clone of the submodule to add content
	subWork := filepath.Join(tmp, "sub-work")
	runGit(t, tmp, "clone", subRemote, subWork)
	if err := os.WriteFile(filepath.Join(subWork, "lib.go"), []byte("package lib\n"), 0644); err != nil {
		t.Fatalf("write sub file: %v", err)
	}
	runGit(t, subWork, "add", ".")
	runGit(t, subWork, "commit", "-m", "initial sub commit")
	runGit(t, subWork, "push", "origin", "main")

	// Create the parent repo
	parent := filepath.Join(tmp, "parent")
	runGit(t, tmp, "init", "--initial-branch", "main", parent)
	if err := os.WriteFile(filepath.Join(parent, "README.md"), []byte("# Parent\n"), 0644); err != nil {
		t.Fatalf("write parent file: %v", err)
	}
	runGit(t, parent, "add", ".")
	runGit(t, parent, "commit", "-m", "initial parent commit")

	// Add the submodule
	runGit(t, parent, "submodule", "add", subRemote, "libs/sub")
	runGit(t, parent, "commit", "-m", "add submodule")

	return parent, subRemote
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	// Prepend -c protocol.file.allow=always to allow local file:// transport
	fullArgs := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func TestIntegrationInitSubmodules_WithSubmodules(t *testing.T) {
	t.Parallel()
	parent, _ := initTestRepoWithSubmodule(t)

	// The submodule should already be initialized from the test setup
	libFile := filepath.Join(parent, "libs", "sub", "lib.go")
	if _, err := os.Stat(libFile); err != nil {
		t.Fatalf("expected submodule file to exist after setup: %v", err)
	}

	// Now test that InitSubmodules works on a fresh clone
	tmp := t.TempDir()
	cloneDest := filepath.Join(tmp, "clone")
	// Clone without --recurse-submodules to simulate current behavior
	cmd := exec.Command("git", "-c", "protocol.file.allow=always", "clone", parent, cloneDest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	// Submodule dir exists but is empty
	subDir := filepath.Join(cloneDest, "libs", "sub")
	entries, _ := os.ReadDir(subDir)
	if len(entries) > 0 {
		t.Fatal("expected empty submodule dir before init")
	}

	// The exported wrapper adds no git config, so git's default refusal of
	// the file:// transport for submodule clones must stand. This guards
	// that InitSubmodules sends git no extra environment.
	err := InitSubmodules(cloneDest)
	if err == nil || !strings.Contains(err.Error(), "transport 'file' not allowed") {
		t.Fatalf("InitSubmodules with the default protocol policy = %v, want a refused file transport", err)
	}

	// initSubmodules with file:// allowed should populate it.
	allowFile := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.file.allow", "GIT_CONFIG_VALUE_0=always"}
	allowFileRun := func(c gitCall) (string, string, error) {
		c.env = append(c.env, allowFile...)
		return realRun(c)
	}
	if err := initSubmodules(allowFileRun, cloneDest); err != nil {
		t.Fatalf("InitSubmodules: %v", err)
	}

	libFile = filepath.Join(cloneDest, "libs", "sub", "lib.go")
	if _, err := os.Stat(libFile); err != nil {
		t.Fatalf("expected submodule file after InitSubmodules: %v", err)
	}
}

func TestIntegrationSubmoduleChanges(t *testing.T) {
	t.Parallel()
	parent, subRemote := initTestRepoWithSubmodule(t)

	// Create a branch with a submodule change
	runGit(t, parent, "checkout", "-b", "feature")

	// Make a new commit in the submodule
	subPath := filepath.Join(parent, "libs", "sub")
	if err := os.WriteFile(filepath.Join(subPath, "new.go"), []byte("package lib\n// new\n"), 0644); err != nil {
		t.Fatalf("write new sub file: %v", err)
	}
	runGit(t, subPath, "add", ".")
	runGit(t, subPath, "commit", "-m", "new sub commit")
	runGit(t, subPath, "push", "origin", "HEAD:main")

	// Update the parent's submodule pointer
	runGit(t, parent, "add", "libs/sub")
	runGit(t, parent, "commit", "-m", "update submodule pointer")

	// Now check for submodule changes between main and feature
	g := NewGit(parent)
	changes, err := g.SubmoduleChanges("main", "feature")
	if err != nil {
		t.Fatalf("SubmoduleChanges: %v", err)
	}

	if len(changes) != 1 {
		t.Fatalf("expected 1 submodule change, got %d", len(changes))
	}

	sc := changes[0]
	if sc.Path != "libs/sub" {
		t.Errorf("expected path libs/sub, got %s", sc.Path)
	}
	if sc.OldSHA == "" {
		t.Error("expected non-empty OldSHA")
	}
	if sc.NewSHA == "" {
		t.Error("expected non-empty NewSHA")
	}
	if sc.OldSHA == sc.NewSHA {
		t.Error("expected different SHAs")
	}
	if sc.URL != subRemote {
		t.Errorf("expected URL %s, got %s", subRemote, sc.URL)
	}
}

func TestIntegrationPushSubmoduleCommit(t *testing.T) {
	t.Parallel()
	parent, subRemote := initTestRepoWithSubmodule(t)

	// Make a new commit in the submodule (but don't push it)
	subPath := filepath.Join(parent, "libs", "sub")
	if err := os.WriteFile(filepath.Join(subPath, "pushed.go"), []byte("package lib\n// pushed\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, subPath, "add", ".")
	runGit(t, subPath, "commit", "-m", "unpushed commit")

	// Get the SHA of the new commit
	cmd := exec.Command("git", "-C", subPath, "rev-parse", "HEAD")
	shaBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	sha := strings.TrimSpace(string(shaBytes))

	// Verify it's not on the remote yet
	lsCmd := exec.Command("git", "ls-remote", subRemote, "refs/heads/main")
	lsOut, _ := lsCmd.Output()
	remoteSHA := strings.Fields(string(lsOut))[0]
	if remoteSHA == sha {
		t.Fatal("commit should not be on remote yet")
	}

	// Push it using PushSubmoduleCommit
	g := NewGit(parent)
	if err := g.PushSubmoduleCommit("libs/sub", sha, "origin"); err != nil {
		t.Fatalf("PushSubmoduleCommit: %v", err)
	}

	// Verify it's now on the remote
	lsCmd = exec.Command("git", "ls-remote", subRemote, "refs/heads/main")
	lsOut, _ = lsCmd.Output()
	remoteSHA = strings.Fields(string(lsOut))[0]
	if remoteSHA != sha {
		t.Errorf("expected remote main to be %s, got %s", sha, remoteSHA)
	}
}

func TestIntegrationCheckUncommittedWorkCapturesPorcelainRenameAndUnmergedPaths(t *testing.T) {
	t.Parallel()
	t.Run("rename to real path blocks", func(t *testing.T) {
		t.Parallel()
		dir := initTestRepo(t)
		runGitTestCmd(t, dir, "mv", "README.md", "renamed.md")

		status, err := NewGit(dir).CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !status.HasUncommittedChanges {
			t.Fatal("rename should mark worktree dirty")
		}
		want := []string{"README.md", "renamed.md"}
		if got := status.NonRuntimePaths(); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("NonRuntimePaths = %v, want %v", got, want)
		}
		if status.CleanExcludingRuntime() {
			t.Fatal("rename to real path must block runtime-excluding clean check")
		}
	})

	t.Run("rename from real path to runtime path blocks", func(t *testing.T) {
		t.Parallel()
		dir := initTestRepo(t)
		if err := os.MkdirAll(filepath.Join(dir, ".opencode", "plugins"), 0755); err != nil {
			t.Fatalf("mkdir opencode plugins: %v", err)
		}
		runGitTestCmd(t, dir, "mv", "README.md", ".opencode/plugins/gastown.js")

		status, err := NewGit(dir).CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !status.HasUncommittedChanges {
			t.Fatal("runtime rename should still mark raw worktree dirty")
		}
		if got := status.NonRuntimePaths(); len(got) != 1 || got[0] != "README.md" {
			t.Fatalf("NonRuntimePaths = %v, want [README.md]", got)
		}
		if status.CleanExcludingRuntime() {
			t.Fatal("real source renamed to runtime destination must block runtime-excluding clean check")
		}
	})

	t.Run("rename from runtime path to runtime path is ignored by runtime filter", func(t *testing.T) {
		t.Parallel()
		dir := initTestRepo(t)
		if err := os.MkdirAll(filepath.Join(dir, ".opencode", "plugins"), 0755); err != nil {
			t.Fatalf("mkdir opencode plugins: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".opencode", "plugins", "old.js"), []byte("runtime\n"), 0644); err != nil {
			t.Fatalf("write runtime source: %v", err)
		}
		runGitTestCmd(t, dir, "add", ".opencode/plugins/old.js")
		runGitTestCmd(t, dir, "commit", "-m", "add tracked runtime source")
		runGitTestCmd(t, dir, "mv", ".opencode/plugins/old.js", ".opencode/plugins/gastown.js")

		status, err := NewGit(dir).CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !status.HasUncommittedChanges {
			t.Fatal("runtime rename should still mark raw worktree dirty")
		}
		if got := status.NonRuntimePaths(); len(got) != 0 {
			t.Fatalf("NonRuntimePaths = %v, want none for runtime-only rename", got)
		}
		if !status.CleanExcludingRuntime() {
			t.Fatal("runtime-only rename should be clean excluding runtime")
		}
	})

	t.Run("unmerged runtime conflict blocks", func(t *testing.T) {
		t.Parallel()
		dir := initTestRepo(t)
		runGitTestCmd(t, dir, "branch", "-M", "main")
		if err := os.MkdirAll(filepath.Join(dir, ".opencode", "plugins"), 0755); err != nil {
			t.Fatalf("mkdir opencode plugins: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".opencode", "plugins", "gastown.js"), []byte("base\n"), 0644); err != nil {
			t.Fatalf("write base runtime conflict file: %v", err)
		}
		runGitTestCmd(t, dir, "add", ".opencode/plugins/gastown.js")
		runGitTestCmd(t, dir, "commit", "-m", "add runtime conflict base")
		runGitTestCmd(t, dir, "switch", "-c", "side")
		if err := os.WriteFile(filepath.Join(dir, ".opencode", "plugins", "gastown.js"), []byte("side\n"), 0644); err != nil {
			t.Fatalf("write side runtime conflict file: %v", err)
		}
		runGitTestCmd(t, dir, "commit", "-am", "side runtime change")
		runGitTestCmd(t, dir, "switch", "main")
		if err := os.WriteFile(filepath.Join(dir, ".opencode", "plugins", "gastown.js"), []byte("main\n"), 0644); err != nil {
			t.Fatalf("write main runtime conflict file: %v", err)
		}
		runGitTestCmd(t, dir, "commit", "-am", "main runtime change")
		runGitTestCmdWantFailure(t, dir, "merge", "side")

		status, err := NewGit(dir).CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if got := status.NonRuntimePaths(); len(got) != 1 || got[0] != ".opencode/plugins/gastown.js" {
			t.Fatalf("NonRuntimePaths = %v, want [.opencode/plugins/gastown.js]", got)
		}
		if status.CleanExcludingRuntime() {
			t.Fatal("runtime unmerged conflict must block runtime-excluding clean check")
		}
	})

	t.Run("unmerged conflict blocks", func(t *testing.T) {
		t.Parallel()
		dir := initTestRepo(t)
		runGitTestCmd(t, dir, "branch", "-M", "main")
		if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("base\n"), 0644); err != nil {
			t.Fatalf("write base conflict file: %v", err)
		}
		runGitTestCmd(t, dir, "add", "conflict.txt")
		runGitTestCmd(t, dir, "commit", "-m", "add conflict base")
		runGitTestCmd(t, dir, "switch", "-c", "side")
		if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("side\n"), 0644); err != nil {
			t.Fatalf("write side conflict file: %v", err)
		}
		runGitTestCmd(t, dir, "commit", "-am", "side change")
		runGitTestCmd(t, dir, "switch", "main")
		if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("main\n"), 0644); err != nil {
			t.Fatalf("write main conflict file: %v", err)
		}
		runGitTestCmd(t, dir, "commit", "-am", "main change")
		runGitTestCmdWantFailure(t, dir, "merge", "side")

		status, err := NewGit(dir).CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if got := status.NonRuntimePaths(); len(got) != 1 || got[0] != "conflict.txt" {
			t.Fatalf("NonRuntimePaths = %v, want [conflict.txt]", got)
		}
		if status.CleanExcludingRuntime() {
			t.Fatal("unmerged conflict must block runtime-excluding clean check")
		}
	})
}

// TestCheckUncommittedWorkIndexSkewVsRealDirty is the gt-ui2x regression
// table. The shared-.repo.git pattern leaves a dormant polecat checkout's
// index describing a commit that moved out from under it: HEAD points at an
// older commit than the index/worktree, but the index/worktree content is
// exactly what's already on origin's default branch. That must classify as
// skew, not risk — while a real staged edit that ISN'T already known
// anywhere, and any unstaged edit at all, must still block.
func TestIntegrationCheckUncommittedWorkIndexSkewVsRealDirty(t *testing.T) {
	t.Parallel()
	t.Run("staged-only content already on origin is skew and does not block reuse", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "commit", "-am", "v2")
		runGitTestCmd(t, localDir, "push", "origin", "HEAD")
		// Move the branch ref back one commit WITHOUT touching the index or
		// worktree — this is the checkout-skew shape: HEAD regresses (as if a
		// shared ref move landed under a dormant checkout) while the index and
		// working tree still hold content origin already has.
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !status.HasUncommittedChanges {
			t.Fatal("staged skew must still be visible as an uncommitted change")
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 1 || skew[0] != "README.md" {
			t.Fatalf("IndexSkewFiles = %v, want [README.md]", skew)
		}
		if status.CleanExcludingRuntime() {
			t.Fatal("CleanExcludingRuntime must still see index skew as dirty (unaffected, gt done path)")
		}
		if !status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("index-skew-only seat must be treated as clean for reuse")
		}
	})

	t.Run("staged content not yet on origin or main still blocks", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "commit", "-am", "v2")
		// Deliberately do NOT push: origin's default branch still has only the
		// original content, so the staged v2 content is real, unpreserved work.
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 0 {
			t.Fatalf("IndexSkewFiles = %v, want none: content is not on origin", skew)
		}
		if status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("staged content absent from origin/main must still block reuse")
		}
	})

	// gt-ycvx: the branch carries a fix origin/main never received, and the
	// index is reverted to main's content. Content equality with main is the
	// revert here, not evidence of a moved ref — HEAD is ahead of every
	// comparison ref — so it must block rather than read as reuse-clean.
	t.Run("staged revert of a fix this branch carries, absent from main, still blocks", func(t *testing.T) {
		t.Parallel()
		localDir, _, mainBranch := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		runGitTestCmd(t, localDir, "checkout", "-b", "polecat/jasper/om-x")
		if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test fixed\n"), 0644); err != nil {
			t.Fatalf("write fix: %v", err)
		}
		runGitTestCmd(t, localDir, "commit", "-am", "fix: confine the reviewer to a read-only allowlist")
		runGitTestCmd(t, localDir, "push", "-u", "origin", "polecat/jasper/om-x")

		// Stage the revert: git checkout <ref> -- <path> puts main's content in
		// the index and the worktree (porcelain "M ") with HEAD still on the
		// fix.
		runGitTestCmd(t, localDir, "checkout", "origin/"+mainBranch, "--", "README.md")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if len(status.StagedOnly) != 1 || status.StagedOnly[0] != "README.md" {
			t.Fatalf("StagedOnly = %v, want [README.md]: the revert is staged, not unstaged", status.StagedOnly)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 0 {
			t.Fatalf("IndexSkewFiles = %v, want none: HEAD is not behind the ref the content matches, so no ref moved under this checkout", skew)
		}
		if status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("a staged security-fix revert must block seat reuse: the content matching origin/main is the revert, not evidence the content is already known")
		}
	})

	t.Run("unstaged edit always blocks regardless of skew classification", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("editor has this open\n"), 0644); err != nil {
			t.Fatalf("write unstaged edit: %v", err)
		}

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 0 {
			t.Fatalf("IndexSkewFiles = %v, want none: an unstaged edit is never a skew candidate", skew)
		}
		if status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("a real unstaged edit is exactly the risk this check exists to catch")
		}
	})

	// TestCheckUncommittedWorkIndexSkewVsRealDirty/pathspec-special filenames
	// is the om-review regression: the pre-fix classifier inferred skew from a
	// path's *absence* in `git diff --name-only` output, so a pathspec that
	// matched nothing at all — glob-magic characters in a real filename, or a
	// still-C-quoted path from Status() — read as "no difference found" and
	// silently misclassified a real staged edit as skew. classifyIndexSkew now
	// confirms identity by comparing blob shas directly (git ls-files -s vs
	// git ls-tree, both with :(literal) pathspecs), so a filename that would
	// have broken the old approach must classify correctly in both
	// directions: skew when the content really matches, blocking when it
	// doesn't.
	t.Run("pathspec-special filename with content on origin is still classified as skew", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		name := "notes [draft] v2.txt"
		if err := os.WriteFile(filepath.Join(localDir, name), []byte("v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "add", name)
		runGitTestCmd(t, localDir, "commit", "-m", "add v2")
		runGitTestCmd(t, localDir, "push", "origin", "HEAD")
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 1 || skew[0] != name {
			t.Fatalf("IndexSkewFiles = %v, want [%q]", skew, name)
		}
		if !status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("pathspec-special filename with content already on origin must still classify as skew")
		}
	})

	t.Run("pathspec-special filename absent from origin is NOT misclassified as skew", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		name := "notes [draft] v2.txt"
		if err := os.WriteFile(filepath.Join(localDir, name), []byte("v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "add", name)
		runGitTestCmd(t, localDir, "commit", "-m", "add v2")
		// Deliberately do NOT push: this content is real, unpreserved work.
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 0 {
			t.Fatalf("IndexSkewFiles = %v, want none: a pathspec that fails to match must fail CLOSED, not be silently classified as skew", skew)
		}
		if status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("a pathspec-special filename's real, unpreserved content must still block reuse")
		}
	})

	// The next two subtests exercise a name porcelain actually C-quotes (a
	// literal double quote, and a non-ASCII byte under the default
	// core.quotepath=true) rather than merely a pathspec-magic one. Left
	// quoted, the string Status() would have handed classifyIndexSkew is not
	// the real filename at all — see TestStatusUnquotesCQuotedPaths — so this
	// is the end-to-end path the om review flagged as untested.
	t.Run("C-quoted filename (embedded quote) with content on origin is classified as skew", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		name := `notes "draft" v2.txt`
		if err := os.WriteFile(filepath.Join(localDir, name), []byte("v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "add", name)
		runGitTestCmd(t, localDir, "commit", "-m", "add v2")
		runGitTestCmd(t, localDir, "push", "origin", "HEAD")
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 1 || skew[0] != name {
			t.Fatalf("IndexSkewFiles = %v, want [%q]", skew, name)
		}
		if !status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("C-quoted filename with content already on origin must still classify as skew")
		}
	})

	t.Run("C-quoted filename (embedded quote) absent from origin MUST block", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		name := `notes "draft" v2.txt`
		if err := os.WriteFile(filepath.Join(localDir, name), []byte("v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "add", name)
		runGitTestCmd(t, localDir, "commit", "-m", "add v2")
		// Deliberately do NOT push: this content is real, unpreserved work.
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 0 {
			t.Fatalf("IndexSkewFiles = %v, want none: real content not on origin must never classify as skew", skew)
		}
		if status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("a C-quoted filename's real, unpreserved content MUST block reuse")
		}
	})

	t.Run("non-ASCII filename with content on origin is classified as skew", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		name := "café résumé.txt"
		if err := os.WriteFile(filepath.Join(localDir, name), []byte("v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "add", name)
		runGitTestCmd(t, localDir, "commit", "-m", "add v2")
		runGitTestCmd(t, localDir, "push", "origin", "HEAD")
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 1 || skew[0] != name {
			t.Fatalf("IndexSkewFiles = %v, want [%q]", skew, name)
		}
		if !status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("non-ASCII filename with content already on origin must still classify as skew")
		}
	})

	t.Run("non-ASCII filename absent from origin MUST block", func(t *testing.T) {
		t.Parallel()
		localDir, _, _ := initTestRepoWithRemote(t)
		g := NewGit(localDir)

		name := "café résumé.txt"
		if err := os.WriteFile(filepath.Join(localDir, name), []byte("v2\n"), 0644); err != nil {
			t.Fatalf("write v2: %v", err)
		}
		runGitTestCmd(t, localDir, "add", name)
		runGitTestCmd(t, localDir, "commit", "-m", "add v2")
		// Deliberately do NOT push: this content is real, unpreserved work.
		runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

		status, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if skew := status.IndexSkewFiles(g); len(skew) != 0 {
			t.Fatalf("IndexSkewFiles = %v, want none: real content not on origin must never classify as skew", skew)
		}
		if status.CleanExcludingRuntimeAndIndexSkew(g) {
			t.Fatal("a non-ASCII filename's real, unpreserved content MUST block reuse")
		}
	})
}

// TestCheckUncommittedWorkIndexSkewIsLazy guards the gt-8q0s perf regression:
// classifyIndexSkew costs several extra git subprocesses (ls-files, ls-tree
// per candidate ref), so CheckUncommittedWork must not run it for a caller —
// like gt done — that never asks for IndexSkewFiles or
// CleanExcludingRuntimeAndIndexSkew. StagedOnly is populated for free from
// the porcelain status already parsed; only calling IndexSkewFiles triggers
// the classification.
func TestIntegrationCheckUncommittedWorkIndexSkewIsLazy(t *testing.T) {
	t.Parallel()
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test v2\n"), 0644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	runGitTestCmd(t, localDir, "commit", "-am", "v2")
	runGitTestCmd(t, localDir, "push", "origin", "HEAD")
	runGitTestCmd(t, localDir, "reset", "--soft", "HEAD~1")

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if len(status.StagedOnly) != 1 || status.StagedOnly[0] != "README.md" {
		t.Fatalf("StagedOnly = %v, want [README.md]: this must come for free from Status(), no classification needed", status.StagedOnly)
	}
	if status.indexSkewComputed {
		t.Fatal("classifyIndexSkew must not run inside CheckUncommittedWork itself — only a caller that asks via IndexSkewFiles should trigger it (gt-8q0s)")
	}

	// Asking now must trigger (and memoize) the classification.
	if skew := status.IndexSkewFiles(g); len(skew) != 1 || skew[0] != "README.md" {
		t.Fatalf("IndexSkewFiles = %v, want [README.md]", skew)
	}
	if !status.indexSkewComputed {
		t.Fatal("IndexSkewFiles must memoize its result")
	}
}

func runGitTestCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func runGitTestCmdWantFailure(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git %v unexpectedly succeeded\n%s", args, out)
	}
}

// initTestRepoWithSplitRemote creates a test setup that mirrors the polecat workflow:
// two bare repos (upstream and fork), a local clone whose origin has fetch URL → upstream
// and push URL → fork. Returns (localDir, upstreamBareDir, forkBareDir, mainBranch).
// It is a copy of splitRemoteFixture.
func initTestRepoWithSplitRemote(t *testing.T) (string, string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	splitRemoteFixture.copyInto(t, tmp)
	return filepath.Join(tmp, "local"), filepath.Join(tmp, "upstream.git"), filepath.Join(tmp, "fork.git"), fixtureMainBranch
}

// TestPushRemoteBranchExists_SplitURL is the core regression test for GH#3224:
// with a split fetch/push URL, RemoteBranchExists checks the fetch URL (upstream)
// while PushRemoteBranchExists checks the push URL (fork/bare repo).
func TestIntegrationPushRemoteBranchExists_SplitURL(t *testing.T) {
	t.Parallel()
	localDir, _, _, _ := initTestRepoWithSplitRemote(t)
	g := NewGit(localDir)

	// Create a feature branch and push to origin (goes to fork via push URL)
	if err := g.CreateBranch("polecat/fix-test"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/fix-test"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "fix.go"), []byte("package fix\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = localDir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "fix commit")
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := g.Push("origin", "polecat/fix-test", false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// RemoteBranchExists checks the fetch URL (upstream) — branch NOT there
	exists, err := g.RemoteBranchExists("origin", "polecat/fix-test")
	if err != nil {
		t.Fatalf("RemoteBranchExists: %v", err)
	}
	if exists {
		t.Error("RemoteBranchExists should return false — branch was pushed to fork, not upstream")
	}

	// PushRemoteBranchExists checks the push URL (fork) — branch IS there
	exists, err = g.PushRemoteBranchExists("origin", "polecat/fix-test")
	if err != nil {
		t.Fatalf("PushRemoteBranchExists: %v", err)
	}
	if !exists {
		t.Error("PushRemoteBranchExists should return true — branch was pushed to fork")
	}
}

func TestIntegrationListPushRemoteRefsWithHashesUsesPushURLHash(t *testing.T) {
	t.Parallel()
	localDir, upstream, _, mainBranch := initTestRepoWithSplitRemote(t)
	g := NewGit(localDir)
	branch := "polecat/split-classifier"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch upstream branch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout upstream branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "upstream.go"), []byte("package upstream\n"), 0644); err != nil {
		t.Fatalf("write upstream: %v", err)
	}
	if err := g.Add("upstream.go"); err != nil {
		t.Fatalf("Add upstream: %v", err)
	}
	if err := g.Commit("upstream branch work"); err != nil {
		t.Fatalf("Commit upstream: %v", err)
	}
	upstreamSHA, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev upstream: %v", err)
	}
	runGit(t, localDir, "push", upstream, branch)
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := g.Merge(branch); err != nil {
		t.Fatalf("Merge upstream branch: %v", err)
	}
	mainSHA, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev main: %v", err)
	}
	runGit(t, localDir, "push", upstream, mainBranch)
	runGit(t, localDir, "update-ref", "refs/remotes/origin/"+branch, upstreamSHA)
	runGit(t, localDir, "update-ref", "refs/remotes/origin/"+mainBranch, mainSHA)

	runGit(t, localDir, "checkout", "-B", branch, "origin/"+mainBranch)
	if err := os.WriteFile(filepath.Join(localDir, "fork.go"), []byte("package fork\n"), 0644); err != nil {
		t.Fatalf("write fork: %v", err)
	}
	if err := g.Add("fork.go"); err != nil {
		t.Fatalf("Add fork: %v", err)
	}
	if err := g.Commit("fork branch work"); err != nil {
		t.Fatalf("Commit fork: %v", err)
	}
	forkSHA, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev fork: %v", err)
	}
	if forkSHA == upstreamSHA {
		t.Fatal("expected fork branch commit to differ from upstream branch commit")
	}
	runGit(t, localDir, "push", "origin", branch)
	runGit(t, localDir, "update-ref", "refs/remotes/origin/"+branch, upstreamSHA)

	refs, err := g.ListPushRemoteRefsWithHashes("origin", "refs/heads/polecat/")
	if err != nil {
		t.Fatalf("ListPushRemoteRefsWithHashes: %v", err)
	}
	var found RemoteRef
	for _, ref := range refs {
		if ref.Name == "refs/heads/"+branch {
			found = ref
			break
		}
	}
	if found.Name == "" {
		t.Fatalf("remote ref %q not found in %#v", branch, refs)
	}
	if found.Hash != forkSHA {
		t.Fatalf("push remote ref hash = %q, want fork hash %q", found.Hash, forkSHA)
	}

	trackingMerged, err := g.IsAncestor("origin/"+branch, "origin/"+mainBranch)
	if err != nil {
		t.Fatalf("IsAncestor tracking ref: %v", err)
	}
	if !trackingMerged {
		t.Fatal("expected fetch-remote tracking branch to be merged")
	}
	hashMerged, err := g.IsAncestor(found.Hash, "origin/"+mainBranch)
	if err != nil {
		t.Fatalf("IsAncestor push hash: %v", err)
	}
	if hashMerged {
		t.Fatal("expected push remote hash to remain unmerged despite merged fetch tracking ref")
	}
}

func TestIntegrationPushRemoteRefTargetStatusPreservesRebasedRemoteBranch(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/rebased-preserved"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "rebased.txt"), []byte("rebased\n"), 0644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	if err := g.Add("rebased.txt"); err != nil {
		t.Fatalf("Add feature: %v", err)
	}
	if err := g.Commit("rebased work"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}
	branchSHA, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev branch: %v", err)
	}
	runGit(t, localDir, "push", "origin", branch)

	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "advance.txt"), []byte("target advanced\n"), 0644); err != nil {
		t.Fatalf("write advance: %v", err)
	}
	if err := g.Add("advance.txt"); err != nil {
		t.Fatalf("Add advance: %v", err)
	}
	if err := g.Commit("advance target"); err != nil {
		t.Fatalf("Commit advance: %v", err)
	}
	runGit(t, localDir, "cherry-pick", strings.TrimSpace(branchSHA))
	runGit(t, localDir, "push", "origin", mainBranch)
	if err := g.Fetch("origin"); err != nil {
		t.Fatalf("Fetch origin: %v", err)
	}

	ref := mustPushRemoteRef(t, g, branch)
	ancestor, err := g.IsAncestor(ref.Hash, "origin/"+mainBranch)
	if err != nil {
		t.Fatalf("IsAncestor remote hash: %v", err)
	}
	if ancestor {
		t.Fatal("test setup invalid: remote hash should not be an ancestor of target")
	}

	status, err := g.PushRemoteRefTargetStatus("origin", ref, "origin/"+mainBranch)
	if err != nil {
		t.Fatalf("PushRemoteRefTargetStatus: %v", err)
	}
	if !status.Preserved || status.UnpreservedPatchCount != 0 {
		t.Fatalf("PushRemoteRefTargetStatus = %+v, want preserved", status)
	}
	if status.Evidence == "" {
		t.Fatalf("PushRemoteRefTargetStatus evidence should be set: %+v", status)
	}
}

func TestIntegrationPushRemoteRefTargetStatusPreservesMultiCommitSquashRemoteBranch(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/squash-remote-preserved"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "squash.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if err := g.Add("squash.txt"); err != nil {
		t.Fatalf("Add one: %v", err)
	}
	if err := g.Commit("checkpoint one"); err != nil {
		t.Fatalf("Commit one: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "squash.txt"), []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatalf("write two: %v", err)
	}
	if err := g.Add("squash.txt"); err != nil {
		t.Fatalf("Add two: %v", err)
	}
	if err := g.Commit("checkpoint two"); err != nil {
		t.Fatalf("Commit two: %v", err)
	}
	runGit(t, localDir, "push", "origin", branch)

	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	runGit(t, localDir, "merge", "--squash", branch)
	runGit(t, localDir, "commit", "-m", "squash polecat work")
	if err := os.WriteFile(filepath.Join(localDir, "advance.txt"), []byte("target advanced\n"), 0644); err != nil {
		t.Fatalf("write advance: %v", err)
	}
	if err := g.Add("advance.txt"); err != nil {
		t.Fatalf("Add advance: %v", err)
	}
	if err := g.Commit("advance target"); err != nil {
		t.Fatalf("Commit advance: %v", err)
	}
	runGit(t, localDir, "push", "origin", mainBranch)
	if err := g.Fetch("origin"); err != nil {
		t.Fatalf("Fetch origin: %v", err)
	}

	ref := mustPushRemoteRef(t, g, branch)
	status, err := g.PushRemoteRefTargetStatus("origin", ref, "origin/"+mainBranch)
	if err != nil {
		t.Fatalf("PushRemoteRefTargetStatus: %v", err)
	}
	if !status.Preserved || status.UnpreservedPatchCount != 0 {
		t.Fatalf("PushRemoteRefTargetStatus = %+v, want squash-preserved", status)
	}
	if status.Evidence != "merge_tree_noop" {
		t.Fatalf("Evidence = %q, want merge_tree_noop", status.Evidence)
	}
}

func TestIntegrationPushRemoteRefTargetStatusKeepsUnmergedSplitPushRemoteBranch(t *testing.T) {
	t.Parallel()
	localDir, upstream, _, mainBranch := initTestRepoWithSplitRemote(t)
	g := NewGit(localDir)
	branch := "polecat/split-unmerged"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch upstream branch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout upstream branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "upstream-split.txt"), []byte("upstream\n"), 0644); err != nil {
		t.Fatalf("write upstream: %v", err)
	}
	if err := g.Add("upstream-split.txt"); err != nil {
		t.Fatalf("Add upstream: %v", err)
	}
	if err := g.Commit("upstream split work"); err != nil {
		t.Fatalf("Commit upstream: %v", err)
	}
	runGit(t, localDir, "push", upstream, branch)
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := g.Merge(branch); err != nil {
		t.Fatalf("Merge upstream branch: %v", err)
	}
	runGit(t, localDir, "push", upstream, mainBranch)
	if err := g.Fetch("origin"); err != nil {
		t.Fatalf("Fetch origin: %v", err)
	}

	runGit(t, localDir, "checkout", "-B", branch, "origin/"+mainBranch)
	if err := os.WriteFile(filepath.Join(localDir, "fork-split.txt"), []byte("fork-only\n"), 0644); err != nil {
		t.Fatalf("write fork: %v", err)
	}
	if err := g.Add("fork-split.txt"); err != nil {
		t.Fatalf("Add fork: %v", err)
	}
	if err := g.Commit("fork-only split work"); err != nil {
		t.Fatalf("Commit fork: %v", err)
	}
	runGit(t, localDir, "push", "origin", branch)

	ref := mustPushRemoteRef(t, g, branch)
	status, err := g.PushRemoteRefTargetStatus("origin", ref, "origin/"+mainBranch)
	if err != nil {
		t.Fatalf("PushRemoteRefTargetStatus: %v", err)
	}
	if status.Preserved || status.UnpreservedPatchCount == 0 {
		t.Fatalf("PushRemoteRefTargetStatus = %+v, want unpreserved split push branch", status)
	}
}

func mustPushRemoteRef(t *testing.T, g *Git, branch string) RemoteRef {
	t.Helper()
	refs, err := g.ListPushRemoteRefsWithHashes("origin", "refs/heads/polecat/")
	if err != nil {
		t.Fatalf("ListPushRemoteRefsWithHashes: %v", err)
	}
	for _, ref := range refs {
		if ref.Name == "refs/heads/"+branch {
			return ref
		}
	}
	t.Fatalf("remote ref %q not found in %#v", branch, refs)
	return RemoteRef{}
}

func TestIntegrationDeleteRemoteBranchIfAtRejectsChangedBranch(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	branch := "polecat/lease-test"
	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "lease.txt"), []byte("old"), 0644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := g.Add("lease.txt"); err != nil {
		t.Fatalf("Add old: %v", err)
	}
	if err := g.Commit("lease old"); err != nil {
		t.Fatalf("Commit old: %v", err)
	}
	oldHash, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev old: %v", err)
	}
	if err := g.Push("origin", branch, false); err != nil {
		t.Fatalf("Push old: %v", err)
	}

	if err := os.WriteFile(filepath.Join(localDir, "lease.txt"), []byte("new"), 0644); err != nil {
		t.Fatalf("write new: %v", err)
	}
	if err := g.Add("lease.txt"); err != nil {
		t.Fatalf("Add new: %v", err)
	}
	if err := g.Commit("lease new"); err != nil {
		t.Fatalf("Commit new: %v", err)
	}
	newHash, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev new: %v", err)
	}
	if err := g.Push("origin", branch, false); err != nil {
		t.Fatalf("Push new: %v", err)
	}

	if err := g.DeleteRemoteBranchIfAt("origin", branch, oldHash); err == nil {
		t.Fatal("DeleteRemoteBranchIfAt should reject a branch that advanced")
	}
	exists, err := g.RemoteBranchExists("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchExists after rejected delete: %v", err)
	}
	if !exists {
		t.Fatal("branch should still exist after rejected delete")
	}
	if err := g.DeleteRemoteBranchIfAt("origin", branch, newHash); err != nil {
		t.Fatalf("DeleteRemoteBranchIfAt current hash: %v", err)
	}
	exists, err = g.RemoteBranchExists("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchExists after delete: %v", err)
	}
	if exists {
		t.Fatal("branch should be deleted when expected hash matches")
	}
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
}

func TestIntegrationVerifyPushedCommitReachableFromPushTarget(t *testing.T) {
	t.Parallel()
	localDir, remoteDir, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	if err := os.WriteFile(filepath.Join(localDir, "shared.txt"), []byte("v1\n"), 0644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := g.Add("shared.txt"); err != nil {
		t.Fatalf("Add v1: %v", err)
	}
	if err := g.Commit("shared target v1"); err != nil {
		t.Fatalf("Commit v1: %v", err)
	}
	v1, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev v1: %v", err)
	}
	if err := g.Push("origin", mainBranch, false); err != nil {
		t.Fatalf("Push v1: %v", err)
	}
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", mainBranch, v1); err != nil {
		t.Fatalf("Verify exact tip: %v", err)
	}

	cloneDir := filepath.Join(t.TempDir(), "advancer")
	if out, err := exec.Command("git", "clone", remoteDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("clone advancer: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "shared.txt"), []byte("v2\n"), 0644); err != nil {
		t.Fatalf("write advancer v2: %v", err)
	}
	for _, args := range [][]string{
		{"git", "add", "shared.txt"},
		{"git", "commit", "-m", "shared target v2"},
		{"git", "push", "origin", mainBranch},
	} {
		cmd := exec.Command("git", args[1:]...)
		cmd.Dir = cloneDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", args, err, out)
		}
	}
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", mainBranch, v1); err != nil {
		t.Fatalf("Verify ancestor after concurrent push: %v", err)
	}

	if err := os.WriteFile(filepath.Join(localDir, "shared.txt"), []byte("local-only\n"), 0644); err != nil {
		t.Fatalf("write local-only: %v", err)
	}
	if err := g.Add("shared.txt"); err != nil {
		t.Fatalf("Add local-only: %v", err)
	}
	if err := g.Commit("local only"); err != nil {
		t.Fatalf("Commit local-only: %v", err)
	}
	localOnly, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev local-only: %v", err)
	}
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", mainBranch, localOnly); err == nil {
		t.Fatal("Verify should fail for commit not reachable from push target")
	}
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", "missing-branch", v1); err == nil {
		t.Fatal("Verify should fail for missing branch")
	}

	for _, args := range [][]string{
		{"git", "checkout", "--orphan", "replacement"},
		{"git", "rm", "-rf", "."},
	} {
		cmd := exec.Command("git", args[1:]...)
		cmd.Dir = cloneDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "replacement.txt"), []byte("replacement\n"), 0644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	for _, args := range [][]string{
		{"git", "add", "replacement.txt"},
		{"git", "commit", "-m", "replace remote history"},
		{"git", "branch", "-M", mainBranch},
		{"git", "push", "--force", "origin", mainBranch},
	} {
		cmd := exec.Command("git", args[1:]...)
		cmd.Dir = cloneDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", args, err, out)
		}
	}
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", mainBranch, v1); err == nil {
		t.Fatal("Verify should fail when stale origin/main still has commit but push target does not")
	}
}

// TestVerifyPushedCommitReachableFromPushTargetAllowsContentPreservedRebase
// reproduces gt-3fq: a sequential merge queue rebases each MR onto the moved
// target before merging, so the SHA that lands is never the submitted
// commit_sha even though the change is identical. The proof check must treat
// that as verified, not as an unverified push.
func TestIntegrationVerifyPushedCommitReachableFromPushTargetAllowsContentPreservedRebase(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch feature: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add feature.txt: %v", err)
	}
	if err := g.Commit("feat: add feature"); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}
	submitted, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev submitted: %v", err)
	}

	// Simulate another MR landing on the target ahead of this one, moving main.
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "other.txt"), []byte("other\n"), 0644); err != nil {
		t.Fatalf("write other.txt: %v", err)
	}
	if err := g.Add("other.txt"); err != nil {
		t.Fatalf("Add other.txt: %v", err)
	}
	if err := g.Commit("other: unrelated MR landed first"); err != nil {
		t.Fatalf("Commit other: %v", err)
	}
	if err := g.Push("origin", mainBranch, false); err != nil {
		t.Fatalf("Push main: %v", err)
	}

	// Rebase the submitted branch onto the moved target — same patch, new SHA.
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature for rebase: %v", err)
	}
	if err := g.Rebase(mainBranch); err != nil {
		t.Fatalf("Rebase feature onto main: %v", err)
	}
	rebased, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev rebased: %v", err)
	}
	if rebased == submitted {
		t.Fatal("rebase should have rewritten the commit SHA")
	}

	// Land the rebased commit on main, as the refinery would.
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main for landing: %v", err)
	}
	if err := g.MergeFFOnly("feature"); err != nil {
		t.Fatalf("MergeFFOnly feature: %v", err)
	}
	if err := g.Push("origin", mainBranch, false); err != nil {
		t.Fatalf("Push landed main: %v", err)
	}

	// The originally-submitted SHA never appears on the target, but its
	// content did — the proof check must succeed.
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", mainBranch, submitted); err != nil {
		t.Fatalf("Verify should succeed for content-preserved rebase: %v", err)
	}
}

func TestIntegrationCommitLandedOnTarget_ExactTipAndAncestor(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("feat: add feature"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	submitted, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}

	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := g.MergeNoFF("feature", "Merge feature into main"); err != nil {
		t.Fatalf("MergeNoFF: %v", err)
	}
	if err := g.Push("origin", mainBranch, false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// The merge commit itself is the exact tip.
	mergeCommit, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev merge commit: %v", err)
	}
	if !g.CommitLandedOnTarget("origin", mainBranch, mergeCommit) {
		t.Error("expected the exact tip to be reported as landed")
	}
	// The submitted commit is an ancestor of the merge commit (its second parent).
	if !g.CommitLandedOnTarget("origin", mainBranch, submitted) {
		t.Error("expected the submitted commit (an ancestor of the merge commit) to be reported as landed")
	}
}

func TestIntegrationCommitLandedOnTarget_ContentPreservedRebase(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("feat: add feature"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	submitted, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}

	// Another MR lands on target first, moving it ahead of feature's base.
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "other.txt"), []byte("other\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("other.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("other: unrelated MR landed first"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := g.Push("origin", mainBranch, false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Rebase feature onto the moved target — same patch, new SHA — and land it.
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := g.Rebase(mainBranch); err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	rebased, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev rebased: %v", err)
	}
	if rebased == submitted {
		t.Fatal("rebase should have rewritten the commit SHA")
	}
	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if err := g.MergeFFOnly("feature"); err != nil {
		t.Fatalf("MergeFFOnly: %v", err)
	}
	if err := g.Push("origin", mainBranch, false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// The originally-submitted SHA never appears on target, but its content
	// (same patch-id) did.
	if !g.CommitLandedOnTarget("origin", mainBranch, submitted) {
		t.Error("expected the content-preserved rebase to be reported as landed")
	}
}

// TestCommitLandedOnTarget_EmptyNetDiff_NotLanded is the check this method
// exists to get right where VerifyPushedCommitReachableFromPushTarget would
// not: a branch whose commits net out to no change (added then removed the
// same payload) has never actually been merged anywhere, but merging it now
// would still be a no-op — the same "merge-tree no-op" signal
// VerifyPushedCommitReachableFromPushTarget's patch-preservation fallback
// accepts. CommitLandedOnTarget must not report this as landed: reporting a
// never-merged, still-open MR as already landed would skip closing it as the
// empty/superseded submission it actually is (gt-j5cc).
func TestIntegrationCommitLandedOnTarget_EmptyNetDiff_NotLanded(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	if err := g.CreateBranch("feature"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("feature"); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "payload.txt"), []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := g.Add("payload.txt"); err != nil {
		t.Fatalf("Add payload: %v", err)
	}
	if err := g.Commit("feat: add payload"); err != nil {
		t.Fatalf("Commit payload: %v", err)
	}
	if err := os.Remove(filepath.Join(localDir, "payload.txt")); err != nil {
		t.Fatalf("remove payload: %v", err)
	}
	if err := g.Add("payload.txt"); err != nil {
		t.Fatalf("Add removal: %v", err)
	}
	if err := g.Commit("fix: drop the payload again"); err != nil {
		t.Fatalf("Commit removal: %v", err)
	}
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}

	// Sanity check: this really would present as a merge-tree no-op, the
	// broader (and here, wrong) signal this method deliberately avoids.
	if noop, err := g.mergeTreeNoopBetweenRefs(head, mainBranch); err != nil || !noop {
		t.Fatalf("expected merging %s into %s to be a tree no-op (err=%v, noop=%v)", shortSHA(head), mainBranch, err, noop)
	}

	if g.CommitLandedOnTarget("origin", mainBranch, head) {
		t.Error("expected an empty/never-merged submission not to be reported as landed")
	}
}

func TestIntegrationVerifyPushedCommitSplitURL(t *testing.T) {
	t.Parallel()
	localDir, _, _, _ := initTestRepoWithSplitRemote(t)
	g := NewGit(localDir)

	if err := g.CreateBranch("polecat/verified-split"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/verified-split"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "split.txt"), []byte("split\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("split.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("verified split push"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	sha, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}
	if err := g.Push("origin", "polecat/verified-split", false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	fetchTip, err := g.RemoteBranchTip("origin", "polecat/verified-split")
	if err != nil {
		t.Fatalf("RemoteBranchTip: %v", err)
	}
	if fetchTip != "" {
		t.Fatalf("fetch remote should not have split push branch, got %s", fetchTip)
	}
	if err := g.VerifyPushedCommit("origin", "polecat/verified-split", sha); err != nil {
		t.Fatalf("VerifyPushedCommit should query push URL: %v", err)
	}
}

func TestIntegrationVerifyPushedCommitReachableFromPushTargetSplitURL(t *testing.T) {
	t.Parallel()
	localDir, _, forkDir, _ := initTestRepoWithSplitRemote(t)
	g := NewGit(localDir)
	branch := "integration/verified-split"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(localDir, "split-shared.txt"), []byte("v1\n"), 0644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := g.Add("split-shared.txt"); err != nil {
		t.Fatalf("Add v1: %v", err)
	}
	if err := g.Commit("split shared v1"); err != nil {
		t.Fatalf("Commit v1: %v", err)
	}
	v1, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev v1: %v", err)
	}
	if err := g.Push("origin", branch, false); err != nil {
		t.Fatalf("Push v1: %v", err)
	}

	cloneDir := filepath.Join(t.TempDir(), "fork-advancer")
	if out, err := exec.Command("git", "clone", forkDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("clone fork advancer: %v\n%s", err, out)
	}
	cmd := exec.Command("git", "checkout", branch)
	cmd.Dir = cloneDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout fork branch: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "split-shared.txt"), []byte("v2\n"), 0644); err != nil {
		t.Fatalf("write fork v2: %v", err)
	}
	for _, args := range [][]string{
		{"git", "add", "split-shared.txt"},
		{"git", "commit", "-m", "split shared v2"},
		{"git", "push", "origin", branch},
	} {
		cmd := exec.Command("git", args[1:]...)
		cmd.Dir = cloneDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", args, err, out)
		}
	}

	fetchTip, err := g.RemoteBranchTip("origin", branch)
	if err != nil {
		t.Fatalf("RemoteBranchTip: %v", err)
	}
	pushTip, err := g.PushRemoteBranchTip("origin", branch)
	if err != nil {
		t.Fatalf("PushRemoteBranchTip: %v", err)
	}
	if fetchTip == pushTip {
		t.Fatalf("test setup expected split fetch/push tips to differ, got %s", fetchTip)
	}
	if err := g.VerifyPushedCommitReachableFromPushTarget("origin", branch, v1); err != nil {
		t.Fatalf("Verify should query push URL and accept ancestor: %v", err)
	}
}

// TestBranchPushedToRemote_SplitURL verifies that BranchPushedToRemote correctly
// reports a branch as pushed when it exists on the push target (fork), even though
// it's absent from the fetch URL (upstream). This is the GH#3224 fix.
func TestIntegrationBranchPushedToRemote_SplitURL(t *testing.T) {
	t.Parallel()
	localDir, _, _, _ := initTestRepoWithSplitRemote(t)
	g := NewGit(localDir)

	// Create and push a feature branch (goes to fork via push URL)
	if err := g.CreateBranch("polecat/status-test"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout("polecat/status-test"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "status.go"), []byte("package status\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = localDir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "status commit")
	cmd.Dir = localDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := g.Push("origin", "polecat/status-test", false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	pushed, unpushed, err := g.BranchPushedToRemote("polecat/status-test", "origin")
	if err != nil {
		t.Fatalf("BranchPushedToRemote: %v", err)
	}
	if !pushed {
		t.Error("BranchPushedToRemote should report pushed=true (branch is on fork)")
	}
	if unpushed != 0 {
		t.Errorf("BranchPushedToRemote unpushed = %d, want 0", unpushed)
	}
}

func TestIntegrationUnpushedCommitsPrefersExactRemoteBranchOverUpstream(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/already-pushed"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "work.go"), []byte("package work\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("work.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("polecat work"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := g.Push("origin", branch, false); err != nil {
		t.Fatalf("Push: %v", err)
	}
	runGit(t, localDir, "branch", "--set-upstream-to=origin/"+mainBranch, branch)

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed != 0 {
		t.Fatalf("UnpushedCommits = %d, want 0 for pushed branch tracking origin/%s", unpushed, mainBranch)
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if !status.Clean() {
		t.Fatalf("CheckUncommittedWork should be clean, got %s", status)
	}
}

// A branch pushed and then extended locally has work origin lacks. The exact
// remote branch is the only evidence, and it does not hold HEAD, so the commit
// must count as unpreserved (gt-70m1).
func TestIntegrationBranchPreservationStatusCountsWorkAheadOfExactRemoteBranch(t *testing.T) {
	t.Parallel()
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/pushed-then-extended"

	runGit(t, localDir, "checkout", "-b", branch)
	commitFile := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(localDir, name), []byte(name+"\n"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		runGit(t, localDir, "add", name)
		runGit(t, localDir, "commit", "-m", name)
	}
	commitFile("pushed.txt")
	runGit(t, localDir, "push", "origin", branch)

	status, err := g.BranchPreservationStatus(branch, "origin", nil)
	if err != nil {
		t.Fatalf("BranchPreservationStatus: %v", err)
	}
	if !status.Preserved || status.UnpreservedPatchCount != 0 {
		t.Fatalf("pushed branch = %+v, want preserved", status)
	}

	commitFile("local-only.txt")
	status, err = g.BranchPreservationStatus(branch, "origin", nil)
	if err != nil {
		t.Fatalf("BranchPreservationStatus after local commit: %v", err)
	}
	if status.Preserved || status.UnpreservedPatchCount != 1 {
		t.Fatalf("branch ahead of its remote = %+v, want 1 unpreserved commit", status)
	}
	if status.ComparisonBase != "origin/"+branch {
		t.Errorf("ComparisonBase = %q, want origin/%s", status.ComparisonBase, branch)
	}
}

// foreignPolecatUpstreamRepo builds the gt-y6w8y state: origin/main has moved
// past origin/polecat/shale/x, and a local polecat/quartz/x branch (never
// pushed) is configured to track that other polecat's branch. HEAD sits on
// origin/main's tip.
func foreignPolecatUpstreamRepo(t *testing.T) string {
	t.Helper()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	runGit(t, localDir, "branch", "polecat/shale/x")
	runGit(t, localDir, "push", "origin", "polecat/shale/x")
	if err := os.WriteFile(filepath.Join(localDir, "landed.go"), []byte("package landed\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, localDir, "add", "landed.go")
	runGit(t, localDir, "commit", "-m", "landed on main")
	runGit(t, localDir, "push", "origin", mainBranch)

	branch := "polecat/quartz/x"
	runGit(t, localDir, "checkout", "-b", branch)
	runGit(t, localDir, "config", "branch."+branch+".remote", "origin")
	runGit(t, localDir, "config", "branch."+branch+".merge", "refs/heads/polecat/shale/x")
	return localDir
}

// TestUnpushedCommitsIgnoresForeignPolecatUpstream pins gt-y6w8y: a polecat
// branch whose upstream names ANOTHER polecat's branch must not be judged
// against that branch. HEAD is contained in origin/main, so nothing is at risk.
func TestIntegrationUnpushedCommitsIgnoresForeignPolecatUpstream(t *testing.T) {
	t.Parallel()
	g := NewGit(foreignPolecatUpstreamRepo(t))

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed != 0 {
		t.Fatalf("UnpushedCommits = %d, want 0 for HEAD already contained in origin/main", unpushed)
	}
}

// TestUnpushedCommitsForeignPolecatUpstreamStillFailsClosed: ignoring the
// foreign upstream must not make real unpushed work look safe.
func TestIntegrationUnpushedCommitsForeignPolecatUpstreamStillFailsClosed(t *testing.T) {
	t.Parallel()
	localDir := foreignPolecatUpstreamRepo(t)
	g := NewGit(localDir)
	if err := os.WriteFile(filepath.Join(localDir, "work.go"), []byte("package work\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, localDir, "add", "work.go")
	runGit(t, localDir, "commit", "-m", "unpushed polecat work")

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed == 0 {
		t.Fatalf("UnpushedCommits = 0, want > 0 for a commit on no remote branch")
	}
}

// detachAtPushedBranchTip builds the gt-1bpgm state: a branch pushed to
// origin, then checked out detached at its own tip, which is how a finished
// polecat's worktree is left. Returns the tip's sha.
func detachAtPushedBranchTip(t *testing.T, localDir, branch string) string {
	t.Helper()
	g := NewGit(localDir)
	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "work.go"), []byte("package work\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("work.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("polecat work"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := g.Push("origin", branch, false); err != nil {
		t.Fatalf("Push: %v", err)
	}
	tip, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev(HEAD): %v", err)
	}
	tip = strings.TrimSpace(tip)
	runGit(t, localDir, "checkout", "--detach", tip)
	return tip
}

// TestUnpushedCommitsDetachedHeadOnRemoteBranch pins the gt-1bpgm fix: a
// detached worktree at a branch tip origin already holds was measured against
// origin/main alone, so its work read as unpushed and its seat stayed blocked
// for the whole review cycle. Reachability from any remote-tracking branch is
// the preservation evidence a named branch already gets from the exact-branch
// arm, so the count must be 0 here without the seat's branch being named.
func TestIntegrationUnpushedCommitsDetachedHeadOnRemoteBranch(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/topaz/gt-glfh+mudjlvex"
	tip := detachAtPushedBranchTip(t, localDir, branch)

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed != 0 {
		t.Fatalf("UnpushedCommits = %d, want 0: detached HEAD %s is the tip of origin/%s", unpushed, tip, branch)
	}

	unpushedLocal, err := g.UnpushedCommitsLocal()
	if err != nil {
		t.Fatalf("UnpushedCommitsLocal: %v", err)
	}
	if unpushedLocal != 0 {
		t.Fatalf("UnpushedCommitsLocal = %d, want 0: detached HEAD %s is the tip of origin/%s", unpushedLocal, tip, branch)
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if !status.Clean() {
		t.Fatalf("CheckUncommittedWork = %s, want clean for a detached worktree whose tip is on origin", status)
	}

	preservation, err := g.BranchPreservationStatus("HEAD", "origin", nil)
	if err != nil {
		t.Fatalf("BranchPreservationStatus: %v", err)
	}
	if !preservation.Preserved {
		t.Fatalf("BranchPreservationStatus = %+v, want preserved via a remote-tracking branch", preservation)
	}
	if preservation.Evidence != "detached_head_on_remote_branch" {
		t.Fatalf("Evidence = %q, want detached_head_on_remote_branch", preservation.Evidence)
	}

	// The target gate asks a different question — is this in the integration
	// branch — and a pushed branch tip is not, so the reuse-gate evidence must
	// not leak into it.
	target, err := g.BranchTargetStatus("HEAD", "origin", []string{"origin/" + mainBranch})
	if err != nil {
		t.Fatalf("BranchTargetStatus: %v", err)
	}
	if target.Preserved || target.UnpreservedPatchCount == 0 {
		t.Fatalf("BranchTargetStatus = %+v, want the detached tip unpreserved against origin/%s", target, mainBranch)
	}
}

// TestDetachedHeadCustodyLocalHonoursRemote pins gt-jg2e8: the custody lookup
// judges the remote it was asked about. A detached tip that only a second
// remote (a fork, a backup) holds is not on origin, so the origin verdict must
// not treat that ref as custody.
func TestIntegrationDetachedHeadCustodyLocalHonoursRemote(t *testing.T) {
	t.Parallel()
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/jade/gt-jg2e8+munuz8c4"
	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "work.go"), []byte("package work\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("work.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("polecat work"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	fork := filepath.Join(t.TempDir(), "fork.git")
	runGit(t, "", "init", "--bare", fork)
	runGit(t, localDir, "remote", "add", "fork", fork)
	if err := g.Push("fork", branch, false); err != nil {
		t.Fatalf("Push fork: %v", err)
	}
	tip, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev(HEAD): %v", err)
	}
	tip = strings.TrimSpace(tip)
	runGit(t, localDir, "checkout", "--detach", tip)

	if ref, ok := g.detachedHeadCustodyLocal("origin", tip); ok {
		t.Fatalf("detachedHeadCustodyLocal(origin) = %q, want none: only fork holds %s", ref, tip)
	}
	if ref, ok := g.detachedHeadCustodyLocal("fork", tip); !ok || ref != "fork/"+branch {
		t.Fatalf("detachedHeadCustodyLocal(fork) = %q, %v, want fork/%s", ref, ok, branch)
	}

	preservation, err := g.BranchPreservationStatusLocal("HEAD", "origin", nil)
	if err == nil && preservation.Evidence == "detached_head_on_remote_branch" {
		t.Fatalf("BranchPreservationStatusLocal(origin) = %+v, want no detached-custody evidence from fork", preservation)
	}
}

// TestUnpushedCommitsDetachedHeadUnfetchedRemoteBranch pins the live level's
// extra reach: by the time a seat is left detached, its local branch and the
// tracking ref the push created are usually gone too, so only the remote can
// still name the work. The local level cannot see it and says so, which is its
// documented one-sided error; the live level asks.
func TestIntegrationUnpushedCommitsDetachedHeadUnfetchedRemoteBranch(t *testing.T) {
	t.Parallel()
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/granite/gt-7dxw+mudlcgyf"
	tip := detachAtPushedBranchTip(t, localDir, branch)
	runGit(t, localDir, "update-ref", "-d", "refs/remotes/origin/"+branch)

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed != 0 {
		t.Fatalf("UnpushedCommits = %d, want 0: %s is the tip of origin/%s", unpushed, tip, branch)
	}

	unpushedLocal, err := g.UnpushedCommitsLocal()
	if err != nil {
		t.Fatalf("UnpushedCommitsLocal: %v", err)
	}
	if unpushedLocal == 0 {
		t.Fatal("UnpushedCommitsLocal = 0, want > 0: with the tracking ref gone the local level has no evidence, and must not claim preservation it did not see")
	}
}

// runGitEnv runs git in dir with env appended, failing the test on error, and
// returns its trimmed stdout. The bare-remote fixtures carry no committer
// identity of their own, so landing on one passes it explicitly.
func runGitEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// landOnRemoteSquash is the shape a Forgejo landing leaves on the remote: the
// work merged onto the remote's default branch as a squash (a commit with the
// branch tip's tree, and a different id), and the branch itself deleted. The
// seat's clone is untouched, so its origin/<default> keeps the pre-landing tip.
func landOnRemoteSquash(t *testing.T, remoteDir, branch, mainBranch string) {
	t.Helper()
	ident := []string{
		"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@test.com",
	}
	tree := runGitEnv(t, remoteDir, nil, "rev-parse", branch+"^{tree}")
	base := runGitEnv(t, remoteDir, nil, "rev-parse", mainBranch)
	landed := runGitEnv(t, remoteDir, ident, "commit-tree", tree, "-p", base, "-m", "land "+branch)
	runGit(t, remoteDir, "update-ref", "refs/heads/"+mainBranch, landed)
	runGit(t, remoteDir, "update-ref", "-d", "refs/heads/"+branch)
}

// TestIntegrationLivePreservationRefreshesStaleDefaultBranch pins gt-fn9e6.36:
// a Forgejo landing merges the work on the remote and deletes the seat's
// branch, so a finished seat has no custody of its own and its verdict falls
// back to this clone's origin/<default> — a ref nothing fetched, still
// pointing at the pre-landing main. The live check refreshes that ref before
// judging it, so the seat reads preserved without a manual fetch; the local
// check stays offline and still reports the work unpushed.
func TestIntegrationLivePreservationRefreshesStaleDefaultBranch(t *testing.T) {
	t.Parallel()
	localDir, remoteDir, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/obsidian/gt-fn9e6+muvgoylj"
	tip := detachAtPushedBranchTip(t, localDir, branch)
	// The landing deletes the seat's branch, and the clone's tracking ref for
	// it goes with it — the seat's own evidence is gone, not stale.
	runGit(t, localDir, "update-ref", "-d", "refs/remotes/origin/"+branch)
	landOnRemoteSquash(t, remoteDir, branch, mainBranch)

	// Offline: the local level has not seen the landing and must say so. It
	// runs before the live probe, which refreshes the ref the local level
	// would otherwise have read.
	unpushedLocal, err := g.UnpushedCommitsLocal()
	if err != nil {
		t.Fatalf("UnpushedCommitsLocal: %v", err)
	}
	if unpushedLocal == 0 {
		t.Fatal("UnpushedCommitsLocal = 0, want > 0: offline, the pre-landing main does not hold the seat's work")
	}

	// Live: the refresh sees the landing and clears the seat.
	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed != 0 {
		t.Fatalf("UnpushedCommits = %d, want 0: %s is on the remote's %s after the landing", unpushed, tip, mainBranch)
	}
	preservation, err := g.BranchPreservationStatus("HEAD", "origin", nil)
	if err != nil || !preservation.Preserved {
		t.Fatalf("BranchPreservationStatus = %+v, %v; want preserved", preservation, err)
	}

	// The refresh moved refs only: HEAD is where the seat left it.
	if head, err := g.Rev("HEAD"); err != nil || strings.TrimSpace(head) != tip {
		t.Fatalf("HEAD after the refresh = %q, %v; want the seat's tip %s", strings.TrimSpace(head), err, tip)
	}

	// Work that is on no remote branch and not in the remote's main is still
	// flagged: the refresh must not launder unpushed commits.
	if err := os.WriteFile(filepath.Join(localDir, "local-only.go"), []byte("package localonly\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("local-only.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("never pushed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if unpushed, err := g.UnpushedCommits(); err != nil || unpushed == 0 {
		t.Fatalf("UnpushedCommits of local-only work = %d, %v; want it flagged", unpushed, err)
	}
}

// TestIntegrationLivePreservationUnreachableRemoteKeepsTodayVerdict is the
// fail-closed half of the refresh: when the fetch cannot run, the live check
// judges whatever ref it already had, exactly as it did before it could fetch,
// and never turns the failure into a preservation claim.
func TestIntegrationLivePreservationUnreachableRemoteKeepsTodayVerdict(t *testing.T) {
	t.Parallel()
	localDir, remoteDir, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/obsidian/gt-fn9e6+munreachable"
	detachAtPushedBranchTip(t, localDir, branch)
	runGit(t, localDir, "update-ref", "-d", "refs/remotes/origin/"+branch)
	landOnRemoteSquash(t, remoteDir, branch, mainBranch)

	runGit(t, localDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits with an unreachable remote: %v", err)
	}
	if unpushed == 0 {
		t.Fatal("UnpushedCommits = 0 with an unreachable remote: a failed fetch must never read as preserved")
	}
}

// TestUnpushedCommitsDetachedHeadOffRemoteStillBlocks is the fail-closed half:

// TestUnpushedCommitsDetachedHeadOffRemoteStillBlocks is the fail-closed half:
// a detached worktree whose commits exist nowhere on the remote must keep
// reporting unpreserved work. Without it, widening the detached-head evidence
// could clear a seat holding the only copy of a commit.
func TestIntegrationUnpushedCommitsDetachedHeadOffRemoteStillBlocks(t *testing.T) {
	t.Parallel()
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	detachAtPushedBranchTip(t, localDir, "polecat/opal/gt-glfh+mudjlvex")

	if err := os.WriteFile(filepath.Join(localDir, "local-only.go"), []byte("package localonly\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("local-only.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("never pushed"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	unpushed, err := g.UnpushedCommits()
	if err != nil {
		t.Fatalf("UnpushedCommits: %v", err)
	}
	if unpushed == 0 {
		t.Fatal("UnpushedCommits = 0, want > 0: a detached commit on no remote ref is not preserved")
	}

	unpushedLocal, err := g.UnpushedCommitsLocal()
	if err != nil {
		t.Fatalf("UnpushedCommitsLocal: %v", err)
	}
	if unpushedLocal == 0 {
		t.Fatal("UnpushedCommitsLocal = 0, want > 0: a detached commit on no remote ref is not preserved")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if status.Clean() {
		t.Fatal("CheckUncommittedWork = clean, want the local-only commit to block reuse")
	}
}

// TestUnpushedCommitsLocalFailClosed_NoComparisonRefs pins the gt-utt4 fix:
// UnpushedCommitsLocal reads "no comparison ref resolved" as "0 unpushed" —
// correct for a caller that only reports a count, wrong for one about to
// discard a worktree because it looks clean. The fail-closed variant must
// report the branch as having unpreserved work in exactly this situation: a
// remote is configured but was never fetched, so there is no exact-branch,
// upstream, or default-branch ref to compare HEAD against at all.
func TestIntegrationUnpushedCommitsLocalFailClosed_NoComparisonRefs(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remoteDir := filepath.Join(tmp, "remote.git")
	if err := exec.Command("git", "init", "--bare", remoteDir).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}

	localDir := filepath.Join(tmp, "local")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, localDir, "init")
	runGit(t, localDir, "remote", "add", "origin", remoteDir)
	runGit(t, localDir, "checkout", "-b", "polecat/never-fetched")
	runGit(t, localDir, "commit", "--allow-empty", "-m", "initial")

	g := NewGit(localDir)

	loose, err := g.UnpushedCommitsLocal()
	if err != nil {
		t.Fatalf("UnpushedCommitsLocal: %v", err)
	}
	if loose != 0 {
		t.Fatalf("UnpushedCommitsLocal = %d, want 0 (fail-open baseline for a plain count)", loose)
	}

	failClosed, err := g.UnpushedCommitsLocalFailClosed()
	if err != nil {
		t.Fatalf("UnpushedCommitsLocalFailClosed: %v", err)
	}
	if failClosed == 0 {
		t.Fatalf("UnpushedCommitsLocalFailClosed = 0, want > 0: an unresolvable comparison must read as unpreserved work, not as clean")
	}

	status, err := g.CheckUncommittedWorkLocalFailClosed()
	if err != nil {
		t.Fatalf("CheckUncommittedWorkLocalFailClosed: %v", err)
	}
	if status.Clean() {
		t.Fatalf("CheckUncommittedWorkLocalFailClosed().Clean() = true, want false for a never-fetched branch")
	}
}

func TestIntegrationBranchTargetStatusPreservesSquashMergedAdvancedTarget(t *testing.T) {
	t.Parallel()
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/squash-preserved"

	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("checkpoint one"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("checkpoint two"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := g.Checkout(mainBranch); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	runGit(t, localDir, "merge", "--squash", branch)
	runGit(t, localDir, "commit", "-m", "squash polecat work")
	if err := os.WriteFile(filepath.Join(localDir, "advance.txt"), []byte("target advanced\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("advance.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("advance target"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	runGit(t, localDir, "push", "origin", mainBranch)

	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout branch: %v", err)
	}
	status, err := g.BranchTargetStatus(branch, "origin", []string{"origin/" + mainBranch})
	if err != nil {
		t.Fatalf("BranchTargetStatus: %v", err)
	}
	if !status.Preserved || status.UnpreservedPatchCount != 0 {
		t.Fatalf("BranchTargetStatus = %+v, want squash-preserved target", status)
	}
	if status.Evidence != "merge_tree_noop" {
		t.Fatalf("Evidence = %q, want merge_tree_noop", status.Evidence)
	}

	if err := os.WriteFile(filepath.Join(localDir, "feature.txt"), []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("feature.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("extra local work"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	status, err = g.BranchTargetStatus(branch, "origin", []string{"origin/" + mainBranch})
	if err != nil {
		t.Fatalf("BranchTargetStatus after extra work: %v", err)
	}
	if status.Preserved || status.UnpreservedPatchCount == 0 {
		t.Fatalf("BranchTargetStatus after extra work = %+v, want unpreserved work", status)
	}
}

// revParse returns the SHA a ref resolves to in dir.
func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "protocol.file.allow=always", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s in %s: %v\n%s", ref, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// notesOriginFixture returns a bare origin and a clone of it, both with main
// checked out, for exercising the notes-push retry path (gt-2rcx).
func notesOriginFixture(t *testing.T) (originDir, cloneDir string) {
	t.Helper()
	originDir = t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", originDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	seed := initTestRepo(t)
	runGit(t, seed, "remote", "add", "origin", originDir)
	runGit(t, seed, "push", "origin", "HEAD:refs/heads/main")

	return originDir, notesTestClone(t, originDir)
}

// notesTestClone clones origin into a fresh temp dir, configured to commit.
func notesTestClone(t *testing.T, originDir string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-c", "protocol.file.allow=always", "clone", originDir, dir).CombinedOutput(); err != nil {
		t.Fatalf("git clone %s: %v\n%s", originDir, err, out)
	}
	return dir
}

// commitNotesTestFile commits a new file in dir and returns the new commit's
// sha — an object a writer can hang a note on.
func commitNotesTestFile(t *testing.T, dir, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", message)
	g := NewGit(dir)
	rev, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return rev
}

// notesPublishedOnOrigin reads the notes origin currently serves, keyed by
// annotated object, from a clone sharing nothing with the writer.
func notesPublishedOnOrigin(t *testing.T, originDir, ref string) map[string]string {
	t.Helper()
	dir := notesTestClone(t, originDir)
	g := NewGit(dir)
	if err := g.FetchNotes("origin", ref); err != nil {
		t.Fatalf("FetchNotes: %v", err)
	}
	entries, err := g.NotesList(ref)
	if err != nil {
		t.Fatalf("NotesList: %v", err)
	}
	published := make(map[string]string, len(entries))
	for _, e := range entries {
		published[e.Annotated] = e.Content
	}
	return published
}

// TestPushNotes_MergesRemoteNotesOnNonFastForwardRejection covers gt-2rcx: a
// notes ref is append-only and keyed per commit, so two writers adding notes
// for different commits never logically conflict — yet git rejects the second
// push as non-fast-forward. PushNotes must merge the remote ref in and retry
// once instead of reporting a failure that leaves a verdict unpublished in
// every other clone (and, at the `gt mq review` callsite, exits 2
// record_failed).
func TestIntegrationPushNotes_MergesRemoteNotesOnNonFastForwardRejection(t *testing.T) {
	t.Parallel()
	origin, cloneA := notesOriginFixture(t)
	cloneB := notesTestClone(t, origin)
	gA, gB := NewGit(cloneA), NewGit(cloneB)
	const ref = "om"

	// B publishes first; A never fetches, so A's push is the rejected one.
	// B's commit stays unknown to A, as a foreign writer's note would be.
	commitB := commitNotesTestFile(t, cloneB, "b.txt", "b\n", "B change")
	if err := gB.NotesAdd(ref, commitB, `{"mr":"mr-b"}`); err != nil {
		t.Fatalf("B NotesAdd: %v", err)
	}
	if err := gB.PushNotes("origin", ref); err != nil {
		t.Fatalf("B PushNotes: %v", err)
	}

	commitA := commitNotesTestFile(t, cloneA, "a.txt", "a\n", "A change")
	if err := gA.NotesAdd(ref, commitA, `{"mr":"mr-a"}`); err != nil {
		t.Fatalf("A NotesAdd: %v", err)
	}
	if err := gA.PushNotes("origin", ref); err != nil {
		t.Fatalf("PushNotes should merge the remote notes ref and retry, got: %v", err)
	}

	published := notesPublishedOnOrigin(t, origin, ref)
	if got := published[commitA]; got != `{"mr":"mr-a"}` {
		t.Errorf("note on A's commit = %q, want mr-a's", got)
	}
	if got := published[commitB]; got != `{"mr":"mr-b"}` {
		t.Errorf("note on B's commit = %q, want mr-b's (remote notes must survive the merge)", got)
	}

	if _, err := gA.Rev(scratchNotesRef(ref)); err == nil {
		t.Errorf("scratch ref %s outlived the retry", scratchNotesRef(ref))
	}
}

// TestPushNotes_ConflictOnSameCommitFailsClosed: when both writers re-keyed
// the same commit there is no merge that preserves both verdicts, so the push
// must fail loudly — ErrNotesPushConflict — with both the local and the remote
// notes ref left exactly as they were, rather than one silently replacing the
// other.
func TestIntegrationPushNotes_ConflictOnSameCommitFailsClosed(t *testing.T) {
	t.Parallel()
	origin, cloneA := notesOriginFixture(t)
	cloneB := notesTestClone(t, origin)
	gA, gB := NewGit(cloneA), NewGit(cloneB)
	const ref = "om"

	shared, err := gA.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	if err := gB.NotesAdd(ref, shared, `{"mr":"mr-b"}`); err != nil {
		t.Fatalf("B NotesAdd: %v", err)
	}
	if err := gB.PushNotes("origin", ref); err != nil {
		t.Fatalf("B PushNotes: %v", err)
	}

	if err := gA.NotesAdd(ref, shared, `{"mr":"mr-a"}`); err != nil {
		t.Fatalf("A NotesAdd: %v", err)
	}
	err = gA.PushNotes("origin", ref)
	if !errors.Is(err, ErrNotesPushConflict) {
		t.Fatalf("PushNotes on a same-commit conflict = %v, want ErrNotesPushConflict", err)
	}

	local, err := gA.NotesShow(ref, shared)
	if err != nil {
		t.Fatalf("NotesShow after failed push: %v", err)
	}
	if local != `{"mr":"mr-a"}` {
		t.Errorf("local note after abort = %q, want mr-a's own note restored", local)
	}
	if got := notesPublishedOnOrigin(t, origin, ref)[shared]; got != `{"mr":"mr-b"}` {
		t.Errorf("remote note = %q, want mr-b's untouched", got)
	}
	if _, err := gA.Rev(scratchNotesRef(ref)); err == nil {
		t.Errorf("scratch ref %s outlived the failed retry", scratchNotesRef(ref))
	}
}

// TestMergedTree covers the two answers MergedTree distinguishes: the tree a
// conflict-free merge of two revs would have, and an error when the two have
// no conflict-free merge at all.
func TestIntegrationMergedTree(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)
	g := NewGit(dir)
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte(content), 0644); err != nil {
			t.Fatalf("write shared.txt: %v", err)
		}
	}

	write("line-01\nline-02\nline-03\n")
	gitRun("add", ".")
	gitRun("commit", "-m", "base")
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev base: %v", err)
	}

	// Two sides editing different lines of one file: no conflict, so a merge
	// tree exists and holds both edits.
	gitRun("checkout", "-b", "left")
	write("left-01\nline-02\nline-03\n")
	gitRun("add", ".")
	gitRun("commit", "-m", "left edit")

	gitRun("checkout", "-b", "right", base)
	write("line-01\nline-02\nright-03\n")
	gitRun("add", ".")
	gitRun("commit", "-m", "right edit")

	merged, err := g.MergedTree("left", "right")
	if err != nil {
		t.Fatalf("MergedTree: %v", err)
	}
	// Argument order must not matter: it is the same merge either way.
	reversed, err := g.MergedTree("right", "left")
	if err != nil {
		t.Fatalf("MergedTree reversed: %v", err)
	}
	if merged != reversed {
		t.Errorf("MergedTree(left, right) = %s, MergedTree(right, left) = %s; want one tree", merged, reversed)
	}
	for _, side := range []string{"left", "right"} {
		if merged == treeOf(t, g, side) {
			t.Errorf("MergedTree = %s, the %s side's own tree: it did not merge the other side", merged, side)
		}
	}
	if merged == treeOf(t, g, base) {
		t.Errorf("MergedTree = %s, the base tree: it dropped both edits", merged)
	}

	// Both sides now edit the same line differently: no conflict-free merge
	// tree exists, which is an error rather than a tree.
	gitRun("checkout", "left")
	write("left-01\nline-02\nleft-03\n")
	gitRun("add", ".")
	gitRun("commit", "-m", "edit the same line")
	if tree, err := g.MergedTree("left", "right"); err == nil {
		t.Errorf("MergedTree on a conflicting pair = %q, want an error", tree)
	}
}

func treeOf(t *testing.T, g *Git, rev string) string {
	t.Helper()
	tree, err := g.Rev(rev + "^{tree}")
	if err != nil {
		t.Fatalf("Rev %s^{tree}: %v", rev, err)
	}
	return tree
}
