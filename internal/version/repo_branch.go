package version

import (
	"fmt"
	"path/filepath"
)

// GetRepoRootForTownBranch returns the branch-checkout gt source belonging to
// the town at townRoot: the town's gastown rig directory (<town>/gastown), or
// the mayor's worktree under it (<town>/gastown/mayor/rig) when the source
// lives there.
//
// Only a caller that must have a branch checkout uses it. The native installer
// that gt update runs refuses a checkout that is not on main and fast-forwards
// it with --ff-only; the job-owned build checkout GetRepoRootForTown returns is
// detached and cannot serve that caller.
//
// The installer and update commands are retired under gt-yjzepk, which deletes
// them; this lookup and its allowlist line in
// internal/mayorrig/testdata/mayor-rig-allow.txt go with them.
func GetRepoRootForTownBranch(townRoot string) (string, error) {
	for _, candidate := range townBranchSourceCandidates(townRoot) {
		if hasGtSource(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("town %s has no branch gt source checkout (no %s below it)", townRoot,
		filepath.Join("gastown", "cmd", "gt", "main.go"))
}

// townBranchSourceCandidates lists the branch checkouts a town can keep the gt
// source in: the gastown rig directory canonically, or the mayor's worktree
// under it.
func townBranchSourceCandidates(townRoot string) []string {
	return []string{
		filepath.Join(townRoot, "gastown"),
		filepath.Join(townRoot, "gastown", "mayor", "rig"),
	}
}
