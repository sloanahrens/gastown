package rigaudit

import "testing"

// TestVerdict checks the last line the audit prints and whether the town
// passes, for a clean town, info-only findings, a hard problem, and parked
// rigs.
func TestVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		problems []Problem
		unparked []string
		parked   []string
		wantLine string
		wantOK   bool
	}{
		{"no problems", nil, []string{"a", "b"}, nil, "RIG AUDIT: OK (2 unparked, 0 parked: none; 0 info)", true},
		{"info only", []Problem{{"info", "x"}}, []string{"a"}, nil, "RIG AUDIT: OK (1 unparked, 0 parked: none; 1 info)", true},
		{"one hard problem", []Problem{{"required", "x"}}, []string{"a"}, nil, "RIG AUDIT: 1 problem(s) (1 unparked, 0 parked: none; 0 info)", false},
		{"no parked rig", []Problem{{"info", "x"}}, []string{"a", "b"}, nil, "RIG AUDIT: OK (2 unparked, 0 parked: none; 1 info)", true},
		{"two parked rigs", nil, []string{"a"}, []string{"b", "c"}, "RIG AUDIT: OK (1 unparked, 2 parked: b, c; 0 info)", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, ok := Verdict(tc.problems, tc.unparked, tc.parked)
			if line != tc.wantLine || ok != tc.wantOK {
				t.Errorf("Verdict(%v, %v, %v) = (%q, %v), want (%q, %v)", tc.problems, tc.unparked, tc.parked, line, ok, tc.wantLine, tc.wantOK)
			}
		})
	}
}
