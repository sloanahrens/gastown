package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
)

// followupWeek is the window of "filed in the last seven days" (Q37).
const followupWeek = 7 * 24 * time.Hour

// dashFollowupReader builds the follow-up flow of the Landings pane from the
// om-followup beads of every store. list is injectable so the counts are
// testable without a database; since reads the landings from a start time.
type dashFollowupReader struct {
	list  func() ([]*beads.Issue, error)
	since func(t time.Time) ([]land.LandingRecord, error)
}

func newDashFollowupReader(townRoot string) *dashFollowupReader {
	return &dashFollowupReader{
		list:  func() ([]*beads.Issue, error) { return dashFollowupIssues(townRoot) },
		since: func(t time.Time) ([]land.LandingRecord, error) { return dashFollowupLandings(townRoot, t) },
	}
}

// dashFollowupClosed reports whether a follow-up bead's status is finished.
func dashFollowupClosed(status string) bool { return status == "closed" }

// read counts the open follow-ups, the ones filed within followupWeek of now,
// the landings of that week and the filed share of them in whole percent (zero
// landings give a share of zero). A failed list or landings read, or a
// created_at that does not parse, is nil: a partial flow would draw as a real,
// smaller one.
func (r *dashFollowupReader) read(now time.Time) *dashboard.FollowupFlow {
	issues, err := r.list()
	if err != nil {
		return nil
	}
	flow := &dashboard.FollowupFlow{}
	for _, issue := range issues {
		created, err := time.Parse(time.RFC3339Nano, issue.CreatedAt)
		if err != nil {
			return nil
		}
		if !dashFollowupClosed(issue.Status) {
			flow.Open++
		}
		if now.Sub(created) <= followupWeek {
			flow.FiledWeek++
		}
	}
	landings, err := r.since(now.Add(-followupWeek))
	if err != nil {
		return nil
	}
	flow.LandedWeek = len(landings)
	if flow.LandedWeek > 0 {
		flow.SharePct = flow.FiledWeek * 100 / flow.LandedWeek
	}
	return flow
}
