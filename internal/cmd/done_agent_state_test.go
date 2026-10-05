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

// TestClearDoneCheckpoints verifies that clearDoneCheckpoints removes
// only done-cp labels while preserving other labels.
//
// Moved here from done_convoy_test.go when the convoy helpers it shared a
// file with were deleted (gt-gzhin.7).
func TestClearDoneCheckpoints(t *testing.T) {
	t.Parallel()
	allLabels := []string{
		"gt:agent",
		"idle:3",
		"done-cp:pushed:mybranch:1738972801",
		"done-cp:mr-created:gt-xyz:1738972802",
		"backoff-until:1738972900",
	}

	var kept []string
	var removed []string
	for _, label := range allLabels {
		if strings.HasPrefix(label, "done-cp:") {
			removed = append(removed, label)
		} else {
			kept = append(kept, label)
		}
	}

	if len(removed) != 2 {
		t.Errorf("expected 2 checkpoint labels removed, got %d: %v", len(removed), removed)
	}
	if len(kept) != 3 {
		t.Errorf("expected 3 labels kept, got %d: %v", len(kept), kept)
	}

	// Verify no checkpoint labels in kept set
	for _, label := range kept {
		if strings.HasPrefix(label, "done-cp:") {
			t.Errorf("checkpoint label was not removed: %s", label)
		}
	}
}

// TestDeferredDoneLeavesReadyToLandBeadUntouched: a polecat that runs
// `gt done --status DEFERRED` on a bead it no longer owns must lose neither
// gt:ready-to-land nor its assignee. DEFERRED preserves the bead for resume;
// the crew takeover nudge relies on this, since the landing flow owns the
// bead's labels and assignee (gt-qmnm3).
func TestDeferredDoneLeavesReadyToLandBeadUntouched(t *testing.T) {
	t.Parallel()
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

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID:       "gt-held",
		Title:    "held by a polecat",
		Type:     "task",
		Status:   string(beads.StatusInProgress),
		Assignee: "gastown/polecats/agate",
		Labels:   []string{land.LabelReadyToLand},
	})

	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func() (string, error) {
			return "", errors.New("resolving current HEAD: not a git repository")
		},
	}
	if err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitDeferred, "gt-held"); err != nil {
		t.Fatalf("deferred gt done: %v", err)
	}

	got, err := db.Show("gt-held")
	if err != nil {
		t.Fatalf("show gt-held: %v", err)
	}
	if !beads.HasLabel(got, land.LabelReadyToLand) {
		t.Errorf("deferred gt done removed %s: labels %v", land.LabelReadyToLand, got.Labels)
	}
	if got.Assignee != "gastown/polecats/agate" {
		t.Errorf("deferred gt done changed the assignee to %q", got.Assignee)
	}
	if got.Status == string(beads.StatusClosed) {
		t.Error("deferred gt done closed the bead")
	}
}

// TestCompletedDoneOnUnchangedReworkRecordsTheRequeueHint: a completed gt done
// on a rework bead still sitting on the head its rejection names skips the
// close (the close-time invariant, gt-6hmz) and leaves the bead open — but the
// reason it records is the actionable one: the head is unchanged, nothing was
// resubmitted, and an operator can re-queue it (gt-3e1z4).
func TestCompletedDoneOnUnchangedReworkRecordsTheRequeueHint(t *testing.T) {
	t.Parallel()
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

	const head = "1111111111111111111111111111111111111111"
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{
		ID:     "gt-rework",
		Title:  "the work",
		Type:   "task",
		Status: string(beads.StatusOpen),
		Labels: []string{land.LabelRework},
		Notes: land.FormatRejectionNote(land.RejectionNote{
			Attempt: 1, Kind: "gate", Reason: "the gate refused it",
			Branch: "polecat/mica/gt-rework+abc", Target: "main", MR: "gt-rework", Head: head,
		}),
	})

	env := doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "mica"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func() (string, error) {
			return head, nil
		},
	}
	if err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-rework"); err != nil {
		t.Fatalf("completed gt done: %v", err)
	}

	got, err := db.Show("gt-rework")
	if err != nil {
		t.Fatalf("show gt-rework: %v", err)
	}
	if got.Status == string(beads.StatusClosed) {
		t.Error("gt done closed a rework bead whose head was unchanged")
	}
	comments, err := db.Comments("gt-rework")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) == 0 {
		t.Fatal("gt done recorded nothing on the unchanged rework bead")
	}
	text := comments[len(comments)-1].Text
	for _, want := range []string{"DONE_CLOSE_SKIPPED", "head unchanged since the rejection", "gt land requeue gt-rework"} {
		if !strings.Contains(text, want) {
			t.Errorf("comment %q does not carry %q", text, want)
		}
	}
}
