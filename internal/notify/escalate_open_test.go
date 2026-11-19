package notify

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// failedList is a store whose listing fails, as a store that is down does.
type failedList struct{ beads.Client }

func (failedList) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("store down")
}

// TestOpenByKey: a key reads open only while an escalation filed under it is
// open. A blank key or a failed lookup is refused, not read as closed
// (gt-il3out).
func TestOpenByKey(t *testing.T) {
	t.Parallel()
	const key = "landing-needs-overseer:gt-abc#polecat"
	for _, tc := range []struct {
		name, filed, status, ask string
		fail, wantErr, want      bool
	}{
		{"no escalation reads closed", "", "", key, false, false, false},
		{"open under the key reads open", key, "open", key, false, false, true},
		{"closed under the key reads closed", key, "closed", key, false, false, false},
		{"open under another key reads closed", key + "-other", "open", key, false, false, false},
		{"a blank key is refused", "", "", " ", false, true, false},
		{"a failed lookup is refused", "", "", key, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := beadsfake.New()
			if tc.filed != "" {
				fake.Seed(beads.Issue{ID: "esc-1", Status: tc.status, Labels: []string{"gt:escalation", FingerprintLabel(tc.filed)}})
			}
			var c beads.Client = fake
			if tc.fail {
				c = failedList{fake}
			}
			got, err := OpenByKey(c, tc.ask)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("OpenByKey(%q) = %v, %v; want %v, wantErr %v", tc.ask, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
