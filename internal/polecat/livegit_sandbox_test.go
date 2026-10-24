package polecat

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func seatSettings(t *testing.T, seats map[string]config.SeatConfig) *config.TownSettings {
	t.Helper()
	return &config.TownSettings{
		PolecatPool: &config.PolecatPool{Seats: seats},
		Sandboxes:   map[string]config.SandboxProfile{"polecat": {Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml"}},
	}
}

func TestIsSandboxSeatAgent(t *testing.T) {
	t.Parallel()
	ts := seatSettings(t, map[string]config.SeatConfig{
		"deepseek-flash-sandbox": {Cap: 1, Backend: config.BackendOpenShell, Sandbox: "polecat"},
		"deepseek-flash":         {Cap: 0},
		"claude-sonnet":          {Cap: 2, Backend: config.BackendTmux},
	})
	tests := []struct {
		name  string
		ts    *config.TownSettings
		agent string
		want  bool
	}{
		{"openshell seat", ts, "deepseek-flash-sandbox", true},
		{"padded name", ts, "  deepseek-flash-sandbox\n", true},
		{"default backend seat", ts, "deepseek-flash", false},
		{"explicit tmux seat", ts, "claude-sonnet", false},
		{"agent with no seat", ts, "claude-haiku", false},
		{"no recorded agent", ts, "", false},
		{"no pool configured", &config.TownSettings{}, "deepseek-flash-sandbox", false},
		{"nil settings", nil, "deepseek-flash-sandbox", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := IsSandboxSeatAgent(tc.ts, tc.agent)
			if err != nil {
				t.Fatalf("IsSandboxSeatAgent: %v", err)
			}
			if got != tc.want {
				t.Errorf("IsSandboxSeatAgent(%q) = %v, want %v", tc.agent, got, tc.want)
			}
		})
	}
}

// An unreadable seat table is an error, not "not a sandbox seat": the caller
// would otherwise judge a sandbox seat by its stale host worktree.
func TestIsSandboxSeatAgentSeatTableErrorIsReturned(t *testing.T) {
	t.Parallel()
	ts := seatSettings(t, map[string]config.SeatConfig{
		"bad": {Cap: 1, Backend: config.BackendOpenShell}, // openshell without a sandbox
	})
	if _, err := IsSandboxSeatAgent(ts, "bad"); err == nil {
		t.Fatal("IsSandboxSeatAgent = nil error for an invalid seat table")
	}
}

func TestSandboxSeatGitStateRunningSandboxIsUnknown(t *testing.T) {
	t.Parallel()
	st := SandboxSeatGitState("polecat/x/gt-1", true, nil, "ma-papaya")
	if st.Source != GitStateSourceUnknown {
		t.Fatalf("Source = %q, want unknown: the work lives in the running sandbox", st.Source)
	}
	if !strings.Contains(st.FailedReason, "ma-papaya") {
		t.Errorf("FailedReason = %q, want it to name the sandbox", st.FailedReason)
	}
}

func TestSandboxSeatGitStateGoneSandboxIsCleanAndLive(t *testing.T) {
	t.Parallel()
	st := SandboxSeatGitState("polecat/x/gt-1", false, nil, "ma-papaya")
	want := LiveGitState{Branch: "polecat/x/gt-1", Source: GitStateSourceLive}
	if st != want {
		t.Errorf("SandboxSeatGitState = %+v, want %+v: no unpushed, dirty or stash facts from a host worktree", st, want)
	}
}

// A gateway that errors (down, timed out) is not an answer: a live sandbox must
// not read as gone, so the state stays unknown whatever exists says.
func TestSandboxSeatGitStateGatewayErrorIsUnknown(t *testing.T) {
	t.Parallel()
	for _, exists := range []bool{false, true} {
		st := SandboxSeatGitState("polecat/x/gt-1", exists, errors.New("gateway unreachable"), "ma-papaya")
		if st.Source != GitStateSourceUnknown {
			t.Errorf("exists=%v with a gateway error: Source = %v, want unknown", exists, st.Source)
		}
		if !strings.Contains(st.FailedReason, "gateway unreachable") {
			t.Errorf("exists=%v: FailedReason = %q, want the gateway error in it", exists, st.FailedReason)
		}
	}
}
