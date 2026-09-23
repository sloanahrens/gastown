package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/refinery/overlap"
)

func TestRunMQVerify_RequiresRehearsedFlag(t *testing.T) {
	wasRehearsed := mqVerifyRehearsed
	mqVerifyRehearsed = ""
	t.Cleanup(func() { mqVerifyRehearsed = wasRehearsed })

	err := runMQVerify(mqVerifyCmd, []string{"gt-mr-doesnotmatter"})
	if err == nil {
		t.Fatal("expected an error when --rehearsed is not set")
	}
	if !strings.Contains(err.Error(), "--rehearsed is required") {
		t.Fatalf("error = %q, want it to name the missing --rehearsed flag", err.Error())
	}
}

// TestPrintMQVerifyResult_TextMode pins the human-readable summary a
// refinery agent reads off gt mq verify's non-JSON output: suite outcome,
// review verdict (when one ran), and the decision that joins them.
func TestPrintMQVerifyResult_TextMode(t *testing.T) {
	wasJSON := mqVerifyJSON
	mqVerifyJSON = false
	t.Cleanup(func() { mqVerifyJSON = wasJSON })

	for _, tc := range []struct {
		name   string
		result mqVerifyResult
		want   []string
	}{
		{
			name: "approve and red suite discards the review",
			result: mqVerifyResult{
				Suite:    overlap.SuiteResult{Ran: true, Success: false, FailedStep: "test"},
				Review:   &editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 0.9}},
				Decision: overlap.ActionRejectSuite,
				Discard:  true,
			},
			want: []string{"success=false", "failed_step=test", "exit=0", "decision: reject_suite", "review_discarded=true"},
		},
		{
			name: "reject and green suite acts on the review",
			result: mqVerifyResult{
				Suite:    overlap.SuiteResult{Ran: true, Success: true},
				Review:   &editorial.ReviewResult{Exit: 1, Note: &editorial.Note{Score: 0.4, FindingsCount: 3}},
				Decision: overlap.ActionRejectReview,
				Discard:  false,
			},
			want: []string{"success=true", "exit=1", "score=0.40", "findings=3", "decision: reject_review", "review_discarded=false"},
		},
		{
			name: "both green merges",
			result: mqVerifyResult{
				Suite:    overlap.SuiteResult{Ran: true, Success: true},
				Review:   &editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 0.95}},
				Decision: overlap.ActionMerge,
			},
			want: []string{"success=true", "exit=0", "decision: merge"},
		},
		{
			name: "no review configured",
			result: mqVerifyResult{
				Suite:    overlap.SuiteResult{Ran: true, Success: true},
				Review:   nil,
				Decision: overlap.ActionMerge,
			},
			want: []string{"success=true", "decision: merge"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { printMQVerifyResult(tc.result) })
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output %q does not contain %q", out, want)
				}
			}
		})
	}
}

func TestPrintMQVerifyResult_JSONMode(t *testing.T) {
	wasJSON := mqVerifyJSON
	mqVerifyJSON = true
	t.Cleanup(func() { mqVerifyJSON = wasJSON })

	result := mqVerifyResult{
		LaunchID: "verify-1",
		HeadSHA:  "deadbeef",
		Suite:    overlap.SuiteResult{Ran: true, Success: true},
		Review:   &editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 1}},
		Decision: overlap.ActionMerge,
	}
	out := captureStdout(t, func() { printMQVerifyResult(result) })

	var decoded mqVerifyResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}
	if decoded.LaunchID != "verify-1" || decoded.HeadSHA != "deadbeef" || decoded.Decision != overlap.ActionMerge {
		t.Fatalf("decoded = %+v, want it to round-trip the input", decoded)
	}
}
