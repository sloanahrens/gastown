package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

// followupPolicyCase is one minor finding the policy judges. Class is what
// omlessons.Classify must give it: a negative row has to reach the rule it
// tests, or it would pass for the wrong reason (gt-4qbe1w).
type followupPolicyCase struct {
	name, path, title, claim, bead string
	class, reason                  string // reason is "" for a finding that stays filed
	// heldOutClass, when set, is the class omlessons.HeldOut is probed with for
	// the row's bead: it must answer heldOut at the policy's clock.
	heldOutClass string
	heldOut      bool
}

// TestFollowupSkipPolicy: a comment claim, a typo in a title and a file under
// the workflow directory are skipped, each with the reason the feed logs
// (gt-4qbe1w, Q34).
func TestFollowupSkipPolicy(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, followupBeforeHoldOut, []followupPolicyCase{
		{name: "a comment that only narrates is a doc nit", path: "internal/land/advice.go",
			title: "Comment narrates the old retry loop",
			claim: "the comment describes a loop the caller no longer runs", class: "comment-claim", reason: "doc-nit"},
		{name: "a typo in the log line is a doc nit", path: "internal/daemon/landing_worker.go",
			title: "typo in the log line", claim: "the line reads 'recieved' where it means received",
			class: omlessons.Unclassified, reason: "doc-nit"},
		{name: "a phrasing nit on a message is a doc nit", path: "internal/daemon/landing_worker.go",
			title: "rephrase the message", claim: "the sentence repeats itself",
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
	runFollowupPolicyCases(t, followupBeforeHoldOut, []followupPolicyCase{
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

// TestFollowupSkipPolicyHoldOut: inside the window, a doc-contradicts-code
// finding on a held-out bead is skipped as hold-out, the same finding on a bead
// the hash does not hold out is filed, and a finding an earlier rule skips keeps
// that reason (gt-um10tq, Q37).
func TestFollowupSkipPolicyHoldOut(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, followupHoldOutInside, []followupPolicyCase{
		{name: "a held-out bead's doc finding is withheld", bead: "gt-b0000", path: "internal/daemon/landing_worker.go",
			title: "the doc says the policy is off by default", claim: "the parser reads it as on, so the sentence is no longer true",
			class: "doc-contradicts-code", reason: "hold-out", heldOutClass: "doc-contradicts-code", heldOut: true},
		{name: "the same finding on a bead outside the hold-out is filed", bead: "gt-b0007", path: "internal/daemon/landing_worker.go",
			title: "the doc says the policy is off by default", claim: "the parser reads it as on, so the sentence is no longer true",
			class: "doc-contradicts-code", reason: "", heldOutClass: "doc-contradicts-code", heldOut: false},
		{name: "a protected path keeps its reason on a held-out bead", bead: "gt-b0013", path: ".forgejo/workflows/gate.yml",
			title: "the doc says the gate workflow runs make test", claim: "the workflow runs make test where the Makefile target is gate",
			class: "doc-contradicts-code", reason: "protected-path", heldOutClass: "doc-contradicts-code", heldOut: true},
	})
}

// TestFollowupSkipPolicyHoldOutScope: the hold-out withholds two classes only
// and only inside its window, so a fail-open finding on a held-out bead and a
// doc finding outside the window are both filed (gt-um10tq, Q37).
func TestFollowupSkipPolicyHoldOutScope(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, followupHoldOutInside, []followupPolicyCase{
		{name: "a fail-open finding on a held-out bead is filed", bead: "gt-b0000", path: "internal/daemon/attention.go",
			title: "unreadable main status suppresses escalation", claim: "the read error is taken as green",
			class: "fail-open", reason: "", heldOutClass: "doc-contradicts-code", heldOut: true},
	})
	runFollowupPolicyCases(t, followupHoldOutAfter, []followupPolicyCase{
		{name: "a doc finding after the window is filed", bead: "gt-b0018", path: "internal/daemon/landing_worker.go",
			title: "the doc says the policy is off by default", claim: "the parser reads it as on, so the sentence is no longer true",
			class: "doc-contradicts-code", reason: "", heldOutClass: "doc-contradicts-code", heldOut: false},
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
	comment := land.FollowupCandidate{Path: "internal/land/advice.go", Title: "Comment restates the caller's retry loop", Claim: "the loop is gone", Severity: "minor"}
	if got, want := l.FollowupSkip(land.Work{}, comment), followupReasonDocNit; got != want {
		t.Errorf("FollowupSkip(comment form nit) = %q, want %q", got, want)
	}
	defect := land.FollowupCandidate{Path: "internal/land/advice.go", Title: "the retry never resets its counter", Claim: "the code does not reset it", Severity: "minor"}
	if got := l.FollowupSkip(land.Work{}, defect); got != "" {
		t.Errorf("FollowupSkip(real defect) = %q, want it filed", got)
	}
}

// TestFollowupPolicyClockIsTimeNow: the policy the daemon builds names the wall
// clock, so the hold-out window is judged by the clock the rest of the daemon
// reads (gt-ahffi6). The two function values are compared, never called: a test
// that read a clock to decide its outcome could flip on its own.
func TestFollowupPolicyClockIsTimeNow(t *testing.T) {
	t.Parallel()
	if got, want := reflect.ValueOf(newFollowupPolicy().now).Pointer(), reflect.ValueOf(time.Now).Pointer(); got != want {
		t.Errorf("newFollowupPolicy().now = %#x, want time.Now = %#x: the hold-out would be judged by a clock nothing names", got, want)
	}
}

// TestFollowupPolicyNilClockFallsBackToTimeNow: a policy built by hand with no
// clock falls back to the wall clock rather than panicking (gt-ahffi6). Only
// the function value is compared, so the outcome never depends on the wall
// clock or on which beads are held out.
func TestFollowupPolicyNilClockFallsBackToTimeNow(t *testing.T) {
	t.Parallel()
	if got, want := reflect.ValueOf(followupPolicy{}.clock()).Pointer(), reflect.ValueOf(time.Now).Pointer(); got != want {
		t.Errorf("followupPolicy{}.clock() = %#x, want time.Now = %#x", got, want)
	}
}

// TestFollowupSkipPolicyKeepsMismatchFindings: a title that states a mismatch
// or an unverified assumption is filed, even when it names a comment or a
// wording word (gt-yzdg9r). The rows are real titles the old policy skipped.
func TestFollowupSkipPolicyKeepsMismatchFindings(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, followupBeforeHoldOut, []followupPolicyCase{
		{name: "a comment claim that is wrong is filed", path: "internal/beads/beads.go",
			title: "Doc comment claims whole-word matching the code does not implement",
			claim: "the regex matches a fragment of the word the comment promises", class: "comment-claim"},
		{name: "a comment that says something the code does not do is filed", path: "deploy/k8s/daemon/daemon.go",
			title: "Comment says the grant gives name-only access; `get` returns Secret data",
			claim: "the role grants get on Secrets; the comment says names only", class: "comment-claim"},
		{name: "a grant broader than its comment is filed", path: "deploy/polecat/policy.yaml",
			title: "Grant is broader than the comment implies and may be insufficient alone",
			claim: "the binding also allows delete, which the sentence does not mention", class: "comment-claim"},
		{name: "a doc comment that overstates is filed", path: "internal/adminplane/registry/migrations.go",
			title: "Doc comments overstate which files are rejected",
			claim: "the walk accepts generated files the text says it rejects", class: "comment-claim"},
		{name: "a comment that overstates atomicity is filed", path: "internal/bdgateway/codec.go",
			title: "WriteFrame comment overstates write atomicity",
			claim: "the write is not atomic across the header and the body", class: "comment-claim"},
		{name: "an assumption asserted by comment is filed", path: "internal/beads/beads_escalation.go",
			title: "bd's `--limit=0` semantics are asserted by comment, not verified",
			claim: "no test or doc pins what limit 0 does", class: "comment-claim"},
		{name: "wiring the comment names but no test drives is filed", path: "internal/cmd/bead.go",
			title: "Cobra wiring for file-mode comment is not exercised (a `bd comment` command, not a code comment)",
			claim: "only the cobra path is registered; the file-mode comment is never run", class: "comment-claim"},
		{name: "a comment-chunk gap is filed", path: "docs/design/jev-duplicate-findings.md",
			title: "Retrieval filter by creation time has a comment-chunk gap",
			claim: "the filter drops a comment created before the chunk window", class: "comment-claim"},
		{name: "a form word that also states a mismatch is filed", path: "internal/daemon/landing_worker.go",
			title: "Comment narrates history and overstates the guard",
			claim: "the comment describes a past fix and calls the check a guard", class: "comment-claim"},
		{name: "a path spelling nit is filed", path: "internal/tiertrigger/trigger.go",
			title: "clean does not normalize interior or `..` spellings",
			claim: "the clean helper strips a leading segment but keeps an interior `..`", class: omlessons.Unclassified},
		{name: "a spelling word as a fragment of another claim is filed", path: "internal/manifestdata/podpaths_test.go",
			title: "Guard regex misses literal spellings followed by other punctuation",
			claim: "the guard's character class omits a literal followed by a comma or a period", class: omlessons.Unclassified},
		{name: "a parser grammar is filed", path: "tracker/cmd/trackerd/pool.go",
			title: "Hand-rolled keyword tokenizer may diverge from pgconn's grammar",
			claim: "the tokenizer splits on punctuation where the pgconn parser does not", class: "secret-in-output"},
		{name: "a grammar word about a command set is filed", path: "internal/cmd/tap_guard_sandbox_secrets.go",
			title: "No-loosening rests on a finite command set, not on the parser's grammar",
			claim: "the no-loosening check tests the commands it lists, not the parser that reads them", class: omlessons.Unclassified},
	})
}

// TestFollowupSkipPolicySkipsFormNits: a comment that only narrates, restates,
// reflows or duplicates, and a wording fix that names its text, stays skipped
// as doc-nit (gt-yzdg9r). Each row would be filed if its form or text subject
// were not recognized.
func TestFollowupSkipPolicySkipsFormNits(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, followupBeforeHoldOut, []followupPolicyCase{
		{name: "duplicated comments are a doc nit", path: "internal/fakeapi/root.go",
			title: "Redundant doc comments on the fake API root",
			claim: "the fake root repeats the package doc three times", class: "comment-claim", reason: "doc-nit"},
		{name: "a reflowed line is a doc nit", path: "internal/fakeapi/root.go",
			title: "Package doc comment line reflowed past the wrap width",
			claim: "the line runs to 90 columns where the file wraps at 80", class: "comment-claim", reason: "doc-nit"},
		{name: "a dated narrative comment is a doc nit", path: "internal/land/advice.go",
			title: "Dated, narrative comment on cloneWith",
			claim: "the comment describes a 2021 rewrite", class: "comment-claim", reason: "doc-nit"},
		{name: "a narrative history is a doc nit", path: "internal/land/advice.go",
			title: "Comment carries narrative history",
			claim: "the comment narrates three past fixes", class: "comment-claim", reason: "doc-nit"},
		{name: "a header citing its bead is a doc nit", path: "internal/daemon/attention.go",
			title: "Header comment narrates the bead and repeats the docs",
			claim: "the header repeats gt-4qbe1w's story", class: "comment-claim", reason: "doc-nit"},
		{name: "a comment that cites a bead is a doc nit", path: "internal/daemon/attention.go",
			title: "Comment cites a bead as its source",
			claim: "the rule number stands in for the reason", class: "comment-claim", reason: "doc-nit"},
		{name: "a comment carrying a rule number is a doc nit", path: "internal/daemon/attention.go",
			title: "Comment carries R11 only",
			claim: "the rule number stands in for the reason", class: "comment-claim", reason: "doc-nit"},
		{name: "a typo in a doc is a doc nit", path: "docs/reference.md",
			title: "Typo: 'noul' used as a word (docs/reference.md)",
			claim: "the word reads 'noul' where the source says 'null'", class: omlessons.Unclassified, reason: "doc-nit"},
		{name: "awkward log wording is a doc nit", path: "internal/forgejoprobe/tail.go",
			title: "Awkward log wording (internal/forgejoprobe/tail.go)",
			claim: "the sentence reads 'probe did not not return'", class: omlessons.Unclassified, reason: "doc-nit"},
		{name: "a grammar fix in a message is a doc nit", path: "internal/doctor/om_token_check.go",
			title: "Message grammar for one day left (internal/doctor/om_token_check.go)",
			claim: "the line reads '1 days left'", class: "secret-in-output", reason: "doc-nit"},
	})
}

// TestFollowupSkipPolicyFilesUnsureTitles: a title the rule cannot read as a
// form or wording nit is filed, so a word the lists miss costs a bead rather
// than a dropped defect (gt-yzdg9r).
func TestFollowupSkipPolicyFilesUnsureTitles(t *testing.T) {
	t.Parallel()
	runFollowupPolicyCases(t, followupBeforeHoldOut, []followupPolicyCase{
		{name: "a comment nit with no form word is filed", path: "internal/land/advice.go",
			title: "Comment on adviceFailureText is out of date",
			claim: "the sentence predates the rename", class: "comment-claim"},
		{name: "a spelling of a path with no text subject is filed", path: "internal/tiertrigger/trigger.go",
			title: "Spelling of the `..` segment",
			claim: "the check reads the two dots as one segment", class: omlessons.Unclassified},
		{name: "a spelling word that is not the whole ask is filed", path: "internal/manifestdata/podpaths_test.go",
			title: "the guard rejects a spelling the comment calls valid",
			claim: "the character class and the comment disagree", class: "comment-claim"},
		{name: "a prose nit with no text subject is filed", path: "internal/daemon/landing_worker.go",
			title: "rephrase the refusal to name the file",
			claim: "the sentence repeats itself", class: omlessons.Unclassified},
		{name: "a grammar word about a parser is filed", path: "internal/tiertrigger/trigger.go",
			title: "Grammar of the log filter rejects valid lines",
			claim: "the filter splits a quoted value where the parser does not", class: omlessons.Unclassified},
		{name: "a typo word before a stated result is filed", path: "internal/daemon/landing_worker.go",
			title: "Typo in the message drops the reason",
			claim: "the error text is truncated at the first newline", class: omlessons.Unclassified},
		{name: "a typo label with no text named is filed", path: "internal/daemon/landing_worker.go",
			title: "Typo: the retry drops the error",
			claim: "the identifier is misspelled and the retry never runs", class: omlessons.Unclassified},
	})
}

// The hold-out clocks: inside the window, and after it ends.
var (
	followupHoldOutInside = omlessons.HoldOutStart.Add(24 * time.Hour)
	followupBeforeHoldOut = omlessons.HoldOutStart.Add(-24 * time.Hour)
	followupHoldOutAfter  = omlessons.HoldOutStart.AddDate(0, 0, omlessons.HoldOutDays)
)

// runFollowupPolicyCases judges every row with the production policy at a fixed
// clock, and fails a row whose finding or bead does not reach the rule it names.
func runFollowupPolicyCases(t *testing.T, at time.Time, cases []followupPolicyCase) {
	t.Helper()
	p := followupPolicy{now: func() time.Time { return at }}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotClass := omlessons.Classify(land.ReviewFindingText{Title: tc.title, Claim: tc.claim})
			if gotClass != tc.class {
				t.Fatalf("omlessons.Classify(%q, %q) = %q, want %q: the row would judge another rule", tc.title, tc.claim, gotClass, tc.class)
			}
			if tc.heldOutClass != "" {
				if got := omlessons.HeldOut(tc.bead, tc.heldOutClass, at); got != tc.heldOut {
					t.Fatalf("omlessons.HeldOut(%q, %q, %s) = %v, want %v: the row would judge another rule",
						tc.bead, tc.heldOutClass, at.Format(time.RFC3339), got, tc.heldOut)
				}
			}
			c := land.FollowupCandidate{Path: tc.path, Title: tc.title, Claim: tc.claim, Severity: "minor"}
			if got := p.reason(land.Work{BeadID: tc.bead}, c); got != tc.reason {
				t.Errorf("followupPolicy.reason(%q on %s, bead %q) = %q, want %q", tc.title, tc.path, tc.bead, got, tc.reason)
			}
		})
	}
}
