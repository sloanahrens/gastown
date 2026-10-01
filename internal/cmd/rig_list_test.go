package cmd

import (
	"testing"
)

func TestGetRigLED(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hasWitness bool
		opState    string
		want       string
	}{
		// Operational state overrides session state (GH#2555)
		{"parked no sessions", false, "PARKED", "🅿️"},
		{"parked with witness", true, "PARKED", "🅿️"},
		{"docked no sessions", false, "DOCKED", "🛑"},
		{"docked with witness", true, "DOCKED", "🛑"},

		// Witness running - active
		{"witness running", true, "OPERATIONAL", "🟢"},

		// Nothing running
		{"stopped operational", false, "OPERATIONAL", "⚫"},
		{"stopped empty state", false, "", "⚫"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetRigLED(tt.hasWitness, tt.opState)
			if got != tt.want {
				t.Errorf("GetRigLED(%v, %q) = %q, want %q",
					tt.hasWitness, tt.opState, got, tt.want)
			}
		})
	}
}
