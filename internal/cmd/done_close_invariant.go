package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// closeTimeMRFinder is the narrow interface closeTimeInvariantSkipReason
// needs to evaluate invariant exit (b). *beads.Beads satisfies it.
type closeTimeMRFinder interface {
	FindOpenMRsForIssue(issueID string) ([]*beads.Issue, error)
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
//	(b) an open MR bead tracks it (the merge-request beads created by
//	    gt done back-link the source issue; see beads.FindOpenMRsForIssue), or
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
func closeTimeInvariantSkipReason(mrFinder closeTimeMRFinder, counter closeTimeCommitCounter, issueID, branch, target, closeReason string) string {
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
	if mrFinder != nil && strings.TrimSpace(issueID) != "" {
		if openMRs, err := mrFinder.FindOpenMRsForIssue(issueID); err == nil && len(openMRs) > 0 {
			return ""
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
// and the rig's default branch (target), then evaluates
// closeTimeInvariantSkipReason for issueID. This is the wrapper gt done's
// routine self-close of the hooked bead calls — the "gt done closes at
// submit" path gt-6hmz targets, which previously closed the bead on the
// strength of a cached active_mr field alone, without re-verifying that an
// MR was actually still open at close time.
//
// Branch/target resolution failures fail OPEN (return "", allowing the
// close): this check runs for every role (polecat, crew, mayor, deacon),
// not just polecats mid-submit, and git state is not always meaningful in
// every one of those contexts. Blocking on an inconclusive check would risk
// false positives town-wide; the existing doneSourceCloseSkipReason gate
// this supplements already covers the cases that must always block.
func doneCloseTimeInvariantSkipReason(bd *beads.Beads, cwd, townRoot, rigName, issueID string) string {
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
	return closeTimeInvariantSkipReason(bd, g, issueID, branch, defaultBranch, "")
}
