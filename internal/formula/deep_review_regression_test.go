package formula

import (
	"strings"
	"testing"
)

// TestDeepReviewBeadsCarryARegressionTest guards gt-4du83: a green test sweep is
// what lets the deep review run, so a bead it files for a code finding must
// ship the test that proves the fix. Each code finding's `## Acceptance` names
// the test that fails before the fix and passes after, one item per finding of
// a batch bead, a finding with no describable test still files with its reason
// and is marked no-test in the digest, and a doc-drift bead keeps its docs-only
// gate and needs no test.
func TestDeepReviewBeadsCarryARegressionTest(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	// The step is wrapped by hand, so fold the wrapping before matching.
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []struct {
		finding string
		text    string
	}{
		{
			"a code finding's acceptance item names the test that fails before the fix",
			"naming the test that fails before the fix and passes after (file, test name, assertion), which `## Gate` runs",
		},
		{
			"a batch bead takes one such acceptance item per finding",
			"`## Acceptance` with 1-6 checkbox items: one item per code finding, batch beads included,",
		},
		{
			"a finding with no describable test files with its reason and is marked no-test",
			"A finding with no such test (a timing or platform risk) files anyway, `## Goal` saying why in one sentence and its digest `filed` entry reading `no-test`",
		},
		{
			"a doc-drift bead is exempt and keeps its docs-only gate",
			"A `doc-drift` bead needs none, keeping its docs-only gate",
		},
	} {
		if !strings.Contains(text, want.text) {
			t.Errorf("deep-review-run lacks the filing rule %q", want.finding)
		}
	}
}
