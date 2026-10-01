package convoy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func makeExternalTrackingTownWorkspace(t *testing.T) (string, string, string) {
	t.Helper()

	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}

	expectedWD := townRoot
	if resolved, err := filepath.EvalSymlinks(townRoot); err == nil && resolved != "" {
		expectedWD = resolved
	}
	return townRoot, townBeads, expectedWD
}

// TestGetTrackedIssues_UnknownStatusForUnreachableCrossRig verifies the (gt-bs6)
// contract: when the tracked bead lives in a cross-rig DB that cannot be
// resolved from the convoy owner's cwd (routes.jsonl missing, rig parked, or
// rig beads DB unreachable), the returned tracked entry carries status
// TrackedStatusUnknown instead of an empty string. Empty status was
// indistinguishable from a legitimately open bead and silenced the real
// failure mode noted in #2786.
func TestGetTrackedIssues_UnknownStatusForUnreachableCrossRig(t *testing.T) {
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	// bd sql returns a single cross-rig tracks edge, and no store answers
	// for the target bead (an unreachable / unrouted rig DB). The function
	// must still return the tracked dep, with Status = TrackedStatusUnknown.
	db := townDB()
	rawDepsAnswer(db, map[string][]string{"hq-cv-unreach": {"ws-foo"}})

	tracked, err := testTown(townBeads, db, nil).TrackedIssues("hq-cv-unreach")
	if err != nil {
		t.Fatalf("getTrackedIssues: %v", err)
	}
	if len(tracked) != 1 {
		t.Fatalf("expected 1 tracked issue, got %d: %#v", len(tracked), tracked)
	}
	if tracked[0].ID != "ws-foo" {
		t.Fatalf("tracked[0].ID = %q, want %q", tracked[0].ID, "ws-foo")
	}
	if tracked[0].Status != TrackedStatusUnknown {
		t.Fatalf("tracked[0].Status = %q, want %q", tracked[0].Status, TrackedStatusUnknown)
	}
}

// TestGetTrackedIssues_IgnoresNonBeadIDEdge pins the repair path for a convoy
// damaged before the write-time gate existed (gt-gsky): a tracks edge whose
// target is a convoy *title*.
//
// No query resolves such a target, so it came back TrackedStatusUnknown and
// held the convoy open forever — the reported convoy had every real issue
// closed and still never auto-closed. A well-formed but unreachable cross-rig
// target is still unknown and still blocks auto-close (gt-bs6).
func TestGetTrackedIssues_IgnoresNonBeadIDEdge(t *testing.T) {
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	db := townDB()
	db.Seed(beads.Issue{ID: "hq-real", Title: "Real issue", Status: "closed", Type: "task"})
	rawDepsAnswer(db, map[string][]string{"hq-cv-damaged": {"external:om:om-gate coverage: om", "hq-real"}})
	town := testTown(townBeads, db, nil)

	tracked, err := town.TrackedIssues("hq-cv-damaged")
	if err != nil {
		t.Fatalf("getTrackedIssues: %v", err)
	}
	if len(tracked) != 1 {
		t.Fatalf("expected the non-bead-ID edge to be dropped, got %d tracked issue(s): %#v", len(tracked), tracked)
	}
	if tracked[0].ID != "hq-real" || tracked[0].Status != "closed" {
		t.Fatalf("tracked[0] = %#v, want hq-real with status closed", tracked[0])
	}

	// The convoy's only real issue is closed, so it is now closable — which is
	// what the phantom edge used to prevent.
	ready, err := town.closeIfComplete("hq-cv-damaged", "Damaged convoy", tracked, true)
	if !ready {
		t.Errorf("convoy not ready to close after the phantom edge was dropped: %#v", tracked)
	}
	if err != nil {
		t.Fatalf("closeConvoyIfComplete: %v", err)
	}
}

// TestCloseConvoyIfComplete_UnknownBlocksAutoClose verifies (gt-bs6) that an
// unknown-status tracked bead prevents convoy auto-close. The rig DB being
// temporarily unreachable must not be mistaken for a completed bead.
func TestCloseConvoyIfComplete_UnknownBlocksAutoClose(t *testing.T) {
	t.Parallel()
	// No bd stub — closeConvoyIfComplete does not shell out when the convoy
	// isn't closable, which is exactly the scenario under test.
	townBeads := t.TempDir()
	tracked := []TrackedIssue{
		{ID: "ws-foo", Status: TrackedStatusUnknown},
		{ID: "ws-bar", Status: "closed"},
	}

	var out bytes.Buffer
	ready, err := Town{Root: townBeads, Out: &out}.closeIfComplete("hq-cv-unreach", "Mixed", tracked, false)
	if ready {
		t.Fatalf("closeConvoyIfComplete reported ready with unknown tracked status")
	}
	if err != nil {
		t.Fatalf("closeConvoyIfComplete: %v", err)
	}
	if !strings.Contains(out.String(), "unknown") {
		t.Fatalf("diagnostic missing 'unknown' label: %q", out.String())
	}
}
