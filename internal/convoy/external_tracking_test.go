package convoy

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// envValue is key's value in env, the last one winning as in exec.
func envValue(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
}

// caseScript is a bdScript answering by the whole argument line, less any
// leading --allow-stale: answers maps it to stdout, and any other line fails
// as an unexpected call.
func caseScript(answers map[string]string) *bdScript {
	return &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		line := strings.Join(c.Args, " ")
		if out, ok := answers[strings.TrimPrefix(line, "--allow-stale ")]; ok {
			return out, "", 0
		}
		return "", "unexpected bd args: " + line, 1
	}}
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
	t.Parallel()
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

	expectedRigWD := rigDir
	if resolved, err := filepath.EvalSymlinks(rigDir); err == nil && resolved != "" {
		expectedRigWD = resolved
	}
	// show answers a bead only from its own store: the rig's for ws-, the
	// town's for hq-.
	showFrom := func(c beads.BDCall, wd, out string) (string, string, int) {
		if c.Dir != wd {
			return "", "expected " + wd + ", got " + c.Dir, 1
		}
		if got := envValue(c.Env, "BEADS_DIR"); got != wd+"/.beads" {
			return "", "expected BEADS_DIR " + wd + "/.beads, got " + got, 1
		}
		return out, "", 0
	}
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		if envValue(c.Env, "BEADS_DOLT_SERVER_DATABASE") == "wrong-db" {
			return "", "stale Dolt database leaked", 1
		}
		pos := positional(c.Args)
		switch {
		case pos[0] == "version":
			return "bd 1.0.0", "", 0
		case pos[0] == "sql" && strings.Contains(strings.Join(c.Args, " "), "dependencies"):
			return `[{"depends_on_id":"ws-123"},{"depends_on_id":"hq-456"}]`, "", 0
		case pos[0] == "show" && len(pos) == 2 && pos[1] == "ws-123":
			return showFrom(c, expectedRigWD, `[{"id":"ws-123","title":"Worker issue","status":"open","issue_type":"task"}]`)
		case pos[0] == "show" && len(pos) == 2 && pos[1] == "hq-456":
			return showFrom(c, expectedTownWD, `[{"id":"hq-456","title":"Town issue","status":"closed","issue_type":"task"}]`)
		}
		return "", "unexpected bd args: " + strings.Join(c.Args, " "), 1
	}}
	town := testTown(townBeads, bd, nil)
	town.Env = []string{"BEADS_DIR=/wrong/.beads", "BEADS_DOLT_SERVER_DATABASE=wrong-db"}

	tracked, err := town.TrackedIssues("hq-cv-route")
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
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	bd := caseScript(map[string]string{
		"version": "",
		"dep list hq-cv-ext --direction=down --type=tracks --json": "[]",
		"show hq-cv-ext --json":               `[{"id":"hq-cv-ext","title":"External convoy","status":"open","issue_type":"convoy","dependencies":[{"id":"external:ghostty:ghostty-123","title":"Ghost 123","status":"open","type":"task","dependency_type":"tracks"},{"id":"external:ghostty:ghostty-456","title":"Ghost 456","status":"closed","type":"task","dependency_type":"tracks"},{"id":"gt-ignore","title":"Ignore me","status":"open","type":"task","dependency_type":"blocks"}]}]`,
		"show ghostty-123 ghostty-456 --json": `[{"id":"ghostty-123","title":"Ghost 123","status":"open","issue_type":"task"},{"id":"ghostty-456","title":"Ghost 456","status":"closed","issue_type":"task"}]`,
		"show ghostty-456 ghostty-123 --json": `[{"id":"ghostty-123","title":"Ghost 123","status":"open","issue_type":"task"},{"id":"ghostty-456","title":"Ghost 456","status":"closed","issue_type":"task"}]`,
		"show ghostty-123 --json":             `[{"id":"ghostty-123","title":"Ghost 123","status":"open","issue_type":"task"}]`,
		"show ghostty-456 --json":             `[{"id":"ghostty-456","title":"Ghost 456","status":"closed","issue_type":"task"}]`,
	})

	tracked, err := testTown(townBeads, bd, nil).TrackedIssues("hq-cv-ext")
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
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	// bd sql returns a single cross-rig tracks edge. `bd show` fails for the
	// target bead (simulating an unreachable / unrouted rig DB). The function
	// must still return the tracked dep, with Status = TrackedStatusUnknown.
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		line := strings.Join(c.Args, " ")
		switch {
		case line == "--allow-stale version":
			return "", "", 0
		case strings.Contains(line, "sql") && strings.Contains(line, "dependencies"):
			return `[{"depends_on_id":"ws-foo"}]`, "", 0
		case line == "show ws-foo --json":
			return "", `no issue found matching "ws-foo"`, 1
		}
		return "", "unexpected bd args: " + line, 1
	}}

	tracked, err := testTown(townBeads, bd, nil).TrackedIssues("hq-cv-unreach")
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
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		line := strings.Join(c.Args, " ")
		switch {
		case line == "--allow-stale version":
			return "", "", 0
		case strings.Contains(line, "sql") && strings.Contains(line, "dependencies"):
			return `[{"depends_on_id":"external:om:om-gate coverage: om"},{"depends_on_id":"hq-real"}]`, "", 0
		case strings.Contains(line, "show") && strings.Contains(line, "hq-real"):
			return `[{"id":"hq-real","title":"Real issue","status":"closed","issue_type":"task"}]`, "", 0
		}
		return "", "unexpected bd args: " + line, 1
	}}

	tracked, err := testTown(townBeads, bd, nil).TrackedIssues("hq-cv-damaged")
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
	ready, err := testTown(townBeads, bd, nil).closeIfComplete("hq-cv-damaged", "Damaged convoy", tracked, true)
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
