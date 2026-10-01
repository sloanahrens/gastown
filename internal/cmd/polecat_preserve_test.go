package cmd

import (
	"fmt"
	"strings"
	"testing"
)

func TestPreserveFailureBlockerRequiresForceAndAcknowledgement(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("%w: push rejected", errBranchNotPreserved)

	cases := []struct {
		name        string
		cause       error
		force, ack  bool
		wantBlocked bool
	}{
		{"preserved branch never blocks", nil, false, false, false},
		{"unpreserved, no override", cause, false, false, true},
		{"unpreserved, --force alone", cause, true, false, true},
		{"unpreserved, acknowledgement alone", cause, false, true, true},
		{"unpreserved, --force plus acknowledgement", cause, true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preserveFailureBlocker("gastown", "flint", tc.force, tc.ack, tc.cause)
			if (err != nil) != tc.wantBlocked {
				t.Fatalf("blocked = %v (err %v), want %v", err != nil, err, tc.wantBlocked)
			}
			if err == nil {
				return
			}
			// The operator has to be able to see both what happened and how to
			// get out of it, without re-reading the source.
			for _, want := range []string{NukeAcknowledgeUnpreservedFlag, "NOT deleted"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
		})
	}
}
