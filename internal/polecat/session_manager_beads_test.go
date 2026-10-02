package polecat

import (
	"errors"
	"os"
	"path/filepath"
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

// TestBeadsForNamesTheDatabaseEachIDRoutesTo: validateIssue and hookIssue open
// the database each issue ID routes to, named by path, so the BEADS_DIR this
// test plants in the manager's own environment reaches neither the read nor
// the hook write (gt-9hou3).
//
// Not parallel: it sets BEADS_DIR.
func TestBeadsForNamesTheDatabaseEachIDRoutesTo(t *testing.T) {
	t.Setenv("BEADS_DIR", filepath.Join(t.TempDir(), ".beads"))

	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	worktree := filepath.Join(townRoot, "gastown", "polecats", "Toast", "gastown")
	if err := os.MkdirAll(filepath.Join(worktree, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".beads", "redirect"),
		[]byte("../../../mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// gt- routes to the gastown rig, hq- to the town, and a prefix no route
	// claims falls back to the worktree, whose redirect names the rig's
	// database.
	ids := []struct{ id, want string }{
		{"gt-f1", rigBeads},
		{"hq-town1", townBeads},
		{"xx-unknown1", rigBeads},
	}
	db := beadsfake.New()
	for _, tc := range ids {
		db.Seed(beads.Issue{ID: tc.id, Title: "work", Status: "open"})
	}
	m := newTestSessionManager(&rig.Rig{Name: "gastown", Path: filepath.Join(townRoot, "gastown")})
	var opened []string
	m.beadsAt = func(beadsDir string) beads.Client {
		opened = append(opened, beadsDir)
		return db
	}

	for _, tc := range ids {
		if err := m.validateIssue(tc.id, worktree); err != nil {
			t.Fatalf("validateIssue(%s): %v", tc.id, err)
		}
	}
	if len(opened) != len(ids) {
		t.Fatalf("validateIssue opened %d databases, want %d: %v", len(opened), len(ids), opened)
	}
	for i, tc := range ids {
		if opened[i] != tc.want {
			t.Errorf("validateIssue(%s) opened %q, want %q", tc.id, opened[i], tc.want)
		}
	}

	opened = nil
	if err := m.hookIssue("gt-f1", "gastown/polecats/Toast", worktree); err != nil {
		t.Fatalf("hookIssue: %v", err)
	}
	if len(opened) != 1 || opened[0] != rigBeads {
		t.Errorf("hookIssue opened %v, want [%s]", opened, rigBeads)
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
