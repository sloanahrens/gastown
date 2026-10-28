package sling

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// Refusal is the error text a sling of beadID gets when Admit found blockers:
// it names each one and the override that dispatches the bead anyway. Every
// dispatch path refuses with this text, so the operator sees the same message
// whether the sling was direct, queued or batched (gt-e3hl2).
func Refusal(beadID string, blockers []string) string {
	return fmt.Sprintf("%s %s is blocked by unresolved dependencies:\n  - %s\nClose the blockers first, or pass --ignore-blockers to dispatch it anyway",
		dispatch.SlingRefusalMarker, beadID, strings.Join(blockers, "\n  - "))
}

// Admit reports why a bead with these blocking dependencies may not be
// dispatched: one reason per unresolved blocker, naming the blocker and the
// status the caller read for it. No reasons admits the dispatch.
//
// The list is one bd show's dependencies. An empty or nil list reads as no
// blockers, so a caller whose read failed refuses the dispatch itself instead
// of asking Admit about the list it does not have (gt-o70a9).
func Admit(blockers []beads.IssueDep) []string {
	var reasons []string
	seen := make(map[string]bool, len(blockers))
	for _, dep := range blockers {
		reason := refusal(dep)
		if reason == "" || seen[reason] {
			continue
		}
		seen[reason] = true
		reasons = append(reasons, reason)
	}
	return reasons
}

// refusal is one edge's reason to refuse, or "" when the edge admits the
// dispatch.
func refusal(dep beads.IssueDep) string {
	// An unread relation refuses: beads reads an empty relation as
	// non-blocking, and a dispatch that agreed would sling work over an edge
	// the read never classified (gt-o70a9).
	if strings.TrimSpace(dep.DependencyType) == "" {
		return fmt.Sprintf("%s (%s, dependency type unread)", blockerLabel(dep), edgeStatus(dep))
	}
	// Asked one edge at a time, because the list form — UnresolvedBlockerIDs —
	// drops an unresolved edge whose id will not extract, and that is exactly
	// the edge to refuse. HasUnresolvedBlockers counts it (gt-o70a9).
	if !beads.HasUnresolvedBlockers(&beads.Issue{Dependencies: []beads.IssueDep{dep}}) {
		return ""
	}
	return fmt.Sprintf("%s (%s)", blockerLabel(dep), edgeStatus(dep))
}

// blockerLabel is the id a reason names, with a stand-in when the edge carries
// none: the refusal must still name the edge, and a reader with no id has
// nothing to look up.
func blockerLabel(dep beads.IssueDep) string {
	if id := strings.TrimSpace(beads.ExtractIssueID(dep.ID)); id != "" {
		return id
	}
	return "a blocker with an unreadable id"
}

// edgeStatus is the status the caller's read reported for the edge, or a
// stand-in when it reported none. Admit echoes the status; beads decides what
// it resolves (gt-o70a9).
func edgeStatus(dep beads.IssueDep) string {
	if status := strings.TrimSpace(dep.Status); status != "" {
		return status
	}
	return "status unread"
}
