package done

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

func TestFollowupBudget(t *testing.T) {
	t.Parallel()
	const maxAdded, maxFiles = 80, 3
	followup := &beads.Issue{Labels: []string{land.FollowUpLabel}}
	tests := []struct {
		name     string
		issue    *beads.Issue
		added    int
		files    int
		wantErr  bool
		wantText []string
	}{
		{"at the limit passes", followup, 80, 3, false, nil},
		{"one line over refuses", followup, 81, 1, true, []string{
			"81 lines", "over the diff budget of a follow-up bead",
			"80 added lines", "3 files", "gt done --status ESCALATED",
			"instead of expanding the change",
		}},
		{"one file over refuses", followup, 20, 4, true, []string{
			"20 lines", "4 file(s)", "80 added lines", "3 files",
			"gt done --status ESCALATED",
		}},
		{"unlabelled bead passes", &beads.Issue{Labels: []string{"other"}}, 800, 30, false, nil},
		{"nil issue passes", nil, 800, 30, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := followupBudgetError(tc.issue, tc.added, tc.files, maxAdded, maxFiles)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("followupBudgetError() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("followupBudgetError() = nil, want a refusal")
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
		})
	}
}
