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

// TestDeepReviewCollectsOMQueuedFindings guards gt-6m461: om's critical and
// major findings on an approved landing must reach deep-review's verifier
// instead of being filed straight from the landing, so the run reads the
// queued: lines the landing worker wrote onto the work beads (scripts/
// deep-review.sh om-queued) into the ledger the plan and step 3 read, before
// either of them reads it, and says where those findings came from.
func TestDeepReviewCollectsOMQueuedFindings(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	const collect = "bash scripts/deep-review.sh om-queued --repo . >> /tmp/deep-review-{{issue}}/prev-notes.txt"
	at := strings.Index(text, collect)
	if at < 0 {
		t.Fatalf("deep-review-run does not append om-queued to prev-notes.txt (%q)", collect)
	}
	for _, later := range []struct{ what, marker string }{
		{"the plan that reads the ledger", "bash scripts/deep-review.sh plan"},
		{"step 3, which reads the queued findings", "bash scripts/deep-review.sh queued"},
	} {
		i := strings.Index(text, later.marker)
		if i < 0 {
			t.Fatalf("deep-review-run lacks %s (%q)", later.what, later.marker)
		}
		if i < at {
			t.Errorf("deep-review-run runs %s before it collects the om-queued findings", later.what)
		}
	}
	// The append target is the literal path: the polecat-paths guard refuses a
	// redirect built from $D.
	for _, bad := range []string{">> $D/", ">> ../"} {
		if strings.Contains(text, bad) {
			t.Errorf("deep-review-run builds the append target from a variable (%q)", bad)
		}
	}
	// One sentence carries the provenance and the route: step 3 names them as
	// om's findings on approved landings and sends them to the verifier.
	flat := strings.Join(strings.Fields(text), " ")
	if !strings.Contains(flat, "plus the `om-queued` lines from om's reviews of approved landings. Step 5 refutes each one like any other finding") {
		t.Error("deep-review-run does not say the om-queued findings go through the refute step")
	}
}
