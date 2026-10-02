package polecat

import (
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// pinBranchUpstream points branch at the branch its work merges into, replacing
// the upstream git inferred from the start point it was cut from.
//
// git writes branch.<b>.remote/.merge at every branch creation whose start point
// is a remote-tracking ref (branch.autoSetupMerge defaults on), and gt creates
// polecat branches from whatever base a dispatch supplies — so a base naming
// another polecat's branch becomes the new branch's upstream (gt-voz8q:
// polecat/malachite/gt-hqji+mturmak0 was created from
// origin/polecat/slate/gt-hqji+mturmak0 and tracked it). Nothing in gt sets an
// upstream, so that inherited pair stands until one is written here.
//
// The inherited upstream is read back as @{u} by the preservation verdict, which
// then weighs the branch against work it did not come from (gt-y6w8y), and by
// GetGitState's ahead/behind counts. A polecat branch tracks where its work
// lands, not where it was cut from — the split resolveSpawnBaseBranch makes for
// the MR target (gt-a8i3).
//
// Failures warn rather than fail the spawn: a missing upstream degrades
// reporting, a refused spawn costs a seat.
//
// baseRef is a remote-tracking ref, e.g. "origin/main" (see polecatBaseRef).
func pinBranchUpstream(g gitRepo, branch, baseRef string) {
	remote, base, ok := strings.Cut(baseRef, "/")
	if !ok || branch == "" || base == "" {
		return
	}
	// merge before remote: written alone it would leave the inherited remote
	// resolving @{u} to a branch that need not exist, and unwritten the pair git
	// made stands unchanged.
	if err := g.ConfigSet("branch."+branch+".merge", "refs/heads/"+base); err != nil {
		style.PrintWarning("could not point branch %s at %s: %v", branch, baseRef, err)
		return
	}
	if err := g.ConfigSet("branch."+branch+".remote", remote); err != nil {
		style.PrintWarning("could not point branch %s at %s: %v", branch, baseRef, err)
	}
}

// branchBaseRef is the remote-tracking ref this manager's new polecat branches
// merge into, given the base the dispatch supplied.
func (m *Manager) branchBaseRef(baseBranch string) string {
	return polecatBaseRef(baseBranch, m.rig.DefaultBranch())
}

// branchBaseRef is branchBaseRef for a session manager's rig.
func (m *SessionManager) branchBaseRef(baseBranch string) string {
	return polecatBaseRef(baseBranch, m.rig.DefaultBranch())
}

// polecatBaseRef resolves a dispatch's base branch to the remote-tracking ref a
// new polecat branch tracks: the base itself when it names a branch, else the
// rig default.
//
// A base naming a polecat branch falls back to the rig default. Such a base is
// either a leaked base_branch (gt-a8i3) or a deliberate stack, and a stack is
// expressed by the MR target rather than by tracking — while tracking it is
// exactly the poison pinBranchUpstream exists to remove (gt-voz8q).
func polecatBaseRef(baseBranch, defaultBranch string) string {
	base := strings.TrimSpace(baseBranch)
	remote := "origin"
	if ref := git.RemoteForRef(base); ref != "" {
		remote, base = ref, strings.TrimPrefix(base, ref+"/")
	}
	if base == "" || strings.HasPrefix(base, polecatBranchPrefix) {
		remote, base = "origin", defaultBranch
	}
	return remote + "/" + base
}
