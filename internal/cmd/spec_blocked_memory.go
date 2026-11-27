package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// specRigBlocked is a rig's blocked list under the hashes and time it was read.
// Only its hashes and age decide reuse (gt-bbtsi4).
type specRigBlocked struct {
	Head    string             `json:"head"`
	Working string             `json:"working"`
	At      time.Time          `json:"at"`
	Issues  []specBlockedIssue `json:"issues,omitempty"`
}

// specBlockedIssue keeps the fields a reused list is read for.
type specBlockedIssue struct {
	ID        string   `json:"id"`
	Priority  int      `json:"priority"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// rigBlocked answers rig's stored list while heads holds the hashes it was read
// under and it is under five minutes old at now; otherwise nothing.
func (a *specAnnotated) rigBlocked(rig string, heads map[string]beadsql.RigHash, now time.Time) ([]*beads.Issue, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	hash, inHeads := heads[rig]
	last, known := a.blocked[rig]
	age := now.Sub(last.At)
	if !inHeads || !known || hash.Head == "" || hash.Working == "" ||
		hash.Head != last.Head || hash.Working != last.Working || age < 0 || age >= 5*time.Minute {
		return nil, false
	}
	issues := make([]*beads.Issue, len(last.Issues))
	for i, stored := range last.Issues {
		issues[i] = &beads.Issue{ID: stored.ID, Priority: stored.Priority, BlockedBy: stored.BlockedBy}
	}
	return issues, true
}
