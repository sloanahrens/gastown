package cmd

import (
	"strings"
	"testing"
)

// TestConvoyDispatchPathsCarryRecordedAgent is the gt-mxyk table: every path
// that re-dispatches a convoy's tracked beads must carry the runtime agent the
// convoy recorded at sling time, so a routing decision made with
// 'gt sling <bead> <rig> --agent X' survives the round trip instead of falling
// back to the rig default.
//
// gt-yg24 established that invariant for the automatic feeders (the daemon's
// stranded scan and the event-driven feeder); these rows are the two manual
// 'gt sling <convoy>' schedulers, which om flagged as still dropping the record.
// A new dispatch path belongs in this table.
//
// Each row runs its path's own resolution — the immediate path builds its
// SlingParams literal in runConvoySlingByID and takes the agent straight off the
// job; the deferred path hands it to scheduleBead through
// convoyScheduleOptionsFor, which the row exercises in full.
func TestConvoyDispatchPathsCarryRecordedAgent(t *testing.T) {
	townRoot := t.TempDir()
	const want = "deepseek-flash"

	// Convoy descriptions as beads.SetConvoyFields writes them, with and without
	// a recorded sling-time agent.
	recorded := "Auto-created convoy tracking gt-abc\n\nmerge: mr\nagent: " + want + "\n"
	unrecorded := "Auto-created convoy tracking gt-abc\n\nmerge: mr\n"

	candidate := convoyCandidate{ID: "gt-abc", Title: "Work", RigName: "gastown"}
	opts := convoyScheduleOpts{Formula: "mol-polecat-work", Force: true, NoBoot: true}

	tests := []struct {
		name string
		// dispatchAgent is the agent the path hands its dispatcher.
		dispatchAgent func(description string) string
	}{
		{
			name: "deferred: gt sling <convoy> -> scheduleBead",
			dispatchAgent: func(description string) string {
				jobs := planConvoyDispatch([]convoyCandidate{candidate}, description, townRoot)
				return convoyScheduleOptionsFor(opts, jobs[0].agent).Agent
			},
		},
		{
			name: "immediate: gt sling <convoy> -> executeSling",
			dispatchAgent: func(description string) string {
				jobs := planConvoyDispatch([]convoyCandidate{candidate}, description, townRoot)
				return jobs[0].agent
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.dispatchAgent(recorded); got != want {
				t.Errorf("dispatched agent = %q, want %q (agent recorded on the convoy at sling time)", got, want)
			}
			// Nothing recorded means gt sling's own resolution stays
			// authoritative — an empty flag, not a hard-coded rig default.
			if got := tc.dispatchAgent(unrecorded); got != "" {
				t.Errorf("dispatched agent = %q, want empty (no agent recorded, choice left to gt sling)", got)
			}
		})
	}
}

// TestConvoyDispatchPlanPerCandidateAgent covers the case the two manual paths
// share a candidate list but not a rig: the recorded agent is convoy-wide, so it
// must reach every candidate, while the no-agent fallback description names the
// rig each candidate is actually going to.
func TestConvoyDispatchPlanPerCandidateAgent(t *testing.T) {
	townRoot := t.TempDir()

	candidates := []convoyCandidate{
		{ID: "gt-abc", Title: "First", RigName: "gastown"},
		{ID: "gt-def", Title: "Second", RigName: "beads"},
	}
	description := "Auto-created convoy\n\nmerge: mr\nagent: deepseek-flash\n"

	jobs := planConvoyDispatch(candidates, description, townRoot)
	if len(jobs) != len(candidates) {
		t.Fatalf("planConvoyDispatch returned %d jobs, want %d", len(jobs), len(candidates))
	}
	for i, job := range jobs {
		if job.candidate != candidates[i] {
			t.Errorf("job %d candidate = %+v, want %+v", i, job.candidate, candidates[i])
		}
		if job.agent != "deepseek-flash" {
			t.Errorf("job %d agent = %q, want %q for every candidate on the convoy", i, job.agent, "deepseek-flash")
		}
		if !strings.Contains(job.agentDesc, "recorded on convoy") {
			t.Errorf("job %d description %q should say the agent was recorded on the convoy", i, job.agentDesc)
		}
	}
}

// TestConvoyDescriptionByID_UnreadableConvoy pins the failure mode of the
// convoy lookup behind planConvoyDispatch: a convoy whose description cannot be
// read must degrade to the gt-sling-decides path, not to a wrong agent.
func TestConvoyDescriptionByID_UnreadableConvoy(t *testing.T) {
	// No bd on PATH and no workspace: the lookup fails rather than inventing a
	// convoy.
	t.Setenv("PATH", t.TempDir())

	if got := convoyDescriptionByID("gt-nosuchconvoy"); got != "" {
		t.Errorf("convoyDescriptionByID() = %q, want empty when the convoy cannot be read", got)
	}

	// An empty description carries no agent, so the dispatch falls through to
	// gt sling's own resolution.
	jobs := planConvoyDispatch(
		[]convoyCandidate{{ID: "gt-abc", RigName: "gastown"}},
		"", t.TempDir(),
	)
	if len(jobs) != 1 || jobs[0].agent != "" {
		t.Fatalf("unreadable convoy should leave the agent choice to gt sling, got %+v", jobs)
	}
	if !strings.Contains(jobs[0].agentDesc, "no --agent recorded on convoy") {
		t.Errorf("description %q should explain why the default is used", jobs[0].agentDesc)
	}
}
