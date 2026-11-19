package daemon

import (
	"testing"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// followupPolicyCase is one minor finding the policy judges. Class is what
// omlessons.Classify must give it: a negative row has to reach the rule it
// tests, or it would pass for the wrong reason (gt-4qbe1w).
type followupPolicyCase struct {
	name, path, title, claim string
	class, reason            string // reason is "" for a finding that stays filed
}

// TestFollowupSkipPolicy: a comment claim, a typo in a title and a file under
// the workflow directory are skipped, each with the reason the feed logs
// (gt-4qbe1w, Q34).
func TestFollowupSkipPolicy(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, []followupPolicyCase{
		{name: "a comment claim is a doc nit", path: "internal/land/advice.go",
			title: "the comment on adviceFailureText claims a caller shows the sentence",
			claim: "the code does not show it on the error path", class: "comment-claim", reason: "doc-nit"},
		{name: "a typo in the log line is a doc nit", path: "internal/daemon/landing_worker.go",
			title: "typo in the log line", claim: "the line reads 'recieved' where it means received",
			class: omlessons.Unclassified, reason: "doc-nit"},
		{name: "a phrasing nit is a doc nit", path: "internal/daemon/landing_worker.go",
			title: "rephrase the refusal to name the file", claim: "the sentence repeats itself",
			class: omlessons.Unclassified, reason: "doc-nit"},
		{name: "a workflow file is a protected path", path: ".forgejo/workflows/gate.yml",
			title: "the gate workflow runs make test", claim: "gate.yml runs make test where the Makefile target is gate",
			class: omlessons.Unclassified, reason: "protected-path"},
		{name: "an absolute path under the workflow directory is protected", path: "/repo/.forgejo/workflows/gate.yml",
			title: "the gate workflow runs make test", claim: "gate.yml runs make test where the Makefile target is gate",
			class: omlessons.Unclassified, reason: "protected-path"},
	})
}

// TestFollowupSkipPolicyKeepsRealDefects: a defect a bead can fix stays filed,
// including one whose claim only mentions a comment and one whose claim says
// "the code does not" (gt-4qbe1w).
func TestFollowupSkipPolicyKeepsRealDefects(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, []followupPolicyCase{
		{name: "a fail-open gate is filed", path: "internal/daemon/attention.go",
			title: "unreadable main status suppresses escalation", claim: "the read error is taken as green",
			class: "fail-open"},
		{name: "a refusal branch with no test is filed", path: "internal/daemon/landing_worker.go",
			title: "the refusal branch has no test", claim: "the deny path is never driven",
			class: "missing-refusal-test"},
		{name: "a doc that is wrong is filed", path: "internal/daemon/landing_worker.go",
			title: "the doc says the policy is off by default", claim: "the parser reads it as on, so the sentence is no longer true",
			class: "doc-contradicts-code"},
		{name: "a claim that only mentions a comment is filed", path: "internal/daemon/landing_worker.go",
			title: "the retry drops the error", claim: "the comment on failureCount promises a reset the caller never does",
			class: "comment-claim"},
		{name: "a claim that says the code does not is filed", path: "internal/daemon/landing_worker.go",
			title: "the retry never resets its counter", claim: "the code does not reset the counter on a new pass, so the second pass skips a package",
			class: "comment-claim"},
		{name: "a wording word as a fragment is filed", path: "internal/jev/omclass/omclass.go",
			title: "the misspelling check discards the finding's severity", claim: "the check drains severities into a set and keeps one",
			class: "silent-drop"},
		{name: "a path outside the workflow directory is filed", path: "internal/ci/workflows.go",
			title: "the workflow file is read twice", claim: "runWorkflow reads the file in two places",
			class: omlessons.Unclassified},
		{name: "a path that climbs out of the tree is filed", path: "../.forgejo/workflows/gate.yml",
			title: "the gate workflow drops the exit code", claim: "the step runs its command and keeps going",
			class: omlessons.Unclassified},
	})
}

// TestFollowupSkipPolicyIsWired: applyFollowupPolicy sets the hook on a zero
// Lander, and the hook files what the policy keeps (gt-4qbe1w).
func TestFollowupSkipPolicyIsWired(t *testing.T) {
	t.Parallel()
	var l land.Lander
	applyFollowupPolicy(&l)
	if l.FollowupSkip == nil {
		t.Fatal("applyFollowupPolicy left FollowupSkip nil, so the feed files every finding")
	}
	comment := land.FollowupCandidate{Path: "internal/land/advice.go", Title: "the comment claims the caller retries", Claim: "the code does not", Severity: "minor"}
	if got, want := l.FollowupSkip(land.Work{}, comment), followupReasonDocNit; got != want {
		t.Errorf("FollowupSkip(comment claim) = %q, want %q", got, want)
	}
	defect := land.FollowupCandidate{Path: "internal/land/advice.go", Title: "the retry never resets its counter", Claim: "the code does not reset it", Severity: "minor"}
	if got := l.FollowupSkip(land.Work{}, defect); got != "" {
		t.Errorf("FollowupSkip(real defect) = %q, want it filed", got)
	}
}

// runFollowupPolicyCases judges every row with the production policy and fails
// a row that does not reach the class it names.
func runFollowupPolicyCases(t *testing.T, cases []followupPolicyCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotClass := omlessons.Classify(land.ReviewFindingText{Title: tc.title, Claim: tc.claim})
			if gotClass != tc.class {
				t.Fatalf("omlessons.Classify(%q, %q) = %q, want %q: the row would judge another rule", tc.title, tc.claim, gotClass, tc.class)
			}
			c := land.FollowupCandidate{Path: tc.path, Title: tc.title, Claim: tc.claim, Severity: "minor"}
			if got := followupSkipReason(land.Work{}, c); got != tc.reason {
				t.Errorf("followupSkipReason(%q on %s) = %q, want %q", tc.title, tc.path, got, tc.reason)
			}
		})
	}
}
