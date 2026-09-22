package deacon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCycleAge covers the read-only cycle dating `gt deacon status` reports:
// the first sighting of a cycle only records it, a repeat sighting ages from
// the record, and a new cycle starts over.
func TestCycleAge(t *testing.T) {
	townRoot := t.TempDir()
	start := time.Now()

	if age := CycleAge(townRoot, 7, start); age != 0 {
		t.Errorf("first sighting age = %s, want 0", age)
	}

	if age := CycleAge(townRoot, 7, start.Add(6*time.Minute)); age != 6*time.Minute {
		t.Errorf("repeat sighting age = %s, want 6m0s", age)
	}

	if age := CycleAge(townRoot, 8, start.Add(7*time.Minute)); age != 0 {
		t.Errorf("age after the cycle advanced = %s, want 0", age)
	}

	if age := CycleAge(townRoot, 8, start.Add(8*time.Minute)); age != time.Minute {
		t.Errorf("age for the new cycle = %s, want 1m0s", age)
	}
}

// TestCycleAge_FirstObservationIsPerTownRoot verifies the observation is keyed
// to the town it was taken in.
func TestCycleAge_FirstObservationIsPerTownRoot(t *testing.T) {
	townA := t.TempDir()
	townB := t.TempDir()
	start := time.Now()

	CycleAge(townA, 7, start)

	if age := CycleAge(townB, 7, start.Add(time.Minute)); age != 0 {
		t.Errorf("age in a town with no observation = %s, want 0", age)
	}
}

// TestCycleAge_UnusableObservationIsIgnored verifies that a missing, corrupt or
// timestamp-less observation file is treated as no observation at all, rather
// than as an infinitely old cycle.
func TestCycleAge_UnusableObservationIsIgnored(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"corrupt", "{not json"},
		{"no timestamp", `{"cycle": 7}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			path := CycleObservationPath(townRoot)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write observation: %v", err)
			}

			if age := CycleAge(townRoot, 7, time.Now()); age != 0 {
				t.Errorf("age with an unusable observation = %s, want 0", age)
			}
		})
	}
}
