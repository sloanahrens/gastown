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

// TestRehearsalCommandCompareRefusals: a command line the command cannot use
// exits 2, and a record it cannot read exits 1, each naming the step on stderr
// with nothing on stdout.
func TestRehearsalCommandCompareRefusals(t *testing.T) {
	t.Parallel()
	good := rehearsal.Record{RunID: "run-good", ChangeSet: "abc123"}
	tests := []struct {
		name string
		args func(dir string) []string
		want int
	}{
		{"no subcommand", func(string) []string { return nil }, 2},
		{"unknown subcommand", func(string) []string { return []string{"rehearse"} }, 2},
		{"compare with no files", func(string) []string { return []string{"compare"} }, 2},
		{"compare with one file", func(dir string) []string {
			return []string{"compare", filepath.Join(dir, "before.json")}
		}, 2},
		{"compare with three files", func(dir string) []string {
			return []string{"compare", filepath.Join(dir, "before.json"), filepath.Join(dir, "after.json"), filepath.Join(dir, "extra.json")}
		}, 2},
		{"missing before record", func(dir string) []string {
			return []string{"compare", filepath.Join(dir, "missing.json"), filepath.Join(dir, "after.json")}
		}, 1},
		{"missing after record", func(dir string) []string {
			return []string{"compare", filepath.Join(dir, "before.json"), filepath.Join(dir, "missing.json")}
		}, 1},
		{"undecodable record", func(dir string) []string {
			return []string{"compare", filepath.Join(dir, "broken.json"), filepath.Join(dir, "after.json")}
		}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range []string{"before.json", "after.json"} {
				if err := rehearsal.Save(filepath.Join(dir, name), good); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run(tc.args(dir), &stdout, &stderr); code != tc.want {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tc.want, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing on a refusal", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Error("stderr is empty, want the step that refused")
			}
		})
	}
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
