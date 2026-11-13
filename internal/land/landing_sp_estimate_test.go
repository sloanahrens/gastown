package land

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestLandingRecordSPEstimate: a landing writes the sp: estimate its work
// carries to the landings file, and a landing whose estimate was never read
// writes no sp_estimate key at all, so an absent estimate is not written as a
// zero (gt-cdhagm).
func TestLandingRecordSPEstimate(t *testing.T) {
	t.Parallel()
	three := 3
	for _, tc := range []struct {
		name     string
		estimate *int
	}{
		{"estimate three", &three},
		{"no estimate read", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.work.SPEstimate = tc.estimate
			if _, err := f.lander().Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			line := f.landingLines()[0]
			var rec LandingRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.estimate == nil && rec.SPEstimate != nil:
				t.Errorf("landings file sp_estimate = %d, want none: %s", *rec.SPEstimate, line)
			case tc.estimate != nil && (rec.SPEstimate == nil || *rec.SPEstimate != *tc.estimate):
				t.Errorf("landings file sp_estimate = %v, want %d: %s", rec.SPEstimate, *tc.estimate, line)
			}
			if got := strings.Contains(line, `"sp_estimate"`); got != (tc.estimate != nil) {
				t.Errorf("landings line has an sp_estimate key = %v, want %v: %s", got, tc.estimate != nil, line)
			}
		})
	}
}
