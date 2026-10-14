package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
)

// escalatedTown lays out a town whose routes send gt- beads to the gastown
// rig and returns its root.
func escalatedTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	return townRoot
}

// runEscalatedDone runs gt done --status ESCALATED's agent-state step against
// db for the hooked bead gt-blocked, standing in cwd.
func runEscalatedDone(t *testing.T, db *beadsfake.Fake, cwd, townRoot string) {
	t.Helper()
	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func(string) (string, error) {
			return "", errors.New("resolving HEAD: not a git repository")
		},
	}
	if _, err := updateAgentStateOnDoneIn(env, cwd, townRoot, done.ExitEscalated, "gt-blocked", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("escalated gt done: %v", err)
	}
}

// assertEscalatedBeadHeld checks what an ESCALATED exit owes the hooked bead:
// still open, its molecule untouched, and a note naming the escalation with no
// skipped-close comment in its place (gt-tdxsx).
func assertEscalatedBeadHeld(t *testing.T, db *beadsfake.Fake) {
	t.Helper()
	got, err := db.Show("gt-blocked")
	if err != nil {
		t.Fatalf("show gt-blocked: %v", err)
	}
	if beads.IssueStatus(got.Status).IsTerminal() {
		t.Errorf("escalated gt done left the bead %s, want it open", got.Status)
	}
	wisp, err := db.Show("gt-wisp-blocked")
	if err != nil {
		t.Fatalf("show gt-wisp-blocked: %v", err)
	}
	if beads.IssueStatus(wisp.Status).IsTerminal() {
		t.Errorf("escalated gt done closed the molecule (%s), want it left for the resumed seat", wisp.Status)
	}
	comments, err := db.Comments("gt-blocked")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	var escalated bool
	for _, c := range comments {
		if strings.Contains(c.Text, "DONE_CLOSE_SKIPPED") {
			t.Errorf("comment %q reports a skipped close, want an escalation note", c.Text)
		}
		if strings.HasPrefix(c.Text, "ESCALATED:") {
			escalated = true
		}
	}
	if !escalated {
		t.Errorf("bead carries no ESCALATED note; comments = %v", comments)
	}
}

func seedEscalatedBead(db *beadsfake.Fake) {
	db.Seed(beads.Issue{
		ID:          "gt-blocked",
		Title:       "hit a blocker",
		Type:        "task",
		Status:      string(beads.StatusHooked),
		Assignee:    "gastown/polecats/nux",
		Description: "attached_molecule: gt-wisp-blocked",
	})
	db.Seed(beads.Issue{ID: "gt-wisp-blocked", Title: "mol-polecat-work", Status: "open", Ephemeral: true})
}

// TestEscalatedDoneWithZeroCommitsAheadLeavesBeadOpen: with nothing ahead of
// the target the close-time invariant passes, so before gt-tdxsx the ESCALATED
// exit force-closed the molecule and closed the bead as if completed, and the
// blocker was recorded nowhere. The cwd here is no repository, which that
// invariant reads as zero commits ahead.
func TestEscalatedDoneWithZeroCommitsAheadLeavesBeadOpen(t *testing.T) {
	t.Parallel()
	townRoot := escalatedTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedEscalatedBead(db)

	runEscalatedDone(t, db, filepath.Join(townRoot, "gastown"), townRoot)

	assertEscalatedBeadHeld(t, db)
}
