package polecat

import "testing"

func TestSelectAssignedIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		branchIssue string
		assigned    []string
		wantIssue   string
		wantAmbig   bool
	}{
		{
			name:      "single assignment selected",
			assigned:  []string{"gt-real"},
			wantIssue: "gt-real",
		},
		{
			name:        "stale branch overridden by single assignment",
			branchIssue: "gt-old",
			assigned:    []string{"gt-real"},
			wantIssue:   "gt-real",
		},
		{
			name:        "branch matching assignment needs no override",
			branchIssue: "gt-real",
			assigned:    []string{"gt-real"},
		},
		{
			name:        "subtask branch matching assignment needs no override",
			branchIssue: "gt-real.1",
			assigned:    []string{"gt-real"},
		},
		{
			name:        "branch matching one of multiple assignments needs no override",
			branchIssue: "gt-real",
			assigned:    []string{"gt-real", "gt-other"},
		},
		{
			name:      "duplicate assignment ids collapse",
			assigned:  []string{"gt-real", "gt-real"},
			wantIssue: "gt-real",
		},
		{
			name:      "multiple assignments are ambiguous",
			assigned:  []string{"gt-b", "gt-a"},
			wantAmbig: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIssue, gotAmbig := SelectAssignedIssue(tt.branchIssue, tt.assigned)
			if gotIssue != tt.wantIssue || gotAmbig != tt.wantAmbig {
				t.Fatalf("SelectAssignedIssue(%q, %v) = (%q, %v), want (%q, %v)",
					tt.branchIssue, tt.assigned, gotIssue, gotAmbig, tt.wantIssue, tt.wantAmbig)
			}
		})
	}
}

// TestIsStaleBranchIssue verifies the stale-branch guard (hq-l0fj): a
// branch-derived issue id is overridden only when it conflicts with the
// hooked bead and is not a subtask of it.
func TestIsStaleBranchIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		branchIssue string
		hookedIssue string
		want        bool
	}{
		{"matching ids are not stale", "hq-oibv", "hq-oibv", false},
		{"reused branch from closed bead is stale", "re-ofo", "hq-oibv", true},
		{"subtask of hooked bead is not stale", "gt-abc.1", "gt-abc", false},
		{"different bead with shared prefix is stale", "gt-abc1", "gt-abc", true},
		{"no branch issue is not stale", "", "hq-oibv", false},
		{"no hooked bead is not stale", "re-ofo", "", false},
		{"both empty is not stale", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStaleBranchIssue(tt.branchIssue, tt.hookedIssue); got != tt.want {
				t.Errorf("IsStaleBranchIssue(%q, %q) = %v, want %v", tt.branchIssue, tt.hookedIssue, got, tt.want)
			}
		})
	}
}
