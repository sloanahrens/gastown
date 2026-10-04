package cmd

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// escalationIssue builds an escalation bead the way gt escalate does, so the
// reader is tested against the real description shape.
func escalationIssue(id, title, severity, by string, at time.Time) *beads.Issue {
	fields := &beads.EscalationFields{Severity: severity, EscalatedBy: by, Reason: "test", EscalatedAt: at.Format(time.RFC3339)}
	return &beads.Issue{
		ID:          id,
		Title:       title,
		CreatedAt:   at.Format(time.RFC3339),
		Description: beads.FormatEscalationDescription(title, fields),
	}
}

func escalationReaderOver(issues []*beads.Issue, err error) *dashEscalationReader {
	return &dashEscalationReader{list: func() ([]*beads.Issue, error) { return issues, err }}
}

// The pane lists the newest six, and the ones past the cap are counted rather
// than dropped.
func TestEscalationReadOrdersNewestFirstAndCapsAtSix(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var issues []*beads.Issue
	for i := 0; i < 8; i++ {
		at := now.Add(-time.Duration(8-i) * time.Hour) // gt-0 oldest, gt-7 newest
		issues = append(issues, escalationIssue(fmt.Sprintf("gt-%d", i), fmt.Sprintf("escalation %d", i), "high", "gastown/Toast", at))
	}
	e := escalationReaderOver(issues, nil).read(now)
	if e.Unavailable {
		t.Fatal("a real reading must not read unavailable")
	}
	if len(e.Rows) != 6 || e.More != 2 {
		t.Fatalf("rows = %d, more = %d, want 6 rows and 2 more", len(e.Rows), e.More)
	}
	if e.Rows[0].ID != "gt-7" || e.Rows[5].ID != "gt-2" {
		t.Errorf("rows = %v, want gt-7..gt-2 (newest first)", escalationIDs(e.Rows))
	}
	if e.Rows[0].AgeSec == nil || *e.Rows[0].AgeSec != 3600 {
		t.Errorf("gt-7 age = %v, want 3600", e.Rows[0].AgeSec)
	}
	if e.Rows[5].AgeSec == nil || *e.Rows[5].AgeSec != 6*3600 {
		t.Errorf("gt-2 age = %v, want 21600", e.Rows[5].AgeSec)
	}
}

// The severity and the raiser come off the escalation's own description lines.
func TestEscalationRowSeverityIsLowercasedAndRaiserNamed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	issues := []*beads.Issue{
		escalationIssue("gt-a", "A", "CRITICAL", "gastown/Mayor", now.Add(-time.Minute)),
		escalationIssue("gt-b", "B", "", "", now.Add(-2*time.Minute)),
	}
	e := escalationReaderOver(issues, nil).read(now)
	if got := e.Rows[0].Severity; got != "critical" {
		t.Errorf("severity = %q, want critical", got)
	}
	if got := e.Rows[0].EscalatedBy; got != "gastown/Mayor" {
		t.Errorf("escalated_by = %q", got)
	}
	if got := e.Rows[1].Severity; got != "" {
		t.Errorf("a bead with no severity line read %q, want empty", got)
	}
}

// A bead with no time to measure from keeps a nil age, never a zero that would
// read as "just raised", and sorts after the ones that do carry a time.
func TestEscalationRowWithoutATimeKeepsANilAgeAndSortsLast(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	timed := escalationIssue("gt-timed", "timed", "high", "gastown/Toast", now.Add(-time.Hour))
	untimed := &beads.Issue{ID: "gt-untimed", Title: "no stamp"}
	e := escalationReaderOver([]*beads.Issue{untimed, timed}, nil).read(now)
	if e.Rows[0].ID != "gt-timed" || e.Rows[1].ID != "gt-untimed" {
		t.Errorf("rows = %v, want the timed one first", escalationIDs(e.Rows))
	}
	if e.Rows[1].AgeSec != nil {
		t.Errorf("untimed age = %v, want nil", *e.Rows[1].AgeSec)
	}
}

// A town with none open is an empty list, not an unavailable one.
func TestEscalationReadNoneOpen(t *testing.T) {
	t.Parallel()
	e := escalationReaderOver(nil, nil).read(time.Now())
	if e.Unavailable || e.More != 0 || len(e.Rows) != 0 {
		t.Errorf("reading = %+v, want an empty list that is not unavailable", e)
	}
}

// A read that failed says so rather than looking like a town with none open.
func TestEscalationReadUnavailable(t *testing.T) {
	t.Parallel()
	e := escalationReaderOver(nil, errors.New("dolt down")).read(time.Now())
	if !e.Unavailable {
		t.Errorf("reading = %+v, want unavailable", e)
	}
}

func escalationIDs(rows []dashboard.EscalationRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
