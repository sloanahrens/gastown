package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestHandedBackHook(t *testing.T) {
	t.Parallel()
	openUnassigned := &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen)}
	openAssigned := &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen), Assignee: "gastown/polecats/nux"}
	hooked := &beads.Issue{ID: "gt-work", Status: string(beads.IssueStatusHooked)}
	inProgress := &beads.Issue{ID: "gt-work", Status: string(beads.StatusInProgress)}

	tests := []struct {
		name       string
		agentState string
		hook       *beads.Issue
		want       bool
	}{
		{name: "paused with open unassigned bead", agentState: string(beads.AgentStatePaused), hook: openUnassigned, want: true},
		{name: "spawning seat with the same open unassigned bead", agentState: string(beads.AgentStateSpawning), hook: openUnassigned, want: false},
		{name: "working seat with open unassigned bead", agentState: string(beads.AgentStateWorking), hook: openUnassigned, want: false},
		{name: "stuck seat with open unassigned bead", agentState: string(beads.AgentStateStuck), hook: openUnassigned, want: false},
		{name: "paused with open assigned bead", agentState: string(beads.AgentStatePaused), hook: openAssigned, want: false},
		{name: "paused with hooked bead", agentState: string(beads.AgentStatePaused), hook: hooked, want: false},
		{name: "paused with in_progress bead", agentState: string(beads.AgentStatePaused), hook: inProgress, want: false},
		{name: "paused with nil bead", agentState: string(beads.AgentStatePaused), hook: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := handedBackHook(tt.agentState, tt.hook); got != tt.want {
				t.Errorf("handedBackHook(%q, %+v) = %v, want %v", tt.agentState, tt.hook, got, tt.want)
			}
		})
	}
}
