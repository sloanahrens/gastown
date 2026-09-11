package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// closeTimeMRTracker is the narrow interface closeTimeInvariantSkipReason
// needs to evaluate invariant exit (b). *beads.Beads satisfies it.
//
// Exit (b) checks the specific pendingMRID the caller already knows about
// (the MR gt done just created for this issue, or resumed from the agent
// bead's active_mr field) rather than re-listing MRs by issueID/status. This
// sidesteps two production bugs a status-filtered list-by-issue lookup hit:
// a refinery-claimed MR transitions open -> in_progress the moment the
// refinery picks it up (internal/refinery/types.go:183), which a
// status=="open" filter would treat as gone; and MR beads always live in the
// rig beads DB, which callers must pass here directly rather than a
// cross-prefix-routed client (gt-6hmz om-editorial findings 2 and 3).
type closeTimeMRTracker interface {
	Show(id string) (*beads.Issue, error)
}

// closeTimeCommitCounter is the narrow interface closeTimeInvariantSkipReason
// needs to evaluate invariant exit (a). *git.Git satisfies it.
type closeTimeCommitCounter interface {
	CommitsAhead(base, branch string) (int, error)
}

// closeTimeInvariantSkipReason enforces gt-6hmz: a bead that is the
// source_issue of polecat work may transition to CLOSED only if one of
// these holds:
//
//	(a) its branch has zero commits the target branch lacks
//	    (git rev-list --count target..branch == 0), or
//	(b) pendingMRID names an MR bead that is not yet terminal (open or
//	    in_progress) — the merge-request bead gt done created for this issue
//	    and back-linked via the agent bead's active_mr field, or
//	(c) closeReason is an explicit operator override — a "supersede:" or
//	    "cancel:" prefix, recorded as the close reason on the bead.
//
// Returns "" when the close may proceed. Returns a non-empty refusal
// message — naming the branch and the unmerged commit count — when none of
// (a)/(b)/(c) hold; callers must leave the bead open in that case rather
// than closing it.
//
// (c) is checked by prefix only. The invariant's "written by an operator"
// qualifier is enforced socially — the reason is permanently recorded on
// the bead as the close reason, giving a durable audit trail — not by
// verifying actor identity here. No actor signal available to this check
// is unforgeable enough to add real access control, so gating on one would
// add complexity without adding safety.
func closeTimeInvariantSkipReason(mrTracker closeTimeMRTracker, counter closeTimeCommitCounter, issueID, pendingMRID, branch, target, closeReason string) string {
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
	if err != nil {
		return ""
	}
	if aheadCount == 0 {
		return ""
	}
	if mrTracker != nil {
		if id := strings.TrimSpace(pendingMRID); id != "" {
			if mr, err := mrTracker.Show(id); err == nil && mr != nil && !beads.IssueStatus(mr.Status).IsTerminal() {
				return ""
			}
		}
	}
	return fmt.Sprintf(
		"branch %s has %d commit(s) not on %s and no open MR tracks %s — refusing close (gt-6hmz close-time invariant)",
		branch, aheadCount, target, issueID,
	)
}

// hasOperatorOverridePrefix reports whether reason is an explicit operator
// override per gt-6hmz condition (c): a "supersede:" or "cancel:" prefix.
func hasOperatorOverridePrefix(reason string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(reason))
	return strings.HasPrefix(trimmed, "supersede:") || strings.HasPrefix(trimmed, "cancel:")
}

// doneCloseTimeInvariantSkipReason resolves the current branch (from cwd)
// and the resolved origin/upstream base ref (target), then evaluates
// closeTimeInvariantSkipReason for issueID. This is the wrapper gt done's
// routine self-close of the hooked bead calls — the "gt done closes at
// submit" path gt-6hmz targets, which previously closed the bead on the
// strength of a cached active_mr field alone, without re-verifying that an
// MR was actually still open at close time.
//
// bd must be the RIG beads client (not a cross-prefix-routed client) — MR
// beads always live in the rig DB, and pendingMRID is looked up there
// (gt-6hmz om-editorial finding 2). pendingMRID is the MR this same gt done
// invocation just created for issueID (or resumed from the agent bead's
// active_mr field); passing "" means the caller found no such MR, so exit
// (b) can never apply.
//
// Branch/target resolution failures fail OPEN (return "", allowing the
// close): this check runs for every role (polecat, crew, mayor, deacon),
// not just polecats mid-submit, and git state is not always meaningful in
// every one of those contexts. Blocking on an inconclusive check would risk
// false positives town-wide; the existing doneSourceCloseSkipReason gate
// this supplements already covers the cases that must always block.
func doneCloseTimeInvariantSkipReason(bd *beads.Beads, cwd, townRoot, rigName, issueID, pendingMRID string) string {
	if bd == nil || strings.TrimSpace(issueID) == "" {
		return ""
	}
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	g := git.NewGit(cwd)
	branch, err := g.CurrentBranch()
	if err != nil {
		return ""
	}
	if branch == defaultBranch {
		// Not on a feature branch — nothing to compare against itself.
		return ""
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
	target := defaultBranch
	if originRef := g.CleanBaseRef("origin", defaultBranch, ""); originRef != "" {
		if exists, err := g.RefExists(originRef); err == nil && exists {
			target = originRef
		}
	}
	return closeTimeInvariantSkipReason(bd, g, issueID, pendingMRID, branch, target, "")
}
