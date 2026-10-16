package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A seat whose session ended (it ran gt done and waits for its bead to land)
// keeps the agent the sling stored on its agent bead; a live session's own
// record still wins; a seat with neither shows blank (gt-y6cja).
func TestDashSeatAgent(t *testing.T) {
	t.Parallel()
	bead := func(agent string) *beads.Issue {
		return &beads.Issue{Description: beads.FormatAgentDescription("agent", &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "done", Agent: agent,
		})}
	}
	sessions := polecatSessionSet{
		polecatSessionKey("gastown", "live"): {Name: "gt-live", Agent: "deepseek-flash"},
		polecatSessionKey("gastown", "bare"): {Name: "gt-bare"},
	}
	tests := []struct {
		name    string
		polecat string
		bead    *beads.Issue
		want    string
	}{
		{"no session, stored agent", "gone", bead("claude-sonnet"), "claude-sonnet"},
		{"live session wins", "live", bead("claude-sonnet"), "deepseek-flash"},
		{"live session is not overridden by the stored agent", "bare", bead("claude-sonnet"), ""},
		{"no session, older bead", "gone", bead(""), ""},
		{"no session, no agent bead", "gone", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dashSeatAgent(sessions, "gastown", tt.polecat, tt.bead); got != tt.want {
				t.Errorf("agent = %q, want %q", got, tt.want)
			}
		})
	}
}
