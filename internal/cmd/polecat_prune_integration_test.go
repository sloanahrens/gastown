//go:build integration

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The command-level --remote prune runs: a real rig clone with a file
// origin, the package's flag globals and captured stdout. The prune rules
// and the summary are unit-tested in polecat_prune_test.go.

func TestIntegrationRunPolecatPruneRemoteDryRunIncludesPatchEquivalentBranch(t *testing.T) {
	stubRemotePolecatBranchOpenPR(t)
	townRoot, rigName := setupTestRigForSettings(t)
	localDir := filepath.Join(townRoot, rigName, "mayor", "rig")
	mainBranch := initPolecatPruneTestRepoAt(t, localDir)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("prunecmdpatch", "", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)

	oldRemote, oldDryRun := polecatPruneRemote, polecatPruneDryRun
	polecatPruneRemote = true
	polecatPruneDryRun = true
	t.Cleanup(func() {
		polecatPruneRemote = oldRemote
		polecatPruneDryRun = oldDryRun
	})

	out := captureStdout(t, func() {
		if err := runPolecatPrune(nil, []string{rigName}); err != nil {
			t.Fatalf("runPolecatPrune: %v", err)
		}
	})
	assertRemotePruneDryRunKeptBranch(t, repoGit, out, branch)
}

// TestRunPolecatPruneReportsRemoteBranchKeptForOpenPR pins the command-level
// reporting for the same case: a run that pruned nothing because a branch is
// PR-protected must not report "No stale remote polecat branches found", which
// would say the opposite of what happened.
func TestIntegrationRunPolecatPruneReportsRemoteBranchKeptForOpenPR(t *testing.T) {
	townRoot, rigName := setupTestRigForSettings(t)
	localDir := filepath.Join(townRoot, rigName, "mayor", "rig")
	mainBranch := initPolecatPruneTestRepoAt(t, localDir)
	repoGit := git.NewGit(localDir)
	branch := polecat.FormatGeneratedBranchName("prcmdcat", "", oldEnoughSuffix())
	createPatchEquivalentRemoteBranch(t, repoGit, localDir, mainBranch, branch)
	stubRemotePolecatBranchOpenPR(t, branch)

	oldRemote, oldDryRun := polecatPruneRemote, polecatPruneDryRun
	polecatPruneRemote = true
	polecatPruneDryRun = false
	t.Cleanup(func() {
		polecatPruneRemote = oldRemote
		polecatPruneDryRun = oldDryRun
	})

	out := captureStdout(t, func() {
		if err := runPolecatPrune(nil, []string{rigName}); err != nil {
			t.Fatalf("runPolecatPrune: %v", err)
		}
	})
	if strings.Contains(out, "No stale remote polecat branches found") {
		t.Fatalf("output %q must not claim no stale branches were found", out)
	}
	if !strings.Contains(out, "left in place: open PR exists (gas-fk4)") {
		t.Fatalf("output %q should report the branch kept for an open PR", out)
	}
	assertRemoteBranchStillExists(t, repoGit, branch)
}
