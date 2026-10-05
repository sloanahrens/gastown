package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// closeTimeCommitCounter is the narrow interface closeTimeInvariantSkipReason
// needs to count the branch's commits. *git.Git satisfies it.
type closeTimeCommitCounter interface {
	CommitsAhead(base, branch string) (int, error)
}

// closeTimeInvariantSkipReason enforces gt-6hmz: a bead that is the
// source_issue of polecat work may transition to CLOSED only if one of
// these holds:
//
//	(a) its branch has zero commits the target branch lacks
//	    (git rev-list --count target..branch == 0), or
//	(b) closeReason is an explicit operator override — a "supersede:" or
//	    "cancel:" prefix, recorded as the close reason on the bead.
//
// Work with commits to land is closed by the landing worker when it lands,
// never by the agent that wrote it (ADR 0004).
//
// Returns "" when the close may proceed. Returns a non-empty refusal
// message — naming the branch and the unmerged commit count — when neither
// holds; callers must leave the bead open in that case rather than closing
// it.
//
// (b) is checked by prefix only. The invariant's "written by an operator"
// qualifier is enforced socially — the reason is permanently recorded on
// the bead as the close reason, giving a durable audit trail — not by
// verifying actor identity here. No actor signal available to this check
// is unforgeable enough to add real access control, so gating on one would
// add complexity without adding safety.
func closeTimeInvariantSkipReason(counter closeTimeCommitCounter, issueID, branch, target, closeReason string) string {
	if hasOperatorOverridePrefix(closeReason) {
		return ""
	}
	branch = strings.TrimSpace(branch)
	target = strings.TrimSpace(target)
	if counter == nil || branch == "" || target == "" {
		// No git context to evaluate the branch state — don't refuse an
		// otherwise-legitimate close on an inconclusive check.
		return ""
	}
	aheadCount, err := counter.CommitsAhead(target, branch)
	if err != nil || aheadCount == 0 {
		return ""
	}
	return fmt.Sprintf(
		"branch %s has %d commit(s) not on %s and %s is not landed — refusing close (gt-6hmz close-time invariant)",
		branch, aheadCount, target, issueID,
	)
}

// hasOperatorOverridePrefix reports whether reason is an explicit operator
// override per gt-6hmz condition (b): a "supersede:" or "cancel:" prefix.
func hasOperatorOverridePrefix(reason string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(reason))
	return strings.HasPrefix(trimmed, "supersede:") || strings.HasPrefix(trimmed, "cancel:")
}

// doneCloseTimeInvariantSkipReason resolves the current branch (from cwd)
// and the resolved origin/upstream base ref (target), then evaluates
// closeTimeInvariantSkipReason for issueID. It guards gt done's routine
// self-close of the hooked bead.
//
// Branch/target resolution failures fail OPEN (return "", allowing the
// close): this check runs for every role (polecat and crew),
// not just polecats mid-submit, and git state is not always meaningful in
// every one of those contexts. Blocking on an inconclusive check would risk
// false positives town-wide; the existing doneSourceCloseSkipReason gate
// this supplements already covers the cases that must always block.
func doneCloseTimeInvariantSkipReason(cwd, townRoot, rigName, issueID string) string {
	if strings.TrimSpace(issueID) == "" {
		return ""
	}
	g := git.NewGit(cwd)
	branch, target, ok := closeTimeBranchTarget(g, townRoot, rigName)
	if !ok {
		return ""
	}
	return closeTimeInvariantSkipReason(g, issueID, branch, target, "")
}

// closeTimeBranchTarget resolves the branch under test (the cwd's current
// branch) and the ref to compare its commit count against (the resolved
// origin/upstream base ref for the rig's default branch).
//
// Shared by gt done's self-close path and the bd-close-invariant PreToolUse
// guard (gt-arno) so the two agree on which branch is being judged and what
// it's being judged against — a guard that computed the target differently
// from gt done would refuse closes gt done allows, or vice versa.
//
// ok is false when there is nothing meaningful to compare: no branch
// resolvable from cwd (detached HEAD, not a repo), or the current branch IS
// the default branch (nothing to compare against itself).
func closeTimeBranchTarget(g *git.Git, townRoot, rigName string) (branch, target string, ok bool) {
	defaultBranch := "main"
	rigPath := filepath.Join(townRoot, rigName)
	rigCfg, cfgErr := rig.LoadRigConfigIfPresent(rigPath)
	if cfgErr != nil {
		rig.WarnRigConfigOnce(rigPath, cfgErr)
	}
	if rigCfg != nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	branch, err := g.CurrentBranch()
	if err != nil {
		return "", "", false
	}
	if branch == defaultBranch {
		return "", "", false
	}
	// Compare against the resolved origin/upstream base ref, mirroring the
	// submit path's ahead-count resolution (done.go:1191-1202): polecat
	// worktrees are created from origin/<default> (internal/polecat/
	// manager.go:798), so the shared .repo.git's local default branch is
	// routinely stale, and comparing against it directly overcounts commits
	// the polecat never authored — refusing no_merge/review_only/report-only
	// closes that legitimately have zero polecat commits (gt-6hmz
	// om-editorial finding 1). Fall back to the local branch only when the
	// origin ref itself doesn't resolve (e.g. no remote configured).
	target = defaultBranch
	if originRef := g.CleanBaseRef("origin", defaultBranch, ""); originRef != "" {
		if exists, err := g.RefExists(originRef); err == nil && exists {
			target = originRef
		}
	}
	return branch, target, true
}
