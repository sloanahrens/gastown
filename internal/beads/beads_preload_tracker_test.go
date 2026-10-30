package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trackerRouteTown lays out a town the way a real one is laid out — a
// mayor/town.json at the root, a route table under .beads, and a rig whose own
// .beads redirects into its mayor clone — and writes one route for rig
// gastown. backend "" writes the route that names no backend, which the route
// table reads as Dolt.
//
// It returns the rig's own directory, which is what a caller opens a beads
// wrapper on.
func trackerRouteTown(t *testing.T, backend string) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "gastown", ".beads"),
		filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("write town: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	route := `{"prefix":"gt-","path":"gastown/mayor/rig"`
	if backend != "" {
		route += fmt.Sprintf(`,"backend":%q`, backend)
	}
	route += "}\n"
	writeRouteTable(t, townRoot, route)
	if err := os.WriteFile(filepath.Join(townRoot, "gastown", ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	return filepath.Join(townRoot, "gastown")
}

// writeRouteTable puts table in the town's routes.jsonl.
func writeRouteTable(t *testing.T, townRoot, table string) {
	t.Helper()
	path := filepath.Join(townRoot, ".beads", RoutesFileName)
	if err := os.WriteFile(path, []byte(table), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestPreloadBeadsOnTrackerRouteIssuesNoRead is gt-p85pw.108: `gt polecat
// list` used to preload a rig that has no bd behind it, so the one read failed
// and warned on every list. On a tracker route the preload now answers without
// touching the store at all — the recorder fails any call it sees, and a read
// is the failure.
func TestPreloadBeadsOnTrackerRouteIssuesNoRead(t *testing.T) {
	t.Parallel()
	rigPath := trackerRouteTown(t, BackendTracker)
	r := newRecorder(func(args []string) reply {
		return reply{stderr: "the preload read a store on a tracker route: " + strings.Join(args, " "), err: exitError{code: 1}}
	})
	b := newBeads(beadsFields{workDir: rigPath, isolated: true, exec: r.exec})

	if err := b.PreloadBeads(preloadTestLabels, preloadTestStatuses); err != nil {
		t.Fatalf("PreloadBeads() on a tracker route error = %v, want nil", err)
	}
	if calls := r.argvs(); len(calls) != 0 {
		t.Fatalf("PreloadBeads() on a tracker route ran %d bd call(s):\n%s", len(calls), strings.Join(calls, "\n"))
	}
}

// TestPreloadBeadsOnDoltRouteStillReads is the other half of gt-p85pw.108: a
// rig the route table leaves on Dolt, and one it leaves on Dolt explicitly,
// warm the snapshots exactly as before — one bd sql read.
func TestPreloadBeadsOnDoltRouteStillReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		backend string
	}{
		{name: "a route that names the dolt backend", backend: BackendDolt},
		{name: "a route that names no backend", backend: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rigPath := trackerRouteTown(t, tt.backend)
			r := newRecorder(preloadAnswer)
			b := newBeads(beadsFields{workDir: rigPath, isolated: true, exec: r.exec})

			if err := b.PreloadBeads(preloadTestLabels, preloadTestStatuses); err != nil {
				t.Fatalf("PreloadBeads() error = %v", err)
			}
			calls := r.argvs()
			if len(calls) != 1 || !strings.HasPrefix(calls[0], "sql --json ") {
				t.Fatalf("PreloadBeads() on a Dolt route ran %d bd call(s), want the one preload read:\n%s",
					len(calls), strings.Join(calls, "\n"))
			}
			if b.issueSnapshot == nil {
				t.Fatal("PreloadBeads() on a Dolt route warmed no issues snapshot")
			}
		})
	}
}

// TestPreloadBeadsFailsClosedOnUnreadableRouteTable: whether a rig is on the
// tracker is read from the town's route table, and a table that cannot be read
// is an error rather than "not the tracker" — a false answer there would send
// the preload into a store the table may have said is not there.
func TestPreloadBeadsFailsClosedOnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	rigPath := trackerRouteTown(t, BackendTracker)
	// A directory where the table should be: LoadRoutes opens it and its read
	// fails, which is the unreadable case rather than a missing one.
	path := filepath.Join(filepath.Dir(rigPath), ".beads", RoutesFileName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove route table: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("make route table unreadable: %v", err)
	}
	r := newRecorder(func(args []string) reply {
		return reply{stderr: "the preload read a store after a failed route read: " + strings.Join(args, " "), err: exitError{code: 1}}
	})
	b := newBeads(beadsFields{workDir: rigPath, isolated: true, exec: r.exec})

	err := b.PreloadBeads(preloadTestLabels, preloadTestStatuses)
	if err == nil {
		t.Fatal("PreloadBeads() with an unreadable route table error = nil, want the route read's error")
	}
	if !strings.Contains(err.Error(), "reading the route table") {
		t.Fatalf("PreloadBeads() error = %v, want it to name the route read", err)
	}
	if calls := r.argvs(); len(calls) != 0 {
		t.Fatalf("PreloadBeads() read the store after an unreadable route table:\n%s", strings.Join(calls, "\n"))
	}
}
