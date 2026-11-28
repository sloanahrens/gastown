package polecat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
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

// An unset Agent (a sling that named none) must leave the agent bead's agent
// empty rather than inventing one, on every write.
func TestAgentWritesWithoutAgentLeaveItEmpty(t *testing.T) {
	t.Parallel()
	var bd *polecatDB
	m, _, _ := addRig(t, func(t *testing.T, mayorRig, mayorBeads string) *polecatDB {
		bd = fakeAddBeads(t, mayorRig, mayorBeads)
		return bd
	}, map[string]string{"README.md": "# Test\n"})

	name, _, err := m.AllocateAndAdd(AddOptions{HookBead: "gt-work"})
	if err != nil {
		t.Fatalf("AllocateAndAdd: %v", err)
	}
	fields := agentFieldsOf(t, bd, m.agentBeadID(name))
	if fields.Agent != "" || fields.HookBead != "gt-work" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want no agent and gt-work", fields.Agent, fields.HookBead)
	}

	mgr, _, rbd, _, _ := canonicalWithPolecats(t, true, "toast")
	if _, err := mgr.RepairWorktreeWithOptions("toast", true, AddOptions{HookBead: "gt-redo"}); err != nil {
		t.Fatalf("RepairWorktreeWithOptions: %v", err)
	}
	fields = agentFieldsOf(t, rbd, mgr.agentBeadID("toast"))
	if fields.Agent != "" || fields.HookBead != "gt-redo" {
		t.Errorf("repaired agent bead fields = agent %q hook_bead %q, want no agent and gt-redo", fields.Agent, fields.HookBead)
	}
}

// When the agent bead cannot be written, the allocation and the repair fail
// rather than hand back a seat whose agent was dropped.
func TestAgentWriteFailureFailsTheSpawn(t *testing.T) {
	t.Parallel()

	t.Run("AllocateAndAdd", func(t *testing.T) {
		t.Parallel()
		m, _, _ := addRig(t, func(t *testing.T, mayorRig, mayorBeads string) *polecatDB {
			bd := fakeAddBeads(t, mayorRig, mayorBeads)
			bd.createErr = errors.New("error: database not initialized")
			return bd
		}, map[string]string{"README.md": "# Test\n"})

		_, _, err := m.AllocateAndAdd(AddOptions{HookBead: "gt-work", Agent: "deepseek-flash"})
		if err == nil || !strings.Contains(err.Error(), "agent bead required") {
			t.Fatalf("AllocateAndAdd error = %v, want an agent bead required failure", err)
		}
	})

	t.Run("Repair", func(t *testing.T) {
		t.Parallel()
		mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
		// The seat's old agent bead exists, so a failed create would fall back
		// to reopening it; hiding it leaves the create error as the outcome.
		bd.hidden[mgr.agentBeadID("toast")] = true
		bd.createErr = errors.New("error: database not initialized")

		_, err := mgr.RepairWorktreeWithOptions("toast", true, AddOptions{HookBead: "gt-redo", Agent: "deepseek-flash"})
		if err == nil || !strings.Contains(err.Error(), "agent bead required") {
			t.Fatalf("RepairWorktreeWithOptions error = %v, want an agent bead required failure", err)
		}
	})
}

func writeSeatRecord(t *testing.T, m *Manager, name string, rec intent.Record) {
	t.Helper()
	if _, err := intent.Update(m.townRoot, intentSeat(m.rig.Name, name), func(r *intent.Record) error { *r = rec; return nil }); err != nil {
		t.Fatalf("writing %s's record: %v", name, err)
	}
}

func readSeatRecord(t *testing.T, m *Manager, name string) intent.Record {
	t.Helper()
	rec, err := intent.Read(m.townRoot, intentSeat(m.rig.Name, name))
	if err != nil {
		t.Fatalf("reading %s's record: %v", name, err)
	}
	return rec
}

// seedToast seeds toast's record on a canonical rig, its clock pinned so the
// write's timestamp is checkable.
func seedToast(t *testing.T, rec intent.Record) *Manager {
	t.Helper()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "toast")
	mgr.clock = clockwork.NewFakeClockAt(testEpoch)
	writeSeatRecord(t, mgr, "toast", rec)
	return mgr
}

// A reuse records its new agent at once, touching Agent, Actor and UpdatedAt
// only; without both a new agent and hook it leaves the record alone.
func TestSeatAgentRecordReuse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ agent, hook, want string }{{"sbx-flash", "gt-next", "sbx-flash"}, {"", "gt-next", "sbx-haiku"}, {"sbx-flash", "", "sbx-haiku"}} {
		mgr := seedToast(t, intent.Record{Agent: "sbx-haiku", WorkBead: "gt-old", LastWorkBead: "gt-done"})
		before := readSeatRecord(t, mgr, "toast")
		if _, err := mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: tt.hook, Agent: tt.agent}); err != nil {
			t.Fatalf("agent %q hook %q: ReuseIdlePolecat: %v", tt.agent, tt.hook, err)
		}
		after := readSeatRecord(t, mgr, "toast")
		if after.Agent != tt.want {
			t.Fatalf("agent %q hook %q: record agent = %q, want %s", tt.agent, tt.hook, after.Agent, tt.want)
		}
		if tt.want == "sbx-flash" && (after.Actor != "gt sling" || !after.UpdatedAt.Equal(testEpoch)) {
			t.Errorf("record actor %q updated %s, want gt sling at %s", after.Actor, after.UpdatedAt, testEpoch)
		}
		before.Agent, before.Actor, before.UpdatedAt = after.Agent, after.Actor, after.UpdatedAt
		if !reflect.DeepEqual(before, after) {
			t.Errorf("record changed beyond agent, actor and updated_at:\nbefore %+v\nafter  %+v", before, after)
		}
	}
}

// An allocation records the agent its drawn name is slung on; a held seat
// keeps the agent it had.
func TestSeatAgentRecordAllocateNamesTheNewAgent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		held bool
		want string
	}{{false, "sbx-flash"}, {true, "sbx-haiku"}} {
		m, _, _ := addRig(t, fakeAddBeads, map[string]string{"README.md": "# Test\n"})
		rec := intent.Record{Agent: "sbx-haiku"}
		if tt.held {
			rec.Desired = intent.DesiredPark
		}
		m.afterPoolNameAllocated = func(name string) { writeSeatRecord(t, m, name, rec) }
		name, _, err := m.AllocateAndAdd(AddOptions{HookBead: "gt-work", Agent: "sbx-flash"})
		if err != nil {
			t.Fatalf("AllocateAndAdd: %v", err)
		}
		if got := readSeatRecord(t, m, name); got.Agent != tt.want {
			t.Errorf("held %v: record agent = %q, want %s", tt.held, got.Agent, tt.want)
		}
	}
}

// A record that cannot be written is warned about; the spawn still succeeds.
func TestSeatAgentRecordWriteFailureStillSpawns(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
	writeSeatRecord(t, mgr, "toast", intent.Record{Agent: "sbx-haiku"})
	// A file where the intent lock directory belongs fails every intent write.
	locks := filepath.Join(mgr.townRoot, ".runtime", "locks")
	if err := os.RemoveAll(locks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locks, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var warned []string
	mgr.warn = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	if _, err := mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next", Agent: "sbx-flash"}); err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}
	if fields := agentFieldsOf(t, bd, mgr.agentBeadID("toast")); fields.Agent != "sbx-flash" || fields.HookBead != "gt-next" {
		t.Errorf("agent bead fields = agent %q hook_bead %q, want sbx-flash and gt-next", fields.Agent, fields.HookBead)
	}
	seat := intentSeat(mgr.rig.Name, "toast").String()
	if len(warned) != 1 || !strings.Contains(warned[0], seat) {
		t.Errorf("warnings = %q, want one naming seat %s", warned, seat)
	}
}
