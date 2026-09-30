package convoy

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func writeExternalTrackingBdStub(t *testing.T, scriptBody string) {
	t.Helper()

	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := "#!/bin/sh\n" + scriptBody
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func chdirExternalTrackingTest(t *testing.T, dir string) {
	t.Helper()

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
}

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

func TestGetTrackedIssues_RoutesShowByPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	townRoot, townBeads, expectedTownWD := makeExternalTrackingTownWorkspace(t)
	rigDir := filepath.Join(townRoot, "worker", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir rig beads: %v", err)
	}
	routes := `{"prefix":"hq-","path":"."}
{"prefix":"ws-","path":"worker/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	chdirExternalTrackingTest(t, townRoot)
	t.Setenv("BEADS_DIR", "/wrong/.beads")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrong-db")

	expectedRigWD := rigDir
	if resolved, err := filepath.EvalSymlinks(rigDir); err == nil && resolved != "" {
		expectedRigWD = resolved
	}
	scriptBody := fmt.Sprintf(`
case "$*" in
	  "--allow-stale version")
	    echo 'bd 1.0.0'
	    exit 0
	    ;;
	  *sql*dependencies*)
	    if [ "$BEADS_DOLT_SERVER_DATABASE" = "wrong-db" ]; then
	      echo "stale Dolt database leaked into sql" >&2
	      exit 1
	    fi
	    echo '[{"depends_on_id":"ws-123"},{"depends_on_id":"hq-456"}]'
	    ;;
	  "show --json ws-123"|"--allow-stale show --json ws-123"|"show ws-123 --json"|"--allow-stale show ws-123 --json")
    if [ "$PWD" != "%s" ]; then
      echo "expected rig dir, got $PWD" >&2
      exit 1
    fi
	    if [ "$BEADS_DIR" != "%s/.beads" ]; then
	      echo "expected rig BEADS_DIR, got $BEADS_DIR" >&2
	      exit 1
	    fi
	    if [ "$BEADS_DOLT_SERVER_DATABASE" = "wrong-db" ]; then
	      echo "stale Dolt database leaked" >&2
	      exit 1
	    fi
	    echo '[{"id":"ws-123","title":"Worker issue","status":"open","issue_type":"task"}]'
	    ;;
	  "show --json hq-456"|"--allow-stale show --json hq-456"|"show hq-456 --json"|"--allow-stale show hq-456 --json")
    if [ "$PWD" != "%s" ]; then
      echo "expected town dir, got $PWD" >&2
      exit 1
    fi
	    if [ "$BEADS_DIR" != "%s/.beads" ]; then
	      echo "expected town BEADS_DIR, got $BEADS_DIR" >&2
	      exit 1
	    fi
	    if [ "$BEADS_DOLT_SERVER_DATABASE" = "wrong-db" ]; then
	      echo "stale Dolt database leaked" >&2
	      exit 1
	    fi
	    echo '[{"id":"hq-456","title":"Town issue","status":"closed","issue_type":"task"}]'
	    ;;
	  *"show ws-123 hq-456 --json"*|*"show hq-456 ws-123 --json"*|*"show --json ws-123 hq-456"*|*"show --json hq-456 ws-123"*)
    echo "mixed-prefix batch should not be used" >&2
    exit 1
    ;;
  *)
    echo "unexpected bd args: $*" >&2
    exit 1
    ;;
esac
`, expectedRigWD, expectedRigWD, expectedTownWD, expectedTownWD)
	writeExternalTrackingBdStub(t, scriptBody)

	tracked, err := StdTown(townBeads).TrackedIssues("hq-cv-route")
	if err != nil {
		t.Fatalf("getTrackedIssues: %v", err)
	}
	if len(tracked) != 2 {
		t.Fatalf("expected 2 tracked issues, got %d: %#v", len(tracked), tracked)
	}

	statusByID := map[string]string{}
	for _, item := range tracked {
		statusByID[item.ID] = item.Status
	}
	if statusByID["ws-123"] != "open" {
		t.Fatalf("ws-123 status = %q, want %q", statusByID["ws-123"], "open")
	}
	if statusByID["hq-456"] != "closed" {
		t.Fatalf("hq-456 status = %q, want %q", statusByID["hq-456"], "closed")
	}
}

func TestGetTrackedIssues_FallsBackToShowTrackedDependencies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	townRoot, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	chdirExternalTrackingTest(t, townRoot)

	scriptBody := fmt.Sprintf(`
case "$*" in
  "--allow-stale version")
    exit 0
    ;;
  "dep list hq-cv-ext --direction=down --type=tracks --json")
    echo '[]'
    ;;
  "show hq-cv-ext --json")
    echo '[{"id":"hq-cv-ext","title":"External convoy","status":"open","issue_type":"convoy","dependencies":[{"id":"external:ghostty:ghostty-123","title":"Ghost 123","status":"open","type":"task","dependency_type":"tracks"},{"id":"external:ghostty:ghostty-456","title":"Ghost 456","status":"closed","type":"task","dependency_type":"tracks"},{"id":"gt-ignore","title":"Ignore me","status":"open","type":"task","dependency_type":"blocks"}]}]'
    ;;
  "show ghostty-123 ghostty-456 --json"|"show ghostty-456 ghostty-123 --json")
    echo '[{"id":"ghostty-123","title":"Ghost 123","status":"open","issue_type":"task"},{"id":"ghostty-456","title":"Ghost 456","status":"closed","issue_type":"task"}]'
    ;;
  "show ghostty-123 --json")
    echo '[{"id":"ghostty-123","title":"Ghost 123","status":"open","issue_type":"task"}]'
    ;;
  "show ghostty-456 --json")
    echo '[{"id":"ghostty-456","title":"Ghost 456","status":"closed","issue_type":"task"}]'
    ;;
  *)
    echo "unexpected bd args: $*" >&2
    exit 1
    ;;
esac
`)
	writeExternalTrackingBdStub(t, scriptBody)

	tracked, err := StdTown(townBeads).TrackedIssues("hq-cv-ext")
	if err != nil {
		t.Fatalf("getTrackedIssues: %v", err)
	}
	if len(tracked) != 2 {
		t.Fatalf("expected 2 tracked issues, got %d", len(tracked))
	}

	ids := []string{tracked[0].ID, tracked[1].ID}
	sort.Strings(ids)
	if ids[0] != "ghostty-123" || ids[1] != "ghostty-456" {
		t.Fatalf("unexpected tracked IDs: %v", ids)
	}

	statusByID := map[string]string{}
	for _, item := range tracked {
		statusByID[item.ID] = item.Status
	}
	if statusByID["ghostty-123"] != "open" || statusByID["ghostty-456"] != "closed" {
		t.Fatalf("unexpected tracked statuses: %#v", statusByID)
	}
}

// TestGetTrackedIssues_UnknownStatusForUnreachableCrossRig verifies the (gt-bs6)
// contract: when the tracked bead lives in a cross-rig DB that cannot be
// resolved from the convoy owner's cwd (routes.jsonl missing, rig parked, or
// rig beads DB unreachable), the returned tracked entry carries status
// TrackedStatusUnknown instead of an empty string. Empty status was
// indistinguishable from a legitimately open bead and silenced the real
// failure mode noted in #2786.
func TestGetTrackedIssues_UnknownStatusForUnreachableCrossRig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	townRoot, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	chdirExternalTrackingTest(t, townRoot)

	// bd sql returns a single cross-rig tracks edge. `bd show` fails for the
	// target bead (simulating an unreachable / unrouted rig DB). The function
	// must still return the tracked dep, with Status = TrackedStatusUnknown.
	scriptBody := `
case "$*" in
  "--allow-stale version")
    exit 0
    ;;
  *sql*dependencies*)
    echo '[{"depends_on_id":"ws-foo"}]'
    ;;
  "show ws-foo --json")
    echo "no issue found matching \"ws-foo\"" >&2
    exit 1
    ;;
  *)
    echo "unexpected bd args: $*" >&2
    exit 1
    ;;
esac
`
	writeExternalTrackingBdStub(t, scriptBody)

	tracked, err := StdTown(townBeads).TrackedIssues("hq-cv-unreach")
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
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	townRoot, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	chdirExternalTrackingTest(t, townRoot)

	scriptBody := `
case "$*" in
  "--allow-stale version")
    exit 0
    ;;
  *sql*dependencies*)
    echo '[{"depends_on_id":"external:om:om-gate coverage: om"},{"depends_on_id":"hq-real"}]'
    ;;
  *show*hq-real*)
    echo '[{"id":"hq-real","title":"Real issue","status":"closed","issue_type":"task"}]'
    ;;
  *)
    echo "unexpected bd args: $*" >&2
    exit 1
    ;;
esac
`
	writeExternalTrackingBdStub(t, scriptBody)

	tracked, err := StdTown(townBeads).TrackedIssues("hq-cv-damaged")
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
	ready, err := Town{Root: townBeads}.closeIfComplete("hq-cv-damaged", "Damaged convoy", tracked, true)
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
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

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
