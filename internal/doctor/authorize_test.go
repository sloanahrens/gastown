package doctor

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestValidateFixAuthorization covers the gt-638go.3 guard: a destructive
// repair by an agent needs a real authorization record, not any bead ID.
func TestValidateFixAuthorization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issue   *beads.Issue
		actor   string
		wantErr string
	}{
		{
			name:    "nil bead",
			issue:   nil,
			actor:   "gastown/polecats/amber",
			wantErr: "not found",
		},
		{
			name:    "unlabeled bead is not an authorization",
			issue:   &beads.Issue{ID: "hq-1", Status: "open", CreatedBy: "overseer"},
			actor:   "gastown/polecats/amber",
			wantErr: "not an authorization record",
		},
		{
			name:    "closed authorization is spent",
			issue:   &beads.Issue{ID: "hq-1", Status: "closed", CreatedBy: "overseer", Labels: []string{FixAuthLabel}},
			actor:   "gastown/polecats/amber",
			wantErr: "requires an open authorization",
		},
		{
			name:    "self-authorization is refused",
			issue:   &beads.Issue{ID: "hq-1", Status: "open", CreatedBy: "gastown/polecats/amber", Labels: []string{FixAuthLabel}},
			actor:   "gastown/polecats/amber",
			wantErr: "self-authorization is not allowed",
		},
		{
			name:  "an open bead the overseer created passes",
			issue: &beads.Issue{ID: "hq-1", Status: "open", CreatedBy: "overseer", Labels: []string{"other", FixAuthLabel}},
			actor: "gastown/polecats/amber",
		},
		{
			name:  "an unidentified actor cannot self-authorize",
			issue: &beads.Issue{ID: "hq-1", Status: "open", CreatedBy: "overseer", Labels: []string{FixAuthLabel}},
			actor: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateFixAuthorization(tc.issue, tc.actor)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateFixAuthorization() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateFixAuthorization() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateFixAuthorization() = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
