package metrics

import (
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestReviewsLoadersAgreeWithTheQueueLinePredicate: the review log has one
// decision on what a queue line is, land.IsQueueLine, and the two loaders that
// read the log take it rather than carrying a copy that could drift. The rows
// below are the cases a copy has drifted on before — a null payload, an empty
// one, a line with no field at all — so a loader that classifies any of them
// its own way fails here (gt-3arxj).
func TestReviewsLoadersAgreeWithTheQueueLinePredicate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		line  string
		queue bool
	}{
		{"a verdict", `{"bead":"gt-1","rig":"gastown","attempt":1,"score":0.9,"verdict":"approve","at":"2026-10-20T12:00:00Z"}`, false},
		{"a queue line", `{"bead":"gt-2","rig":"gastown","at":"2026-10-20T12:00:00Z","followup_queue":{"findings":[{"title":"name the error"}]}}`, true},
		{"a queue line with an empty payload", `{"bead":"gt-3","rig":"gastown","at":"2026-10-20T12:00:00Z","followup_queue":{}}`, true},
		{"a null payload", `{"bead":"gt-4","rig":"gastown","at":"2026-10-20T12:00:00Z","followup_queue":null}`, false},
		{"a line with no queue field", `{"bead":"gt-5","rig":"gastown","at":"2026-10-20T12:00:00Z"}`, false},
		{"a line that is not JSON", `{"bead":"gt-6"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(tc.line)
			if got := land.IsQueueLine(raw); got != tc.queue {
				t.Fatalf("land.IsQueueLine = %v, want %v", got, tc.queue)
			}
			var dropped Dropped
			rows := LoadReviews([]string{tc.line}, &dropped)
			// A queue line earns neither a row nor a drop; every other line is
			// one or the other.
			if got := len(rows) == 0 && dropped.Count() == 0; got != tc.queue {
				t.Errorf("LoadReviews read a queue line as %v: %d rows, %d drops", got, len(rows), dropped.Count())
			}
			records, skipped := parseReviewRecords(raw)
			if got := len(records) == 0 && skipped == 0; got != tc.queue {
				t.Errorf("parseReviewRecords read a queue line as %v: %d records, %d skipped", got, len(records), skipped)
			}
		})
	}
}
