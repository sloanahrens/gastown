package polecat

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/rig"
)

func sessionManagerOn(db *beadsfake.Fake) *SessionManager {
	m := newTestSessionManager(&rig.Rig{Name: "gastown", Path: "/town/gastown"})
	m.beadsAt = func(string) beads.Client { return db }
	return m
}

func TestValidateIssueRefusesMissingAndTerminal(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	open, _ := db.Create(beads.CreateOptions{Title: "work"})
	done, _ := db.Create(beads.CreateOptions{Title: "done"})
	if err := db.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	m := sessionManagerOn(db)

	if err := m.validateIssue(open.ID, "/work"); err != nil {
		t.Errorf("validateIssue(open) = %v, want nil", err)
	}
	for _, id := range []string{done.ID, "gt-missing"} {
		if err := m.validateIssue(id, "/work"); !errors.Is(err, ErrIssueInvalid) {
			t.Errorf("validateIssue(%s) = %v, want ErrIssueInvalid", id, err)
		}
	}
}

func TestHookIssueSetsHookedAndAssignee(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	issue, _ := db.Create(beads.CreateOptions{Title: "work"})
	m := sessionManagerOn(db)

	if err := m.hookIssue(issue.ID, "gastown/polecats/Toast", "/work"); err != nil {
		t.Fatal(err)
	}
	got, err := db.Show(issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != beads.StatusHooked || got.Assignee != "gastown/polecats/Toast" {
		t.Errorf("after hookIssue: status=%q assignee=%q", got.Status, got.Assignee)
	}
}
