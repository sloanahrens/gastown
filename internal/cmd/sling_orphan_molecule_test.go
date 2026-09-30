package cmd

import "testing"

// TestIsOrphanMolecule_HookedNoAssignee covers gh-3697: a bead stuck in
// status=hooked with empty assignee was not auto-burnable, so any further
// sling to the rig refused with "bead already has N attached molecule(s)".
// The fix treats this state as orphaned so sling can self-heal.
func TestIsOrphanMolecule_HookedNoAssignee(t *testing.T) {
	t.Parallel()
	info := &beadInfo{Status: "hooked", Assignee: ""}
	if !isOrphanMoleculeWith(info, func(string) bool { return false }) {
		t.Errorf("isOrphanMolecule(status=hooked, assignee='') = false, want true (gh-3697)")
	}
}

// TestIsOrphanMolecule_TableDriven: an unassigned live bead is orphaned, an
// assigned one only when its holder's session is dead, and closed or blocked
// beads never are.
func TestIsOrphanMolecule_TableDriven(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		info     *beadInfo
		deadFn   func(string) bool
		expected bool
	}{
		{
			name:     "nil info",
			info:     nil,
			deadFn:   func(string) bool { return false },
			expected: false,
		},
		{
			name:     "open, no assignee — orphan from sling crash",
			info:     &beadInfo{Status: "open", Assignee: ""},
			deadFn:   func(string) bool { return false },
			expected: true,
		},
		{
			name:     "in_progress, no assignee — orphan from sling crash",
			info:     &beadInfo{Status: "in_progress", Assignee: ""},
			deadFn:   func(string) bool { return false },
			expected: true,
		},
		{
			name:     "hooked, no assignee — gh-3697 wedge",
			info:     &beadInfo{Status: "hooked", Assignee: ""},
			deadFn:   func(string) bool { return false },
			expected: true,
		},
		{
			name:     "closed, no assignee — keep refuse path",
			info:     &beadInfo{Status: "closed", Assignee: ""},
			deadFn:   func(string) bool { return false },
			expected: false,
		},
		{
			name:     "blocked, no assignee — keep refuse path",
			info:     &beadInfo{Status: "blocked", Assignee: ""},
			deadFn:   func(string) bool { return false },
			expected: false,
		},
		{
			name:     "hooked, assignee, session alive — refuse",
			info:     &beadInfo{Status: "hooked", Assignee: "rig/polecats/Toast"},
			deadFn:   func(string) bool { return false },
			expected: false,
		},
		{
			name:     "hooked, assignee, session dead — auto-burn",
			info:     &beadInfo{Status: "hooked", Assignee: "rig/polecats/Toast"},
			deadFn:   func(string) bool { return true },
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isOrphanMoleculeWith(tt.info, tt.deadFn)
			if got != tt.expected {
				t.Errorf("isOrphanMolecule() = %v, want %v", got, tt.expected)
			}
		})
	}
}
