package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/session"
)

// startupDeathAgentBead is the agent bead id a spawn of gastown/capable derives
// under the "gt" prefix.
var startupDeathAgentBead = beads.PolecatBeadIDWithPrefix("gt", "gastown", "capable")

func startupDeathAgents(t *testing.T, hook string) *beadsfake.Fake {
	t.Helper()
	c := beadsfake.New()
	if _, err := c.Create(beads.CreateOptions{
		ID: startupDeathAgentBead, Title: startupDeathAgentBead, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(startupDeathAgentBead, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "spawning", HookBead: hook,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func startupDeathHook(t *testing.T, c beads.Client) string {
	t.Helper()
	issue, err := c.Show(startupDeathAgentBead)
	if err != nil {
		t.Fatal(err)
	}
	return beads.ParseAgentFields(issue.Description).HookBead
}

// A session that dies during startup rolls the spawn back: the sandbox and
// record are removed and the agent bead the spawn hooked is released, with the
// original cause returned. Driven through paneAfterStart, the path the spawn
// takes.
func TestPaneAfterStartDeathReleasesTheAgentBead(t *testing.T) {
	t.Parallel()
	c := startupDeathAgents(t, "gt-nad3h")
	s := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "capable", SessionName: "gt-capable", HookBead: "gt-nad3h"}
	deleter := func(string, session.SandboxRecord) error { return nil }
	town := t.TempDir()
	killed := false

	_, err := paneAfterStart("gt-capable",
		func(string) (string, error) { return "", errors.New("can't find pane") },
		func() { killed = true },
		s.startupDeathRollback(deleter, c, town))

	if err == nil || !strings.Contains(err.Error(), "can't find pane") {
		t.Fatalf("paneAfterStart = %v, want the pane error as the cause", err)
	}
	if !killed {
		t.Error("the dead session was not killed")
	}
	if got := startupDeathHook(t, c); got != "" {
		t.Errorf("agent bead hook = %q after the rollback, want cleared", got)
	}
}

// A reset that fails is reported next to the original cause, which stays
// findable.
func TestStartupDeathRollbackReportsAFailedAgentBeadReset(t *testing.T) {
	t.Parallel()
	c := startupDeathAgents(t, "gt-nad3h")
	boom := errors.New("dolt unreachable")
	c.FailWith("show", boom)
	s := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "capable", SessionName: "gt-capable", HookBead: "gt-nad3h"}
	cause := errors.New("session likely died during startup")

	err := s.startupDeathRollback(nil, c, t.TempDir())(cause)

	if !errors.Is(err, cause) || !errors.Is(err, boom) || !strings.Contains(err.Error(), "resetting the agent bead failed too") {
		t.Fatalf("rollback = %v, want the cause and the reset failure both findable", err)
	}
}

// A spawn that hooked nothing (an unknown work bead) leaves the agent bead
// alone.
func TestStartupDeathRollbackWithoutAHookBeadLeavesTheAgentBead(t *testing.T) {
	t.Parallel()
	c := startupDeathAgents(t, "gt-other")
	s := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "capable"}
	cause := errors.New("died")
	if err := s.startupDeathRollback(nil, c, t.TempDir())(cause); !errors.Is(err, cause) {
		t.Fatalf("rollback = %v, want the cause", err)
	}
	if got := startupDeathHook(t, c); got != "gt-other" {
		t.Errorf("agent bead hook = %q, want gt-other untouched", got)
	}
}
