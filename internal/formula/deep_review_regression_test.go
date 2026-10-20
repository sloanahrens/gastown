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

// deepReviewRange returns the folded text between two step headings, so a test
// can scope a rule to the step that states it without deepReviewStep's fixed
// end at step 9.
func deepReviewRange(t *testing.T, folded, from, to string) string {
	t.Helper()
	_, rest, ok := strings.Cut(folded, from)
	if !ok {
		t.Fatalf("deep-review-run lacks a step opening %q", from)
	}
	step, _, ok := strings.Cut(rest, to)
	if !ok {
		t.Fatalf("the step opening %q does not run to %q", from, to)
	}
	return step
}

// TestDeepReviewReportsDuplicateDefinitions guards gt-8ezud: one question ("is
// this worktree's work landed?") was computed three ways in three packages and
// each per-caller fix moved the others, so a slice reviewer also asks whether
// another package decides the same thing differently, and the verifier refutes
// the finding only when the definitions agree or one already calls the other.
// The kind rides the existing reviewers and verifiers; it adds neither.
func TestDeepReviewReportsDuplicateDefinitions(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	folded := strings.Join(strings.Fields(string(raw)), " ")
	step4 := deepReviewRange(t, folded, "**4. Review (at most", "**5. Verify")
	step5 := deepReviewRange(t, folded, "**5. Verify (one subagent per three findings", "**6. File the CONFIRMED findings")

	for _, want := range []struct{ rule, text string }{
		{
			"the slice reviewer checks each predicate or decision its files compute",
			"For each predicate or decision a slice file computes (a verdict, a status, an eligibility, a `landed`, `pushed` or `idle` judgment)",
		},
		{
			"it greps the base for other code answering the same question",
			"grep the tree at origin/{{base_branch}} for other code answering the same question",
		},
		{
			"a different definition is a kind, a P2 and a fingerprint shape",
			"finding of kind `duplicate-definition`, severity P2, fingerprint `<file>:<symbol>:duplicate-definition`",
		},
		{
			"the claim names both places and how the definitions differ",
			"claim naming both places and how they differ",
		},
		{
			"the failure scenario is an input on which they disagree",
			"failure scenario an input on which they disagree",
		},
	} {
		if !strings.Contains(step4, want.text) {
			t.Errorf("step 4 lacks the duplicate-definition rule %q", want.rule)
		}
	}
	if !strings.Contains(step4, "{{max_reviewers}} subagents") {
		t.Error("step 4's reviewer count changed; the new kind must ride the existing reviewers")
	}

	for _, want := range []struct{ rule, text string }{
		{
			"the verifier refutes by showing the definitions agree on every input",
			"Refute a `duplicate-definition` by showing the definitions agree on every input",
		},
		{
			"or by showing one already calls the other",
			"or one already calls the other",
		},
	} {
		if !strings.Contains(step5, want.text) {
			t.Errorf("step 5 lacks the duplicate-definition refutation %q", want.rule)
		}
	}
	if !strings.Contains(step5, "at most {{max_verifiers}}") {
		t.Error("step 5's verifier count changed; the new kind must ride the existing verifiers")
	}
}

// TestDeepReviewDiffsEachLandingFromItsBaseCommit guards gt-8ezud: a
// Forgejo-primary landing is a one-parent squash, so the formula must not
// describe land commits as merges whose first parent is main. The diff base
// stays <landed_commit>^1 either way.
func TestDeepReviewDiffsEachLandingFromItsBaseCommit(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("deep-review-run")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent: %v", err)
	}
	folded := strings.Join(strings.Fields(string(raw)), " ")
	step := deepReviewRange(t, folded, "**4. Review (at most", "**5. Verify")
	for _, stale := range []string{"land commits are merges", "the first parent is main"} {
		if strings.Contains(step, stale) {
			t.Errorf("step 4 still describes a landing as a merge (%q)", stale)
		}
	}
	for _, want := range []string{
		"git diff <landed_commit>^1 <landed_commit>",
		"a Forgejo-primary landing is a one-parent squash",
		"`<landed_commit>^1` is its base either way",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("step 4 lacks %q", want)
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
