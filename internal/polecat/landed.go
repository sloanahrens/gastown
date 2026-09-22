package polecat

import (
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// LandedEvidence is the git fact "the work this polecat was carrying is already
// contained in the integration branch on origin". Verified is the only thing
// callers may act on; Ref labels what was measured, for diagnostics.
type LandedEvidence struct {
	Verified bool
	Ref      string
}

// LandedEvidenceProbe answers "has this polecat's work landed?" on demand. It
// is a function rather than a bool because the answer costs a git call and is
// only needed for an active_mr that is already known to be gone or closed —
// see AssessActiveMRWithLandedEvidence.
type LandedEvidenceProbe func() LandedEvidence

// ProbeWorkLandedOnRef measures whether the work the active_mr carried is
// already contained in the default branch on remote — the "the source issue's
// work is on origin/main" half of the dangling-active_mr gate (gt-wprt).
//
// It asks about the *submitted* tip, and about the local branch only as a
// stand-in for a submitted branch deleted from origin after its merge. A
// finished polecat's local ref is not a witness that its MR landed: it can have
// been left behind, cross-merged with another polecat's branch, or grown
// auto-checkpoint commits, all of which read as "unpreserved" about work that
// has been on main for days — the stranding this gate exists to end.
//
// Leftover local work is not this probe's business, and ignoring it here does
// not let any through: the reuse gate overlays its own preservation check
// against the polecat's remote branch onto UnpushedCommits (see
// Manager.workstateInputForPolecat), and dirty/stashed worktrees are measured
// independently, so an unpreserved commit, a dirty tree, or a stash still
// blocks reuse on its own.
//
// Fail-closed by construction: every unknown (no worktree, no branch, no such
// ref, an unresolvable integration branch, a git error) returns the zero
// LandedEvidence, so a caller that cannot measure this treats the MR as still
// holding its slot.
func ProbeWorkLandedOnRef(clonePath, branch, remote string) LandedEvidence {
	clonePath = strings.TrimSpace(clonePath)
	branch = strings.TrimSpace(branch)
	if clonePath == "" || branch == "" || branch == "HEAD" {
		return LandedEvidence{}
	}
	if remote == "" {
		remote = "origin"
	}
	g := git.NewGit(clonePath)
	// RemoteDefaultBranch falls back to "main" when it cannot resolve
	// origin/HEAD; an unresolvable ref then fails the preservation checks
	// below, which is the fail-closed answer rather than a wrong one.
	integration := remote + "/" + g.RemoteDefaultBranch()
	for _, head := range []string{remote + "/" + branch, "refs/heads/" + branch} {
		if refPreservedBy(g, head, integration) {
			return LandedEvidence{Verified: true, Ref: integration}
		}
	}
	return LandedEvidence{}
}

// refPreservedBy reports whether head's work is contained in ref: ancestry
// where possible, merge-tree no-op where the work landed by merge or squash.
// Any error — an unresolvable ref, a broken worktree — is "not preserved".
func refPreservedBy(g *git.Git, head, ref string) bool {
	status, err := g.RefPreservedByRef(head, ref)
	return err == nil && status.Preserved
}
