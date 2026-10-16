package polecat

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// gt-y6cja: the sling writes the agent it spawned the seat on into the agent
// bead's fields, in the same write as hook_bead, so the dashboard can name the
// seat's agent after its session ends.

func agentFieldsOf(t *testing.T, bd *polecatDB, agentID string) *beads.AgentFields {
	t.Helper()
	issue, err := bd.Show(agentID)
	if err != nil {
		t.Fatalf("Show(%s): %v", agentID, err)
	}
	return beads.ParseAgentFields(issue.Description)
}

func TestAddWritesAgentWithHookBead(t *testing.T) {
	t.Parallel()
	var bd *polecatDB
	m, _, _ := addRig(t, func(t *testing.T, mayorRig, mayorBeads string) *polecatDB {
		bd = fakeAddBeads(t, mayorRig, mayorBeads)
		return bd
	}, map[string]string{"README.md": "# Test\n"})

	if _, err := m.AddWithOptions("toast", AddOptions{HookBead: "gt-work", Agent: "deepseek-flash"}); err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	fields := agentFieldsOf(t, bd, m.agentBeadID("toast"))
	if fields.Agent != "deepseek-flash" || fields.HookBead != "gt-work" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want deepseek-flash and gt-work", fields.Agent, fields.HookBead)
	}
}

func TestReuseWritesAgentWithHookBead(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")

	if _, err := mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next", Agent: "claude-sonnet"}); err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}
	fields := agentFieldsOf(t, bd, mgr.agentBeadID("toast"))
	if fields.Agent != "claude-sonnet" || fields.HookBead != "gt-next" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want claude-sonnet and gt-next", fields.Agent, fields.HookBead)
	}
}

// The writes above go through AddWithOptions and ReuseIdlePolecat. A sling's
// fresh allocation (AllocateAndAdd, AddNamedWithOptions) builds the polecat in
// addWithOptionsLocked, and a repair rebuilds it in RepairWorktreeWithOptions;
// each has its own agent bead write that a missing Agent field would silently
// skip (gt-224md).

func TestAllocateAndAddWritesAgentWithHookBead(t *testing.T) {
	t.Parallel()
	var bd *polecatDB
	m, _, _ := addRig(t, func(t *testing.T, mayorRig, mayorBeads string) *polecatDB {
		bd = fakeAddBeads(t, mayorRig, mayorBeads)
		return bd
	}, map[string]string{"README.md": "# Test\n"})

	name, _, err := m.AllocateAndAdd(AddOptions{HookBead: "gt-work", Agent: "deepseek-flash"})
	if err != nil {
		t.Fatalf("AllocateAndAdd: %v", err)
	}
	fields := agentFieldsOf(t, bd, m.agentBeadID(name))
	if fields.Agent != "deepseek-flash" || fields.HookBead != "gt-work" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want deepseek-flash and gt-work", fields.Agent, fields.HookBead)
	}
}

func TestAddNamedWritesAgentWithHookBead(t *testing.T) {
	t.Parallel()
	var bd *polecatDB
	m, _, _ := addRig(t, func(t *testing.T, mayorRig, mayorBeads string) *polecatDB {
		bd = fakeAddBeads(t, mayorRig, mayorBeads)
		return bd
	}, map[string]string{"README.md": "# Test\n"})

	if _, err := m.AddNamedWithOptions("toast", AddOptions{HookBead: "gt-work", Agent: "claude-sonnet"}); err != nil {
		t.Fatalf("AddNamedWithOptions: %v", err)
	}
	fields := agentFieldsOf(t, bd, m.agentBeadID("toast"))
	if fields.Agent != "claude-sonnet" || fields.HookBead != "gt-work" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want claude-sonnet and gt-work", fields.Agent, fields.HookBead)
	}
}

func TestRepairWritesAgentWithHookBead(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")

	if _, err := mgr.RepairWorktreeWithOptions("toast", true, AddOptions{HookBead: "gt-redo", Agent: "deepseek-flash"}); err != nil {
		t.Fatalf("RepairWorktreeWithOptions: %v", err)
	}
	fields := agentFieldsOf(t, bd, mgr.agentBeadID("toast"))
	if fields.Agent != "deepseek-flash" || fields.HookBead != "gt-redo" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want deepseek-flash and gt-redo", fields.Agent, fields.HookBead)
	}
}
