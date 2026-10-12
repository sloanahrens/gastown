//go:build integration

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// The all-rigs local sweep against real temp repos: which branches a real
// git deletes, keeps, or refuses, and the per-rig and total counts the report
// renders. The rules themselves are unit-tested against fakes in
// polecat_prune_local_test.go. No network: every remote here is a file path.

// initLocalSweepOrigin builds the origin a rig repo tracks: main, plus a
// work branch carrying a commit main does not have.
func initLocalSweepOrigin(t *testing.T, tmp string) string {
	t.Helper()
	remoteDir := filepath.Join(tmp, "origin.git")
	seedDir := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", "--initial-branch", "main", remoteDir)
	runGit(t, tmp, "clone", remoteDir, seedDir)
	runGit(t, seedDir, "config", "user.email", "test@test.com")
	runGit(t, seedDir, "config", "user.name", "Test User")
	writePolecatPruneTestFile(t, filepath.Join(seedDir, "README.md"), "test\n")
	runGit(t, seedDir, "add", "README.md")
	runGit(t, seedDir, "commit", "-m", "initial")
	runGit(t, seedDir, "push", "-u", "origin", "main")
	runGit(t, seedDir, "checkout", "-b", "work")
	writePolecatPruneTestFile(t, filepath.Join(seedDir, "work.txt"), "work\n")
	runGit(t, seedDir, "add", "work.txt")
	runGit(t, seedDir, "commit", "-m", "work")
	runGit(t, seedDir, "push", "-u", "origin", "work")
	return remoteDir
}

// initLocalSweepRigRepo builds a rig's shared bare repo pointed at origin,
// with origin/main fetched and a local main, the shape a real .repo.git has.
func initLocalSweepRigRepo(t *testing.T, repoDir, remoteDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(repoDir), 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	runGit(t, filepath.Dir(repoDir), "init", "--bare", "--initial-branch", "main", repoDir)
	runGit(t, repoDir, "remote", "add", "origin", remoteDir)
	runGit(t, repoDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, repoDir, "fetch", "origin")
	runGit(t, repoDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGit(t, repoDir, "branch", "main", "refs/remotes/origin/main")
}

func localSweepBranchExists(t *testing.T, g *git.Git, branch string) bool {
	t.Helper()
	exists, err := g.BranchExists(branch)
	if err != nil {
		t.Fatalf("BranchExists(%s): %v", branch, err)
	}
	return exists
}

// TestIntegrationPruneLocalPolecatBranchBacklogCountsTempRigRepos pins the
// counts a real sweep reports for two rigs: the merged branch that goes, the
// unmerged and worktree-held branches that stay, the rig it cannot read, and
// the totals over all three. The dry run must delete nothing.
func TestIntegrationPruneLocalPolecatBranchBacklogCountsTempRigRepos(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remoteDir := initLocalSweepOrigin(t, tmp)

	repoDir := filepath.Join(tmp, "goodrig", ".repo.git")
	initLocalSweepRigRepo(t, repoDir, remoteDir)
	merged := "polecat/merged/gt-1+aa"
	unmerged := "polecat/unmerged/gt-2+bb"
	checkedOut := "polecat/checked/gt-3+cc"
	runGit(t, repoDir, "branch", merged, "refs/remotes/origin/main")
	runGit(t, repoDir, "branch", unmerged, "refs/remotes/origin/work")
	runGit(t, repoDir, "branch", checkedOut, "refs/remotes/origin/main")
	// A non-polecat branch in the same repo: the sweep's pattern must not
	// reach it.
	runGit(t, repoDir, "branch", "dog/alpha-gastown-1", "refs/remotes/origin/main")
	runGit(t, repoDir, "worktree", "add", filepath.Join(tmp, "checked-worktree"), checkedOut)

	repoGit := git.NewGitWithDir(repoDir, "")

	targets := []localPruneTarget{
		{Name: "goodrig", RepoDir: repoDir},
		{Name: "norepo"},
	}

	var dryRun bytes.Buffer
	dryTotals := pruneLocalPolecatBranchBacklog(&dryRun, targets, openRealLocalPruneRepo, false)
	for _, want := range []string{"goodrig", "would delete: 1", merged, "kept, checked out: 1", checkedOut, "kept, unmerged: 1", unmerged, "norepo"} {
		if !strings.Contains(dryRun.String(), want) {
			t.Errorf("dry run report %q lacks %q", dryRun.String(), want)
		}
	}
	if dryTotals != (localPruneTotals{Deleted: 1, Held: 1, Unmerged: 1, Skipped: 1}) {
		t.Errorf("dry run totals %+v, want one of each decision and one skipped rig", dryTotals)
	}
	for _, branch := range []string{merged, unmerged, checkedOut} {
		if !localSweepBranchExists(t, repoGit, branch) {
			t.Errorf("dry run deleted %s", branch)
		}
	}

	var applied bytes.Buffer
	appliedTotals := pruneLocalPolecatBranchBacklog(&applied, targets, openRealLocalPruneRepo, true)
	if appliedTotals != (localPruneTotals{Deleted: 1, Held: 1, Unmerged: 1, Skipped: 1}) {
		t.Errorf("applied totals %+v, want one of each decision and one skipped rig", appliedTotals)
	}
	if !strings.Contains(applied.String(), "deleted: 1") {
		t.Errorf("applied report %q should report a delete", applied.String())
	}
	if localSweepBranchExists(t, repoGit, merged) {
		t.Errorf("applied sweep left the merged branch %s in place", merged)
	}
	for _, branch := range []string{unmerged, checkedOut, "dog/alpha-gastown-1"} {
		if !localSweepBranchExists(t, repoGit, branch) {
			t.Errorf("applied sweep deleted %s, which it must keep", branch)
		}
	}
}

// TestIntegrationPruneLocalPolecatBranchBacklogSweepsMayorRigLayout pins the
// second repo layout a rig can have: without .repo.git the branches live in
// the mayor/rig clone, which is a working tree and not a bare repo.
func TestIntegrationPruneLocalPolecatBranchBacklogSweepsMayorRigLayout(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remoteDir := initLocalSweepOrigin(t, tmp)

	cloneDir := filepath.Join(tmp, "legacyrig", "mayor", "rig")
	if err := os.MkdirAll(filepath.Dir(cloneDir), 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	runGit(t, tmp, "clone", remoteDir, cloneDir)
	runGit(t, cloneDir, "config", "user.email", "test@test.com")
	runGit(t, cloneDir, "config", "user.name", "Test User")
	merged := "polecat/legacy/gt-4+dd"
	runGit(t, cloneDir, "branch", merged, "origin/main")

	if got := rigPruneRepoDir(filepath.Dir(filepath.Dir(cloneDir))); got != cloneDir {
		t.Fatalf("rigPruneRepoDir = %q, want the mayor/rig clone %q", got, cloneDir)
	}

	var out bytes.Buffer
	totals := pruneLocalPolecatBranchBacklog(&out, []localPruneTarget{{Name: "legacyrig", RepoDir: cloneDir}}, openRealLocalPruneRepo, true)
	if totals != (localPruneTotals{Deleted: 1}) {
		t.Fatalf("totals %+v, want one delete and no skip; report %q", totals, out.String())
	}
	if localSweepBranchExists(t, git.NewGit(cloneDir), merged) {
		t.Errorf("merged branch %s survived the sweep", merged)
	}
}

// TestIntegrationPruneLocalPolecatBranchBacklogFailsClosedWithoutBaseRef pins
// fail closed on a real repo: a rig whose origin/main cannot be resolved is
// skipped, and even --apply leaves its branches alone.
func TestIntegrationPruneLocalPolecatBranchBacklogFailsClosedWithoutBaseRef(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remoteDir := initLocalSweepOrigin(t, tmp)

	repoDir := filepath.Join(tmp, "noorigin", ".repo.git")
	if err := os.MkdirAll(filepath.Dir(repoDir), 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	runGit(t, tmp, "init", "--bare", "--initial-branch", "main", repoDir)
	// Fetch without an origin remote: the branches exist, the base ref does
	// not, so nothing about them can be judged merged.
	runGit(t, repoDir, "fetch", remoteDir, "+refs/heads/*:refs/polecatseed/*")
	orphan := "polecat/orphan/gt-5+ee"
	runGit(t, repoDir, "branch", orphan, "refs/polecatseed/main")

	var out bytes.Buffer
	totals := pruneLocalPolecatBranchBacklog(&out, []localPruneTarget{{Name: "noorigin", RepoDir: repoDir}}, openRealLocalPruneRepo, true)

	if totals != (localPruneTotals{Skipped: 1}) {
		t.Fatalf("totals %+v, want the rig skipped; report %q", totals, out.String())
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("report %q should say the rig was skipped", out.String())
	}
	if !localSweepBranchExists(t, git.NewGitWithDir(repoDir, ""), orphan) {
		t.Errorf("rig with no base ref deleted %s", orphan)
	}
}

// TestIntegrationPruneLocalPolecatBranchBacklogKeepsTheReposOwnHeadBranch
// pins the guard a bare repo needs: its worktree entry carries no branch, so
// the branch HEAD points at is only visible by asking for it — and git will
// delete it happily, leaving HEAD dangling.
func TestIntegrationPruneLocalPolecatBranchBacklogKeepsTheReposOwnHeadBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remoteDir := initLocalSweepOrigin(t, tmp)
	repoDir := filepath.Join(tmp, "headrig", ".repo.git")
	initLocalSweepRigRepo(t, repoDir, remoteDir)

	merged := "polecat/head/gt-7+gg"
	runGit(t, repoDir, "branch", merged, "refs/remotes/origin/main")
	runGit(t, repoDir, "symbolic-ref", "HEAD", "refs/heads/"+merged)

	var out bytes.Buffer
	totals := pruneLocalPolecatBranchBacklog(&out, []localPruneTarget{{Name: "headrig", RepoDir: repoDir}}, openRealLocalPruneRepo, true)

	if totals != (localPruneTotals{Held: 1}) {
		t.Fatalf("totals %+v, want the branch the rig's HEAD holds kept; report %q", totals, out.String())
	}
	repoGit := git.NewGitWithDir(repoDir, "")
	if !localSweepBranchExists(t, repoGit, merged) {
		t.Errorf("sweep deleted %s, the branch the rig repo's HEAD points at", merged)
	}
	if head, err := repoGit.CurrentBranch(); err != nil || head != merged {
		t.Errorf("HEAD now reads %q (err %v), want %s", head, err, merged)
	}
}

// TestIntegrationRealLocalPruneRepoNeverForcesADelete pins the delete the
// sweep is allowed to make: a branch git itself refuses to delete must
// survive, which a force delete (`git branch -D`) would not. The branch is
// created without an upstream on purpose — `-d` accepts a branch merged into
// its upstream, so a tracked branch is not a case it refuses.
func TestIntegrationRealLocalPruneRepoNeverForcesADelete(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remoteDir := initLocalSweepOrigin(t, tmp)
	repoDir := filepath.Join(tmp, "unmergedrig", ".repo.git")
	initLocalSweepRigRepo(t, repoDir, remoteDir)
	unmerged := "polecat/unmerged/gt-8+hh"
	runGit(t, repoDir, "branch", "--no-track", unmerged, "refs/remotes/origin/work")

	repo := openRealLocalPruneRepo(repoDir)
	if err := repo.DeleteBranch(unmerged); err == nil {
		t.Errorf("DeleteBranch(%s) succeeded on a branch that is not merged into origin/main", unmerged)
	}
	if !localSweepBranchExists(t, git.NewGitWithDir(repoDir, ""), unmerged) {
		t.Errorf("a failed delete of %s still removed it", unmerged)
	}
}

// TestIntegrationRunPolecatPruneAllRigsSweepsEveryRigInTheTownConfig pins the
// command-level path: --all-rigs resolves the rigs from the town config, and
// without --apply it reports a dry run and deletes nothing.
func TestIntegrationRunPolecatPruneAllRigsSweepsEveryRigInTheTownConfig(t *testing.T) {
	townRoot, rigName := setupTestRigForSettings(t)
	t.Chdir(townRoot)
	tmp := t.TempDir()
	remoteDir := initLocalSweepOrigin(t, tmp)
	repoDir := filepath.Join(townRoot, rigName, ".repo.git")
	initLocalSweepRigRepo(t, repoDir, remoteDir)
	merged := "polecat/merged/gt-6+ff"
	runGit(t, repoDir, "branch", merged, "refs/remotes/origin/main")
	addTownRigWithoutRepo(t, townRoot, "norepo")

	oldAllRigs, oldApply := polecatPruneAllRigs, polecatPruneApply
	oldDryRun, oldRemote := polecatPruneDryRun, polecatPruneRemote
	polecatPruneAllRigs, polecatPruneApply = true, false
	// The other prune flags belong to the single-rig form; this run must see
	// them clear however a neighboring test left them.
	polecatPruneDryRun, polecatPruneRemote = false, false
	t.Cleanup(func() {
		polecatPruneAllRigs, polecatPruneApply = oldAllRigs, oldApply
		polecatPruneDryRun, polecatPruneRemote = oldDryRun, oldRemote
	})

	out := captureStdout(t, func() {
		if err := runPolecatPrune(nil, nil); err != nil {
			t.Fatalf("runPolecatPrune --all-rigs: %v", err)
		}
	})
	for _, want := range []string{"dry run", rigName, "would delete: 1", merged, "norepo", "skipped: no git repo", "Total: would delete 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if !localSweepBranchExists(t, git.NewGitWithDir(repoDir, ""), merged) {
		t.Errorf("--all-rigs without --apply deleted %s", merged)
	}
}

// addTownRigWithoutRepo registers a second rig in the town config and creates
// its directory, so the sweep has a rig it must report and skip.
func addTownRigWithoutRepo(t *testing.T, townRoot, rigName string) {
	t.Helper()
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigs, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		t.Fatalf("load rigs config: %v", err)
	}
	rigs.Rigs[rigName] = config.RigEntry{
		GitURL:  "git@github.com:test/" + rigName + ".git",
		AddedAt: time.Now().Truncate(time.Second),
	}
	if err := config.SaveRigsConfig(rigsPath, rigs); err != nil {
		t.Fatalf("save rigs config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, rigName), 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
}
