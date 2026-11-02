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
	"github.com/steveyegge/gastown/internal/land"
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
	// The escalation submits nothing, so the bead's notes are the only run end
	// the host sees when a sandbox seat's done event dies with its town
	// (gt-1m9gr). Without it the bead's next seat is dated from this one's
	// dispatch and the overrun guard stops it at once (gt-ntvac).
	if at := land.EscalatedEndTime(got.Notes); at.IsZero() {
		t.Errorf("notes carry no ESCALATED RUN END block; notes = %q", got.Notes)
	} else if seat := escalatedEndSeat(got.Notes); seat != "gastown/polecats/nux" {
		t.Errorf("run end names seat %q, want gastown/polecats/nux; notes = %q", seat, got.Notes)
	}
}

// escalatedEndSeat returns the seat line of the note's last ESCALATED RUN END
// block, "" when it has none.
func escalatedEndSeat(notes string) string {
	blocks := strings.Split(notes, land.EscalatedEndMarker+"\n")
	last := blocks[len(blocks)-1]
	for _, line := range strings.Split(last, "\n") {
		if seat, ok := strings.CutPrefix(line, "Seat: "); ok {
			return strings.TrimSpace(seat)
		}
		if !strings.Contains(line, ":") {
			break
		}
	}
	return ""
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

// escalatedNotes returns the ESCALATED comments on gt-blocked.
func escalatedNotes(t *testing.T, db *beadsfake.Fake) []string {
	t.Helper()
	comments, err := db.Comments("gt-blocked")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	var notes []string
	for _, c := range comments {
		if strings.HasPrefix(c.Text, "ESCALATED:") {
			notes = append(notes, c.Text)
		}
	}
	return notes
}

// TestEscalatedDoneOnAClosedOrIdentityBead: the ESCALATED note says the bead
// stays open until the escalation is resolved, so it goes only on a bead that
// does. A closed bead gets its molecule ended and no note, however often gt
// done runs again; an identity bead gets neither; an open bead gets one note
// and keeps its molecule (gt-40p93).
func TestEscalatedDoneOnAClosedOrIdentityBead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		status      string
		labels      []string
		wantNotes   int
		wantWispEnd bool
	}{
		{"open bead", string(beads.StatusHooked), nil, 1, false},
		{"already-closed bead", "closed", nil, 0, true},
		{"rig identity bead", string(beads.StatusHooked), []string{"gt:rig"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := escalatedTown(t)
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			db.Seed(beads.Issue{
				ID:          "gt-blocked",
				Title:       "hit a blocker",
				Type:        "task",
				Status:      tc.status,
				Labels:      tc.labels,
				Assignee:    "gastown/polecats/nux",
				Description: "attached_molecule: gt-wisp-blocked",
			})
			db.Seed(beads.Issue{ID: "gt-wisp-blocked", Title: "mol-polecat-work", Status: "open", Ephemeral: true})

			runEscalatedDone(t, db, filepath.Join(townRoot, "gastown"), townRoot)
			if tc.name == "already-closed bead" {
				// A repeat run adds nothing to a bead that stays closed.
				runEscalatedDone(t, db, filepath.Join(townRoot, "gastown"), townRoot)
			}

			if got := escalatedNotes(t, db); len(got) != tc.wantNotes {
				t.Errorf("%d ESCALATED notes %q, want %d", len(got), got, tc.wantNotes)
			}
			wisp, err := db.Show("gt-wisp-blocked")
			if err != nil {
				t.Fatalf("show gt-wisp-blocked: %v", err)
			}
			if ended := beads.IssueStatus(wisp.Status).IsTerminal(); ended != tc.wantWispEnd {
				t.Errorf("molecule status %s, want ended = %v", wisp.Status, tc.wantWispEnd)
			}
			bead, err := db.Show("gt-blocked")
			if err != nil {
				t.Fatalf("show gt-blocked: %v", err)
			}
			if tc.status != "closed" && beads.IssueStatus(bead.Status).IsTerminal() {
				t.Errorf("bead is %s, want it left open", bead.Status)
			}
		})
	}
}
