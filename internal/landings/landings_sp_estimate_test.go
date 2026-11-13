package landings

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// TestRecord_ReadsTheWritersSPEstimate: the sp: estimate the landing worker
// writes reads back unchanged, and a record written without one reads back with
// none, so an absent estimate is never read as a zero (gt-cdhagm).
func TestRecord_ReadsTheWritersSPEstimate(t *testing.T) {
	t.Parallel()
	three := 3
	for _, tc := range []struct {
		name     string
		estimate *int
	}{
		{"estimate three", &three},
		{"no estimate", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writer := land.LandingRecord{
				BeadID: "gt-abc", Rig: "gastown", Branch: "polecat/opal/gt-abc", Head: "1111111111111111111111111111111111111111",
				Target: "main", Base: "2222222222222222222222222222222222222222",
				LandedCommit: "3333333333333333333333333333333333333333",
				PatchID:      "4444444444444444444444444444444444444444",
				GateResult:   "pass", OMVerdict: "approve", OMScore: 0.92, Route: "daemon",
				LandedAt:   time.Date(2026, 11, 13, 14, 5, 6, 0, time.UTC),
				SPEstimate: tc.estimate,
			}
			line, err := json.Marshal(writer)
			if err != nil {
				t.Fatal(err)
			}
			var rec Record
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.estimate == nil && rec.SPEstimate != nil:
				t.Fatalf("sp_estimate = %d, want none", *rec.SPEstimate)
			case tc.estimate != nil && (rec.SPEstimate == nil || *rec.SPEstimate != *tc.estimate):
				t.Fatalf("sp_estimate = %v, want %d", rec.SPEstimate, *tc.estimate)
			}
			out, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if keys(t, out) != keys(t, line) {
				t.Fatalf("key set drifted from the writer's:\n got %s\nwant %s", keys(t, out), keys(t, line))
			}
		})
	}
}
