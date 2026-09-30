package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// U-01: Simple 2-node cycle A→B→A
func TestDetectCycles_Simple2NodeCycle(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", BlockedBy: []string{"b"}, Blocks: []string{"b"}},
		"b": {ID: "b", BlockedBy: []string{"a"}, Blocks: []string{"a"}},
	}}
	cycle := detectCycles(dag)
	if cycle == nil {
		t.Fatal("expected cycle, got nil")
	}
	// Cycle should contain both "a" and "b"
	if len(cycle) < 2 {
		t.Fatalf("cycle too short: %v", cycle)
	}
}

// U-02: No cycle - linear chain A→B→C
func TestDetectCycles_NoCycleLinearChain(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Blocks: []string{"b"}},
		"b": {ID: "b", BlockedBy: []string{"a"}, Blocks: []string{"c"}},
		"c": {ID: "c", BlockedBy: []string{"b"}},
	}}
	cycle := detectCycles(dag)
	if cycle != nil {
		t.Fatalf("expected no cycle, got: %v", cycle)
	}
}

// U-03: Self-loop A blocks A
func TestDetectCycles_SelfLoop(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", BlockedBy: []string{"a"}, Blocks: []string{"a"}},
	}}
	cycle := detectCycles(dag)
	if cycle == nil {
		t.Fatal("expected cycle for self-loop, got nil")
	}
}

// U-04: Diamond shape (no cycle) - A→B, A→C, B→D, C→D
func TestDetectCycles_DiamondNoCycle(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Blocks: []string{"b", "c"}},
		"b": {ID: "b", BlockedBy: []string{"a"}, Blocks: []string{"d"}},
		"c": {ID: "c", BlockedBy: []string{"a"}, Blocks: []string{"d"}},
		"d": {ID: "d", BlockedBy: []string{"b", "c"}},
	}}
	cycle := detectCycles(dag)
	if cycle != nil {
		t.Fatalf("expected no cycle in diamond, got: %v", cycle)
	}
}

// U-05: Long chain with back-edge - A→B→C→D→B (cycle: B→C→D→B)
func TestDetectCycles_LongChainWithBackEdge(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Blocks: []string{"b"}},
		"b": {ID: "b", BlockedBy: []string{"a", "d"}, Blocks: []string{"c"}},
		"c": {ID: "c", BlockedBy: []string{"b"}, Blocks: []string{"d"}},
		"d": {ID: "d", BlockedBy: []string{"c"}, Blocks: []string{"b"}},
	}}
	cycle := detectCycles(dag)
	if cycle == nil {
		t.Fatal("expected cycle, got nil")
	}
	// Cycle should include b, c, d
	if len(cycle) < 3 {
		t.Fatalf("cycle too short, expected at least b,c,d: %v", cycle)
	}
}

// ---------------------------------------------------------------------------
// computeWaves tests (U-06 through U-14)
// ---------------------------------------------------------------------------

// helper: collect all task IDs across all waves
func allWaveTaskIDs(waves []Wave) []string {
	var all []string
	for _, w := range waves {
		all = append(all, w.Tasks...)
	}
	return all
}

// helper: find which wave a task is in (returns -1 if not found)
func waveOf(waves []Wave, taskID string) int {
	for _, w := range waves {
		for _, id := range w.Tasks {
			if id == taskID {
				return w.Number
			}
		}
	}
	return -1
}

// U-06: 3 independent tasks (no deps) → all Wave 1
func TestComputeWaves_AllIndependent(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task"},
		"b": {ID: "b", Type: "task"},
		"c": {ID: "c", Type: "task"},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 1 {
		t.Fatalf("expected 1 wave, got %d: %+v", len(waves), waves)
	}
	if waves[0].Number != 1 {
		t.Fatalf("expected wave number 1, got %d", waves[0].Number)
	}
	if len(waves[0].Tasks) != 3 {
		t.Fatalf("expected 3 tasks in wave 1, got %d: %v", len(waves[0].Tasks), waves[0].Tasks)
	}
	// Tasks should be sorted for determinism
	expected := []string{"a", "b", "c"}
	for i, id := range waves[0].Tasks {
		if id != expected[i] {
			t.Errorf("wave 1 task[%d] = %q, want %q", i, id, expected[i])
		}
	}
}

// U-07: Linear chain A→B→C → 3 waves
func TestComputeWaves_LinearChain(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task", Blocks: []string{"b"}},
		"b": {ID: "b", Type: "task", BlockedBy: []string{"a"}, Blocks: []string{"c"}},
		"c": {ID: "c", Type: "task", BlockedBy: []string{"b"}},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %+v", len(waves), waves)
	}
	// Wave 1=[a], Wave 2=[b], Wave 3=[c]
	checks := []struct {
		waveNum int
		tasks   []string
	}{
		{1, []string{"a"}},
		{2, []string{"b"}},
		{3, []string{"c"}},
	}
	for _, c := range checks {
		w := waves[c.waveNum-1]
		if w.Number != c.waveNum {
			t.Errorf("wave %d: got number %d", c.waveNum, w.Number)
		}
		if fmt.Sprintf("%v", w.Tasks) != fmt.Sprintf("%v", c.tasks) {
			t.Errorf("wave %d: got tasks %v, want %v", c.waveNum, w.Tasks, c.tasks)
		}
	}
}

// U-08: Diamond deps → correct waves. A→B, A→C, B→D, C→D = 3 waves: [A], [B,C], [D]
func TestComputeWaves_Diamond(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task", Blocks: []string{"b", "c"}},
		"b": {ID: "b", Type: "task", BlockedBy: []string{"a"}, Blocks: []string{"d"}},
		"c": {ID: "c", Type: "task", BlockedBy: []string{"a"}, Blocks: []string{"d"}},
		"d": {ID: "d", Type: "task", BlockedBy: []string{"b", "c"}},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %+v", len(waves), waves)
	}
	// Wave 1=[a], Wave 2=[b,c], Wave 3=[d]
	if fmt.Sprintf("%v", waves[0].Tasks) != "[a]" {
		t.Errorf("wave 1: got %v, want [a]", waves[0].Tasks)
	}
	if fmt.Sprintf("%v", waves[1].Tasks) != "[b c]" {
		t.Errorf("wave 2: got %v, want [b c]", waves[1].Tasks)
	}
	if fmt.Sprintf("%v", waves[2].Tasks) != "[d]" {
		t.Errorf("wave 3: got %v, want [d]", waves[2].Tasks)
	}
}

// U-09: Mixed parallel + serial. A→B, C (independent), B→D = waves: [A,C], [B], [D]
func TestComputeWaves_MixedParallelSerial(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task", Blocks: []string{"b"}},
		"b": {ID: "b", Type: "task", BlockedBy: []string{"a"}, Blocks: []string{"d"}},
		"c": {ID: "c", Type: "task"},
		"d": {ID: "d", Type: "task", BlockedBy: []string{"b"}},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %+v", len(waves), waves)
	}
	// Wave 1=[a,c], Wave 2=[b], Wave 3=[d]
	if fmt.Sprintf("%v", waves[0].Tasks) != "[a c]" {
		t.Errorf("wave 1: got %v, want [a c]", waves[0].Tasks)
	}
	if fmt.Sprintf("%v", waves[1].Tasks) != "[b]" {
		t.Errorf("wave 2: got %v, want [b]", waves[1].Tasks)
	}
	if fmt.Sprintf("%v", waves[2].Tasks) != "[d]" {
		t.Errorf("wave 3: got %v, want [d]", waves[2].Tasks)
	}
}

// U-11: Excludes epics from waves
func TestComputeWaves_ExcludesEpics(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"epic-1": {ID: "epic-1", Type: "epic"},
		"task-1": {ID: "task-1", Type: "task"},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 1 {
		t.Fatalf("expected 1 wave, got %d", len(waves))
	}
	if len(waves[0].Tasks) != 1 || waves[0].Tasks[0] != "task-1" {
		t.Errorf("wave 1: got %v, want [task-1]", waves[0].Tasks)
	}
	// epic should not appear in any wave
	if waveOf(waves, "epic-1") != -1 {
		t.Error("epic-1 should not be in any wave")
	}
}

// U-12: Excludes non-slingable types (decision, epic, etc.)
func TestComputeWaves_ExcludesNonSlingable(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"d1":     {ID: "d1", Type: "decision"},
		"e1":     {ID: "e1", Type: "epic"},
		"task-1": {ID: "task-1", Type: "task"},
		"bug-1":  {ID: "bug-1", Type: "bug"},
		"feat-1": {ID: "feat-1", Type: "feature"},
		"ch-1":   {ID: "ch-1", Type: "chore"},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 1 {
		t.Fatalf("expected 1 wave, got %d", len(waves))
	}
	// Only slingable types in the wave
	all := allWaveTaskIDs(waves)
	if len(all) != 4 {
		t.Fatalf("expected 4 slingable tasks, got %d: %v", len(all), all)
	}
	// decision and epic should not appear
	for _, id := range all {
		if id == "d1" || id == "e1" {
			t.Errorf("non-slingable %q should not appear in waves", id)
		}
	}
}

// #2141: decision beads block downstream tasks even though decisions aren't slingable.
// A task blocked by an open decision must NOT appear in Wave 1.
func TestComputeWaves_DecisionBlocksTask(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"d1":     {ID: "d1", Type: "decision", Status: "open", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"d1"}},
		"task-2": {ID: "task-2", Type: "task", Status: "open"},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) < 1 {
		t.Fatalf("expected at least 1 wave, got %d", len(waves))
	}
	wave1Tasks := waves[0].Tasks
	for _, id := range wave1Tasks {
		if id == "task-1" {
			t.Errorf("task-1 should NOT be in Wave 1 — it's blocked by decision d1")
		}
		if id == "d1" {
			t.Errorf("decision d1 should NOT appear in any wave (not slingable)")
		}
	}
	found := false
	for _, id := range wave1Tasks {
		if id == "task-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("task-2 should be in Wave 1, got: %v", wave1Tasks)
	}
	for _, w := range waves {
		for _, id := range w.Tasks {
			if id == "d1" {
				t.Errorf("decision d1 should not appear in wave %d", w.Number)
			}
		}
	}
}

// #2141: closed decision beads do NOT block downstream tasks.
func TestComputeWaves_ClosedDecisionDoesNotBlock(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"d1":     {ID: "d1", Type: "decision", Status: "closed", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"d1"}},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 1 {
		t.Fatalf("expected 1 wave, got %d", len(waves))
	}
	if len(waves[0].Tasks) != 1 || waves[0].Tasks[0] != "task-1" {
		t.Errorf("task-1 should be in Wave 1 (decision is closed), got: %v", waves[0].Tasks)
	}
}

// U-13: parent-child deps don't create execution edges
func TestComputeWaves_ParentChildNotExecution(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"epic-1": {ID: "epic-1", Type: "epic", Children: []string{"task-1", "task-2"}},
		"task-1": {ID: "task-1", Type: "task", Parent: "epic-1"},
		"task-2": {ID: "task-2", Type: "task", Parent: "epic-1"},
	}}
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 1 {
		t.Fatalf("expected 1 wave, got %d: %+v", len(waves), waves)
	}
	// Both tasks in Wave 1 (parent-child doesn't block)
	if len(waves[0].Tasks) != 2 {
		t.Fatalf("expected 2 tasks in wave 1, got %d: %v", len(waves[0].Tasks), waves[0].Tasks)
	}
	if waveOf(waves, "task-1") != 1 || waveOf(waves, "task-2") != 1 {
		t.Errorf("both tasks should be in wave 1, got task-1=%d, task-2=%d",
			waveOf(waves, "task-1"), waveOf(waves, "task-2"))
	}
}

// U-14: Empty DAG (no slingable tasks) → error
func TestComputeWaves_EmptyDAG(t *testing.T) {
	t.Parallel()
	// Completely empty
	dag1 := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{}}
	_, _, err := computeWaves(dag1)
	if err == nil {
		t.Error("expected error for empty DAG, got nil")
	}

	// Only non-slingable types
	dag2 := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"epic-1":     {ID: "epic-1", Type: "epic"},
		"decision-1": {ID: "decision-1", Type: "decision"},
	}}
	_, _, err = computeWaves(dag2)
	if err == nil {
		t.Error("expected error for DAG with only non-slingable types, got nil")
	}
}

// ---------------------------------------------------------------------------
// Gated task tests — non-slingable blockers
// ---------------------------------------------------------------------------

// Task blocked by open decision → excluded from waves, returned as gated.
func TestComputeWaves_GatedByDecision(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"dec-1":  {ID: "dec-1", Type: "decision", Status: "open", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"dec-1"}},
		"task-2": {ID: "task-2", Type: "task", Status: "open"},
	}}
	waves, gated, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// task-2 should be in waves, task-1 should be gated
	if len(waves) != 1 || len(waves[0].Tasks) != 1 || waves[0].Tasks[0] != "task-2" {
		t.Errorf("expected wave 1 = [task-2], got %+v", waves)
	}
	if len(gated) != 1 || gated[0].TaskID != "task-1" {
		t.Errorf("expected gated = [task-1], got %+v", gated)
	}
	if len(gated[0].GatedBy) != 1 || gated[0].GatedBy[0] != "dec-1" {
		t.Errorf("expected gated by dec-1, got %v", gated[0].GatedBy)
	}
}

// task-A gated by decision, task-B depends on task-A → both gated.
func TestComputeWaves_GatedTransitive(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"dec-1":  {ID: "dec-1", Type: "decision", Status: "open", Blocks: []string{"task-a"}},
		"task-a": {ID: "task-a", Type: "task", Status: "open", BlockedBy: []string{"dec-1"}, Blocks: []string{"task-b"}},
		"task-b": {ID: "task-b", Type: "task", Status: "open", BlockedBy: []string{"task-a"}},
		"task-c": {ID: "task-c", Type: "task", Status: "open"},
	}}
	waves, gated, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// task-c in waves, task-a and task-b gated
	if len(waves) != 1 || len(waves[0].Tasks) != 1 || waves[0].Tasks[0] != "task-c" {
		t.Errorf("expected wave 1 = [task-c], got %+v", waves)
	}
	if len(gated) != 2 {
		t.Fatalf("expected 2 gated tasks, got %d: %+v", len(gated), gated)
	}
	gatedIDs := map[string]bool{}
	for _, g := range gated {
		gatedIDs[g.TaskID] = true
	}
	if !gatedIDs["task-a"] || !gatedIDs["task-b"] {
		t.Errorf("expected task-a and task-b gated, got %v", gatedIDs)
	}
	// task-a should have direct gate, task-b should have empty GatedBy (transitive)
	for _, g := range gated {
		if g.TaskID == "task-a" && (len(g.GatedBy) != 1 || g.GatedBy[0] != "dec-1") {
			t.Errorf("task-a should be gated by dec-1, got %v", g.GatedBy)
		}
		if g.TaskID == "task-b" && len(g.GatedBy) != 0 {
			t.Errorf("task-b should be transitively gated (empty GatedBy), got %v", g.GatedBy)
		}
	}
}

// Task blocked by closed decision → in waves (gate resolved).
func TestComputeWaves_ResolvedDecision(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"dec-1":  {ID: "dec-1", Type: "decision", Status: "closed", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"dec-1"}},
	}}
	waves, gated, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gated) != 0 {
		t.Errorf("expected no gated tasks (decision closed), got %+v", gated)
	}
	if len(waves) != 1 || len(waves[0].Tasks) != 1 || waves[0].Tasks[0] != "task-1" {
		t.Errorf("expected wave 1 = [task-1], got %+v", waves)
	}
}

// Task blocked by tombstoned decision → in waves.
func TestComputeWaves_TombstoneDecision(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"dec-1":  {ID: "dec-1", Type: "decision", Status: "tombstone", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"dec-1"}},
	}}
	waves, gated, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gated) != 0 {
		t.Errorf("expected no gated tasks (decision tombstoned), got %+v", gated)
	}
	if len(waves) != 1 || len(waves[0].Tasks) != 1 || waves[0].Tasks[0] != "task-1" {
		t.Errorf("expected wave 1 = [task-1], got %+v", waves)
	}
}

// Task blocked by open epic → gated.
func TestComputeWaves_GatedByEpic(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"epic-1": {ID: "epic-1", Type: "epic", Status: "open", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"epic-1"}},
		"task-2": {ID: "task-2", Type: "task", Status: "open"},
	}}
	waves, gated, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gated) != 1 || gated[0].TaskID != "task-1" {
		t.Errorf("expected task-1 gated by epic, got %+v", gated)
	}
	if len(waves) != 1 || waves[0].Tasks[0] != "task-2" {
		t.Errorf("expected wave 1 = [task-2], got %+v", waves)
	}
}

// All slingable tasks gated → empty waves, all returned as gated.
func TestComputeWaves_AllGated(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"dec-1":  {ID: "dec-1", Type: "decision", Status: "open", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"dec-1"}},
	}}
	waves, gated, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(waves) != 0 {
		t.Errorf("expected 0 waves when all tasks gated, got %d", len(waves))
	}
	if len(gated) != 1 {
		t.Errorf("expected 1 gated task, got %d", len(gated))
	}
}

// merge-blocks creates execution edge in DAG.
func TestBuildConvoyDAG_MergeBlocks(t *testing.T) {
	t.Parallel()
	beads := []BeadInfo{
		{ID: "mr-1", Title: "MR", Type: "task", Status: "open"},
		{ID: "task-1", Title: "Task", Type: "task", Status: "open"},
	}
	deps := []DepInfo{
		{IssueID: "task-1", DependsOnID: "mr-1", Type: "merge-blocks"},
	}
	dag := buildConvoyDAG(beads, deps)

	if node := dag.Nodes["task-1"]; node == nil {
		t.Fatal("task-1 not in DAG")
	} else if len(node.BlockedBy) != 1 || node.BlockedBy[0] != "mr-1" {
		t.Errorf("expected task-1 blocked by mr-1, got %v", node.BlockedBy)
	}
	if node := dag.Nodes["mr-1"]; node == nil {
		t.Fatal("mr-1 not in DAG")
	} else if len(node.Blocks) != 1 || node.Blocks[0] != "task-1" {
		t.Errorf("expected mr-1 blocks task-1, got %v", node.Blocks)
	}
}

// Task blocked by decision → not flagged as orphan.
func TestDetectOrphans_DecisionGatedNotOrphan(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"dec-1":  {ID: "dec-1", Type: "decision", Status: "open", Blocks: []string{"task-1"}},
		"task-1": {ID: "task-1", Type: "task", Status: "open", BlockedBy: []string{"dec-1"}},
	}}
	input := &StageInput{Kind: StageInputEpic}
	findings := detectOrphans(dag, input)
	for _, f := range findings {
		if f.Category == "orphan" && f.BeadIDs[0] == "task-1" {
			t.Error("task-1 should not be flagged as orphan — it is blocked by decision dec-1")
		}
	}
}

// ---------------------------------------------------------------------------
// Input parsing + validation tests (gt-csl.3.1)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// buildConvoyDAG tests (U-15 through U-19)
// ---------------------------------------------------------------------------

// sliceContains checks if a string slice contains a value.
func sliceContains(ss []string, val string) bool {
	for _, s := range ss {
		if s == val {
			return true
		}
	}
	return false
}

// U-15: blocks deps create execution edges
func TestBuildDAG_BlocksCreateEdges(t *testing.T) {
	t.Parallel()
	beads := []BeadInfo{
		{ID: "a", Title: "Task A", Type: "task", Status: "open"},
		{ID: "b", Title: "Task B", Type: "task", Status: "open"},
	}
	deps := []DepInfo{
		{IssueID: "b", DependsOnID: "a", Type: "blocks"},
	}
	dag := buildConvoyDAG(beads, deps)
	if dag == nil {
		t.Fatal("expected non-nil DAG")
	}
	nodeA := dag.Nodes["a"]
	nodeB := dag.Nodes["b"]
	if nodeA == nil || nodeB == nil {
		t.Fatal("expected both nodes to exist")
	}
	if !sliceContains(nodeA.Blocks, "b") {
		t.Errorf("a.Blocks should contain 'b', got %v", nodeA.Blocks)
	}
	if !sliceContains(nodeB.BlockedBy, "a") {
		t.Errorf("b.BlockedBy should contain 'a', got %v", nodeB.BlockedBy)
	}
}

// U-16: conditional-blocks create execution edges (same as blocks for DAG purposes)
func TestBuildDAG_ConditionalBlocksCreateEdges(t *testing.T) {
	t.Parallel()
	beads := []BeadInfo{
		{ID: "a", Title: "Task A", Type: "task", Status: "open"},
		{ID: "b", Title: "Task B", Type: "task", Status: "open"},
	}
	deps := []DepInfo{
		{IssueID: "b", DependsOnID: "a", Type: "conditional-blocks"},
	}
	dag := buildConvoyDAG(beads, deps)
	nodeA := dag.Nodes["a"]
	nodeB := dag.Nodes["b"]
	if !sliceContains(nodeA.Blocks, "b") {
		t.Errorf("a.Blocks should contain 'b' for conditional-blocks, got %v", nodeA.Blocks)
	}
	if !sliceContains(nodeB.BlockedBy, "a") {
		t.Errorf("b.BlockedBy should contain 'a' for conditional-blocks, got %v", nodeB.BlockedBy)
	}
}

// U-17: waits-for creates execution edges
func TestBuildDAG_WaitsForCreateEdges(t *testing.T) {
	t.Parallel()
	beads := []BeadInfo{
		{ID: "x", Title: "Task X", Type: "task", Status: "open"},
		{ID: "y", Title: "Task Y", Type: "task", Status: "open"},
	}
	deps := []DepInfo{
		{IssueID: "y", DependsOnID: "x", Type: "waits-for"},
	}
	dag := buildConvoyDAG(beads, deps)
	nodeX := dag.Nodes["x"]
	nodeY := dag.Nodes["y"]
	if !sliceContains(nodeX.Blocks, "y") {
		t.Errorf("x.Blocks should contain 'y' for waits-for, got %v", nodeX.Blocks)
	}
	if !sliceContains(nodeY.BlockedBy, "x") {
		t.Errorf("y.BlockedBy should contain 'x' for waits-for, got %v", nodeY.BlockedBy)
	}
}

// U-18: parent-child recorded as hierarchy but NO execution edge
func TestBuildDAG_ParentChildNoExecutionEdge(t *testing.T) {
	t.Parallel()
	beads := []BeadInfo{
		{ID: "epic-1", Title: "Root", Type: "epic", Status: "open"},
		{ID: "task-1", Title: "Child", Type: "task", Status: "open"},
	}
	deps := []DepInfo{
		{IssueID: "task-1", DependsOnID: "epic-1", Type: "parent-child"},
	}
	dag := buildConvoyDAG(beads, deps)
	epicNode := dag.Nodes["epic-1"]
	taskNode := dag.Nodes["task-1"]
	// Hierarchy should be set
	if !sliceContains(epicNode.Children, "task-1") {
		t.Errorf("epic-1.Children should contain 'task-1', got %v", epicNode.Children)
	}
	if taskNode.Parent != "epic-1" {
		t.Errorf("task-1.Parent should be 'epic-1', got %q", taskNode.Parent)
	}
	// Execution edges must NOT be set
	if len(epicNode.Blocks) != 0 {
		t.Errorf("epic-1.Blocks should be empty for parent-child, got %v", epicNode.Blocks)
	}
	if len(taskNode.BlockedBy) != 0 {
		t.Errorf("task-1.BlockedBy should be empty for parent-child, got %v", taskNode.BlockedBy)
	}
}

// U-19: related/tracks deps ignored entirely
func TestBuildDAG_RelatedTracksIgnored(t *testing.T) {
	t.Parallel()
	beads := []BeadInfo{
		{ID: "a", Title: "A", Type: "task", Status: "open"},
		{ID: "b", Title: "B", Type: "task", Status: "open"},
	}
	deps := []DepInfo{
		{IssueID: "a", DependsOnID: "b", Type: "related"},
		{IssueID: "a", DependsOnID: "b", Type: "tracks"},
	}
	dag := buildConvoyDAG(beads, deps)
	nodeA := dag.Nodes["a"]
	nodeB := dag.Nodes["b"]
	if len(nodeA.BlockedBy) != 0 || len(nodeA.Blocks) != 0 {
		t.Errorf("related/tracks should not create edges on a: BlockedBy=%v Blocks=%v", nodeA.BlockedBy, nodeA.Blocks)
	}
	if len(nodeB.BlockedBy) != 0 || len(nodeB.Blocks) != 0 {
		t.Errorf("related/tracks should not create edges on b: BlockedBy=%v Blocks=%v", nodeB.BlockedBy, nodeB.Blocks)
	}
	// Also no hierarchy
	if len(nodeA.Children) != 0 || nodeA.Parent != "" {
		t.Error("related/tracks should not set hierarchy on a")
	}
	if len(nodeB.Children) != 0 || nodeB.Parent != "" {
		t.Error("related/tracks should not set hierarchy on b")
	}
}

// ---------------------------------------------------------------------------
// collectBeads tests — Epic DAG walking (IT-01 through IT-04)
// ---------------------------------------------------------------------------

// IT-01: Epic walk collects all descendants across 3 levels.
// Tree: gt-epic → {gt-sub (epic), gt-task1 (task)}
//
//	gt-sub → {gt-task2 (task), gt-task3 (task)}
func TestEpicWalk_CollectsAllDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Epic("gt-epic", "Root Epic").
		Epic("gt-sub", "Sub Epic").ParentOf("gt-epic").
		Task("gt-task1", "Task 1", withRig("gastown")).ParentOf("gt-epic").
		Task("gt-task2", "Task 2", withRig("gastown")).ParentOf("gt-sub").
		Task("gt-task3", "Task 3", withRig("gastown")).ParentOf("gt-sub")

	dag.Setup(t)

	input := &StageInput{Kind: StageInputEpic, IDs: []string{"gt-epic"}}
	beads, _, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}

	// Should have 5 beads: epic, sub, task1, task2, task3
	if len(beads) != 5 {
		ids := make([]string, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
		}
		t.Errorf("expected 5 beads, got %d: %v", len(beads), ids)
	}

	// Verify all expected IDs present.
	idSet := make(map[string]bool)
	for _, b := range beads {
		idSet[b.ID] = true
	}
	for _, want := range []string{"gt-epic", "gt-sub", "gt-task1", "gt-task2", "gt-task3"} {
		if !idSet[want] {
			t.Errorf("missing bead %q in collected set", want)
		}
	}
}

// IT-02: Nonexistent epic bead returns error.
func TestEpicWalk_NonexistentBeadErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	// Set up a DAG with only one bead so "gt-missing" doesn't exist.
	dag := newTestDAG(t).
		Task("gt-exists", "Existing task", withRig("gastown"))
	dag.Setup(t)

	input := &StageInput{Kind: StageInputEpic, IDs: []string{"gt-missing"}}
	_, _, err := collectBeads(input)
	if err == nil {
		t.Fatal("expected error for nonexistent epic, got nil")
	}
}

// IT-03: Task list analyzes only given tasks.
func TestTaskListWalk_AnalyzesOnlyGiven(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Task("gt-a", "Task A", withRig("gastown")).
		Task("gt-b", "Task B", withRig("gastown")).BlockedBy("gt-a").
		Task("gt-c", "Task C", withRig("gastown")) // not requested
	dag.Setup(t)

	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b"}}
	beads, deps, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}

	// Should have exactly 2 beads.
	if len(beads) != 2 {
		ids := make([]string, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
		}
		t.Errorf("expected 2 beads, got %d: %v", len(beads), ids)
	}

	// Verify only gt-a and gt-b.
	idSet := make(map[string]bool)
	for _, b := range beads {
		idSet[b.ID] = true
	}
	if !idSet["gt-a"] || !idSet["gt-b"] {
		t.Errorf("expected gt-a and gt-b, got %v", idSet)
	}
	if idSet["gt-c"] {
		t.Error("gt-c should not be in collected beads")
	}

	// gt-b should have a dep on gt-a.
	foundDep := false
	for _, d := range deps {
		if d.IssueID == "gt-b" && d.DependsOnID == "gt-a" && d.Type == "blocks" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Errorf("expected dep gt-b blocked-by gt-a, got deps: %+v", deps)
	}
}

// IT-04: Convoy reads tracked beads.
func TestConvoyWalk_ReadsTrackedBeads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Convoy("gt-convoy", "Test Convoy").
		Task("gt-t1", "Tracked 1", withRig("gastown")).TrackedBy("gt-convoy").
		Task("gt-t2", "Tracked 2", withRig("gastown")).TrackedBy("gt-convoy")
	dag.Setup(t)

	input := &StageInput{Kind: StageInputConvoy, IDs: []string{"gt-convoy"}}
	beads, _, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}

	// Should have 2 tracked beads (convoy itself is not returned as a bead to stage).
	if len(beads) != 2 {
		ids := make([]string, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
		}
		t.Errorf("expected 2 beads, got %d: %v", len(beads), ids)
	}

	idSet := make(map[string]bool)
	for _, b := range beads {
		idSet[b.ID] = true
	}
	if !idSet["gt-t1"] || !idSet["gt-t2"] {
		t.Errorf("expected gt-t1 and gt-t2 in tracked beads, got %v", idSet)
	}
}

// IT-05: Epic walk collects deps across the tree.
func TestEpicWalk_CollectsDeps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Epic("gt-epic", "Root Epic").
		Task("gt-t1", "Task 1", withRig("gastown")).ParentOf("gt-epic").
		Task("gt-t2", "Task 2", withRig("gastown")).ParentOf("gt-epic").BlockedBy("gt-t1")
	dag.Setup(t)

	input := &StageInput{Kind: StageInputEpic, IDs: []string{"gt-epic"}}
	beads, deps, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}
	if len(beads) != 3 {
		t.Fatalf("expected 3 beads, got %d", len(beads))
	}

	// Should find the blocks dep and the parent-child deps.
	var depTypes []string
	for _, d := range deps {
		depTypes = append(depTypes, fmt.Sprintf("%s→%s(%s)", d.IssueID, d.DependsOnID, d.Type))
	}
	sort.Strings(depTypes)

	// Expect parent-child deps for gt-t1 and gt-t2, plus blocks dep gt-t2→gt-t1.
	foundBlocks := false
	for _, d := range deps {
		if d.IssueID == "gt-t2" && d.DependsOnID == "gt-t1" && d.Type == "blocks" {
			foundBlocks = true
		}
	}
	if !foundBlocks {
		t.Errorf("expected blocks dep gt-t2→gt-t1, got: %v", depTypes)
	}
}

// ---------------------------------------------------------------------------
// renderWaveTable tests (U-30, U-38, gt-csl.4.2)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Error detection + categorization tests (gt-csl.3.3)
// ---------------------------------------------------------------------------

// U-20: Cycle is categorized as error, not warning
func TestCategorize_CycleIsError(t *testing.T) {
	t.Parallel()
	findings := []StagingFinding{
		{Severity: "error", Category: "cycle", BeadIDs: []string{"a", "b"}, Message: "cycle"},
	}
	errs, warns := categorizeFindings(findings)
	if len(errs) != 1 {
		t.Errorf("expected 1 error, got %d", len(errs))
	}
	if len(warns) != 0 {
		t.Errorf("expected 0 warnings, got %d", len(warns))
	}
}

// U-21: No-rig is categorized as error
func TestCategorize_NoRigIsError(t *testing.T) {
	t.Parallel()
	findings := []StagingFinding{
		{Severity: "error", Category: "no-rig", BeadIDs: []string{"gt-xyz"}, Message: "no rig"},
	}
	errs, warns := categorizeFindings(findings)
	if len(errs) != 1 {
		t.Errorf("expected 1 error, got %d", len(errs))
	}
	if len(warns) != 0 {
		t.Errorf("expected 0 warnings, got %d", len(warns))
	}
}

// U-25: No errors + no warnings → staged_ready
func TestChooseStatus_Ready(t *testing.T) {
	t.Parallel()
	status := chooseStatus(nil, nil)
	if status != "staged_ready" {
		t.Errorf("expected staged_ready, got %q", status)
	}
}

// U-26: Warnings only → staged_warnings
func TestChooseStatus_Warnings(t *testing.T) {
	t.Parallel()
	warns := []StagingFinding{{Severity: "warning", Category: "blocked-rig"}}
	status := chooseStatus(nil, warns)
	if status != "staged_warnings" {
		t.Errorf("expected staged_warnings, got %q", status)
	}
}

// U-27: Any errors → no creation (empty string)
func TestChooseStatus_Errors(t *testing.T) {
	t.Parallel()
	errs := []StagingFinding{{Severity: "error", Category: "cycle"}}
	status := chooseStatus(errs, nil)
	if status != "" {
		t.Errorf("expected empty (no creation), got %q", status)
	}
}

// U-39: Error output includes bead IDs and suggested fix
func TestRenderErrors_IncludesFixAndIDs(t *testing.T) {
	t.Parallel()
	findings := []StagingFinding{
		{Severity: "error", Category: "cycle", BeadIDs: []string{"a", "b"},
			Message:      "cycle detected: a → b → a",
			SuggestedFix: "remove one blocking dep"},
	}
	output := renderErrors(findings)
	if !strings.Contains(output, "a, b") {
		t.Error("should include bead IDs")
	}
	if !strings.Contains(output, "remove one blocking dep") {
		t.Error("should include suggested fix")
	}
	if !strings.Contains(output, "cycle") {
		t.Error("should include category")
	}
}

// Test detectErrors with cycle
func TestErrorDetection_CycleDetected(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task", Rig: "gastown", Blocks: []string{"b"}, BlockedBy: []string{"b"}},
		"b": {ID: "b", Type: "task", Rig: "gastown", BlockedBy: []string{"a"}, Blocks: []string{"a"}},
	}}

	findings := detectErrors(dag)
	errs, _ := categorizeFindings(findings)
	if len(errs) == 0 {
		t.Fatal("expected cycle error")
	}
	if errs[0].Category != "cycle" {
		t.Errorf("expected cycle, got %s", errs[0].Category)
	}
}

// Test detectErrors with no rig
func TestErrorDetection_NoRig(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task", Rig: ""}, // no rig!
	}}
	findings := detectErrors(dag)
	errs, _ := categorizeFindings(findings)
	if len(errs) == 0 {
		t.Fatal("expected no-rig error")
	}
	if errs[0].Category != "no-rig" {
		t.Errorf("expected no-rig, got %s", errs[0].Category)
	}
}

// Test detectErrors clean DAG → no errors
func TestErrorDetection_Clean(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"a": {ID: "a", Type: "task", Rig: "gastown", Blocks: []string{"b"}},
		"b": {ID: "b", Type: "task", Rig: "gastown", BlockedBy: []string{"a"}},
	}}
	findings := detectErrors(dag)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// renderDAGTree tests (gt-csl.4.1)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Warning detection tests (gt-csl.3.4)
// ---------------------------------------------------------------------------

// U-22: Parked rig detected and warned
// This test uses the isRigParkedFn seam to mock parked rig detection.
func TestDetectWarnings_ParkedRig(t *testing.T) {
	// Set up a temp dir as town root and cd there for workspace.FindFromCwd()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}
	oldDir, _ := os.Getwd()
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(oldDir) })

	// Override isRigBlockedFn to return true for "parkedrig"
	origFn := isRigBlockedFn
	isRigBlockedFn = func(townRoot, rigName string) (bool, string) {
		if rigName == "parkedrig" {
			return true, "parked"
		}
		return false, ""
	}
	t.Cleanup(func() { isRigBlockedFn = origFn })

	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"gt-a": {ID: "gt-a", Type: "task", Rig: "parkedrig"},
		"gt-b": {ID: "gt-b", Type: "task", Rig: "gastown"},
	}}
	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b"}}
	findings := detectWarnings(dag, input)

	var parkedFindings []StagingFinding
	for _, f := range findings {
		if f.Category == "blocked-rig" {
			parkedFindings = append(parkedFindings, f)
		}
	}
	if len(parkedFindings) != 1 {
		t.Fatalf("expected 1 blocked-rig warning, got %d: %+v", len(parkedFindings), findings)
	}
	f := parkedFindings[0]
	if f.Severity != "warning" {
		t.Errorf("severity = %q, want %q", f.Severity, "warning")
	}
	if !sliceContains(f.BeadIDs, "gt-a") {
		t.Errorf("BeadIDs should contain gt-a, got %v", f.BeadIDs)
	}
}

// Regression test for #2120 review item #1: docked rigs should also be detected.
func TestDetectWarnings_DockedRig(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}
	oldDir, _ := os.Getwd()
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(oldDir) })

	// Override isRigBlockedFn to return docked for "dockedrig"
	origFn := isRigBlockedFn
	isRigBlockedFn = func(townRoot, rigName string) (bool, string) {
		if rigName == "dockedrig" {
			return true, "docked"
		}
		return false, ""
	}
	t.Cleanup(func() { isRigBlockedFn = origFn })

	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"gt-a": {ID: "gt-a", Type: "task", Rig: "dockedrig"},
		"gt-b": {ID: "gt-b", Type: "task", Rig: "gastown"},
	}}
	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b"}}
	findings := detectWarnings(dag, input)

	var blockedFindings []StagingFinding
	for _, f := range findings {
		if f.Category == "blocked-rig" {
			blockedFindings = append(blockedFindings, f)
		}
	}
	if len(blockedFindings) != 1 {
		t.Fatalf("expected 1 blocked-rig warning for docked rig, got %d: %+v", len(blockedFindings), findings)
	}
	f := blockedFindings[0]
	if f.Severity != "warning" {
		t.Errorf("severity = %q, want %q", f.Severity, "warning")
	}
	if !sliceContains(f.BeadIDs, "gt-a") {
		t.Errorf("BeadIDs should contain gt-a, got %v", f.BeadIDs)
	}
	if !strings.Contains(f.Message, "docked") {
		t.Errorf("message should mention 'docked', got: %s", f.Message)
	}
	if !strings.Contains(f.SuggestedFix, "undock") {
		t.Errorf("suggested fix should mention 'undock', got: %s", f.SuggestedFix)
	}
}

// U-23: Orphan detection for epic input
func TestDetectWarnings_OrphanEpicInput(t *testing.T) {
	t.Parallel()
	// 3 tasks under an epic: A blocks B (connected), C is isolated.
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"epic-1": {ID: "epic-1", Type: "epic", Children: []string{"gt-a", "gt-b", "gt-c"}},
		"gt-a":   {ID: "gt-a", Type: "task", Rig: "gastown", Parent: "epic-1", Blocks: []string{"gt-b"}},
		"gt-b":   {ID: "gt-b", Type: "task", Rig: "gastown", Parent: "epic-1", BlockedBy: []string{"gt-a"}},
		"gt-c":   {ID: "gt-c", Type: "task", Rig: "gastown", Parent: "epic-1"},
	}}
	input := &StageInput{Kind: StageInputEpic, IDs: []string{"epic-1"}}
	findings := detectWarnings(dag, input)

	var orphanFindings []StagingFinding
	for _, f := range findings {
		if f.Category == "orphan" {
			orphanFindings = append(orphanFindings, f)
		}
	}
	if len(orphanFindings) != 1 {
		t.Fatalf("expected 1 orphan warning, got %d: %+v", len(orphanFindings), findings)
	}
	if !sliceContains(orphanFindings[0].BeadIDs, "gt-c") {
		t.Errorf("orphan warning should reference gt-c, got %v", orphanFindings[0].BeadIDs)
	}
}

// U-24: Missing integration branch warning
func TestDetectWarnings_MissingBranch(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"root-epic": {ID: "root-epic", Type: "epic", Children: []string{"sub-epic"}},
		"sub-epic":  {ID: "sub-epic", Type: "epic", Parent: "root-epic", Children: []string{"gt-a", "gt-b"}},
		"gt-a":      {ID: "gt-a", Type: "task", Rig: "gastown", Parent: "sub-epic"},
		"gt-b":      {ID: "gt-b", Type: "task", Rig: "gastown", Parent: "sub-epic"},
	}}
	input := &StageInput{Kind: StageInputEpic, IDs: []string{"root-epic"}}
	findings := detectWarnings(dag, input)

	var branchFindings []StagingFinding
	for _, f := range findings {
		if f.Category == "missing-branch" {
			branchFindings = append(branchFindings, f)
		}
	}
	if len(branchFindings) != 1 {
		t.Fatalf("expected 1 missing-branch warning, got %d: %+v", len(branchFindings), findings)
	}
	f := branchFindings[0]
	if f.Severity != "warning" {
		t.Errorf("severity = %q, want %q", f.Severity, "warning")
	}
	if !sliceContains(f.BeadIDs, "sub-epic") {
		t.Errorf("BeadIDs should contain sub-epic, got %v", f.BeadIDs)
	}
	if !strings.Contains(f.SuggestedFix, "sub-epic") {
		t.Errorf("SuggestedFix should mention sub-epic, got %q", f.SuggestedFix)
	}
}

// U-34: Cross-rig routing mismatch warned
func TestDetectWarnings_CrossRig(t *testing.T) {
	t.Parallel()
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"gt-a": {ID: "gt-a", Type: "task", Rig: "gastown"},
		"gt-b": {ID: "gt-b", Type: "task", Rig: "gastown"},
		"bd-c": {ID: "bd-c", Type: "task", Rig: "beads"},
	}}
	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b", "bd-c"}}
	findings := detectWarnings(dag, input)

	var crossFindings []StagingFinding
	for _, f := range findings {
		if f.Category == "cross-rig" {
			crossFindings = append(crossFindings, f)
		}
	}
	if len(crossFindings) != 1 {
		t.Fatalf("expected 1 cross-rig warning, got %d: %+v", len(crossFindings), findings)
	}
	f := crossFindings[0]
	if f.Severity != "warning" {
		t.Errorf("severity = %q, want %q", f.Severity, "warning")
	}
	if !sliceContains(f.BeadIDs, "bd-c") {
		t.Errorf("BeadIDs should contain bd-c, got %v", f.BeadIDs)
	}
	if !strings.Contains(f.Message, "gastown") {
		t.Errorf("Message should mention primary rig gastown, got %q", f.Message)
	}
}

// U-35: Capacity estimation
func TestDetectWarnings_Capacity(t *testing.T) {
	t.Parallel()
	// Create a DAG where wave 1 has 6 independent tasks (all in-degree 0).
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"t1": {ID: "t1", Type: "task", Rig: "gastown"},
		"t2": {ID: "t2", Type: "task", Rig: "gastown"},
		"t3": {ID: "t3", Type: "task", Rig: "gastown"},
		"t4": {ID: "t4", Type: "task", Rig: "gastown"},
		"t5": {ID: "t5", Type: "task", Rig: "gastown"},
		"t6": {ID: "t6", Type: "task", Rig: "gastown"},
	}}

	// Verify computeWaves puts them all in wave 1.
	waves, _, err := computeWaves(dag)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if len(waves) != 1 || len(waves[0].Tasks) != 6 {
		t.Fatalf("expected 1 wave with 6 tasks, got %d waves with tasks: %+v", len(waves), waves)
	}

	input := &StageInput{Kind: StageInputTasks, IDs: []string{"t1", "t2", "t3", "t4", "t5", "t6"}}
	findings := detectWarnings(dag, input)

	var capFindings []StagingFinding
	for _, f := range findings {
		if f.Category == "capacity" {
			capFindings = append(capFindings, f)
		}
	}
	if len(capFindings) != 1 {
		t.Fatalf("expected 1 capacity warning, got %d: %+v", len(capFindings), findings)
	}
	f := capFindings[0]
	if f.Severity != "warning" {
		t.Errorf("severity = %q, want %q", f.Severity, "warning")
	}
	if !strings.Contains(f.Message, "wave 1") {
		t.Errorf("Message should mention wave 1, got %q", f.Message)
	}
	if !strings.Contains(f.Message, "6 tasks") {
		t.Errorf("Message should mention 6 tasks, got %q", f.Message)
	}
}

// IT-43: Orphan detection skipped for task-list input
func TestDetectWarnings_NoOrphansForTaskList(t *testing.T) {
	t.Parallel()
	// Same DAG as orphan test but with task-list input.
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"gt-a": {ID: "gt-a", Type: "task", Rig: "gastown", Blocks: []string{"gt-b"}},
		"gt-b": {ID: "gt-b", Type: "task", Rig: "gastown", BlockedBy: []string{"gt-a"}},
		"gt-c": {ID: "gt-c", Type: "task", Rig: "gastown"}, // isolated
	}}
	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b", "gt-c"}}
	findings := detectWarnings(dag, input)

	for _, f := range findings {
		if f.Category == "orphan" {
			t.Errorf("task-list input should NOT produce orphan warnings, got: %+v", f)
		}
	}
}

// Test detectWarnings clean DAG — no warnings
func TestDetectWarnings_Clean(t *testing.T) {
	// Override isRigBlockedFn so the test doesn't depend on real rig state.
	origFn := isRigBlockedFn
	isRigBlockedFn = func(townRoot, rigName string) (bool, string) { return false, "" }
	t.Cleanup(func() { isRigBlockedFn = origFn })

	// All tasks on same rig, all have deps between them, epic input.
	dag := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"epic-1": {ID: "epic-1", Type: "epic", Children: []string{"gt-a", "gt-b", "gt-c"}},
		"gt-a":   {ID: "gt-a", Type: "task", Rig: "gastown", Parent: "epic-1", Blocks: []string{"gt-b"}},
		"gt-b":   {ID: "gt-b", Type: "task", Rig: "gastown", Parent: "epic-1", BlockedBy: []string{"gt-a"}, Blocks: []string{"gt-c"}},
		"gt-c":   {ID: "gt-c", Type: "task", Rig: "gastown", Parent: "epic-1", BlockedBy: []string{"gt-b"}},
	}}
	input := &StageInput{Kind: StageInputEpic, IDs: []string{"epic-1"}}
	findings := detectWarnings(dag, input)
	if len(findings) != 0 {
		t.Errorf("expected 0 warnings for clean DAG, got %d: %+v", len(findings), findings)
	}
}

// ---------------------------------------------------------------------------
// Staged convoy creation tests (gt-csl.3.5)
// ---------------------------------------------------------------------------

// IT-10: Stage clean (no errors, no warnings) → creates convoy as staged_ready.
// Uses dagBuilder to set up the bd stub environment. Builds a clean ConvoyDAG
// directly (with rigs set). Verifies `bd create` was called with
// --status=staged_ready and `bd dep add` was called for each slingable bead.
func TestCreateStagedConvoy_CleanReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	// Set up bd stub environment for create/dep add commands.
	testDAG := newTestDAG(t).
		Task("gt-a", "Task A", withRig("gastown")).
		Task("gt-b", "Task B", withRig("gastown")).BlockedBy("gt-a").
		Task("gt-c", "Task C", withRig("gastown")).BlockedBy("gt-b")

	_, logPath := testDAG.Setup(t)

	// Build the ConvoyDAG directly with rigs populated (avoids rigFromBeadID stub).
	convoyDAG := &ConvoyDAG{Nodes: map[string]*ConvoyDAGNode{
		"gt-a": {ID: "gt-a", Title: "Task A", Type: "task", Status: "open", Rig: "gastown",
			Blocks: []string{"gt-b"}},
		"gt-b": {ID: "gt-b", Title: "Task B", Type: "task", Status: "open", Rig: "gastown",
			BlockedBy: []string{"gt-a"}, Blocks: []string{"gt-c"}},
		"gt-c": {ID: "gt-c", Title: "Task C", Type: "task", Status: "open", Rig: "gastown",
			BlockedBy: []string{"gt-b"}},
	}}

	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b", "gt-c"}}

	// Run the full error/warning detection pipeline.
	errFindings := detectErrors(convoyDAG)
	warnFindings := detectWarnings(convoyDAG, input)
	errs, warns := categorizeFindings(append(errFindings, warnFindings...))
	status := chooseStatus(errs, warns)

	if status != "staged_ready" {
		t.Fatalf("expected staged_ready, got %q", status)
	}

	waves, _, err := computeWaves(convoyDAG)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}

	convoyID, err := createStagedConvoy(convoyDAG, waves, status, "")
	if err != nil {
		t.Fatalf("createStagedConvoy: %v", err)
	}

	if convoyID == "" {
		t.Fatal("expected non-empty convoy ID")
	}
	if !strings.HasPrefix(convoyID, "hq-cv-") {
		t.Errorf("convoy ID should start with hq-cv-, got %q", convoyID)
	}

	// Read bd.log and verify bd create was called with --status=staged_ready.
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd.log: %v", err)
	}
	logContent := string(logBytes)

	if !strings.Contains(logContent, "create") {
		t.Errorf("bd.log should contain 'create' command, got:\n%s", logContent)
	}
	if !strings.Contains(logContent, "--status=staged_ready") {
		t.Errorf("bd.log should contain '--status=staged_ready', got:\n%s", logContent)
	}

	// Verify bd dep add was called for each slingable bead.
	for _, beadID := range []string{"gt-a", "gt-b", "gt-c"} {
		targetID := "external:gt:" + beadID
		if !strings.Contains(logContent, "dep add "+convoyID+" "+targetID) {
			t.Errorf("bd.log should contain 'dep add %s %s', got:\n%s", convoyID, targetID, logContent)
		}
	}
}

// IT-11: Stage convoy tracks all slingable beads via deps.
// Verifies that epics are NOT tracked, but tasks/bugs ARE tracked.
func TestCreateStagedConvoy_TracksOnlySlingable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Epic("gt-epic", "Root Epic").
		Task("gt-t1", "Task 1", withRig("gastown")).ParentOf("gt-epic").
		Bug("gt-b1", "Bug 1", withRig("gastown")).ParentOf("gt-epic").
		Task("gt-t2", "Task 2", withRig("gastown")).ParentOf("gt-epic").BlockedBy("gt-t1")

	_, logPath := dag.Setup(t)

	input := &StageInput{Kind: StageInputEpic, IDs: []string{"gt-epic"}}
	beads, deps, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}

	convoyDAG := buildConvoyDAG(beads, deps)

	waves, _, err := computeWaves(convoyDAG)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}

	convoyID, err := createStagedConvoy(convoyDAG, waves, "staged_ready", "")
	if err != nil {
		t.Fatalf("createStagedConvoy: %v", err)
	}

	// Read bd.log.
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd.log: %v", err)
	}
	logContent := string(logBytes)

	// Slingable beads (tasks and bugs) should be tracked.
	for _, beadID := range []string{"gt-t1", "gt-b1", "gt-t2"} {
		targetID := "external:gt:" + beadID
		if !strings.Contains(logContent, "dep add "+convoyID+" "+targetID) {
			t.Errorf("bd.log should contain 'dep add %s %s' for slingable bead, got:\n%s", convoyID, targetID, logContent)
		}
	}

	// Epics should NOT be tracked.
	lines := strings.Split(logContent, "\n")
	for _, line := range lines {
		if strings.Contains(line, "dep add") && strings.Contains(line, "gt-epic") {
			t.Errorf("epic gt-epic should NOT be tracked via dep add, but found: %s", line)
		}
	}
}

// IT-12: Stage convoy description includes wave count + timestamp.
func TestCreateStagedConvoy_DescriptionFormat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Task("gt-a", "Task A", withRig("gastown")).
		Task("gt-b", "Task B", withRig("gastown")).BlockedBy("gt-a")

	_, logPath := dag.Setup(t)

	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a", "gt-b"}}
	beads, deps, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}

	convoyDAG := buildConvoyDAG(beads, deps)

	waves, _, err := computeWaves(convoyDAG)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}

	_, err = createStagedConvoy(convoyDAG, waves, "staged_ready", "")
	if err != nil {
		t.Fatalf("createStagedConvoy: %v", err)
	}

	// Read bd.log to find the create command and verify description.
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd.log: %v", err)
	}
	logContent := string(logBytes)

	// Find the create command line.
	lines := strings.Split(logContent, "\n")
	var createLine string
	for _, line := range lines {
		if strings.Contains(line, "create") && strings.Contains(line, "--type=task") && strings.Contains(line, "--labels=gt:convoy") {
			createLine = line
			break
		}
	}
	if createLine == "" {
		t.Fatalf("no create command found in bd.log:\n%s", logContent)
	}

	// Description should include task count, wave count, and a timestamp.
	if !strings.Contains(createLine, "2 tasks") {
		t.Errorf("create command should mention '2 tasks' in description, got: %s", createLine)
	}
	if !strings.Contains(createLine, "2 waves") {
		t.Errorf("create command should mention '2 waves' in description, got: %s", createLine)
	}
	// Timestamp should look like an RFC3339 date (contains T and Z or +).
	if !strings.Contains(createLine, "Staged at") {
		t.Errorf("create command should contain 'Staged at' timestamp, got: %s", createLine)
	}
}

// IT-41: Convoy ID printed to stdout.
func TestCreateStagedConvoy_IDFormat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — shell stubs")
	}

	dag := newTestDAG(t).
		Task("gt-a", "Task A", withRig("gastown"))

	dag.Setup(t)

	input := &StageInput{Kind: StageInputTasks, IDs: []string{"gt-a"}}
	beads, deps, err := collectBeads(input)
	if err != nil {
		t.Fatalf("collectBeads: %v", err)
	}

	convoyDAG := buildConvoyDAG(beads, deps)

	waves, _, err := computeWaves(convoyDAG)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}

	convoyID, err := createStagedConvoy(convoyDAG, waves, "staged_ready", "")
	if err != nil {
		t.Fatalf("createStagedConvoy: %v", err)
	}

	// Convoy ID must be non-empty and start with hq-cv-.
	if convoyID == "" {
		t.Fatal("convoy ID should not be empty")
	}
	if !strings.HasPrefix(convoyID, "hq-cv-") {
		t.Errorf("convoy ID should start with 'hq-cv-', got %q", convoyID)
	}
	// The suffix should be base36 (lowercase alphanumeric).
	suffix := strings.TrimPrefix(convoyID, "hq-cv-")
	for _, ch := range suffix {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
			t.Errorf("convoy ID suffix should be base36 chars, got %q in %q", string(ch), suffix)
		}
	}
}

// ---------------------------------------------------------------------------
// Re-stage existing convoy tests (gt-csl.3.6)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// JSON output mode tests (gt-csl.4.3)
// ---------------------------------------------------------------------------

func runStageCommandJSONTest(t *testing.T, cmd *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	oldStdout := os.Stdout
	stdoutR, stdoutW, _ := os.Pipe()
	os.Stdout = stdoutW

	oldStderr := os.Stderr
	stderrR, stderrW, _ := os.Pipe()
	os.Stderr = stderrW

	cmd.SetArgs(args)
	err := cmd.Execute()

	stdoutW.Close()
	stderrW.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	outBytes, _ := io.ReadAll(stdoutR)
	stderrBytes, _ := io.ReadAll(stderrR)
	return string(outBytes), string(stderrBytes), err
}
