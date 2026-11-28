package specdispatch

import (
	"strings"
	"testing"
)

// evidenceBoxFix is the sentence a refusal ends with.
const evidenceBoxFix = "delete the evidence clause and keep the behavior and its test command"

// TestEvidenceBox refuses every phrasing that asks for pasted or recorded
// evidence of a run, naming the box, and accepts a command-only or red-change
// box (gt-7qtrif).
func TestEvidenceBox(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		box     string
		refused bool
	}{
		{"PASTE the output", true},
		{"the red line is in the commit body", true},
		{"the failing line is in the commit body", true},
		{"the commit body shows the output", true},
		{"the commit message holds the error tail", true},
		{"the bead note states the failure", true},
		{"the bead comment records the result", true},
		{"go test ./internal/specdispatch -run TestEvidenceBox -count=1 passes", false},
		{"removing the guard turns TestEvidenceBoxRefusesRedLine red", false},
		{"the commit message states which change turns TestEvidenceBoxRefusesPaste red", false},
	} {
		t.Run(tc.box, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance("- [ ] " + tc.box)
			if (v.Route == RouteRefuse) != tc.refused {
				t.Fatalf("%q: Lint = %+v, refused = %v", tc.box, v, v.Route == RouteRefuse)
			}
			if tc.refused && (!strings.Contains(v.Reason, tc.box) || !strings.Contains(v.Reason, evidenceBoxFix)) {
				t.Errorf("%q: reason %q does not name the box and the fix", tc.box, v.Reason)
			}
		})
	}
}
