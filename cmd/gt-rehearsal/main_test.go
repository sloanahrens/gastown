package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rehearsal"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the command under the hermetic harness: internal/rehearsal
// imports internal/polecat, so the test binary reaches the town's beads and
// Dolt.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// TestRehearsalCommandComparePrintsTheReport: compare reads the two named record
// files and prints Compare's report for them, before then after (gt-13dre6.32).
func TestRehearsalCommandComparePrintsTheReport(t *testing.T) {
	t.Parallel()
	before := rehearsal.Record{RunID: "run-before", ChangeSet: "abc123",
		Slings:   []rehearsal.Sling{{Bead: "s1", Outcome: rehearsal.SlingOK, Seconds: 100}},
		SideRuns: []rehearsal.SideRun{{Seconds: 30}},
		Load:     rehearsal.Load{Samples: 10, Max1: 5, P90One: 4}}
	after := rehearsal.Record{RunID: "run-after", ChangeSet: "def456",
		Slings: []rehearsal.Sling{{Bead: "s1", Outcome: rehearsal.SlingKilled, Seconds: 110}}}
	tests := []struct {
		name          string
		before, after rehearsal.Record
	}{
		{"runs side by side", before, after},
		{"empty lists", rehearsal.Record{RunID: "run-empty", ChangeSet: "abc123"}, after},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a, b := filepath.Join(dir, "before.json"), filepath.Join(dir, "after.json")
			if err := rehearsal.Save(a, tc.before); err != nil {
				t.Fatal(err)
			}
			if err := rehearsal.Save(b, tc.after); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"compare", a, b}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("compare = %d, stderr %q: want 0 and no stderr", code, stderr.String())
			}
			if want := rehearsal.Compare(tc.before, tc.after); stdout.String() != want {
				t.Errorf("stdout = %q, want the report %q", stdout.String(), want)
			}
		})
	}
}
