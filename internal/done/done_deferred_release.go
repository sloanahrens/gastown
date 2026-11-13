package done

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// releaseDeferred releases the run's bead on a DEFERRED exit. The store opens
// at the rig directory, as the agent-bead writes do, so it still resolves when
// the worktree is gone. Without a work bead or a seat identity there is no
// claim to release, and the run is left as it is.
func (r *doneRun) releaseDeferred() error {
	if r.issueID == "" || r.sender == "" {
		return nil
	}
	open := r.openSource
	if open == nil {
		open = OpenSourceStore
	}
	bd, _, _ := RoutedIssueBeadsIn(filepath.Join(r.townRoot, r.rigName), r.issueID, open)
	return releaseDeferredBead(bd, r.issueID, r.sender)
}

// releaseDeferredBead returns the bead a seat paused on DEFERRED to the open,
// unassigned pool, so the seat's claim does not outlive its session (gt-tq23e9).
// The attached molecule is left as it is, as for any DEFERRED exit.
//
// It acts only on a bead the seat holds, or one with no assignee. A bead that
// is closed (or tombstoned), a workflow step (its DEFERRED is a completion), in
// landing (gt:ready-to-land, which the landing worker owns), a rig identity
// bead, or claimed by another actor is left exactly as it is, and so is a bead
// that no longer exists. The status, the assignee and the DEFERRED note go in
// one bd update, so the release is one Dolt commit.
//
// A read or write failure is returned, not logged, so gt done exits non-zero.
// The agent-bead write ahead of this release has already cleared the hook; the
// release itself keys on the issue id, not on the hook.
func releaseDeferredBead(bd beads.Client, issueID, seat string) error {
	issue, err := bd.Show(issueID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("reading %s to release it on DEFERRED: %w", issueID, err)
	}
	switch {
	case beads.IssueStatus(issue.Status).IsTerminal():
		return nil
	case strings.Contains(issueID, "-wfs-"):
		// A workflow step's DEFERRED exit is a completion, not a pause (the
		// agent-state write closes it), so it is never reopened.
		return nil
	case beads.HasLabel(issue, land.LabelReadyToLand):
		return nil
	case beads.HasLabel(issue, "gt:rig"):
		return nil
	case issue.Assignee != "" && issue.Assignee != seat:
		return nil
	}
	open, unassigned := string(beads.StatusOpen), ""
	note := fmt.Sprintf("DEFERRED by %s: work paused; the bead is open and unassigned for the next sling", seat)
	// Force is needed because the store fences clearing the assignee of an
	// in_progress bead. The fence is the seat's own claim, which the check above
	// has just established, so overriding it releases nothing that is not this
	// seat's.
	if err := bd.Update(issueID, beads.UpdateOptions{
		Status:      &open,
		Assignee:    &unassigned,
		AppendNotes: note,
		Force:       true,
	}); err != nil {
		return fmt.Errorf("releasing %s on DEFERRED: %w", issueID, err)
	}
	return nil
}
