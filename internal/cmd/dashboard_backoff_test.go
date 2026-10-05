package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
)

// writeBackoffFile seeds one rig's snapshot in a town root.
func writeBackoffFile(t *testing.T, townRoot, rig string, st landings.BackoffState) {
	t.Helper()
	path, err := landings.BackoffPath(townRoot, rig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A landing that is failing in backoff is a Landings row carrying the stage,
// the run of failures, the next retry and the error, so the operator reads it
// where they read the landings (gt-fn9e6.44).
func TestBackoffRowsCarryTheFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	rows := backoffRows("gastown", landings.BackoffState{
		Rig: "gastown", At: now,
		Beads: []landings.BackoffRecord{{
			Bead: "gt-abc", Stage: "push", Failures: 3,
			NextTry: now.Add(4 * time.Minute), Error: "the pre-push hook refused land/gt-abc",
		}},
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	got := rows[0]
	if got.Outcome != "backoff" || got.Bead != "gt-abc" || got.Rig != "gastown" {
		t.Fatalf("row %+v, want a backoff row for gt-abc on gastown", got)
	}
	if got.Stage != "push" || got.Failures != 3 || got.Detail != "the pre-push hook refused land/gt-abc" {
		t.Fatalf("row %+v; want the stage, the count and the error", got)
	}
	if got.NextTry == nil || !got.NextTry.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("row %+v; want the next retry", got)
	}
}

// The pane puts a failing landing above the day's finished rows, like the
// landings running now: it is not one of the day's results either. A rig whose
// snapshot is empty contributes nothing, and the rows are titled like the ones
// they join.
func TestWithBackoffRowsPutsThemOnTop(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	finished := []dashboard.LandingRow{{At: now, Bead: "gt-done", Outcome: "landed"}}

	if got := withBackoffRows(finished, nil, nil); len(got) != 1 || got[0].Bead != "gt-done" {
		t.Fatalf("rows = %+v; want the finished rows alone with nothing failing", got)
	}

	backoff := []dashboard.LandingRow{{At: now, Bead: "gt-abc", Rig: "gastown", Outcome: "backoff"}}
	got := withBackoffRows(finished, backoff, func(rig, bead string) string { return rig + "/" + bead })
	if len(got) != 2 || got[0].Bead != "gt-abc" || got[1].Bead != "gt-done" {
		t.Fatalf("rows = %+v; want the failing landing above the finished one", got)
	}
	if got[0].Title != "gastown/gt-abc" {
		t.Errorf("title %q, want the bead named like the rows below it", got[0].Title)
	}
}

// The pane reads every rig's snapshot, and a rig with nothing failing adds no
// row (gt-fn9e6.44).
func TestDashBackoffReadsEveryRigsSnapshot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown", "quiet")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	writeBackoffFile(t, town, "gastown", landings.BackoffState{
		Rig: "gastown", At: now,
		Beads: []landings.BackoffRecord{{Bead: "gt-abc", Stage: "ci", Failures: 2, NextTry: now.Add(time.Minute), Error: "no verdict"}},
	})
	writeBackoffFile(t, town, "quiet", landings.BackoffState{Rig: "quiet", At: now})

	rows := newDashBackoff(town).get()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only the rig with a failing landing", rows)
	}
	if rows[0].Bead != "gt-abc" || rows[0].Rig != "gastown" || rows[0].Stage != "ci" {
		t.Fatalf("row = %+v, want gt-abc failing at ci on gastown", rows[0])
	}
}
