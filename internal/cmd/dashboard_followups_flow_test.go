package cmd

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// followupIssueAt is one fake om-followup bead created at the given time.
func followupIssueAt(id, status string, created time.Time) *beads.Issue {
	return &beads.Issue{ID: id, Status: status, CreatedAt: created.Format(time.RFC3339Nano)}
}

// landingsOf is a fake landings read of n records that records the start it was asked for.
func landingsOf(n int, err error, asked ...*time.Time) func(time.Time) ([]land.LandingRecord, error) {
	return func(t time.Time) ([]land.LandingRecord, error) {
		if len(asked) > 0 {
			*asked[0] = t
		}
		return make([]land.LandingRecord, n), err
	}
}

// followupNine is 9 fake issues: 3 open, 5 created within the week.
func followupNine(now time.Time) []*beads.Issue {
	day := 24 * time.Hour
	var out []*beads.Issue
	for i, status := range []string{"open", "open", "open", "closed", "closed", "closed", "closed", "closed", "closed"} {
		age := time.Duration(i) * day // the first five are within the week
		if i >= 5 {
			age += 10 * day
		}
		out = append(out, followupIssueAt(fmt.Sprintf("gt-%d", i), status, now.Add(-age-time.Hour)))
	}
	return out
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
			r := &dashFollowupReader{list: func() ([]*beads.Issue, error) { return row.issues, row.err }, since: landingsOf(0, nil)}
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

// The flow carries the week's landings and the filed share: 20 landings since a
// week before now and 5 filed give 25 percent (gt-qpwmo8).
func TestDashFollowupsCounts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var asked time.Time
	r := &dashFollowupReader{
		list:  func() ([]*beads.Issue, error) { return followupNine(now), nil },
		since: landingsOf(20, nil, &asked),
	}
	got := r.read(now)
	if got == nil || got.Open != 3 || got.FiledWeek != 5 || got.LandedWeek != 20 || got.SharePct != 25 {
		t.Fatalf("read = %+v, want Open 3 FiledWeek 5 LandedWeek 20 SharePct 25", got)
	}
	if want := now.Add(-followupWeek); !asked.Equal(want) {
		t.Errorf("landings read since %v, want %v", asked, want)
	}
}

// No landings give a share of zero, and a failed landings read is nil.
func TestDashFollowupsZeroAndFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	list := func() ([]*beads.Issue, error) { return followupNine(now), nil }
	zero := (&dashFollowupReader{list: list, since: landingsOf(0, nil)}).read(now)
	if zero == nil || zero.LandedWeek != 0 || zero.SharePct != 0 || zero.FiledWeek != 5 {
		t.Errorf("zero landings read = %+v, want LandedWeek 0, SharePct 0, FiledWeek 5", zero)
	}
	if failed := (&dashFollowupReader{list: list, since: landingsOf(20, errors.New("read failed"))}).read(now); failed != nil {
		t.Errorf("a failed landings read = %+v, want nil", failed)
	}
}
