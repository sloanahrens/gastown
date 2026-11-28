package cmd

import "testing"

// TestInFlightTable fails if gt sling is renamed; gt spec sling and gt unsling are not listed.
func TestInFlightTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path []string
		want bool
	}{
		{[]string{"sling"}, true},
		{[]string{"spec", "sling"}, false},
		{[]string{"unsling"}, false},
	} {
		c := lookupCommand(rootCmd, tc.path)
		if c == nil {
			t.Fatalf("%v is not a command in rootCmd", tc.path)
		}
		if got := listedInFlight(c); got != tc.want {
			t.Errorf("listedInFlight(%s) = %v, want %v", c.CommandPath(), got, tc.want)
		}
	}
}
