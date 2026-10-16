package formula

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
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

// deepReviewStep returns the folded text of one numbered step of the review
// workflow, from its heading to the next step's. Scoping a rule to the step
// that states it keeps a phrase from somewhere else in the formula from
// answering for it.
func deepReviewStep(t *testing.T, folded, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(folded, heading)
	if !ok {
		t.Fatalf("deep-review-run lacks a step opening %q", heading)
	}
	step, _, ok := strings.Cut(rest, "**9. ")
	if !ok {
		t.Fatalf("the step opening %q does not run to step 9", heading)
	}
	return step
}

// TestDeepReviewFailsALandingOnlyOnItsOwnLines guards gt-86uzs: step 8 records
// a FAIL only for a CONFIRMED finding on a line the landing's own diff added or
// changed, PASSes one in a file the landing merely touched, and has the FAIL
// name the finding beads covering it. The rule it replaced failed a landing for
// any confirmed finding citing one of its paths, which failed 53 landings on
// tap_guard*.go — many of them the fixes for those very findings.
func TestDeepReviewFailsALandingOnlyOnItsOwnLines(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	folded := strings.Join(strings.Fields(string(raw)), " ")
	step := deepReviewStep(t, folded, "**8. Record the reviews of the risk-path items")

	for _, want := range []struct {
		rule string
		text string
	}{
		{
			"the FAIL carries the finding beads it broke, one --finding each",
			"`FAIL --finding <bead>`",
		},
		{
			"the FAIL is for a finding on a line the landing's own diff added or changed",
			"when a CONFIRMED finding sits on a line its diff added or changed",
		},
		{
			"any other finding, a file merely touched included, is a PASS",
			"or changed; else PASS.",
		},
		{
			"the open item names the findings and clears on a later PASS",
			"A FAIL stays open, naming findings, until a later PASS",
		},
	} {
		if !strings.Contains(step, want.text) {
			t.Errorf("step 8 lacks the review rule %q", want.rule)
		}
	}
	// The rule must rest on the landing's own diff, not on a finding's cited
	// paths: the wording that failed a landing for code it never wrote is what
	// this bead removed.
	for _, gone := range []string{"cites its paths", "cites a path in"} {
		if strings.Contains(step, gone) {
			t.Errorf("step 8 still triggers on the finding's paths alone: %q", gone)
		}
	}
}

// TestDeepReviewRunsOnAnyRig guards gt-23w8z: a rig other than gastown is
// reviewed by naming its landings file and the gastown checkout's script, so
// the run formula hardcodes neither, and a sling that sets neither var renders
// the gastown run it always did.
func TestDeepReviewRunsOnAnyRig(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	text := string(raw)
	for _, bad := range []string{"bash scripts/deep-review.sh", "landings/gastown.jsonl", "bash {{review_script}}"} {
		if strings.Contains(text, bad) {
			t.Errorf("deep-review-run hardcodes the gastown run (%q)", bad)
		}
	}
	for _, want := range []string{
		"landings/{{landings_rig}}.jsonl",
		"cat {{review_script}} > /tmp/deep-review-{{issue}}/deep-review.sh",
		"bash /tmp/deep-review-{{issue}}/deep-review.sh plan",
		"`internal/land/riskpaths.txt`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deep-review-run lacks %q", want)
		}
	}

	mol, err := GetEmbeddedFormulaContent("mol-deep-review")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	var f struct {
		Vars map[string]struct {
			Default string `toml:"default"`
		} `toml:"vars"`
	}
	if _, err := toml.Decode(string(mol), &f); err != nil {
		t.Fatalf("decode mol-deep-review: %v", err)
	}
	for name, want := range map[string]string{
		"landings_rig":  "gastown",
		"review_script": "scripts/deep-review.sh",
	} {
		if got := f.Vars[name].Default; got != want {
			t.Errorf("mol-deep-review var %s default = %q, want %q", name, got, want)
		}
	}
}
