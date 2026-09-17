package beads

import (
	"testing"
)

func TestParseLoadavgField(t *testing.T) {
	tests := []struct {
		input    string
		wantVal  float64
		wantErr  bool
	}{
		{"1.23", 1.23, false},
		{"0.00", 0.0, false},
		{"25.5", 25.5, false},
		{"{", 0, true},   // brace alone should fail
		{"}", 0, true},   // brace alone should fail
		{"", 0, true},    // empty should fail
		{"abc", 0, true}, // non-numeric should fail
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			val, err := parseLoadavgField(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseLoadavgField(%q) expected error, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Errorf("parseLoadavgField(%q) unexpected error: %v", tc.input, err)
				return
			}
			if val != tc.wantVal {
				t.Errorf("parseLoadavgField(%q) = %v, want %v", tc.input, val, tc.wantVal)
			}
		})
	}
}
