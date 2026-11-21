package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// followupWeek is the window of "filed in the last seven days" (Q37).
const followupWeek = 7 * 24 * time.Hour

// dashFollowupReader builds the follow-up flow of the Landings pane from the
// om-followup beads of every store. list is injectable so the counts are
// testable without a database. This is the issue counts (gt-b7h3pm); the
// landings and the share are added by gt-qpwmo8.
type dashFollowupReader struct {
	list func() ([]*beads.Issue, error)
}

func newDashFollowupReader(townRoot string) *dashFollowupReader {
	return &dashFollowupReader{list: func() ([]*beads.Issue, error) { return dashFollowupIssues(townRoot) }}
}

// dashFollowupClosed reports whether a follow-up bead's status is finished.
func dashFollowupClosed(status string) bool { return status == "closed" }

// read counts the open follow-ups and the ones filed within followupWeek of
// now. A failed list or a created_at that does not parse is nil: a partial flow
// would draw as a real, smaller one.
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
	return flow
}
