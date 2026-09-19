package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/witness"
)

func TestPatrolStateCollapseOutputJSON(t *testing.T) {
	output := PatrolStateCollapseOutput{
		Rig:            "gastown",
		Checked:        2,
		BranchChecked:  3,
		BranchMRLookup: true,
		BranchOpenMRs:  4,
		Findings: []witness.StateCollapseFinding{
			{
				IssueID:  "gt-wdr",
				MRID:     "gt-wisp-shks",
				MRStatus: "open",
				Branch:   "polecat/topaz/gt-wdr+mttf3tgr",
				Target:   "main",
			},
		},
	}

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}

	var parsed PatrolStateCollapseOutput
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	if parsed.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", parsed.Rig, "gastown")
	}
	if parsed.Checked != 2 {
		t.Errorf("Checked = %d, want 2", parsed.Checked)
	}
	if len(parsed.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(parsed.Findings))
	}
	if parsed.Findings[0].IssueID != "gt-wdr" {
		t.Errorf("Findings[0].IssueID = %q, want gt-wdr", parsed.Findings[0].IssueID)
	}
	if parsed.Findings[0].MRID != "gt-wisp-shks" {
		t.Errorf("Findings[0].MRID = %q, want gt-wisp-shks", parsed.Findings[0].MRID)
	}
	// A machine consumer must be able to tell a clean branch scan from one
	// that never resolved the MR queue (gt-akap).
	if !parsed.BranchMRLookup {
		t.Error("BranchMRLookup = false, want true")
	}
	if parsed.BranchOpenMRs != 4 {
		t.Errorf("BranchOpenMRs = %d, want 4", parsed.BranchOpenMRs)
	}

	notRun := PatrolStateCollapseOutput{Rig: "gastown"}
	data, err = json.Marshal(notRun)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}
	if !strings.Contains(string(data), `"branch_mr_lookup_ran":false`) {
		t.Errorf("skipped MR lookup must be explicit in JSON, got %s", data)
	}
}

// TestPatrolStateCollapseJSONMRLookupHonesty is the machine-readable half of
// gt-92ry. The MR-driven check used to read the queue with a plain
// `bd list --label=gt:merge-request`, which returns no wisps — it saw an
// empty queue and emitted a clean-looking result. A consumer now has to be
// able to tell a scan that resolved the queue from one that never did, and
// must not read the latter as a clean bill of health.
func TestPatrolStateCollapseJSONMRLookupHonesty(t *testing.T) {
	clean := PatrolStateCollapseOutput{Rig: "gastown", MRLookupRan: true, BranchMRLookup: true, AllClear: true}
	data, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}
	for _, want := range []string{`"mr_lookup_ran":true`, `"all_clear":true`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("resolved lookup must be explicit in JSON: want %s in %s", want, data)
		}
	}

	unchecked := PatrolStateCollapseOutput{Rig: "gastown"}
	data, err = json.Marshal(unchecked)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}
	for _, want := range []string{`"mr_lookup_ran":false`, `"all_clear":false`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("unresolved lookup must be explicit in JSON: want %s in %s", want, data)
		}
	}
}

func TestPatrolStateCollapseCmdRegistered(t *testing.T) {
	found := false
	for _, c := range patrolCmd.Commands() {
		if c.Name() == "state-collapse" {
			found = true
			break
		}
	}
	if !found {
		t.Error("state-collapse subcommand not registered under patrolCmd")
	}
}
