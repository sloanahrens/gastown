package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// mrSupersedeStore is the slice of the beads API the supersede step needs. A
// narrow interface so the loop is testable without a Dolt server.
type mrSupersedeStore interface {
	FindOpenMRsForIssue(issueID string) ([]*beads.Issue, error)
	CloseWithReason(reason string, ids ...string) error
}

// agentActiveMRClearer clears an agent bead's active_mr pointer, but only when
// it still names the expected MR. *beads.Beads obtained through ForAgentBead
// implements it, so the agent bead's own database resolves.
type agentActiveMRClearer interface {
	ClearAgentActiveMRIfMatches(id string, expectedMR string) (bool, error)
}

// supersededMR records one merge request closed by a newer submission.
type supersededMR struct {
	// ID is the MR that was closed.
	ID string
	// AgentBead is the agent bead of the worker that submitted it, when the
	// MR recorded one (or one could be derived from its rig + worker).
	AgentBead string
	// AgentCleared reports that the dangling active_mr pointer on AgentBead
	// was actually cleared.
	AgentCleared bool
}

// supersedeOpenMRsForIssue closes every open MR for issueID older than keep,
// the submission just created, and clears the superseded worker's agent-bead
// active_mr pointer. It prints nothing about successes; callers report what it
// returns (failures warn here, since the submission itself succeeded).
//
// Closing the old MR is queue hygiene (GH#3032). Clearing the pointer is not:
// the worker that submitted the old MR is usually a *different* polecat — a
// deacon redispatch with resume_branch after a rejection, or a re-sling — and
// nothing else ever clears its active_mr. A pointer at a closed (and, after the
// witness's wisp GC, deleted) MR reads as "active_mr=<id> status=closed" and
// blocks reuse forever in both directions of the reuse gate: the inventory
// reports the polecat idle-pr-open/reusable=false, and AssessActiveMR keeps it
// pending while the source issue is still open — which it is, because another
// polecat now owns it. The slot never returns to the allocator (gt-c5uv).
//
// A no-op when there is no replacement MR: superseding without one would leave
// the issue with nothing in the queue.
//
// gt-2x9sc: two `gt done` processes can race to submit for the same issue —
// observed when a polecat's Stop hook auto-ran a second `gt done` while the
// first was still finishing. Each process's submission sees the other's MR as
// "old" and, with no ordering check, both closed each other as "superseded" in
// the same instant: two CLOSED MRs pointing at each other, neither open, the
// verified work gone from the queue. mrSupersedes gives every caller the same
// answer for the same pair regardless of which one asks, so only the
// direction where keep is actually newer (by created_at, then id as a
// tiebreak) ever closes anything — the cycle can't form.
func supersedeOpenMRsForIssue(store mrSupersedeStore, agents agentActiveMRClearer, issueID string, keep *beads.Issue, townRoot, rigName string) []supersededMR {
	if issueID == "" || keep == nil || keep.ID == "" {
		return nil
	}
	oldMRs, err := store.FindOpenMRsForIssue(issueID)
	if err != nil {
		return nil
	}

	var superseded []supersededMR
	for _, old := range oldMRs {
		if old == nil || old.ID == "" || old.ID == keep.ID {
			continue // keep the one just submitted
		}
		if !mrSupersedes(keep, old) {
			continue // old wins the tiebreak; leave it open for its own supersede call to close keep
		}
		reason := fmt.Sprintf("superseded by %s", keep.ID)
		if closeErr := store.CloseWithReason(reason, old.ID); closeErr != nil {
			style.PrintWarning("could not supersede old MR %s: %v", old.ID, closeErr)
			continue
		}

		entry := supersededMR{ID: old.ID, AgentBead: supersededMRAgentBead(old, townRoot, rigName)}
		if entry.AgentBead != "" && agents != nil {
			cleared, clearErr := agents.ClearAgentActiveMRIfMatches(entry.AgentBead, old.ID)
			if clearErr != nil {
				style.PrintWarning("could not clear active_mr on %s after superseding %s: %v", entry.AgentBead, old.ID, clearErr)
			}
			entry.AgentCleared = cleared
		}
		superseded = append(superseded, entry)
	}
	return superseded
}

// mrSupersedes reports whether a should supersede b: a strictly later
// CreatedAt, or, on a tie (the same second, unparseable, or both empty), a
// strictly greater ID. ID is a stand-in total order for creation order the
// database doesn't expose once two MRs land in the same second — which is
// exactly when two racing `gt done` processes each need the same answer.
//
// This differs from mrIsNewerThan (polecat_inventory.go), which leaves a
// timestamp tie to the incumbent: fine for picking one MR to *display*, but
// unusable here, since two racing processes each see the other as the
// "incumbent" and neither would supersede the other — the queue keeps both,
// silently doubling every raced submission instead of resolving it.
//
// The property that matters: mrSupersedes(a, b) and mrSupersedes(b, a) never
// both return true, so two racing calls computing it for the same pair, in
// either order, agree on which one wins — a mutual-close cycle (gt-2x9sc)
// can't form.
func mrSupersedes(a, b *beads.Issue) bool {
	at, bt := beads.ParseIssueTime(a.CreatedAt), beads.ParseIssueTime(b.CreatedAt)
	if !at.Equal(bt) {
		return at.After(bt)
	}
	return a.ID > b.ID
}

// carriedMRPriority returns the priority a replacement MR should carry for
// issueID, and the superseded MR it came from ("" when the derived priority
// stands).
//
// An MR's priority is derived from its source issue at submit time, so a bump
// applied to the MR itself exists only on that bead. Re-deriving on resubmit
// would drop it — or5's bumped P0 returned as its replacement's P2 (gt-m7fm),
// costing a P0 its place in the queue. Only a strictly better priority carries:
// a superseded MR left lower than its source issue cannot pull the replacement
// down.
//
// An unreadable queue returns the derived priority: inheritance is queue
// hygiene and must not fail a submission that already succeeded.
func carriedMRPriority(store mrSupersedeStore, issueID string, derived int) (int, string) {
	if issueID == "" || store == nil {
		return derived, ""
	}
	oldMRs, err := store.FindOpenMRsForIssue(issueID)
	if err != nil {
		return derived, ""
	}

	from := ""
	for _, old := range oldMRs {
		// P0 is the highest priority, so the lowest number wins. A negative
		// priority is bd's "unset", not a bump.
		if old != nil && old.Priority >= 0 && old.ID != "" && old.Priority < derived {
			derived, from = old.Priority, old.ID
		}
	}
	return derived, from
}

// supersededMRAgentBead resolves the agent bead of the worker that submitted a
// now-superseded MR, from the MR's own description: the agent_bead field
// `gt done` writes, else the polecat agent bead implied by the branch's worker
// name and the MR's rig (an MR created by `gt mq submit` carries no agent_bead).
//
// Deriving the second form is a guess, but a safe one: the only caller clears
// through ClearAgentActiveMRIfMatches, which no-ops unless that bead's
// active_mr names this exact MR — a wrong or absent id clears nothing, and a
// polecat whose bead id doesn't match the branch name (case, say) is left to
// the recovery paths rather than cleared by accident.
func supersededMRAgentBead(mr *beads.Issue, townRoot, rigName string) string {
	fields := beads.ParseMRFields(mr)
	if fields == nil {
		return ""
	}
	if agentBead := strings.TrimSpace(fields.AgentBead); agentBead != "" {
		return agentBead
	}

	worker := strings.TrimSpace(fields.Worker)
	if worker == "" {
		return ""
	}
	rig := strings.TrimSpace(fields.Rig)
	if rig == "" {
		rig = rigName
	}
	if rig == "" {
		return ""
	}
	prefix := beads.GetPrefixForRig(townRoot, rig)
	if prefix == "" {
		return ""
	}
	return beads.PolecatBeadIDWithPrefix(prefix, rig, worker)
}
