package cmd

import (
	"errors"
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
// Each row runs its path's own SlingParams builder, because carrying the agent
// across that boundary is the step that can drop it: the immediate path through
// convoySlingParams, the deferred path through convoyScheduleOptionsFor. A row
// that reads the planned job instead stops short of the only line a regression
// in either path would break (gt-3e0wv).
func TestConvoyDispatchPathsCarryRecordedAgent(t *testing.T) {
	t.Parallel()
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
				record := convoyRecord{ID: "gt-abc", Description: description}
				jobs := planConvoyDispatch([]convoyCandidate{candidate}, record, townRoot)
				return convoyScheduleOptionsFor(opts, jobs[0].agent).Agent
			},
		},
		{
			name: "immediate: gt sling <convoy> -> convoySlingParams -> executeSling",
			dispatchAgent: func(description string) string {
				record := convoyRecord{ID: "gt-abc", Description: description}
				jobs := planConvoyDispatch([]convoyCandidate{candidate}, record, townRoot)
				return convoySlingParams(jobs[0], opts, townRoot).Agent
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
	t.Parallel()
	townRoot := t.TempDir()

	candidates := []convoyCandidate{
		{ID: "gt-abc", Title: "First", RigName: "gastown"},
		{ID: "gt-def", Title: "Second", RigName: "beads"},
	}
	description := "Auto-created convoy\n\nmerge: mr\nagent: deepseek-flash\n"
	record := convoyRecord{ID: "gt-abc", Description: description}

	jobs := planConvoyDispatch(candidates, record, townRoot)
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

// TestConvoyRecordByID_UnreadableConvoy pins the failure mode of the convoy
// lookup behind planConvoyDispatch: a convoy whose description cannot be read
// (convoyRecordByID carries bdShow's error in Err) must degrade to the
// gt-sling-decides path, not to a wrong agent — and it must say the read
// failed. A convoy whose record never loaded is not a convoy that recorded no
// agent, and the log is the only place that difference is visible (gt-d7hwr).
func TestConvoyRecordByID_UnreadableConvoy(t *testing.T) {
	t.Parallel()
	record := convoyRecord{ID: "gt-nosuchconvoy", Err: errors.New("bd show gt-nosuchconvoy: not found")}

	// An unreadable description carries no agent, so the dispatch falls through
	// to gt sling's own resolution.
	jobs := planConvoyDispatch(
		[]convoyCandidate{{ID: "gt-abc", RigName: "gastown"}},
		record, t.TempDir(),
	)
	if len(jobs) != 1 || jobs[0].agent != "" {
		t.Fatalf("unreadable convoy should leave the agent choice to gt sling, got %+v", jobs)
	}
	// The log line must not report the readable-convoy default: "no --agent
	// recorded" asserts the record was read and held no agent.
	if strings.Contains(jobs[0].agentDesc, "no --agent recorded") {
		t.Errorf("description %q claims the convoy recorded no agent, but its record never loaded", jobs[0].agentDesc)
	}
	if !strings.Contains(jobs[0].agentDesc, "could not be read") {
		t.Errorf("description %q should name the failed convoy read", jobs[0].agentDesc)
	}
}

// TestConvoyDispatchPathsCarryRecordedFormula is the gt-o9sbq counterpart of
// the agent table above: the two manual 'gt sling <convoy>' schedulers must
// re-dispatch with the formula the convoy recorded at sling time, not the
// default formula resolveFormula fills in, unless the operator names one.
// Each row runs its path's own SlingParams builder, because that is the step
// that can drop the formula.
func TestConvoyDispatchPathsCarryRecordedFormula(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	const recorded = "mol-custom"
	const defaulted = "mol-polecat-work"
	withFormula := convoyRecord{ID: "gt-abc", Description: "Auto-created convoy\n\nmerge: mr\nformula: " + recorded + "\n"}
	noFormula := convoyRecord{ID: "gt-abc", Description: "Auto-created convoy\n\nmerge: mr\n"}
	unreadable := convoyRecord{ID: "gt-abc", Err: errors.New("bd show failed")}

	candidate := convoyCandidate{ID: "gt-abc", Title: "Work", RigName: "gastown"}
	job := convoyDispatchJob{candidate: candidate}

	rows := []struct {
		name   string
		opts   convoyScheduleOpts
		record convoyRecord
		want   string
	}{
		{"recorded formula beats the resolved default", convoyScheduleOpts{Formula: defaulted}, withFormula, recorded},
		{"explicit --formula beats the record", convoyScheduleOpts{Formula: "mol-operator", FormulaExplicit: true}, withFormula, "mol-operator"},
		{"--hook-raw-bead stays raw", convoyScheduleOpts{Formula: "", HookRawBead: true}, withFormula, ""},
		{"nothing recorded keeps the resolved default", convoyScheduleOpts{Formula: defaulted}, noFormula, defaulted},
		{"unreadable convoy keeps the resolved default", convoyScheduleOpts{Formula: defaulted}, unreadable, defaulted},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			opts, desc := convoyOptsWithRecordedFormula(tc.opts, tc.record)
			if opts.Formula != tc.want {
				t.Fatalf("opts.Formula = %q, want %q", opts.Formula, tc.want)
			}
			if applied := opts.Formula != tc.opts.Formula; applied != (desc != "") {
				t.Errorf("description %q must be set exactly when the recorded formula was applied", desc)
			}
			if got := convoySlingParams(job, opts, townRoot).FormulaName; got != tc.want {
				t.Errorf("immediate path FormulaName = %q, want %q", got, tc.want)
			}
			if got := convoyScheduleOptionsFor(opts, "").Formula; got != tc.want {
				t.Errorf("deferred path Formula = %q, want %q", got, tc.want)
			}
		})
	}
}
