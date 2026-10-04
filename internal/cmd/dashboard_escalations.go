package cmd

import (
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The Escalations panel: the town's open escalation beads, the same set the
// escalations tile counts (beads.ListEscalationsAcrossRigs). An alert is
// something the dashboard noticed from its own snapshots; an escalation is a
// bead somebody raised, with a severity and a raiser, that stays open until it
// is closed — so the pane lists them instead of only counting them.

// escalationRows is how many escalations the pane lists, newest first. The rest
// are counted in More rather than dropped silently.
const escalationRows = 6

// dashEscalationReader reads the town's open escalations. list is injectable so
// the row building is testable without a database.
type dashEscalationReader struct {
	list func() ([]*beads.Issue, error)
}

func newDashEscalationReader(townRoot string) *dashEscalationReader {
	return &dashEscalationReader{list: func() ([]*beads.Issue, error) {
		return beads.ListEscalationsAcrossRigs(beads.New(beads.ResolveBeadsDir(townRoot)))
	}}
}

// read is the pane's whole reading. A read that fails says so; a town with none
// open is an empty list, which is a different thing.
func (r *dashEscalationReader) read(now time.Time) *dashboard.Escalations {
	issues, err := r.list()
	if err != nil {
		return &dashboard.Escalations{Unavailable: true}
	}
	rows := make([]dashboard.EscalationRow, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, dashEscalationRow(now, issue))
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	out := &dashboard.Escalations{Rows: rows}
	if len(rows) > escalationRows {
		out.Rows = rows[:escalationRows]
		out.More = len(rows) - escalationRows
	}
	return out
}

// dashEscalationRow is one escalation bead as a pane row. Its severity and
// raiser are the escalation's own description fields, which is where gt
// escalate writes them.
func dashEscalationRow(now time.Time, issue *beads.Issue) dashboard.EscalationRow {
	fields := beads.ParseEscalationFields(issue.Description)
	at := escalationRaisedAt(issue, fields)
	row := dashboard.EscalationRow{
		ID:          issue.ID,
		Title:       issue.Title,
		Severity:    strings.ToLower(strings.TrimSpace(fields.Severity)),
		EscalatedBy: fields.EscalatedBy,
		At:          at,
	}
	// A bead with no time it can be measured from keeps a nil age rather than a
	// zero that would read as "just raised".
	if !at.IsZero() {
		d := now.Sub(at)
		if d < 0 {
			d = 0
		}
		sec := int64(d / time.Second)
		row.AgeSec = &sec
	}
	return row
}

// escalationRaisedAt is when an escalation was raised: its own escalated_at,
// falling back to the bead's creation time for a bead that records none.
func escalationRaisedAt(issue *beads.Issue, fields *beads.EscalationFields) time.Time {
	for _, s := range []string{fields.EscalatedAt, issue.CreatedAt} {
		if at, err := time.Parse(time.RFC3339, s); err == nil {
			return at
		}
	}
	return time.Time{}
}
