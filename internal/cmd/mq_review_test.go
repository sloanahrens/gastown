package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// TestPrintMQReviewResult_MarksAReusedVerdict pins the only thing that tells a
// caller apart "om approved this" from "om approved this the first time it was
// asked": a reused verdict exits on the same code as a fresh one, so the line
// printed is the whole difference (gt-bveg).
func TestPrintMQReviewResult_MarksAReusedVerdict(t *testing.T) {
	wasJSON := mqReviewJSON
	mqReviewJSON = false
	t.Cleanup(func() { mqReviewJSON = wasJSON })

	for _, tc := range []struct {
		name       string
		result     editorial.ReviewResult
		wantMarker bool
		wantText   string
	}{
		{
			name:       "fresh approve",
			result:     editorial.ReviewResult{Exit: 0, Note: &editorial.Note{Score: 0.8, Verdict: "approve"}},
			wantText:   "approve (score 0.80)",
			wantMarker: false,
		},
		{
			name:       "reused approve",
			result:     editorial.ReviewResult{Exit: 0, Reused: true, Note: &editorial.Note{Score: 0.8, Verdict: "approve"}},
			wantText:   "approve (score 0.80)",
			wantMarker: true,
		},
		{
			name:       "fresh rejection",
			result:     editorial.ReviewResult{Exit: 1, Note: &editorial.Note{Score: 0.5, Verdict: "request_changes", FindingsCount: 2}},
			wantText:   "request_changes (score 0.50, 2 finding(s))",
			wantMarker: false,
		},
		{
			name:       "reused rejection",
			result:     editorial.ReviewResult{Exit: 1, Reused: true, Note: &editorial.Note{Score: 0.5, Verdict: "request_changes", FindingsCount: 2}},
			wantText:   "request_changes (score 0.50, 2 finding(s))",
			wantMarker: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { printMQReviewResult(tc.result) })

			if !strings.Contains(out, tc.wantText) {
				t.Errorf("output %q does not carry %q", out, tc.wantText)
			}
			if gotMarker := strings.Contains(out, "--reroll"); gotMarker != tc.wantMarker {
				t.Errorf("output %q marks a re-roll = %v, want %v", out, gotMarker, tc.wantMarker)
			}
		})
	}
}
