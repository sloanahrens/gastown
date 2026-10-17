package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/formula"
)

func TestAgentStartResult_Fields(t *testing.T) {
	t.Parallel()
	result := agentStartResult{
		name:   "Mayor",
		ok:     true,
		detail: "hq-mayor",
	}

	if result.name != "Mayor" {
		t.Errorf("name = %q, want %q", result.name, "Mayor")
	}
	if !result.ok {
		t.Error("ok should be true")
	}
	if result.detail != "hq-mayor" {
		t.Errorf("detail = %q, want %q", result.detail, "hq-mayor")
	}
}

func TestMaxConcurrentAgentStarts_Constant(t *testing.T) {
	t.Parallel()
	// Verify the constant is set to a reasonable value
	if maxConcurrentAgentStarts < 1 {
		t.Errorf("maxConcurrentAgentStarts = %d, should be >= 1", maxConcurrentAgentStarts)
	}
	if maxConcurrentAgentStarts > 100 {
		t.Errorf("maxConcurrentAgentStarts = %d, should be <= 100 to prevent resource exhaustion", maxConcurrentAgentStarts)
	}
}

// =============================================================================
// waitForDoltReady tests (gt-zou1n)
// Verifies that gt up waits for Dolt server readiness before starting agents.
// =============================================================================

// =============================================================================
// recoverOrphanedBeads tests (gas-udp)
// Verifies that gt up detects and recovers orphaned hooked beads after crash.
// =============================================================================

func TestWaitForDoltReady_NoServerMode(t *testing.T) {
	t.Parallel()
	// When no server mode metadata exists, waitForDoltReady should not block.
	townRoot := t.TempDir()

	start := time.Now()
	waitForDoltReady(townRoot)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("waitForDoltReady took %v with no server mode, should return immediately", elapsed)
	}
}

// gt up writes the formulas the binary ships, replacing a hand-edited copy,
// and says nothing once the town already matches (gt-y3pgh.6).
func TestSyncUpFormulas_ReplacesDriftThenIsQuiet(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var out, errOut bytes.Buffer

	syncUpFormulas(townRoot, &out, &errOut)
	if !strings.Contains(out.String(), "Synced") || errOut.Len() != 0 {
		t.Fatalf("first sync: out=%q err=%q", out.String(), errOut.String())
	}

	const name = "mol-polecat-work.formula.toml"
	path := filepath.Join(townRoot, ".beads", "formulas", name)
	if err := os.WriteFile(path, []byte("hand edit"), 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	syncUpFormulas(townRoot, &out, &errOut)
	if !strings.Contains(out.String(), "Synced 1 formulas") {
		t.Fatalf("drift sync: out=%q", out.String())
	}
	want, err := formula.GetEmbeddedFormulaContent(name)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatal("hand-edited copy was not replaced with the embedded formula")
	}

	out.Reset()
	syncUpFormulas(townRoot, &out, &errOut)
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("in-sync town: out=%q err=%q", out.String(), errOut.String())
	}
}

// TestPolecatHasWorkHookedAndInProgress: `gt up --restore` must restart a
// polecat whose slung bead sling left hooked, or the polecat flow claimed
// in_progress, not only one holding a pinned bead (gt-b3qpu).
func TestPolecatHasWorkHookedAndInProgress(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/p1"

	cases := []struct {
		name   string
		status beads.IssueStatus
		want   bool
	}{
		{"hooked", beads.IssueStatusHooked, true},
		{"in_progress", beads.StatusInProgress, true},
		{"pinned", beads.IssueStatusPinned, true},
		{"closed", beads.StatusClosed, false},
		{"open", beads.StatusOpen, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := beadsfake.New()
			is, err := store.Create(beads.CreateOptions{Title: "work", Assignee: agent})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			status := string(tc.status)
			if err := store.Update(is.ID, beads.UpdateOptions{Status: &status}); err != nil {
				t.Fatalf("Update to %s: %v", tc.status, err)
			}
			if got := hasAssignedWork(store, agent); got != tc.want {
				t.Errorf("hasAssignedWork with a %s bead = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// A bead assigned to someone else is not this polecat's work.
func TestPolecatHasWorkIgnoresOtherAssignees(t *testing.T) {
	t.Parallel()
	store := beadsfake.New()
	is, err := store.Create(beads.CreateOptions{Title: "work", Assignee: "gastown/polecats/p2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := string(beads.IssueStatusHooked)
	if err := store.Update(is.ID, beads.UpdateOptions{Status: &status}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if hasAssignedWork(store, "gastown/polecats/p1") {
		t.Error("hasAssignedWork counted a bead assigned to another polecat")
	}
	if !hasAssignedWork(store, "gastown/polecats/p2") {
		t.Error("hasAssignedWork missed a hooked bead assigned to the polecat")
	}
}
