package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// followupIssueAt is one fake om-followup bead created at the given time.
func followupIssueAt(id, status string, created time.Time) *beads.Issue {
	return &beads.Issue{ID: id, Status: status, CreatedAt: created.Format(time.RFC3339Nano)}
}

// The reader counts the open beads and the ones filed in the last seven days,
// and a reading it cannot trust is nil, never a partial flow (gt-b7h3pm).
func TestDashFollowupsOpenAndFiled(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	issues := []*beads.Issue{
		followupIssueAt("gt-1", "open", now.Add(-1*time.Hour)),
		followupIssueAt("gt-2", "in_progress", now.Add(-2*day)),
		followupIssueAt("gt-3", "closed", now.Add(-3*day)),
		followupIssueAt("gt-4", "closed", now.Add(-4*day)),
		followupIssueAt("gt-5", "open", now.Add(-followupWeek+time.Minute)),
		followupIssueAt("gt-6", "closed", now.Add(-followupWeek-time.Minute)),
		followupIssueAt("gt-7", "closed", now.Add(-20*day)),
		followupIssueAt("gt-8", "closed", now.Add(-30*day)),
		followupIssueAt("gt-9", "closed", now.Add(-40*day)),
	}
	bad := append(append([]*beads.Issue{}, issues...), &beads.Issue{ID: "gt-x", Status: "open", CreatedAt: "last tuesday"})
	rows := []struct {
		name        string
		issues      []*beads.Issue
		err         error
		open, filed int
		wantNil     bool
	}{
		{name: "counts", issues: issues, open: 3, filed: 5},
		{name: "unparseable created_at", issues: bad, wantNil: true},
		{name: "failed list", issues: issues, err: errors.New("list failed"), wantNil: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := &dashFollowupReader{list: func() ([]*beads.Issue, error) { return row.issues, row.err }}
			got := r.read(now)
			if row.wantNil {
				if got != nil {
					t.Fatalf("read = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Open != row.open || got.FiledWeek != row.filed || got.LandedWeek != 0 || got.SharePct != 0 {
				t.Fatalf("read = %+v, want Open %d FiledWeek %d and zero landings", got, row.open, row.filed)
			}
		})
	}
}
