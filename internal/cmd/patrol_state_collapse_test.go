package cmd

import (
	"encoding/json"
	"testing"

	"github.com/steveyegge/gastown/internal/witness"
)

func TestPatrolStateCollapseOutputJSON(t *testing.T) {
	output := PatrolStateCollapseOutput{
		Rig:     "gastown",
		Checked: 2,
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
