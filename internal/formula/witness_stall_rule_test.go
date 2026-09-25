package formula

import (
	"strings"
	"testing"
)

// TestWitnessPatrolReadsPersistedStallVerdict pins the claude-8w7 step 1
// contract: gt patrol scan persists sample 1 of the gt-xb27 stall rule, so the
// witness formula must tell the agent to read the scan's verdict rather than
// hold sample 1 in its own context, where a respawn would lose it.
func TestWitnessPatrolReadsPersistedStallVerdict(t *testing.T) {
	content, err := formulasFS.ReadFile("formulas/mol-witness-patrol.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)

	for _, want := range []string{"stall_samples.json", "`stalled`", "stall_reason", "stall_sample_age_seconds"} {
		if !strings.Contains(text, want) {
			t.Errorf("witness formula must mention %s", want)
		}
	}
	// The old procedure asked the agent to remember sample 1 itself.
	for _, gone := range []string{"sample 1: note", "sample 2: compare the SAME polecat"} {
		if strings.Contains(text, gone) {
			t.Errorf("witness formula still asks the agent to hold sample 1 in context (%q)", gone)
		}
	}
	// The policy on a stall is unchanged: escalate, never restart unilaterally.
	if !strings.Contains(text, "rather than restarting") {
		t.Error("witness formula must keep the escalate-not-restart stall policy")
	}
}
