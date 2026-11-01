package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
)

const (
	livenessPolecat = "ember"
	livenessAgent   = "deepseek-flash-sandbox"
	livenessBeadID  = "gt-gastown-polecat-ember"
	livenessSession = "gt-gastown-p-ember"
)

// livenessCounter builds the cap's seat question over a fake agent bead store
// and the given probe.
func livenessCounter(bd beads.Client, probe sandboxSeatProbe) liveSeatCounter {
	return newLiveSeatCounter(bd,
		func(string) string { return livenessBeadID },
		func(string) string { return livenessSession },
		probe)
}

// livenessSeatBeads returns a store holding the seat's agent bead, recording
// agent in its agent field.
func livenessSeatBeads(t *testing.T, agent string) *beadsfake.Fake {
	t.Helper()
	bd := beadsfake.New()
	if _, err := bd.Create(beads.CreateOptions{
		ID: livenessBeadID, Title: livenessBeadID, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(livenessBeadID, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", Agent: agent,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return bd
}

// livenessHostProbe answers as the probe does for a tmux seat.
func livenessHostProbe(string, string) (polecat.LiveGitState, bool) {
	return polecat.LiveGitState{}, false
}

// livenessSandboxProbe answers as the probe does for a sandbox seat the gateway
// answered exists for: true is a running sandbox, false a gone one, and an
// error a gateway that did not answer.
func livenessSandboxProbe(exists bool, existsErr error) sandboxSeatProbe {
	return func(string, string) (polecat.LiveGitState, bool) {
		return polecat.SandboxSeatGitState("", exists, existsErr, livenessSession), true
	}
}

// TestPolecatSeatLiveness: the cap counts a polecat directory unless every read
// agrees it is a sandbox seat whose sandbox is gone. Each row is one fact.
func TestPolecatSeatLiveness(t *testing.T) {
	t.Parallel()
	unreadable := beadsfake.New()
	unreadable.FailWith("show", errors.New("bd unavailable"))

	tests := []struct {
		name  string
		bd    beads.Client
		probe sandboxSeatProbe
		want  bool
	}{
		{"host seat counts", livenessSeatBeads(t, "deepseek-flash"), livenessHostProbe, true},
		{"sandbox seat whose sandbox exists counts", livenessSeatBeads(t, livenessAgent), livenessSandboxProbe(true, nil), true},
		{"sandbox seat whose sandbox is gone does not count", livenessSeatBeads(t, livenessAgent), livenessSandboxProbe(false, nil), false},
		{"gateway error counts", livenessSeatBeads(t, livenessAgent), livenessSandboxProbe(false, errors.New("gateway down")), true},
		{"unreadable agent bead counts", unreadable, livenessSandboxProbe(false, nil), true},
		{"no agent bead counts", beadsfake.New(), livenessSandboxProbe(false, nil), true},
		{"no store counts", nil, livenessSandboxProbe(false, nil), true},
		{"no probe counts", livenessSeatBeads(t, livenessAgent), nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := livenessCounter(tt.bd, tt.probe)(livenessPolecat); got != tt.want {
				t.Errorf("live seat = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPolecatSeatLivenessAsksForTheSeat: the probe is asked about the agent the
// bead recorded and the seat's session, which is also its sandbox's name.
func TestPolecatSeatLivenessAsksForTheSeat(t *testing.T) {
	t.Parallel()
	var gotAgent, gotSession string
	probe := func(agent, sessionName string) (polecat.LiveGitState, bool) {
		gotAgent, gotSession = agent, sessionName
		return polecat.SandboxSeatGitState("", false, nil, sessionName), true
	}

	livenessCounter(livenessSeatBeads(t, livenessAgent), probe)(livenessPolecat)

	if gotAgent != livenessAgent {
		t.Errorf("probe agent = %q, want the bead's %q", gotAgent, livenessAgent)
	}
	if gotSession != livenessSession {
		t.Errorf("probe session = %q, want %q", gotSession, livenessSession)
	}
}
