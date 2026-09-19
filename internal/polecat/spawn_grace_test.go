package polecat

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestSpawnGrace guards gt-yteq: a polecat slung seconds ago has an agent bead
// that says spawning and no session yet. Reading that as stalled restarts a
// dispatch that is still booting, so only a spawning bead *inside* the window
// earns the grace — and every missing fact (no window, no timestamp, no clock,
// a bead that already says working) falls back to the existing stalled verdict.
func TestSpawnGrace(t *testing.T) {
	now := time.Date(2026, 9, 19, 16, 32, 0, 0, time.UTC)
	const grace = 5 * time.Minute

	tests := []struct {
		name       string
		agentState string
		updatedAt  time.Time
		now        time.Time
		grace      time.Duration
		want       bool
	}{
		{
			name:       "spawning bead touched 30s ago is inside the grace",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-30 * time.Second),
			now:        now,
			grace:      grace,
			want:       true,
		},
		{
			name:       "spawning bead untouched for 6m has failed to start",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-6 * time.Minute),
			now:        now,
			grace:      grace,
			want:       false,
		},
		{
			name:       "spawning bead at exactly the window edge is stalled",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-grace),
			now:        now,
			grace:      grace,
			want:       false,
		},
		{
			name:       "working bead with a dead session is a crash mid-work",
			agentState: string(beads.AgentStateWorking),
			updatedAt:  now.Add(-30 * time.Second),
			now:        now,
			grace:      grace,
			want:       false,
		},
		{
			name:       "empty agent state earns nothing",
			agentState: "",
			updatedAt:  now.Add(-30 * time.Second),
			now:        now,
			grace:      grace,
			want:       false,
		},
		{
			name:       "idle bead is not spawning",
			agentState: string(beads.AgentStateIdle),
			updatedAt:  now.Add(-30 * time.Second),
			now:        now,
			grace:      grace,
			want:       false,
		},
		{
			name:       "unparseable bead timestamp fails toward stalled",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  time.Time{},
			now:        now,
			grace:      grace,
			want:       false,
		},
		{
			name:       "no clock fails toward stalled",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-30 * time.Second),
			now:        time.Time{},
			grace:      grace,
			want:       false,
		},
		{
			name:       "no window fails toward stalled",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-30 * time.Second),
			now:        now,
			grace:      0,
			want:       false,
		},
		{
			name:       "clock skew ahead of now still reads as just spawned",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(10 * time.Second),
			now:        now,
			grace:      grace,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SpawnGrace(tt.agentState, tt.updatedAt, tt.now, tt.grace)
			if got != tt.want {
				t.Fatalf("SpawnGrace(%q, %s, %s, %s) = %v, want %v",
					tt.agentState, tt.updatedAt.Format(time.RFC3339), tt.now.Format(time.RFC3339), tt.grace, got, tt.want)
			}
		})
	}
}

func TestSessionDownState(t *testing.T) {
	if got := sessionDownState(true); got != StateSpawning {
		t.Errorf("sessionDownState(true) = %q, want %q", got, StateSpawning)
	}
	if got := sessionDownState(false); got != StateStalled {
		t.Errorf("sessionDownState(false) = %q, want %q", got, StateStalled)
	}
}

// TestAgentBeadUpdatedAt covers the nil and unparseable cases SpawnGrace depends
// on: a bead nobody can date must yield the zero time, never a panic and never
// "now".
func TestAgentBeadUpdatedAt(t *testing.T) {
	if got := AgentBeadUpdatedAt(nil); !got.IsZero() {
		t.Errorf("AgentBeadUpdatedAt(nil) = %s, want zero time", got)
	}
	if got := AgentBeadUpdatedAt(&beads.Issue{ID: "gt-agent"}); !got.IsZero() {
		t.Errorf("AgentBeadUpdatedAt(no timestamp) = %s, want zero time", got)
	}
	if got := AgentBeadUpdatedAt(&beads.Issue{UpdatedAt: "not-a-time"}); !got.IsZero() {
		t.Errorf("AgentBeadUpdatedAt(garbage) = %s, want zero time", got)
	}
	updated := "2026-09-19T16:31:30Z"
	got := AgentBeadUpdatedAt(&beads.Issue{UpdatedAt: updated})
	if got.IsZero() {
		t.Fatal("AgentBeadUpdatedAt(dated bead) = zero time, want a parsed time")
	}
	if got.UTC().Format(time.RFC3339) != updated {
		t.Errorf("AgentBeadUpdatedAt(dated bead) = %s, want %s", got.UTC().Format(time.RFC3339), updated)
	}
}
