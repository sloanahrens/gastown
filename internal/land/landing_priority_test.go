package land

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestLandingRecordPriority: a landing writes the priority its work carries to
// the landings file, and a landing whose priority was never read writes none.
// Priority 0 is P0, so a bead at P0 still records its priority (gt-tnml1k).
func TestLandingRecordPriority(t *testing.T) {
	t.Parallel()
	p0, p1 := 0, 1
	for _, tc := range []struct {
		name     string
		priority *int
	}{
		{"priority one", &p1},
		{"priority zero is P0", &p0},
		{"priority never read", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.work.Priority = tc.priority
			if _, err := f.lander().Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			line := f.landingLines()[0]
			var rec LandingRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.priority == nil && rec.Priority != nil:
				t.Errorf("landings file priority = %d, want none: %s", *rec.Priority, line)
			case tc.priority != nil && (rec.Priority == nil || *rec.Priority != *tc.priority):
				t.Errorf("landings file priority = %v, want %d: %s", rec.Priority, *tc.priority, line)
			}
			if got := strings.Contains(line, `"priority"`); got != (tc.priority != nil) {
				t.Errorf("landings line has a priority key = %v, want %v: %s", got, tc.priority != nil, line)
			}
		})
	}
}
