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
	// The window is the scan's, never a hand-applied local-model shortcut.
	if !strings.Contains(text, "Do NOT apply a\\n   shorter 10-minute local-model window by hand") {
		t.Error("witness formula must forbid applying the 10m local-model window by hand")
	}
	// The policy on a stall is unchanged: escalate, never restart unilaterally.
	if !strings.Contains(text, "rather than restarting") {
		t.Error("witness formula must keep the escalate-not-restart stall policy")
	}
}

// TestWitnessPatrolWarnsOfReportRespawn pins the claude-8w7 step 3 contract:
// gt patrol report may respawn the witness after a quiet report, so the
// formula must say so and tell the agent it need not act on it.
func TestWitnessPatrolWarnsOfReportRespawn(t *testing.T) {
	content, err := formulasFS.ReadFile("formulas/mol-witness-patrol.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		"witness.cycle_session_at_idle_cap",
		"A respawn may follow a quiet report",
		"session kept: <cause>",
		"You do not need to do anything",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("witness formula must mention %q", want)
		}
	}
}
