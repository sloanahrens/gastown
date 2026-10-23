package beads_test

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

const hookedAgentBead = "gt-gastown-polecat-capable"

// hookedAgents is a bead database holding one polecat agent bead hooked to
// hook, in the state a spawn leaves it in before its session starts.
func hookedAgents(t *testing.T, hook string) *beadsfake.Fake {
	t.Helper()
	c := beadsfake.New()
	if _, err := c.Create(beads.CreateOptions{
		ID: hookedAgentBead, Title: hookedAgentBead, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(hookedAgentBead, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "spawning", HookBead: hook,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func hookedAgentFields(t *testing.T, c beads.Client) *beads.AgentFields {
	t.Helper()
	issue, err := c.Show(hookedAgentBead)
	if err != nil {
		t.Fatal(err)
	}
	return beads.ParseAgentFields(issue.Description)
}

// ResetAgentBeadWhileHooked clears the seat only while its hook is still the
// stopped bead, and leaves a seat that moved on, one already reset, and a
// missing bead alone.
func TestResetAgentBeadWhileHooked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		seed      bool
		hook      string
		wantReset bool
	}{
		{"still_hooked_is_reset", true, "gt-nad3h", true},
		{"hooked_to_newer_work_is_left", true, "gt-newer", false},
		{"already_reset_is_left", true, "", false},
		{"missing_bead_is_left", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := beadsfake.New()
			if tt.seed {
				c = hookedAgents(t, tt.hook)
			}
			did, err := beads.ResetAgentBeadWhileHooked(c, hookedAgentBead, "gt-nad3h", "test")
			if err != nil || did != tt.wantReset {
				t.Fatalf("ResetAgentBeadWhileHooked = %v, %v; want %v, nil", did, err, tt.wantReset)
			}
			if !tt.seed {
				return
			}
			got := hookedAgentFields(t, c)
			if tt.wantReset && (got.HookBead != "" || got.AgentState != string(beads.AgentStateNuked)) {
				t.Errorf("reset agent bead = %+v, want hook cleared and state nuked", got)
			}
			if !tt.wantReset && got.AgentState != "spawning" {
				t.Errorf("agent bead was rewritten: %+v", got)
			}
		})
	}
}

// A bead the read cannot answer for is an error naming the cause, never a
// reset on a guess.
func TestResetAgentBeadWhileHookedReportsAFailedRead(t *testing.T) {
	t.Parallel()
	c := hookedAgents(t, "gt-nad3h")
	boom := errors.New("dolt unreachable")
	c.FailWith("show", boom)

	did, err := beads.ResetAgentBeadWhileHooked(c, hookedAgentBead, "gt-nad3h", "test")
	if did || !errors.Is(err, boom) {
		t.Fatalf("ResetAgentBeadWhileHooked = %v, %v; want false and the read error", did, err)
	}
	c.FailWith("show", nil)
	if got := hookedAgentFields(t, c); got.HookBead != "gt-nad3h" {
		t.Errorf("agent bead changed after a failed read: %+v", got)
	}
}
