package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

func TestNukeHookDisposition(t *testing.T) {
	t.Parallel()
	paused := string(beads.AgentStatePaused)
	spawning := string(beads.AgentStateSpawning)
	working := string(beads.AgentStateWorking)

	openUnassigned := &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen)}
	openAssigned := &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen), Assignee: "gastown/polecats/nux"}
	hooked := &beads.Issue{ID: "gt-work", Status: string(beads.IssueStatusHooked)}
	inProgress := &beads.Issue{ID: "gt-work", Status: string(beads.StatusInProgress)}
	closed := &beads.Issue{ID: "gt-work", Status: string(beads.StatusClosed)}
	deferred := &beads.Issue{ID: "gt-work", Status: string(beads.StatusDeferred)}
	submitted := &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen), Labels: []string{land.LabelReadyToLand}}
	lookupErr := errors.New("dolt unreachable")

	tests := []struct {
		name       string
		bd         issueShower
		hookBead   string
		agentState string
		want       polecat.HookBeadDisposition
	}{
		{
			name:       "paused seat with open unassigned hook is safe",
			bd:         fakeIssueShower{issue: openUnassigned},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Safe: true},
		},
		{
			name:       "spawning seat with the same open unassigned hook keeps its blocker",
			bd:         fakeIssueShower{issue: openUnassigned},
			hookBead:   "gt-work",
			agentState: spawning,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=open"},
		},
		{
			name:       "working seat with open unassigned hook keeps its blocker",
			bd:         fakeIssueShower{issue: openUnassigned},
			hookBead:   "gt-work",
			agentState: working,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=open"},
		},
		{
			name:       "paused seat with open assigned hook keeps its blocker",
			bd:         fakeIssueShower{issue: openAssigned},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=open"},
		},
		{
			name:       "paused seat with hooked hook keeps its blocker",
			bd:         fakeIssueShower{issue: hooked},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=hooked"},
		},
		{
			name:       "paused seat with in_progress hook keeps its blocker",
			bd:         fakeIssueShower{issue: inProgress},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=in_progress"},
		},
		{
			name:       "paused seat with closed hook is terminal",
			bd:         fakeIssueShower{issue: closed},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Safe: true, Terminal: true},
		},
		{
			name:       "paused seat with deferred hook is inert",
			bd:         fakeIssueShower{issue: deferred},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Safe: true},
		},
		{
			name:       "paused seat with submitted hook is safe",
			bd:         fakeIssueShower{issue: submitted},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Safe: true},
		},
		{
			name:       "paused seat whose lookup failed keeps its blocker",
			bd:         fakeIssueShower{err: lookupErr},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=lookup_error: dolt unreachable"},
		},
		{
			name:       "paused seat whose lookup failed beside an open unassigned bead keeps its blocker",
			bd:         fakeIssueShower{issue: openUnassigned, err: lookupErr},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=lookup_error: dolt unreachable"},
		},
		{
			name:       "paused seat whose hook bead is missing keeps its blocker",
			bd:         fakeIssueShower{},
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=missing"},
		},
		{
			name:       "paused seat with no shower keeps its blocker",
			bd:         nil,
			hookBead:   "gt-work",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Blocker: "hook_bead=gt-work status=unverified"},
		},
		{
			name:       "paused seat with no hook bead is safe",
			bd:         fakeIssueShower{issue: openUnassigned},
			hookBead:   "",
			agentState: paused,
			want:       polecat.HookBeadDisposition{Safe: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nukeHookDisposition(tt.bd, tt.hookBead, tt.agentState); got != tt.want {
				t.Errorf("nukeHookDisposition(%q, %q) = %+v, want %+v", tt.hookBead, tt.agentState, got, tt.want)
			}
		})
	}
}
