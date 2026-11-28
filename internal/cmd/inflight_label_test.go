package cmd

import "testing"

// TestInFlightLabel fails if a listed command loses its path or args, an unlisted one gets a label, or nil panics.
func TestInFlightLabel(t *testing.T) {
	t.Parallel()
	if got, ok := inFlightLabel(nil, nil); ok || got != "" {
		t.Errorf("inFlightLabel(nil) = (%q, %v), want (\"\", false)", got, ok)
	}
	for _, tc := range []struct {
		path   []string
		args   []string
		want   string
		wantOK bool
	}{
		{[]string{"sling"}, []string{"bead-1", "rig"}, "gt sling bead-1 rig", true},
		{[]string{"unsling"}, []string{"bead-1"}, "", false},
		{[]string{"spec", "sling"}, []string{"bead-1"}, "", false},
	} {
		c := lookupCommand(rootCmd, tc.path)
		if c == nil {
			t.Fatalf("%v is not a command in rootCmd", tc.path)
		}
		if got, ok := inFlightLabel(c, tc.args); got != tc.want || ok != tc.wantOK {
			t.Errorf("inFlightLabel(%s, %v) = (%q, %v), want (%q, %v)", c.CommandPath(), tc.args, got, ok, tc.want, tc.wantOK)
		}
	}
}
