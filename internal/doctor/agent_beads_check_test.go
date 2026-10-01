package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestAgentBeadsExistCheck_NoRoutes verifies the check handles missing routes.
func TestAgentBeadsExistCheck_NoRoutes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// No .beads dir at all
	check := NewAgentBeadsCheck()
	ctx := newDoctorTown().ctx(tmpDir, "")

	result := check.Run(ctx)

	// With no routes, only the global agent (mayor) is checked
	// They won't exist without Dolt, so we expect error
	t.Logf("Result: status=%v, message=%s", result.Status, result.Message)
	if result.Status == StatusOK {
		t.Error("expected error for missing global agent beads")
	}
}

// TestAgentBeadsExistCheck_NoRigs verifies the check handles empty routes.
func TestAgentBeadsExistCheck_NoRigs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create .beads dir with empty routes.jsonl
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewAgentBeadsCheck()
	ctx := newDoctorTown().ctx(tmpDir, "")

	result := check.Run(ctx)

	// With empty routes, only the global agent (mayor) is checked
	// They won't exist without Dolt, so we expect error or warning
	t.Logf("Result: status=%v, message=%s", result.Status, result.Message)
}

// TestAgentBeadsExistCheck_ExpectedIDs verifies the check looks for correct agent bead IDs.
func TestAgentBeadsExistCheck_ExpectedIDs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Set up routes pointing to a rig with known prefix
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Use "sw" prefix to match sallaWork pattern
	routesContent := `{"prefix":"sw-","path":"sallaWork/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rig beads directory
	rigBeadsDir := filepath.Join(tmpDir, "sallaWork", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A canonical crew worker (its .git is a directory) needs an agent bead.
	if err := os.MkdirAll(filepath.Join(tmpDir, "sallaWork", "crew", "max", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewAgentBeadsCheck()
	ctx := newDoctorTown().ctx(tmpDir, "")

	result := check.Run(ctx)

	// Should report missing beads
	if result.Status == StatusOK {
		t.Errorf("expected error for missing agent beads, got: %s", result.Message)
	}

	// Should mention the expected bead IDs in details
	if len(result.Details) == 0 {
		t.Error("expected details to contain missing bead IDs")
	}

	// Verify the expected IDs are in the details
	expectedIDs := []string{"sw-sallaWork-crew-max"}
	for _, expectedID := range expectedIDs {
		found := false
		for _, detail := range result.Details {
			if detail == expectedID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected missing bead ID %s in details, got: %v", expectedID, result.Details)
		}
	}

	t.Logf("Result: status=%v, message=%s, details=%v", result.Status, result.Message, result.Details)
}

// TestAgentBeadsExistCheck_RespectsRigScope verifies that --rig excludes
// unrelated rig routes from agent-bead expectations.
func TestAgentBeadsExistCheck_RespectsRigScope(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"coder_dotfiles/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "coder_dotfiles", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "gastown", "crew", "alice", ".git"),
		filepath.Join(tmpDir, "coder_dotfiles", "crew", "bella", ".git"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewAgentBeadsCheck()
	ctx := newDoctorTown().ctx(tmpDir, "gastown")

	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("expected missing agent beads for scoped rig, got OK")
	}
	for _, detail := range result.Details {
		if strings.HasPrefix(detail, "do-") {
			t.Fatalf("expected --rig scope to exclude coder_dotfiles agent bead %q, got details: %v", detail, result.Details)
		}
	}
	foundGastown := false
	for _, detail := range result.Details {
		if strings.HasPrefix(detail, "gs-") {
			foundGastown = true
			break
		}
	}
	if !foundGastown {
		t.Fatalf("expected scoped result to include gastown agent beads, got details: %v", result.Details)
	}
}

// TestAgentBeadsExistCheck_FixRespectsRigScope verifies that --fix with a rig
// scope does not create agent beads for unrelated rig prefixes.
func TestAgentBeadsExistCheck_FixRespectsRigScope(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"coder_dotfiles/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "coder_dotfiles", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "crew", "alice", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "coder_dotfiles", "crew", "bella", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	bd := newDoctorTown()
	check := NewAgentBeadsCheck()
	if err := check.Fix(bd.ctx(tmpDir, "gastown")); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	log := writeLog(bd)
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, " do-") {
			t.Fatalf("expected scoped Fix() to avoid coder_dotfiles beads, got log line %q", line)
		}
	}
	if !strings.Contains(log, "create gs-gastown-crew-alice") {
		t.Fatalf("expected scoped Fix() to create gastown crew bead, got log: %q", log)
	}
}

// townDuplicateBD is a town whose rig-prefixed agent beads exist in the
// TOWN database only (the town .beads holds them; the rig database is
// empty). Used to reproduce gt-abj: a rig agent bead that exists only in the
// town DB must still be treated as missing rig-locally.
func townDuplicateBD(townRoot string) *doctorTown {
	d := newDoctorTown()
	d.db(filepath.Join(townRoot, ".beads")).Seed(
		beads.Issue{ID: "gs-gastown-crew-alice", Title: "Crew alice", Labels: []string{"gt:agent"}},
		beads.Issue{ID: "hq-mayor", Title: "Mayor", Labels: []string{"gt:agent"}},
	)
	return d
}

// writeLog is d's agent-bead writes, "create <id> dir=<dir>" and
// "update <id> dir=<dir>" lines in that order.
func writeLog(d *doctorTown) string {
	var lines []string
	for _, c := range d.creates() {
		lines = append(lines, "create "+c)
	}
	for _, u := range d.updates() {
		lines = append(lines, "update "+u)
	}
	return strings.Join(lines, "\n")
}

// setupTownDuplicateFixture creates routes and rig dirs for the gt-abj
// reproduction: one rig ("gastown", prefix gs-) whose agent beads exist only
// in the town database.
//
// The fixture includes mayor/town.json so beads.FindTownRoot resolves. This
// matters: without a town root, CreateAgentBead's ForAgentBead re-rooting is a
// no-op and the tests exercise a code path production never takes. With a town
// root, a routed wrapper re-targets creates at the town database — the gt-8po
// bug these tests must catch.
func setupTownDuplicateFixture(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gs-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	// One canonical crew worker gives the rig an agent bead to require.
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "crew", "alice", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "town.json"), []byte(`{"name":"testtown","version":2}`), 0644); err != nil {
		t.Fatal(err)
	}
	return tmpDir
}

// TestAgentBeadsExistCheck_TownOnlyRigBeadIsMissing verifies that Run reports
// a rig-scoped agent bead as missing when it exists only in the town database.
// Agent lookups (gt agents resolve --rig) require rig-local agent beads, so
// a town-level duplicate must not satisfy the existence check. See gt-abj.
func TestAgentBeadsExistCheck_TownOnlyRigBeadIsMissing(t *testing.T) {
	t.Parallel()
	tmpDir := setupTownDuplicateFixture(t)
	bd := townDuplicateBD(tmpDir)

	check := NewAgentBeadsCheck()
	ctx := bd.ctx(tmpDir, "gastown")

	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("expected town-only rig agent beads to be reported missing, got OK: %s", result.Message)
	}
	for _, want := range []string{"gs-gastown-crew-alice"} {
		found := false
		for _, detail := range result.Details {
			if strings.HasPrefix(detail, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in missing details, got: %v", want, result.Details)
		}
	}
}

// TestAgentBeadsExistCheck_FixCreatesRigLocalBeadDespiteTownDuplicate verifies
// that Fix creates the rig-local agent bead even when a town-level duplicate
// exists. Before gt-abj, the merged town+rig map made fixAgentBead return
// early on the town duplicate, so the rig-local bead was never created and
// doctor --fix could not repair the missing rig-local bead.
func TestAgentBeadsExistCheck_FixCreatesRigLocalBeadDespiteTownDuplicate(t *testing.T) {
	t.Parallel()
	tmpDir := setupTownDuplicateFixture(t)
	bd := townDuplicateBD(tmpDir)

	check := NewAgentBeadsCheck()
	ctx := bd.ctx(tmpDir, "gastown")
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	log := writeLog(bd)
	rigDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	for _, id := range []string{"gs-gastown-crew-alice"} {
		want := "create " + id + " dir=" + rigDir
		if !strings.Contains(log, want) {
			t.Errorf("expected Fix() to create %s IN THE RIG DATABASE (create running in %s) despite town duplicate, got log: %q", id, rigDir, log)
		}
	}
	// Town agents exist in the town DB — Fix must NOT recreate them.
	for _, unwanted := range []string{"create hq-mayor"} {
		if strings.Contains(log, unwanted) {
			t.Errorf("Fix() should not recreate existing town agent bead (%s), got log: %q", unwanted, log)
		}
	}
}

// legacyUnlabeledBD is a town for the legacy-bead case: the rig database
// holds the crew agent bead, and the town database the mayor's, as plain
// open tasks WITHOUT the gt:agent label (1.1.0-era identity beads). A
// gt:agent list does not return them, but show finds them; the label
// read-back reports the label present.
func legacyUnlabeledBD(townRoot string) *doctorTown {
	d := newDoctorTown()
	d.db(filepath.Join(townRoot, "gastown", "mayor", "rig")).Seed(beads.Issue{ID: "gs-gastown-crew-alice", Title: "gs-gastown-crew-alice", Type: "task"})
	d.db(filepath.Join(townRoot, ".beads")).Seed(beads.Issue{ID: beads.MayorBeadIDTown(), Title: "mayor", Type: "task"})
	d.sql = func(string) ([][]string, error) { return [][]string{{"present"}, {"1"}}, nil }
	return d
}

// TestAgentBeadsExistCheck_FixLabelsLegacyOpenBeadInsteadOfCreating verifies
// that Fix repairs an EXISTING open agent bead that merely lacks the gt:agent
// label (legacy 1.1.0-era identity beads are type=task with no label) by
// adding the label, instead of falling through to a duplicate-ID create.
// See gt-8po: the fall-through create either errored on the duplicate ID or
// silently upserted into the wrong database.
func TestAgentBeadsExistCheck_FixLabelsLegacyOpenBeadInsteadOfCreating(t *testing.T) {
	t.Parallel()
	tmpDir := setupTownDuplicateFixture(t)
	bd := legacyUnlabeledBD(tmpDir)

	check := NewAgentBeadsCheck()
	ctx := bd.ctx(tmpDir, "gastown")
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	log := writeLog(bd)

	if strings.Contains(log, "create ") {
		t.Errorf("Fix() must not create beads that already exist (label them instead), got log: %q", log)
	}
	rigDir := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	for _, id := range []string{"gs-gastown-crew-alice"} {
		want := "update " + id + " dir=" + rigDir
		if !strings.Contains(log, want) {
			t.Errorf("expected Fix() to add gt:agent label to legacy bead %s in the rig database, got log: %q", id, log)
		}
	}
}

// TestListCrewWorkers_FiltersWorktrees verifies that listCrewWorkers skips
// git worktrees (directories where .git is a file) and only returns canonical
// crew workers (where .git is a directory). This is the fix for GH#2767.
func TestListCrewWorkers_FiltersWorktrees(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "myrig"
	crewDir := filepath.Join(tmpDir, rigName, "crew")

	// Create a canonical crew worker: .git is a directory
	canonicalDir := filepath.Join(crewDir, "alice")
	if err := os.MkdirAll(filepath.Join(canonicalDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a worktree: .git is a file (contains gitdir pointer)
	worktreeDir := filepath.Join(crewDir, "alice-worktree")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"),
		[]byte("gitdir: /path/to/main/.git/worktrees/alice-worktree\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a second canonical worker
	bobDir := filepath.Join(crewDir, "bob")
	if err := os.MkdirAll(filepath.Join(bobDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a directory without .git at all (should be included — not a worktree)
	plainDir := filepath.Join(crewDir, "charlie")
	if err := os.MkdirAll(plainDir, 0755); err != nil {
		t.Fatal(err)
	}

	workers := listCrewWorkers(tmpDir, rigName)

	// Should include alice, bob, charlie but NOT alice-worktree
	expected := map[string]bool{"alice": false, "bob": false, "charlie": false}
	for _, w := range workers {
		if w == "alice-worktree" {
			t.Errorf("listCrewWorkers should skip worktree 'alice-worktree', got: %v", workers)
		}
		if _, ok := expected[w]; ok {
			expected[w] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("listCrewWorkers should include canonical worker %q, got: %v", name, workers)
		}
	}
}

// TestListPolecats_FiltersWorktrees verifies that listPolecats skips
// git worktrees, same as listCrewWorkers. See GH#2767.
func TestListPolecats_FiltersWorktrees(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "myrig"
	polecatDir := filepath.Join(tmpDir, rigName, "polecats")

	// Canonical polecat
	if err := os.MkdirAll(filepath.Join(polecatDir, "scout", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Worktree polecat (.git is a file)
	wtDir := filepath.Join(polecatDir, "scout-wt")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"),
		[]byte("gitdir: /path/to/main/.git/worktrees/scout-wt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	polecats := listPolecats(tmpDir, rigName)

	if len(polecats) != 1 || polecats[0] != "scout" {
		t.Errorf("listPolecats should return only [scout], got: %v", polecats)
	}
}
