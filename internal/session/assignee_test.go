package session

import "testing"

func TestAssigneeSessionName_Shapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		assignee       string
		wantPersistent bool
		wantEmpty      bool
	}{
		{"gastown/nux", false, false},
		{"gastown/polecats/nux", false, false},
		{"gastown/crew/amber", true, false},
		{"gastown/witness/x", false, true},
		{"nux", false, true},
		{"a/b/c/d", false, true},
		{"", false, true},
	} {
		name, persistent := AssigneeSessionName(tc.assignee)
		if (name == "") != tc.wantEmpty || persistent != tc.wantPersistent {
			t.Errorf("AssigneeSessionName(%q) = (%q, %v), want empty=%v persistent=%v", tc.assignee, name, persistent, tc.wantEmpty, tc.wantPersistent)
		}
	}
}
