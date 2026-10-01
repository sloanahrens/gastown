package session

import "testing"

func TestAssigneeSessionName_Shapes(t *testing.T) {
	t.Parallel()
	reg := NewPrefixRegistry()
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
		name, persistent := reg.AssigneeSessionName(tc.assignee)
		if (name == "") != tc.wantEmpty || persistent != tc.wantPersistent {
			t.Errorf("AssigneeSessionName(%q) = (%q, %v), want empty=%v persistent=%v", tc.assignee, name, persistent, tc.wantEmpty, tc.wantPersistent)
		}
	}
}

func TestPrefixRegistryAssigneeSessionName(t *testing.T) {
	t.Parallel()
	reg := NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("st", "schema_tools")
	for _, tc := range []struct {
		assignee       string
		wantSession    string
		wantPersistent bool
	}{
		{"schema_tools/nux", "st-nux", false},
		{"schema_tools/crew/fiddler", "st-crew-fiddler", true},
		{"schema_tools/polecats/nux", "st-nux", false},
		{"schema_tools/refinery/rig", "", false},
	} {
		gotSession, gotPersistent := reg.AssigneeSessionName(tc.assignee)
		if gotSession != tc.wantSession || gotPersistent != tc.wantPersistent {
			t.Errorf("AssigneeSessionName(%q) = (%q, %v), want (%q, %v)", tc.assignee, gotSession, gotPersistent, tc.wantSession, tc.wantPersistent)
		}
	}
}
