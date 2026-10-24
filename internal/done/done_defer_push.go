package done

import (
	"fmt"

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
func pushSandboxBranchBeforeDeferral(r *doneRun) {
	if r.sessionBackend != backendOpenShell || (r.cleanupStatus != "unpushed" && r.cleanupStatus != "has_unpushed") {
		return
	}
	fail := func(format string, args ...any) {
		style.PrintWarning("could not push %s before deferring: %s; the sandbox keeps the only copy of its commits", r.branch, fmt.Sprintf(format, args...))
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
		if issue, bd, srcErr := r.deps.source(r.issueID); srcErr == nil {
			sourceIssue, sourceBD = issue, bd
		}
	}
	target := r.defaultBranch
	if t, targetErr := resolveDoneTarget(r, sourceIssue); targetErr == nil {
		target = t
	}
	baseRef := r.deps.repo.CleanBaseRef(remote, r.defaultBranch, target)
	_ = r.deps.repo.Fetch(remote) // best effort: the diverged-push recovery compares against a fresh base
	if err := pushBranchForLanding(r, sourceBD, sourceIssue, head, baseRef); err != nil {
		fail("%v", err)
		return
	}
	r.cleanupStatus = cleanupStatusAfterSuccessfulPush(r.cleanupStatus)
}
