package beads

import (
	"testing"
	"time"
)

func agentIssue(desc string, updated time.Time) *Issue {
	return &Issue{
		ID:          "gt-gastown-polecat-garnet",
		Description: "t\n\nrole_type: polecat\nrig: gastown\n" + desc,
		UpdatedAt:   updated.Format(time.RFC3339),
	}
}

// cleanup_status is deliberately excluded here — it reconciles by severity,
// not recency (see TestMergeLegacyAgentBead_CleanupStatusReconcilesBySeverityNotRecency).
func TestMergeLegacyAgentBead_NewerRowWinsPerField(t *testing.T) {
	older, newer := time.Date(2026, 9, 8, 18, 55, 0, 0, time.UTC), time.Date(2026, 9, 9, 1, 7, 0, 0, time.UTC)
	rig := agentIssue("agent_state: spawning\nhook_bead: gt-911\nbranch: polecat/old\n", older)
	town := agentIssue("agent_state: done\nhook_bead: null\nbranch: polecat/new\n", newer)
	exists := func(string) bool { return true }

	updates, rows := MergeLegacyAgentBead(rig, town, exists)
	if updates.AgentState == nil || *updates.AgentState != "done" {
		t.Fatalf("agent_state: newer town value must win, got %v", updates.AgentState)
	}
	if updates.Branch == nil || *updates.Branch != "polecat/new" {
		t.Fatalf("branch: newer town value must win, got %v", updates.Branch)
	}
	if updates.HookBead == nil || *updates.HookBead != "" {
		t.Fatalf("hook_bead: newer town value (null) must win, got %v", updates.HookBead)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 differing fields: %+v", len(rows), rows)
	}
}

func TestMergeLegacyAgentBead_GhostReferencesAreClearedNeverCopied(t *testing.T) {
	older, newer := time.Date(2026, 9, 8, 18, 55, 0, 0, time.UTC), time.Date(2026, 9, 9, 1, 7, 0, 0, time.UTC)
	// rig row is NEWER and carries a ghost active_mr; town says null.
	rig := agentIssue("agent_state: done\nactive_mr: gt-wisp-0yhh\n", newer)
	town := agentIssue("agent_state: done\nactive_mr: null\n", older)
	exists := func(id string) bool { return id != "gt-wisp-0yhh" }

	updates, rows := MergeLegacyAgentBead(rig, town, exists)
	if updates.ActiveMR == nil || *updates.ActiveMR != "" {
		t.Fatalf("active_mr referencing a missing bead must be cleared, got %v", updates.ActiveMR)
	}
	if len(rows) != 1 || rows[0].Field != "active_mr" || rows[0].Winner != "clear" {
		t.Fatalf("rows = %+v, want one active_mr row with winner=clear", rows)
	}
}

// cleanup_status is the field behind four consecutive fail-open P0s (gt-7kr,
// gt-14a, gt-hsg, gt-ido). Recency must never promote a blocking value to a
// clearing one; severity decides.
func TestMergeLegacyAgentBead_CleanupStatusReconcilesBySeverityNotRecency(t *testing.T) {
	older, newer := time.Date(2026, 9, 8, 18, 55, 0, 0, time.UTC), time.Date(2026, 9, 9, 1, 7, 0, 0, time.UTC)
	exists := func(string) bool { return true }

	// newer 'clean' must NOT overwrite older blocking 'has_unpushed'
	rig := agentIssue("cleanup_status: has_unpushed\n", older)
	town := agentIssue("cleanup_status: clean\n", newer)
	updates, rows := MergeLegacyAgentBead(rig, town, exists)
	if updates.CleanupStatus != nil {
		t.Fatalf("newer 'clean' must not overwrite blocking 'has_unpushed'; got update %q", *updates.CleanupStatus)
	}
	if len(rows) != 1 || rows[0].Field != "cleanup_status" || rows[0].Winner != "rig (severity)" {
		t.Fatalf("rows = %+v, want one cleanup_status row won by rig on severity", rows)
	}

	// newer unknown (empty / null) DOES beat older 'clean': unknown fails closed
	rig2 := agentIssue("cleanup_status: clean\n", older)
	town2 := agentIssue("cleanup_status: null\n", newer)
	updates2, _ := MergeLegacyAgentBead(rig2, town2, exists)
	if updates2.CleanupStatus == nil || *updates2.CleanupStatus != "" {
		t.Fatalf("unknown must beat clean (fail closed); got %v", updates2.CleanupStatus)
	}

	// older blocking on the TOWN side also wins over newer rig 'clean'
	rig3 := agentIssue("cleanup_status: clean\n", newer)
	town3 := agentIssue("cleanup_status: has_stash\n", older)
	updates3, _ := MergeLegacyAgentBead(rig3, town3, exists)
	if updates3.CleanupStatus == nil || *updates3.CleanupStatus != "has_stash" {
		t.Fatalf("blocking town value must win over newer rig 'clean'; got %v", updates3.CleanupStatus)
	}
}

func TestMergeLegacyAgentBead_IdenticalRowsProduceNoUpdates(t *testing.T) {
	ts := time.Date(2026, 9, 9, 4, 43, 0, 0, time.UTC)
	rig := agentIssue("agent_state: done\ncleanup_status: clean\n", ts)
	town := agentIssue("agent_state: done\ncleanup_status: clean\n", ts)
	updates, rows := MergeLegacyAgentBead(rig, town, func(string) bool { return true })
	if updates != (AgentFieldUpdates{}) || len(rows) != 0 {
		t.Fatalf("identical rows must produce no updates; got %+v / %+v", updates, rows)
	}
}
