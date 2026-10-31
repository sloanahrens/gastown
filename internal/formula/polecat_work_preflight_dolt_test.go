package formula

import (
	"strings"
	"testing"
)

// A sandboxed polecat once read a refused shell connection to port 3307 as a
// Dolt outage, escalated HIGH and parked (gt-0chpv). The preflight step must
// name bd as the probe, retry it, and say a shell connection to 3307 proves
// nothing.
func TestPolecatWorkPreflightDoltUsesBdProbeNotTheNetwork(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"bd list --limit 1",
		"up to 3 times",
		"denied by the sandbox policy and is not evidence of an outage",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("mol-polecat-work preflight lacks %q", want)
		}
	}
	if strings.Contains(text, "run `gt dolt status` to check Dolt health") {
		t.Error("preflight still sends the polecat to gt dolt status as its Dolt check")
	}
}
