package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

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

// TestWaitForDoltReady_NoServerMode: a town with no server-mode metadata has
// nothing to wait for, so the wait reports success and gt up says nothing. A
// town with no configured endpoint is the case that pins it: had the wait gone
// on to probe, it would have reported ErrNoEndpoint as a warning (gt-zou1n).
func TestWaitForDoltReady_NoServerMode(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer
	waitForDoltReady(t.TempDir(), &warn)
	if warn.Len() != 0 {
		t.Fatalf("a town with no server mode warned: %q", warn.String())
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
			got, err := hasAssignedWork(store, agent)
			if err != nil {
				t.Fatalf("hasAssignedWork: %v", err)
			}
			if got != tc.want {
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
	if got, err := hasAssignedWork(store, "gastown/polecats/p1"); err != nil || got {
		t.Errorf("hasAssignedWork counted a bead assigned to another polecat: got=%v err=%v", got, err)
	}
	if got, err := hasAssignedWork(store, "gastown/polecats/p2"); err != nil || !got {
		t.Errorf("hasAssignedWork missed a hooked bead assigned to the polecat: got=%v err=%v", got, err)
	}
}

// failingListStore is a bead store whose assigned-issue lookup fails.
type failingListStore struct {
	beads.Client
	err error
}

func (s failingListStore) ListAssignedIssueStatuses(string, ...beads.IssueStatus) ([]*beads.Issue, error) {
	return nil, s.err
}

// A store error is neither "has work" nor "no work": hasAssignedWork returns
// it so gt up can report the polecat it could not check.
func TestHasAssignedWorkReturnsStoreError(t *testing.T) {
	t.Parallel()
	boom := errors.New("dolt unreachable")
	got, err := hasAssignedWork(failingListStore{Client: beadsfake.New(), err: boom}, "gastown/polecats/p1")
	if !errors.Is(err, boom) || got {
		t.Fatalf("hasAssignedWork = (%v, %v), want (false, %v)", got, err, boom)
	}
}

// The production wiring: polecatHasWorkIn opens the store at the polecat's
// own directory, and startPolecatsWithWorkUsing starts only the polecat whose
// hooked bead is there, and reports (rather than skips) the one whose store
// lookup failed.
func TestStartPolecatsWithWorkUsingProductionWiring(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rig = "gastown"
	polecatsDir := filepath.Join(townRoot, rig, "polecats")
	for _, name := range []string{"busy", "idle", "broken"} {
		if err := os.MkdirAll(filepath.Join(polecatsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store := beadsfake.New()
	is, err := store.Create(beads.CreateOptions{Title: "work", Assignee: rig + "/polecats/busy"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	hooked := string(beads.IssueStatusHooked)
	if err := store.Update(is.ID, beads.UpdateOptions{Status: &hooked}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	boom := errors.New("dolt unreachable")
	var mu sync.Mutex
	var opened []string
	open := func(polecatPath string) beads.Client {
		mu.Lock()
		opened = append(opened, polecatPath)
		mu.Unlock()
		if filepath.Base(polecatPath) == "broken" {
			return failingListStore{Client: store, err: boom}
		}
		return store
	}

	var started []string
	start := func(name string) error {
		started = append(started, name)
		return nil
	}
	got, failed := startPolecatsWithWorkUsing(townRoot, rig, io.Discard, polecatHasWorkIn(open), start)

	if len(got) != 1 || got[0] != "busy" || len(started) != 1 || started[0] != "busy" {
		t.Errorf("started = %v (start called for %v), want only busy", got, started)
	}
	if !errors.Is(failed["broken"], boom) || len(failed) != 1 {
		t.Errorf("failed = %v, want only broken, wrapping the store error", failed)
	}
	sort.Strings(opened)
	want := []string{
		filepath.Join(polecatsDir, "broken"),
		filepath.Join(polecatsDir, "busy"),
		filepath.Join(polecatsDir, "idle"),
	}
	if !slices.Equal(opened, want) {
		t.Errorf("store opened at %v, want each polecat's own directory %v", opened, want)
	}
}

// gt up --json (and --restore --json) must leave stdout as one JSON document:
// the DND-reset and parked-polecat lines go through upProgressWriter, which is
// silent under --json and --quiet (gt-f0gp1).
func TestUpJSONStdoutIsOnlyJSON(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rig = "gastown"
	if err := os.MkdirAll(filepath.Join(townRoot, rig, "polecats", "garnet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDeliberateStopMarker(townRoot, rig, "garnet"); err != nil {
		t.Fatalf("writeDeliberateStopMarker: %v", err)
	}
	hasWork := func(string, string) (bool, error) { return true, nil }
	start := func(string) error { return errors.New("must not start a parked polecat") }

	for _, tt := range []struct {
		name        string
		quiet, json bool
		wantLines   bool
	}{
		{"json", false, true, false},
		{"quiet", true, false, false},
		{"text", false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			progress := upProgressWriter(&stdout, tt.quiet, tt.json)
			// The DND-reset line and the parked-polecat line of gt up --restore.
			reportDNDReset(progress)
			startPolecatsWithWorkUsing(townRoot, rig, progress, hasWork, start)

			if got := stdout.Len() > 0; got != tt.wantLines {
				t.Errorf("progress lines on stdout = %v, want %v; stdout = %q", got, tt.wantLines, stdout.String())
			}
			if tt.json {
				if err := emitUpJSON(&stdout, []ServiceStatus{{Name: "Dolt", OK: true}}); err != nil {
					t.Fatalf("emitUpJSON: %v", err)
				}
				var doc map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
					t.Errorf("stdout is not a single JSON document: %v\n%s", err, stdout.String())
				}
			}
		})
	}
}
