package done

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// pushSandboxBranchBeforeDeferral pushes a sandbox seat's branch before a
// DEFERRED or ESCALATED exit, so the commits the exit leaves behind are on
// origin (gt-oq8lc). A host seat's commits survive the exit in its worktree; a
// sandbox clone is the only copy, and the Stop hook (rightly) keeps a sandbox
// with unpushed commits alive, so without the push the work is stranded until
// something rescues it by hand: devops/keel deferred a rebased rework that
// existed only in its sandbox.
//
// It reuses the landing push (a lease on the tip origin had, the diverged-push
// recovery for a rebase or a rework, a read-back of what origin holds). Nothing
// else of a submission runs: no rebase, no gate, no label. A push that fails
// keeps cleanup_status unpushed and says so, and the exit proceeds as it always
// did, with the session left up to protect the commits.
//
// A source bead or target it cannot read still pushes, against the rig
// default, because a stranded branch is the worse outcome; the warning names
// the base the push actually compared against.
func pushSandboxBranchBeforeDeferral(r *doneRun) {
	pushSandboxBranchBeforeDeferralTo(os.Stderr, r)
}

// pushSandboxBranchBeforeDeferralTo is pushSandboxBranchBeforeDeferral warning
// to warn, so a test reads the warnings.
func pushSandboxBranchBeforeDeferralTo(warn io.Writer, r *doneRun) {
	if r.sessionBackend != backendOpenShell || (r.cleanupStatus != "unpushed" && r.cleanupStatus != "has_unpushed") {
		return
	}
	fail := func(format string, args ...any) {
		style.FprintWarning(warn, "could not push %s before deferring: %s; the sandbox keeps the only copy of its commits", r.branch, fmt.Sprintf(format, args...))
	}
	remote, err := r.resolveLandingRemote()
	if err != nil {
		fail("%v", err)
		return
	}
	head, err := r.deps.repo.Rev("HEAD")
	if err != nil {
		fail("resolving HEAD: %v", err)
		return
	}
	var sourceIssue *beads.Issue
	var sourceBD beads.Client
	if r.issueID != "" {
		issue, bd, srcErr := r.deps.source(r.issueID)
		if srcErr != nil {
			style.FprintWarning(warn, "could not read %s to resolve its landing target: %v; pushing against the rig default %s", r.issueID, srcErr, r.defaultBranch)
		} else {
			sourceIssue, sourceBD = issue, bd
		}
	}
	target := r.defaultBranch
	if t, targetErr := resolveDoneTarget(r, sourceIssue); targetErr != nil {
		style.FprintWarning(warn, "could not resolve the landing target for %s: %v; pushing against the rig default %s", r.branch, targetErr, r.defaultBranch)
	} else {
		target = t
	}
	baseRef := r.deps.repo.CleanBaseRef(remote, r.defaultBranch, target)
	if fetchErr := r.deps.repo.Fetch(remote); fetchErr != nil {
		style.FprintWarning(warn, "could not refresh %s before pushing %s: %v; the push compares against the base this clone last fetched", remote, r.branch, fetchErr)
	}
	if err := pushBranchForLanding(r, sourceBD, sourceIssue, head, baseRef); err != nil {
		fail("%v", err)
		return
	}
	r.cleanupStatus = cleanupStatusAfterSuccessfulPush(r.cleanupStatus)
}
