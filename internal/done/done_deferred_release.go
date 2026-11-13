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
	return releaseDeferredBead(bd, r.issueID, r.sender, r.findings)
}

// releaseDeferredBead returns the bead a seat paused on DEFERRED to the open,
// unassigned pool, so the seat's claim does not outlive its session (gt-tq23e9).
// The note names the reason the seat passed (its --finding texts). The attached
// molecule is left as it is, as for any DEFERRED exit.
//
// It acts only on a bead the seat holds, or one with no assignee. A bead is
// left exactly as it is when its status is not actionable (closed, tombstoned,
// or parked as deferred or pinned, which is a person's hold on it), when it is
// a workflow step (its DEFERRED is a completion), when it is in landing
// (gt:ready-to-land, which the landing worker owns), when it is a rig identity
// bead, or when another actor claimed it; a bead that no longer exists is left
// as it is too.
//
// The release is guarded on the assignee and status just read, so another
// actor's claim made between the read and the write is never cleared: a guard
// that no longer holds writes nothing and is not an error. The DEFERRED note
// follows a release that held.
//
// A read or write failure is returned, not logged, so gt done exits non-zero.
// The agent-bead write ahead of this release has already cleared the hook; the
// release itself keys on the issue id, not on the hook.
func releaseDeferredBead(bd beads.Client, issueID, seat string, findings []string) error {
	issue, err := bd.Show(issueID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("reading %s to release it on DEFERRED: %w", issueID, err)
	}
	switch {
	case !beads.IssueStatus(issue.Status).IsActionable():
		// Not actionable: terminal, or parked as deferred or pinned while the
		// seat held it. A parked bead is the person's hold on it, which this
		// seat's exit must not lift (gt-tq23e9).
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
	reason := "work paused"
	if len(findings) > 0 {
		reason = strings.Join(findings, "; ")
	}
	note := fmt.Sprintf("DEFERRED by %s: %s; the bead is open and unassigned for the next sling", seat, reason)
	// The write is guarded on the assignee and status read above (bd's
	// --if-assignee/--if-status), so a claim another actor made after the read
	// is not cleared. A guard that no longer holds writes nothing.
	released, err := beads.TransferIfHeld(bd, issueID, issue.Assignee, issue.Status, open, unassigned)
	if err != nil {
		return fmt.Errorf("releasing %s on DEFERRED: %w", issueID, err)
	}
	if !released {
		return nil
	}
	if err := bd.AppendNotes(issueID, note); err != nil {
		return fmt.Errorf("noting the DEFERRED release of %s: %w", issueID, err)
	}
	return nil
}
