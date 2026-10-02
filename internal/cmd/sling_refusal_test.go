package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/sling"
)

// TestIsSlingRefusal pins which sling failures stop being "the command was
// mistyped" and become "the command refused": the CLI drops cobra's usage block
// for exactly these, and a wrong answer either buries a refusal's reason in
// usage text or hides a real failure's help (gt-fudap).
func TestIsSlingRefusal(t *testing.T) {
	t.Parallel()

	_, blockedErr := sling.Blocked("/town", "gastown",
		func(string, string) (bool, error) { return true, nil },
		func(string, string) (bool, string) { return false, "" })

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate content", sling.DecideDuplicates("gt-x", []sling.DuplicateMatch{{
			Bead: sling.Duplicate{ID: "gt-y", Status: "open", IssueType: "task"}, SharedTests: []string{"TestFoo"},
		}}).Err(), true},
		{"pool full", &poolBackpressureError{Reason: "gastown pool at capacity"}, true},
		{"landing queue", &queueBackpressureError{Rig: "gastown", Ready: 3, Max: 1}, true},
		{"rig e-stopped", blockedErr, true},
		{"resling surviving work", &reslingRefusal{msg: dispatch.ReslingRefusalMarker + " gt-x: branch survives"}, true},
		{"a plain failure", errors.New("bead 'gt-x' not found"), false},
	}
	for _, tc := range cases {
		if got := isSlingRefusal(tc.err); got != tc.want {
			t.Errorf("%s: isSlingRefusal = %v, want %v", tc.name, got, tc.want)
		}
	}
}
