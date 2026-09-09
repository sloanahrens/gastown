package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// TestPatrolAssignee_MatchesHookQueryAddress guards against gt-cut: patrol
// wisps must be written with the same assignee address that `gt hook` queries
// via resolveSelfTarget/canonicalAssigneeAddress. A prior drift (deacon
// patrols written as "deacon" while gt hook queried "deacon/") made deacon
// patrol wisps invisible to gt hook, causing duplicate patrol wisps every
// cycle.
func TestPatrolAssignee_MatchesHookQueryAddress(t *testing.T) {
	tests := []struct {
		name     string
		roleName string
		rig      string
		identity *session.AgentIdentity
	}{
		{
			name:     "deacon is town-level and must carry a trailing slash",
			roleName: "deacon",
			rig:      "",
			identity: &session.AgentIdentity{Role: session.RoleDeacon},
		},
		{
			name:     "witness is rig-scoped and must not carry a trailing slash",
			roleName: "witness",
			rig:      "gastown",
			identity: &session.AgentIdentity{Role: session.RoleWitness, Rig: "gastown", Prefix: session.PrefixFor("gastown")},
		},
		{
			name:     "refinery is rig-scoped and must not carry a trailing slash",
			roleName: "refinery",
			rig:      "gastown",
			identity: &session.AgentIdentity{Role: session.RoleRefinery, Rig: "gastown", Prefix: session.PrefixFor("gastown")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := patrolAssignee(tt.roleName, tt.rig)
			want := canonicalAssigneeAddress(tt.identity)
			if got != want {
				t.Errorf("patrolAssignee(%q, %q) = %q, want %q (must match gt hook's query address)",
					tt.roleName, tt.rig, got, want)
			}
		})
	}
}

// TestPatrolAssignee_DeaconHasTrailingSlash pins the exact regression from
// gt-cut: bare "deacon" (no slash) is what gt patrol report used to write,
// and it is invisible to gt hook's "deacon/" query.
func TestPatrolAssignee_DeaconHasTrailingSlash(t *testing.T) {
	got := patrolAssignee("deacon", "")
	if got != "deacon/" {
		t.Errorf("patrolAssignee(\"deacon\", \"\") = %q, want \"deacon/\"", got)
	}
}
