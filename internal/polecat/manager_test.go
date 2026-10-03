package polecat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/wisp"
)

func TestHasSubmittableWorkForWorkstateUsesBranchTargetStatus(t *testing.T) {
	t.Parallel()
	w, repo := setupManagerSquashPreservedRepo(t)
	if got := hasSubmittableWorkForWorkstate(w.repo(repo), []string{"integration/test"}); got {
		t.Fatal("squash-preserved branch should not require MQ submission through manager workstate helper")
	}

	w.writeAndCommit(t, repo, "extra local work", map[string]string{"feature.txt": "one\ntwo\nthree\n"})
	if got := hasSubmittableWorkForWorkstate(w.repo(repo), []string{"integration/test"}); !got {
		t.Fatal("new local work after squash preservation should still require MQ submission")
	}
}

// setupManagerSquashPreservedRepo is a repo on polecat/squash, two
// checkpoint commits ahead of main, whose change origin's integration/test
// carries as one squash commit, then moves past.
func setupManagerSquashPreservedRepo(t *testing.T) (*world, string) {
	t.Helper()
	w := newWorld()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	w.InitBare(t, remote)
	w.Commit(t, remote, "main", "base", map[string]string{"README.md": "base\n"})
	w.Clone(t, remote, repo)
	g := w.OpenBranchRepo(repo)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(g.CheckoutNewBranch("integration/test", "main"))
	must(g.Push("origin", "integration/test", false))
	must(g.CheckoutNewBranch("polecat/squash", "integration/test"))
	w.writeAndCommit(t, repo, "checkpoint one", map[string]string{"feature.txt": "one\n"})
	w.writeAndCommit(t, repo, "checkpoint two", map[string]string{"feature.txt": "one\ntwo\n"})
	must(g.Checkout("integration/test"))
	must(g.MergeSquash("polecat/squash", "squash polecat work"))
	w.writeAndCommit(t, repo, "advance target", map[string]string{"target.txt": "target advanced\n"})
	must(g.Push("origin", "integration/test", false))
	must(g.Checkout("polecat/squash"))
	return w, repo
}

// addBeads readies the beads side of an AddWithOptions test and returns
// the bd the manager runs: fakeAddBeads in the unit tier, realAddBeads (a
// real bd on the test Dolt container) in the integration tier.
type addBeads func(t *testing.T, mayorRig, mayorBeads string) *polecatDB

// fakeAddBeads returns the fake agent bd and writes the type-config sentinel
// so EnsureCustomTypes is a no-op.
func fakeAddBeads(_ *testing.T, _, mayorBeads string) *polecatDB {
	_ = os.WriteFile(filepath.Join(mayorBeads, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644)
	return newPolecatDB()
}

// createStalePolecatCommit creates branchName at startPoint in the checkout
// at repoPath, commits a marker file to it, and returns the commit.
func createStalePolecatCommit(t *testing.T, w *world, repoPath, startPoint, branchName string) string {
	t.Helper()
	if err := w.repo(repoPath).CheckoutNewBranch(branchName, startPoint); err != nil {
		t.Fatalf("checkout stale branch %s from %s: %v", branchName, startPoint, err)
	}
	fileName := strings.NewReplacer("/", "-", "@", "-").Replace(branchName) + ".txt"
	return w.writeAndCommit(t, repoPath, "Create stale polecat branch", map[string]string{fileName: branchName + "\n"})
}

func TestManagerGetMapsDoneAgentStateFromBead(t *testing.T) {
	t.Parallel()
	bd := newPolecatDB()
	bd.setAgent(t, "gt-testrig-polecat-toast", func(f *beads.AgentFields) { f.Rig = "testrig"; f.AgentState = "done" })

	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", "toast", "testrig"), 0755); err != nil {
		t.Fatalf("mkdir polecat path: %v", err)
	}

	mgr := newTestManager(&rig.Rig{Name: "testrig", Path: rigPath}, nil, nil, bd)
	p, err := mgr.Get("toast")
	if err != nil {
		t.Fatalf("mgr.Get(toast): %v", err)
	}
	if p.State != StateDone {
		t.Fatalf("polecat state = %q, want %q", p.State, StateDone)
	}
	if p.Issue != "" {
		t.Fatalf("polecat issue = %q, want empty", p.Issue)
	}
}

func TestStateIsWorking(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state   State
		working bool
	}{
		{StateWorking, true},
		{StateDone, false},
		{StateStuck, false},
	}

	for _, tt := range tests {
		if got := tt.state.IsWorking(); got != tt.working {
			t.Errorf("%s.IsWorking() = %v, want %v", tt.state, got, tt.working)
		}
	}
}

func TestPolecatSummary(t *testing.T) {
	t.Parallel()
	p := &Polecat{
		Name:  "Toast",
		State: StateWorking,
		Issue: "gt-abc",
	}

	summary := p.Summary()
	if summary.Name != "Toast" {
		t.Errorf("Name = %q, want Toast", summary.Name)
	}
	if summary.State != StateWorking {
		t.Errorf("State = %v, want StateWorking", summary.State)
	}
	if summary.Issue != "gt-abc" {
		t.Errorf("Issue = %q, want gt-abc", summary.Issue)
	}
}

func TestListEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	polecats, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(polecats) != 0 {
		t.Errorf("polecats count = %d, want 0", len(polecats))
	}
}

func TestGetNotFound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	_, err := m.Get("nonexistent")
	if err != ErrPolecatNotFound {
		t.Errorf("Get = %v, want ErrPolecatNotFound", err)
	}
}

func TestRemoveNotFound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	err := m.Remove("nonexistent", false)
	if err != ErrPolecatNotFound {
		t.Errorf("Remove = %v, want ErrPolecatNotFound", err)
	}
}

func TestActiveWorkBeadsForCleanupFiltersAssignedIssues(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		{ID: "open-work", Status: "open", Type: "task"},
		{ID: "progress-work", Status: "in_progress", Type: "task"},
		{ID: "hooked-work", Status: beads.StatusHooked, Type: "task"},
		{ID: "closed-work", Status: "closed", Type: "task"},
		{ID: "agent", Status: "open", Type: "agent"},
		{ID: "protected", Status: "open", Type: "task", Labels: []string{"gt:keep"}},
		{ID: "deferred", Status: "deferred", Type: "task"},
		nil,
	}

	got := activeWorkBeadsForCleanup(issues)
	want := []string{"open-work", "progress-work", "hooked-work"}
	if len(got) != len(want) {
		t.Fatalf("got %d issue(s), want %d: %#v", len(got), len(want), got)
	}
	for i := range got {
		if got[i].ID != want[i] {
			t.Fatalf("got IDs %v, want %v", issueIDs(got), want)
		}
	}
}

func issueIDs(issues []*beads.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue != nil {
			ids = append(ids, issue.ID)
		}
	}
	return ids
}

func TestPolecatDir(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{
		Name: "test-rig",
		Path: "/home/user/ai/test-rig",
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	dir := m.polecatDir("Toast")
	expected := "/home/user/ai/test-rig/polecats/Toast"
	if filepath.ToSlash(dir) != expected {
		t.Errorf("polecatDir = %q, want %q", dir, expected)
	}
}

func TestAssigneeID(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{
		Name: "test-rig",
		Path: "/home/user/ai/test-rig",
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	id := m.assigneeID("Toast")
	expected := "test-rig/polecats/Toast"
	if id != expected {
		t.Errorf("assigneeID = %q, want %q", id, expected)
	}
}

// TestAgentBeadID_Deterministic verifies that agentBeadID returns the same string
// on repeated calls and across Managers for one rig. Regression test for gt-lph:
// the old implementation called workspace.Find on each invocation, which could
// resolve differently depending on cwd; the town root is now filepath.Dir of the
// rig path, fixed at construction, so nothing reads the working directory.
func TestAgentBeadID_Deterministic(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	r := &rig.Rig{
		Name: "myrig",
		Path: rigPath,
	}

	// Construct two Managers from the same rig path — they must produce
	// identical agentBeadIDs regardless of construction context.
	m1 := newTestManager(r, nil, nil, newNoDatabaseDB())
	m2 := newTestManager(r, nil, nil, newNoDatabaseDB())

	id1a := m1.agentBeadID("Toast")
	id1b := m1.agentBeadID("Toast")
	id2 := m2.agentBeadID("Toast")

	// Same Manager, repeated calls — must be identical.
	if id1a != id1b {
		t.Errorf("agentBeadID not stable across calls: %q vs %q", id1a, id1b)
	}

	// Different Manager, same rig — must be identical.
	if id1a != id2 {
		t.Errorf("agentBeadID differs across Managers for same rig: %q vs %q", id1a, id2)
	}

	// Verify the ID is non-empty and contains expected components.
	if id1a == "" {
		t.Fatal("agentBeadID returned empty string")
	}
}

func TestNewManager_NamepoolFromRigConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "myrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write rig config.json with polecat_names (no settings/config.json).
	// The identity keys are required now that one strict loader owns the file
	// (gt-y3pgh.2.5); a names-only file does not decode.
	rigConfig := `{"type":"rig","version":1,"name":"myrig","polecat_names": ["alpha", "beta", "gamma"]}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(rigConfig), 0644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "myrig", Path: rigPath}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())
	pool := m.GetNamePool()

	name, err := pool.Allocate()
	if err != nil {
		t.Fatalf("Allocate error: %v", err)
	}
	if name != "alpha" {
		t.Errorf("expected first name from rig config (alpha), got %q", name)
	}
}

// Note: State persistence tests removed - state is now derived from beads assignee field.
// Integration tests should verify beads-based state management.

func TestGetReturnsWorkingWithoutBeads(t *testing.T) {
	// When beads is not available, Get should return StateWorking
	// (assume the polecat is doing something if it exists)
	t.Parallel()

	root := t.TempDir()
	polecatDir := filepath.Join(root, "polecats", "Test")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create mayor/rig directory for beads (but no actual beads)
	mayorRigDir := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRigDir, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newMissingDB())

	// Get should return polecat with StateWorking (assume active if beads unavailable)
	polecat, err := m.Get("Test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if polecat.Name != "Test" {
		t.Errorf("Name = %q, want Test", polecat.Name)
	}
	if polecat.State != StateWorking {
		t.Errorf("State = %v, want StateWorking (beads not available)", polecat.State)
	}
}

func TestListWithPolecats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create some polecat directories (state is now derived from beads, not state files)
	for _, name := range []string{"Toast", "Cheedo"} {
		polecatDir := filepath.Join(root, "polecats", name)
		if err := os.MkdirAll(polecatDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "polecats", ".claude"), 0755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	// Create mayor/rig for beads path
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	polecats, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(polecats) != 2 {
		t.Errorf("polecats count = %d, want 2", len(polecats))
	}
}

// TestList_BatchesBeadsQueriesAcrossPolecats guards gt-ls4u: List() used to
// have each polecat's loadFromBeads issue its own hooked/assigned/agent-bead
// bd subprocess, so a sling dispatch against an N-polecat pool fanned out to
// up to 3*N concurrent 'bd' processes — measured as 18 concurrent
// 'bd list --status=all' calls plus one 'bd show <agent-id>' per polecat on
// a 29-name pool (1234% CPU, host idle 0% for the duration). The fix
// (beadsBatch/loadBeadsBatch) issues those queries ONCE for the whole rig
// and reuses them across every polecat.
//
// This asserts an actual bd invocation COUNT, not just that List() still
// returns the right polecats — an absence-only assertion would pass even if
// the batching were silently reverted to per-polecat querying.
func TestList_BatchesBeadsQueriesAcrossPolecats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	names := []string{"toast", "cheedo", "capable", "dag", "furiosa"}
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(root, "polecats", name), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	bd := newPolecatDB()

	r := &rig.Rig{Name: "test-rig", Path: root}
	m := newTestManager(r, nil, nil, bd)

	polecats, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(polecats) != len(names) {
		t.Fatalf("polecats count = %d, want %d", len(polecats), len(names))
	}

	// Batched: the rig-wide queries are independent of polecat count —
	// hooked list, assigned/status query, and the two reads ListAgentBeads
	// makes (durable gt:agent issues, then the wisp half, which bd answers
	// from the wisps table). Per-polecat querying would need at least one
	// store read per polecat here (5).
	const wantBatchedReads = 4
	if reads := bd.readCount(); reads != wantBatchedReads {
		t.Fatalf("store read %d times for %d polecats — want exactly %d rig-wide queries (batched)",
			reads, len(names), wantBatchedReads)
	}
}

// Note: TestSetState, TestAssignIssue, and TestClearIssue were removed.
// These operations now require a running beads instance and are tested
// via integration tests. The unit tests here focus on testing the basic
// polecat lifecycle operations that don't require beads.

func TestSetStateWithoutBeads(t *testing.T) {
	t.Parallel()
	// SetState should not error when beads is not available
	root := t.TempDir()
	polecatDir := filepath.Join(root, "polecats", "Test")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create mayor/rig for beads path
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	// SetState should succeed (no-op when no issue assigned)
	err := m.SetState("Test", StateWorking)
	if err != nil {
		t.Errorf("SetState: %v (expected no error when no beads/issue)", err)
	}
}

func TestClearIssueWithoutAssignment(t *testing.T) {
	t.Parallel()
	// ClearIssue should not error when no issue is assigned
	root := t.TempDir()
	polecatDir := filepath.Join(root, "polecats", "Test")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create mayor/rig for beads path
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	// ClearIssue should succeed even when no issue assigned
	err := m.ClearIssue("Test")
	if err != nil {
		t.Errorf("ClearIssue: %v (expected no error when no assignment)", err)
	}
}

// NOTE: TestInstallCLAUDETemplate tests were removed.
// We no longer write CLAUDE.md to worktrees - Gas Town context is injected
// ephemerally via SessionStart hook (gt prime) to prevent leaking internal
// architecture into project repos.

// TestReconcilePoolWith tests all permutations of directory and session existence.
// This is the core allocation policy logic.
//
// Truth table:
//
//	HasDir | HasSession | Result
//	-------|------------|------------------
//	false  | false      | available (not in-use)
//	true   | false      | in-use (normal finished polecat)
//	false  | true       | orphan → kill session, available
//	true   | true       | in-use (normal working polecat)
func TestReconcilePoolWith(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		namesWithDirs     []string
		namesWithSessions []string
		wantInUse         []string // names that should be marked in-use
		wantOrphans       []string // sessions that should be killed
	}{
		{
			name:              "no dirs, no sessions - all available",
			namesWithDirs:     []string{},
			namesWithSessions: []string{},
			wantInUse:         []string{},
			wantOrphans:       []string{},
		},
		{
			name:              "has dir, no session - in use",
			namesWithDirs:     []string{"toast"},
			namesWithSessions: []string{},
			wantInUse:         []string{"toast"},
			wantOrphans:       []string{},
		},
		{
			name:              "no dir, has session - orphan killed",
			namesWithDirs:     []string{},
			namesWithSessions: []string{"nux"},
			wantInUse:         []string{},
			wantOrphans:       []string{"nux"},
		},
		{
			name:              "has dir, has session - in use",
			namesWithDirs:     []string{"capable"},
			namesWithSessions: []string{"capable"},
			wantInUse:         []string{"capable"},
			wantOrphans:       []string{},
		},
		{
			name:              "mixed: one with dir, one orphan session",
			namesWithDirs:     []string{"toast"},
			namesWithSessions: []string{"toast", "nux"},
			wantInUse:         []string{"toast"},
			wantOrphans:       []string{"nux"},
		},
		{
			name:              "multiple dirs, no sessions",
			namesWithDirs:     []string{"toast", "nux", "capable"},
			namesWithSessions: []string{},
			wantInUse:         []string{"capable", "nux", "toast"},
			wantOrphans:       []string{},
		},
		{
			name:              "multiple orphan sessions",
			namesWithDirs:     []string{},
			namesWithSessions: []string{"slit", "rictus"},
			wantInUse:         []string{},
			wantOrphans:       []string{"rictus", "slit"},
		},
		{
			name:              "complex: dirs, valid sessions, orphan sessions",
			namesWithDirs:     []string{"toast", "capable"},
			namesWithSessions: []string{"toast", "nux", "slit"},
			wantInUse:         []string{"capable", "toast"},
			wantOrphans:       []string{"nux", "slit"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary directory for pool state
			tmpDir, err := os.MkdirTemp("", "reconcile-test-*")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()

			// Create rig and manager (nil tmux for unit test)
			// Use "myrig" which hashes to mad-max theme
			r := &rig.Rig{
				Name: "myrig",
				Path: tmpDir,
			}
			m := newTestManager(r, nil, nil, newNoDatabaseDB())

			// Call ReconcilePoolWith
			m.ReconcilePoolWith(tt.namesWithDirs, tt.namesWithSessions)

			// Verify in-use names
			gotInUse := m.namePool.ActiveNames()
			sort.Strings(gotInUse)
			sort.Strings(tt.wantInUse)

			if len(gotInUse) != len(tt.wantInUse) {
				t.Errorf("in-use count: got %d, want %d", len(gotInUse), len(tt.wantInUse))
			}
			for i := range tt.wantInUse {
				if i >= len(gotInUse) || gotInUse[i] != tt.wantInUse[i] {
					t.Errorf("in-use names: got %v, want %v", gotInUse, tt.wantInUse)
					break
				}
			}

			// Verify orphans would be identified correctly
			// (actual killing requires tmux, tested separately)
			dirSet := make(map[string]bool)
			for _, name := range tt.namesWithDirs {
				dirSet[name] = true
			}
			var gotOrphans []string
			for _, name := range tt.namesWithSessions {
				if !dirSet[name] {
					gotOrphans = append(gotOrphans, name)
				}
			}
			sort.Strings(gotOrphans)
			sort.Strings(tt.wantOrphans)

			if len(gotOrphans) != len(tt.wantOrphans) {
				t.Errorf("orphan count: got %d, want %d", len(gotOrphans), len(tt.wantOrphans))
			}
			for i := range tt.wantOrphans {
				if i >= len(gotOrphans) || gotOrphans[i] != tt.wantOrphans[i] {
					t.Errorf("orphans: got %v, want %v", gotOrphans, tt.wantOrphans)
					break
				}
			}
		})
	}
}

func TestReconcilePoolWith_KeepsDirBackedStaleSession(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "myrig")
	tm := newFakeProbe()
	m := newTestManager(&rig.Rig{Name: "myrig", Path: rigPath}, nil, tm, newNoDatabaseDB())
	activeName := "toast"
	orphanName := "nux"
	activeSession := session.PolecatSessionName(session.DefaultPrefix, activeName)
	orphanSession := session.PolecatSessionName(session.DefaultPrefix, orphanName)

	for _, sessionName := range []string{activeSession, orphanSession} {
		if err := tm.NewSessionWithCommandAndEnv(sessionName, townRoot, "sleep 300", nil); err != nil {
			t.Fatalf("create tmux session %s: %v", sessionName, err)
		}
	}

	writeStaleHeartbeat := func(sessionName string) {
		t.Helper()
		if err := os.MkdirAll(heartbeatsDir(townRoot), 0755); err != nil {
			t.Fatalf("mkdir heartbeats: %v", err)
		}
		data, err := json.Marshal(SessionHeartbeat{
			Timestamp: time.Now().Add(-SessionHeartbeatStaleThreshold - time.Minute).UTC(),
			State:     HeartbeatWorking,
		})
		if err != nil {
			t.Fatalf("marshal heartbeat: %v", err)
		}
		if err := os.WriteFile(heartbeatFile(townRoot, sessionName), data, 0644); err != nil {
			t.Fatalf("write heartbeat %s: %v", sessionName, err)
		}
	}
	writeStaleHeartbeat(activeSession)
	writeStaleHeartbeat(orphanSession)

	m.ReconcilePoolWith([]string{activeName}, []string{activeName, orphanName})

	running, err := tm.HasSession(activeSession)
	if err != nil {
		t.Fatalf("check active session: %v", err)
	}
	if !running {
		t.Fatalf("dir-backed stale session %s should survive reconciliation", activeSession)
	}
	if hb := ReadSessionHeartbeat(townRoot, activeSession); hb == nil {
		t.Fatalf("dir-backed stale session heartbeat should survive reconciliation")
	}

	running, err = tm.HasSession(orphanSession)
	if err != nil {
		t.Fatalf("check orphan session: %v", err)
	}
	if running {
		t.Fatalf("orphan session %s should be killed by reconciliation", orphanSession)
	}
	if hb := ReadSessionHeartbeat(townRoot, orphanSession); hb != nil {
		t.Fatalf("orphan session heartbeat should be removed")
	}

	activeNames := m.namePool.ActiveNames()
	if !containsString(activeNames, activeName) {
		t.Fatalf("dir-backed name %q should remain in use; active names: %v", activeName, activeNames)
	}
	if containsString(activeNames, orphanName) {
		t.Fatalf("orphan name %q should not remain in use; active names: %v", orphanName, activeNames)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestReconcilePoolWith_Allocation verifies that allocation respects reconciled state.
func TestReconcilePoolWith_Allocation(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "reconcile-alloc-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Use "myrig" which hashes to mad-max theme
	r := &rig.Rig{
		Name: "myrig",
		Path: tmpDir,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	// Mark first few pool names as in-use via directories
	// (furiosa, nux, slit are first 3 in mad-max theme)
	m.ReconcilePoolWith([]string{"furiosa", "nux", "slit"}, []string{})

	// First allocation should skip in-use names
	name, err := m.namePool.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// Should get "rictus" (4th in mad-max theme), not furiosa/nux/slit
	if name == "furiosa" || name == "nux" || name == "slit" {
		t.Errorf("allocated in-use name %q, should have skipped", name)
	}
	if name != "rictus" {
		t.Errorf("expected rictus (4th name), got %q", name)
	}
}

// TestReconcilePoolWith_OrphanDoesNotBlockAllocation verifies orphan sessions
// don't prevent name allocation (they're killed, freeing the name).
func TestReconcilePoolWith_OrphanDoesNotBlockAllocation(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "reconcile-orphan-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Use "myrig" which hashes to mad-max theme
	r := &rig.Rig{
		Name: "myrig",
		Path: tmpDir,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	// furiosa has orphan session (no dir) - should NOT block allocation
	m.ReconcilePoolWith([]string{}, []string{"furiosa"})

	// furiosa should be available (orphan session killed, name freed)
	name, err := m.namePool.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	if name != "furiosa" {
		t.Errorf("expected furiosa (orphan freed), got %q", name)
	}
}

func TestIsDoltConfigError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"transient optimistic lock", fmt.Errorf("optimistic lock failed"), false},
		{"transient serialization", fmt.Errorf("serialization failure"), false},
		{"not initialized", fmt.Errorf("database not initialized"), true},
		{"no such table", fmt.Errorf("no such table: issues"), true},
		{"table not found", fmt.Errorf("table not found: issues"), true},
		{"issue_prefix missing", fmt.Errorf("issue_prefix not configured"), true},
		{"no database", fmt.Errorf("no database found at path"), true},
		{"database not found", fmt.Errorf("database not found"), true},
		{"connection refused", fmt.Errorf("dial tcp: connection refused"), true},
		{"circuit breaker", fmt.Errorf("Dolt circuit breaker is open: server appears down"), true},
		{"server appears down", fmt.Errorf("server appears down"), true},
		{"server down", fmt.Errorf("server down"), true},
		{"server not running", fmt.Errorf("Dolt server is not running"), true},
		{"server may not be running", fmt.Errorf("Dolt server may not be running"), true},
		{"configure custom types", fmt.Errorf("configure custom types in /path: exit 1"), true},
		{"identity mismatch", fmt.Errorf("identity mismatch: local project_id != database project_id"), true},
		{"Unknown database", fmt.Errorf("Unknown database 'gastown'"), true},
		{"generic error", fmt.Errorf("something else failed"), false},
		{"wrapped not initialized", fmt.Errorf("bd create failed: %w", fmt.Errorf("database not initialized")), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDoltConfigError(tt.err); got != tt.want {
				t.Errorf("isDoltConfigError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsDoltOptimisticLockError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"optimistic lock", fmt.Errorf("optimistic lock failed"), true},
		{"serialization failure", fmt.Errorf("serialization failure"), true},
		{"lock wait timeout", fmt.Errorf("lock wait timeout exceeded"), true},
		{"try restarting transaction", fmt.Errorf("try restarting transaction"), true},
		{"database is read only", fmt.Errorf("database is read only"), true},
		{"cannot update manifest", fmt.Errorf("cannot update manifest"), true},
		{"config error", fmt.Errorf("not initialized"), false},
		{"generic error", fmt.Errorf("something else"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDoltOptimisticLockError(tt.err); got != tt.want {
				t.Errorf("isDoltOptimisticLockError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsCurrentHookedIssueForAssignee(t *testing.T) {
	t.Parallel()
	assignee := "testrig/polecats/toast"

	tests := []struct {
		name  string
		issue *beads.Issue
		want  bool
	}{
		{
			name: "nil issue",
			want: false,
		},
		{
			name: "hooked and matching assignee",
			issue: &beads.Issue{
				Status:   beads.StatusHooked,
				Assignee: assignee,
			},
			want: true,
		},
		{
			name: "hooked but different assignee",
			issue: &beads.Issue{
				Status:   beads.StatusHooked,
				Assignee: "testrig/polecats/nux",
			},
			want: false,
		},
		{
			name: "matching assignee but open status",
			issue: &beads.Issue{
				Status:   "open",
				Assignee: assignee,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCurrentHookedIssueForAssignee(tt.issue, assignee); got != tt.want {
				t.Fatalf("isCurrentHookedIssueForAssignee() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildBranchName(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// A git repo for config access, with user.name set.
	w := newWorld()
	w.InitRepo(t, tmpDir)
	if err := w.repo(tmpDir).ConfigSet("user.name", "testuser"); err != nil {
		t.Fatalf("git config: %v", err)
	}

	tests := []struct {
		name     string
		template string
		issue    string
		want     string
	}{
		{
			name:     "default_with_issue",
			template: "", // Empty template = default behavior
			issue:    "gt-123",
			want:     "polecat/alpha/gt-123+", // timestamp suffix varies
		},
		{
			name:     "default_without_issue",
			template: "",
			issue:    "",
			want:     "polecat/alpha-", // timestamp suffix varies
		},
		{
			name:     "custom_template_user_year_month",
			template: "{user}/{year}/{month}/fix",
			issue:    "",
			want:     "testuser/", // year/month will vary
		},
		{
			name:     "custom_template_with_name",
			template: "feature/{name}",
			issue:    "",
			want:     "feature/alpha",
		},
		{
			name:     "custom_template_with_issue",
			template: "work/{issue}",
			issue:    "gt-456",
			want:     "work/456",
		},
		{
			name:     "custom_template_with_timestamp",
			template: "feature/{name}-{timestamp}",
			issue:    "",
			want:     "feature/alpha-", // timestamp suffix varies
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create rig with test template
			r := &rig.Rig{
				Name: "test-rig",
				Path: tmpDir,
			}

			// Set the template in this rig's wisp layer, which outranks the
			// system default. rig.SystemDefaults is process-wide and read by
			// every parallel test, so it is never written here.
			wispCfg := wisp.NewConfig(filepath.Dir(r.Path), r.Name)
			if tt.template != "" {
				if err := wispCfg.Set("polecat_branch_template", tt.template); err != nil {
					t.Fatalf("set wisp template: %v", err)
				}
			} else if err := wispCfg.Unset("polecat_branch_template"); err != nil {
				t.Fatalf("unset wisp template: %v", err)
			}

			m := newTestManager(r, w, nil, newNoDatabaseDB())

			got := m.buildBranchName("alpha", tt.issue)

			// For default templates, just check prefix since timestamp varies
			if tt.template == "" {
				if !strings.HasPrefix(got, tt.want) {
					t.Errorf("buildBranchName() = %q, want prefix %q", got, tt.want)
				}
			} else {
				// For custom templates with time-varying fields, check prefix
				if strings.Contains(tt.template, "{year}") || strings.Contains(tt.template, "{month}") || strings.Contains(tt.template, "{timestamp}") {
					if !strings.HasPrefix(got, tt.want) {
						t.Errorf("buildBranchName() = %q, want prefix %q", got, tt.want)
					}
				} else {
					if got != tt.want {
						t.Errorf("buildBranchName() = %q, want %q", got, tt.want)
					}
				}
			}
		})
	}
}

func TestBuildBranchName_ClaudeActionCompatible(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	w := newWorld()
	w.InitRepo(t, tmpDir)

	r := &rig.Rig{Name: "test-rig", Path: tmpDir}
	m := newTestManager(r, w, nil, newNoDatabaseDB())

	validator := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/_.#+,-]*$`)
	cases := []struct {
		name    string
		polecat string
		issue   string
	}{
		{name: "simple issue", polecat: "mutant", issue: "gt-abc"},
		{name: "dotted subtask", polecat: "raider", issue: "gt-4kp9.5.5.1"},
		{name: "hq prefix", polecat: "pipboy", issue: "hq-571c"},
		{name: "no issue", polecat: "ghoul", issue: ""},
		{name: "long polecat name", polecat: "thunderchief", issue: "gt-jns7.1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := m.buildBranchName(c.polecat, c.issue)
			if strings.Contains(got, "@") {
				t.Fatalf("buildBranchName(%q, %q) = %q contains @", c.polecat, c.issue, got)
			}
			if !validator.MatchString(got) {
				t.Fatalf("buildBranchName(%q, %q) = %q rejected by claude-code-action head-ref validator", c.polecat, c.issue, got)
			}
		})
	}
}

func TestAddWithOptions_NoPrimeMDCreatedLocally(t *testing.T) {
	t.Parallel()
	checkAddWithOptions_NoPrimeMDCreatedLocally(t, fakeAddBeads)
}

// checkAddWithOptions_NoPrimeMDCreatedLocally is TestAddWithOptions_NoPrimeMDCreatedLocally against the fake bd, or against a real bd on the test Dolt container (the
// integration tier, realAddBeads).
func checkAddWithOptions_NoPrimeMDCreatedLocally(t *testing.T, beadsFor addBeads) {
	// This test verifies that ProvisionPrimeMDForWorktree does NOT create
	// a local .beads/PRIME.md in the worktree when there's no tracked one.
	//
	// Bug: If redirect setup fails or ProvisionPrimeMDForWorktree doesn't
	// follow redirects correctly, it may create PRIME.md locally instead
	// of at the rig-level beads location.

	m, _, mayorRig := addRig(t, beadsFor, map[string]string{"README.md": "# Test\n"})
	mayorBeads := filepath.Join(mayorRig, ".beads")

	// Create polecat
	polecat, err := m.AddWithOptions("TestNoLocal", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	// BUG CHECK: The worktree should NOT have a local .beads/PRIME.md
	// ProvisionPrimeMDForWorktree should follow redirect to mayor/rig/.beads
	worktreePrimeMD := filepath.Join(polecat.ClonePath, ".beads", "PRIME.md")
	if _, err := os.Stat(worktreePrimeMD); err == nil {
		t.Errorf("PRIME.md should NOT exist in worktree .beads/ (should be at rig level via redirect): %s", worktreePrimeMD)
	}

	// Verify the redirect file exists
	worktreeRedirect := filepath.Join(polecat.ClonePath, ".beads", "redirect")
	if _, err := os.Stat(worktreeRedirect); os.IsNotExist(err) {
		t.Errorf("redirect file should exist at: %s", worktreeRedirect)
	}

	// Verify PRIME.md was created at mayor/rig/.beads/ (where redirect points)
	mayorPrimeMD := filepath.Join(mayorBeads, "PRIME.md")
	if _, err := os.Stat(mayorPrimeMD); os.IsNotExist(err) {
		t.Errorf("PRIME.md should exist at mayor/rig/.beads/: %s", mayorPrimeMD)
	}
}

func TestAddWithOptions_UsesCanonicalOriginDefaultBranch(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, w := canonicalRig(t)

	mayorGit := w.repo(mayorRig)
	baseSHA, err := mayorGit.Rev("origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}
	staleSHA := createStalePolecatCommit(t, w, mayorRig, "main", "polecat/stale-source")

	polecat, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	worktreeGit := w.repo(polecat.ClonePath)
	staleAncestor, err := worktreeGit.IsAncestor(staleSHA, polecat.Branch)
	if err != nil {
		t.Fatalf("check stale ancestry: %v", err)
	}
	if staleAncestor {
		t.Fatalf("new polecat branch %q unexpectedly includes stale local commit %s", polecat.Branch, staleSHA)
	}

	baseAncestor, err := worktreeGit.IsAncestor(baseSHA, polecat.Branch)
	if err != nil {
		t.Fatalf("check canonical ancestry: %v", err)
	}
	if !baseAncestor {
		t.Fatalf("new polecat branch %q should descend from origin/main commit %s", polecat.Branch, baseSHA)
	}
}

func TestAllocateAndAdd_RunsWispSetupCommand(t *testing.T) {
	t.Parallel()
	mgr, _, _, _ := canonicalRig(t)
	writeWispSetupCommand(t, mgr, "make setup")
	setup := scriptSetup(mgr, writeMarker("setup-marker", "setup"))

	_, polecat, err := mgr.AllocateAndAdd(AddOptions{})
	if err != nil {
		t.Fatalf("AllocateAndAdd: %v", err)
	}
	setup.ranIn(t, polecat.ClonePath, mgr.rig.Path, "make setup")

	data, err := os.ReadFile(filepath.Join(polecat.ClonePath, "setup-marker"))
	if err != nil {
		t.Fatalf("setup command marker was not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "setup" {
		t.Fatalf("setup marker = %q, want setup", got)
	}
}

func TestAddWithOptions_SetupCommandFailureRollsBack(t *testing.T) {
	t.Parallel()
	mgr, _, _, _ := canonicalRig(t)
	writeWispSetupCommand(t, mgr, "make setup")
	scriptSetup(mgr, func(string) error { return errors.New("exit status 7") })

	_, err := mgr.AddWithOptions("toast", AddOptions{})
	if err == nil {
		t.Fatal("AddWithOptions should fail when setup_command fails")
	}
	if !strings.Contains(err.Error(), "setup_command failed") {
		t.Fatalf("error = %q, want setup_command failure", err.Error())
	}

	polecatDir := filepath.Join(mgr.rig.Path, "polecats", "toast")
	if _, statErr := os.Stat(polecatDir); !os.IsNotExist(statErr) {
		t.Fatalf("polecat dir %s still exists after setup_command rollback", polecatDir)
	}
}

func TestReuseIdlePolecat_RunsSetupCommand(t *testing.T) {
	t.Parallel()
	mgr, _, _, added, _ := canonicalWithPolecats(t, true, "toast")
	polecat := added["toast"]
	writeWispSetupCommand(t, mgr, "make setup")
	scriptSetup(mgr, writeMarker("reuse-setup-marker", "setup"))

	reused, err := mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next"})
	if err != nil {
		t.Fatalf("ReuseIdlePolecat: %v", err)
	}
	if reused.ClonePath != polecat.ClonePath {
		t.Fatalf("reused clone path = %q, want %q", reused.ClonePath, polecat.ClonePath)
	}

	data, err := os.ReadFile(filepath.Join(reused.ClonePath, "reuse-setup-marker"))
	if err != nil {
		t.Fatalf("reuse setup command marker was not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "setup" {
		t.Fatalf("reuse setup marker = %q, want setup", got)
	}
}

// TestFindIdlePolecat_AcceptsDoneCandidateWithZeroIdle is a regression test
// for gt-uu6 exercised through the real allocator entrypoint (not just the
// IsReuseEligible predicate in isolation, see TestStateIsReuseEligible).
// No code path promotes a completed polecat from done to idle (gt-iljx) — a
// clean done polecat with no session IS the normal resting state. FindIdlePolecat
// must be able to hand it out, or every rig freezes at its polecat directory
// cap the moment agent_state=done accumulates and nothing is ever idle.
func TestFindIdlePolecat_AcceptsDoneCandidateWithZeroIdle(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")

	// From here on 'show' reports agent_state=done instead of idle, with
	// the same clean facts (no hook, no active MR) otherwise.
	bd.setAgent(t, "gt-rig-polecat-toast", func(f *beads.AgentFields) { f.AgentState = "done" })

	found, err := mgr.FindIdlePolecat()
	if err != nil {
		t.Fatalf("FindIdlePolecat: %v", err)
	}
	if found == nil {
		t.Fatal("FindIdlePolecat returned nil with a clean StateDone candidate and zero idle polecats — gt-uu6 regressed")
	}
	if found.Name != "toast" {
		t.Fatalf("found.Name = %q, want toast", found.Name)
	}
	if found.State != StateDone {
		t.Fatalf("found.State = %q, want %q", found.State, StateDone)
	}
}

func TestReuseIdlePolecat_SetupCommandFailureCleansWorktree(t *testing.T) {
	t.Parallel()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "toast")
	writeWispSetupCommand(t, mgr, "make setup")
	scriptSetup(mgr, func(dir string) error {
		if err := writeMarker("dirty-setup-marker", "dirty")(dir); err != nil {
			return err
		}
		return errors.New("exit status 7")
	})

	_, err := mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next"})
	if err == nil {
		t.Fatal("ReuseIdlePolecat should fail when setup_command fails")
	}
	if !strings.Contains(err.Error(), "setup_command failed") {
		t.Fatalf("error = %q, want setup_command failure", err.Error())
	}

	dirtyPath := filepath.Join(mgr.clonePath("toast"), "dirty-setup-marker")
	if _, statErr := os.Stat(dirtyPath); !os.IsNotExist(statErr) {
		t.Fatalf("dirty setup marker %s still exists after setup_command cleanup", dirtyPath)
	}
}

func writeWispSetupCommand(t *testing.T, mgr *Manager, command string) {
	t.Helper()

	townRoot := filepath.Dir(mgr.rig.Path)
	wispDir := filepath.Join(townRoot, ".beads-wisp", "config")
	if err := os.MkdirAll(wispDir, 0755); err != nil {
		t.Fatalf("mkdir wisp config: %v", err)
	}
	cfg := fmt.Sprintf(`{"rig":"%s","values":{"setup_command":%q},"blocked":[]}`, mgr.rig.Name, command)
	if err := os.WriteFile(filepath.Join(wispDir, mgr.rig.Name+".json"), []byte(cfg), 0644); err != nil {
		t.Fatalf("write wisp config: %v", err)
	}
}

// setupScript answers setup_command in place of the shell: it records each
// call and runs effect in the directory the command would have run in.
type setupScript struct {
	mu     sync.Mutex
	calls  []setupCall
	effect func(dir string) error
}

type setupCall struct {
	dir  string
	env  []string
	argv []string
}

// scriptSetup makes mgr run setup_command through a new setupScript.
func scriptSetup(mgr *Manager, effect func(dir string) error) *setupScript {
	s := &setupScript{effect: effect}
	mgr.runSetup = func(_ context.Context, dir string, env []string, name string, args ...string) error {
		s.mu.Lock()
		s.calls = append(s.calls, setupCall{dir: dir, env: env, argv: append([]string{name}, args...)})
		s.mu.Unlock()
		return s.effect(dir)
	}
	return s
}

// ranIn fails t unless setup ran once, in worktree, as the shell running
// command, with the worktree and rig paths in its environment.
func (s *setupScript) ranIn(t *testing.T, worktree, rigPath, command string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 {
		t.Fatalf("setup_command ran %d times, want once: %+v", len(s.calls), s.calls)
	}
	c := s.calls[0]
	if c.dir != worktree || c.argv[len(c.argv)-1] != command {
		t.Errorf("setup ran %q in %s, want %q in %s", c.argv, c.dir, command, worktree)
	}
	if !slices.Contains(c.env, "GT_WORKTREE_PATH="+worktree) || !slices.Contains(c.env, "GT_RIG_PATH="+rigPath) {
		t.Errorf("setup env = %q, want GT_WORKTREE_PATH and GT_RIG_PATH", c.env)
	}
}

// writeMarker is a setup effect that writes body to name in the worktree.
func writeMarker(name, body string) func(dir string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
}

// A setup_command that outlives its bound is reported as a timeout, not as
// the killed shell's exit status.
func TestRunSetupCommandReportsATimeout(t *testing.T) {
	t.Parallel()
	mgr, worktree, _, _ := canonicalRig(t)
	writeWispSetupCommand(t, mgr, "make setup")
	mgr.setupTimeout = time.Millisecond
	mgr.runSetup = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) error {
		<-ctx.Done()
		return errors.New("signal: killed")
	}
	if err := mgr.runSetupCommand(worktree); err == nil || !strings.Contains(err.Error(), "setup_command timed out after 1ms") {
		t.Fatalf("runSetupCommand = %v, want a timeout", err)
	}
}

// setupShellCommand runs setup_command through $SHELL -c, /bin/sh when unset.
func TestSetupShellCommand(t *testing.T) {
	t.Parallel()
	shell, args := setupShellCommand("make setup")
	if len(args) != 2 || args[0] != "-c" || args[1] != "make setup" || shell == "" {
		t.Fatalf("setupShellCommand = %q %q", shell, args)
	}
}

// TestWorkstateDispositionForPolecat_MissingCleanupStatusClearsOnVerifiedLiveGit
// covers the gt-7kr -> gt-ui2x evolution of this exact scenario.
//
// gt-7kr found check-recovery returning SAFE_TO_NUKE for a missing
// cleanup_status off of an ad hoc, standalone local git check that said
// nothing about a live session or a still-hooked bead. The fix made
// missing/unknown fail closed unconditionally (RecordedCleanupBlocks).
//
// gt-ui2x adds one narrow way out of that unconditional block, in
// ResolveIgnoreCleanupStatus rather than in RecordedCleanupBlocks itself: an
// agent bead that WAS successfully read (agentBeadRead — so hook_bead,
// push_failed, mr_failed and active_mr below are verified facts, not unread
// defaults) with a live-clean probe (liveGitProbeRan, gitSafe) and safe
// hook/active-MR facts. See
// TestWorkstateDispositionForPolecat_UnreadableAgentBeadStillFailsClosed for
// the case that must NOT take this path: an unreadable bead leaves those
// same facts unverified, so it keeps failing closed regardless of git state.
func TestWorkstateDispositionForPolecat_MissingCleanupStatusClearsOnVerifiedLiveGit(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")

	// Swap to a mock bd that omits cleanup_status entirely while everything
	// else (agent_state, hook_bead) still reads as idle/unhooked, and the
	// worktree itself is locally clean, verified by workstateInputForPolecat's
	// own live probe (GitStateSourceLive) — not a bypass of it.
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.CleanupStatus = "" })

	d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")
	if d.Verdict != WorkstateVerdictSafeToNuke {
		t.Fatalf("WorkstateDispositionForPolecat() verdict = %s (reason=%s blockers=%v), want %s — a verified-clean live probe plus safe hook/MR must clear a missing cleanup_status on a bead that was actually read",
			d.Verdict, d.Reason, d.Blockers, WorkstateVerdictSafeToNuke)
	}
	if !d.Reusable || !d.SafeToNuke {
		t.Fatalf("WorkstateDispositionForPolecat() Reusable=%v SafeToNuke=%v, want both true", d.Reusable, d.SafeToNuke)
	}
}

// TestWorkstateDispositionForPolecat_UnreadableAgentBeadStillFailsClosed is
// the gt-14a regression test: an agent bead that could not be read at all
// (GetAgentBead returns not-found as (nil, nil, nil), same as a lookup
// error) must keep failing closed, even with a locally clean worktree.
// Unlike the successfully-read case above, hook_bead, push_failed, mr_failed
// and active_mr are all unverified here — there is nothing for
// workstateInputForPolecat to have measured them from — so
// ResolveIgnoreCleanupStatus's agentBeadRead precondition must not be met,
// and the missing/unknown CleanupUnknown default set at the top of
// workstateInputForPolecat must keep blocking.
func TestWorkstateDispositionForPolecat_UnreadableAgentBeadStillFailsClosed(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")

	// The bead reads as not found, so GetAgentBead resolves to (nil, nil,
	// nil) — exactly the not-found shape workstateInputForPolecat must not
	// treat as "safe to ignore the missing cleanup_status".
	bd.forgetAgent(mgr.agentBeadID("toast"))

	d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")
	if d.Verdict != WorkstateVerdictNeedsRecovery {
		t.Fatalf("WorkstateDispositionForPolecat() verdict = %s (reason=%s blockers=%v), want %s — an unreadable agent bead must fail closed even when local git looks clean",
			d.Verdict, d.Reason, d.Blockers, WorkstateVerdictNeedsRecovery)
	}
	if d.Reusable || d.SafeToNuke {
		t.Fatalf("WorkstateDispositionForPolecat() Reusable=%v SafeToNuke=%v, want both false", d.Reusable, d.SafeToNuke)
	}
}

// TestWorkstateDispositionForPolecat_StashScopedToOwningSeat is the gt-oznd3
// regression test. Polecat worktrees share a single .repo.git, so git stores
// stashes in ONE ref (refs/stash) rather than per-worktree: `git stash list`
// run from any seat returns every seat's stash entries. workstateInputForPolecat
// reads StashCount through git.Git.StashCount, which filters stash entries by
// the branch recorded in each entry's "WIP on <branch>:"/"On <branch>:" text —
// so a stash taken on seat A's branch must disqualify only seat A, never seat
// B, even though both worktrees see the identical raw `git stash list` output.
func TestWorkstateDispositionForPolecat_StashScopedToOwningSeat(t *testing.T) {
	t.Parallel()
	mgr, _, bd, added, w := canonicalWithPolecats(t, false, "seata")
	seatA := added["seata"]
	seatB, err := mgr.AddWithOptions("seatb", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions(seatb): %v", err)
	}
	bd.settleAgent(t, mgr.agentBeadID("seatb"), seatB.Branch)
	_ = w.repo(seatA.ClonePath).CleanForce()

	// Stash uncommitted work on seat A's branch only. Both worktrees share the
	// same underlying repo, so this stash lands in the one shared refs/stash.
	if err := os.WriteFile(filepath.Join(seatA.ClonePath, "README.md"), []byte("seata wip\n"), 0644); err != nil {
		t.Fatalf("write seata dirt: %v", err)
	}
	w.Stash(t, seatA.ClonePath, "seata-wip")

	dA := mgr.WorkstateDispositionForPolecat("seata", StateIdle, "")
	if dA.Reason != "git-stash" {
		t.Fatalf("seata WorkstateDispositionForPolecat() reason = %q (verdict=%s blockers=%v), want %q — a seat with its own stash must still block",
			dA.Reason, dA.Verdict, dA.Blockers, "git-stash")
	}
	if dA.Reusable || dA.SafeToNuke {
		t.Fatalf("seata Reusable=%v SafeToNuke=%v, want both false — its own stash must block reuse", dA.Reusable, dA.SafeToNuke)
	}

	dB := mgr.WorkstateDispositionForPolecat("seatb", StateIdle, "")
	if dB.Verdict != WorkstateVerdictSafeToNuke {
		t.Fatalf("seatb WorkstateDispositionForPolecat() verdict = %s (reason=%s blockers=%v), want %s — seat A's stash must not disqualify seat B",
			dB.Verdict, dB.Reason, dB.Blockers, WorkstateVerdictSafeToNuke)
	}
	if !dB.Reusable || !dB.SafeToNuke {
		t.Fatalf("seatb Reusable=%v SafeToNuke=%v, want both true — sibling seats must remain reusable", dB.Reusable, dB.SafeToNuke)
	}
}

func TestReuseIdlePolecat_UsesCanonicalOriginDefaultBranch(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, w := canonicalRig(t)

	mayorGit := w.repo(mayorRig)
	baseSHA, err := mayorGit.Rev("origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}

	polecat, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	staleSHA := createStalePolecatCommit(t, w, polecat.ClonePath, "HEAD", "polecat/toast-stale")

	_, err = mgr.ReuseIdlePolecat("toast", AddOptions{HookBead: "gt-next"})
	if !errors.Is(err, ErrPolecatNeedsRecovery) {
		t.Fatalf("ReuseIdlePolecat error = %v, want ErrPolecatNeedsRecovery", err)
	}
	worktreeGit := w.repo(polecat.ClonePath)
	currentSHA, err := worktreeGit.Rev("HEAD")
	if err != nil {
		t.Fatalf("resolve current HEAD: %v", err)
	}
	if currentSHA != staleSHA {
		t.Fatalf("reuse should preserve stale local commit %s, got HEAD %s", staleSHA, currentSHA)
	}
	if baseSHA == "" {
		t.Fatal("base SHA unexpectedly empty")
	}
}

// TestAddWithOptions_ResumeBranch verifies gh#3602: when ResumeBranch is set,
// AddWithOptions checks out the named existing branch instead of creating a
// fresh polecat/<name>/<bead>+<ts> branch. This lets `gt sling --branch/--pr`
// resume work on an existing PR branch without creating duplicates.
func TestAddWithOptions_ResumeBranch(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, _, w := canonicalRig(t)

	// Create a "PR branch" with a marker commit, mimicking an existing open PR a
	// polecat needs to resume. It is created as a ref alone: leaving it checked
	// out in the rig's own clone would make this a second test, of the refusal to
	// attach another worktree to a live ref (gt-0kk2).
	prBranch := "polecat/example/gh-1234@abcdef"
	prCommit := w.branchAtNewCommit(t, mayorRig, prBranch, w.rev(t, mayorRig, "main"), "PR work (gh-1234)")
	w.SetRef(t, mayorRig, "refs/remotes/origin/"+prBranch, prCommit)

	polecat, err := mgr.AddWithOptions("toast", AddOptions{ResumeBranch: prBranch})
	if err != nil {
		t.Fatalf("AddWithOptions with ResumeBranch: %v", err)
	}

	if polecat.Branch != prBranch {
		t.Fatalf("polecat.Branch = %q, want %q (ResumeBranch should override fresh-branch naming)", polecat.Branch, prBranch)
	}

	worktreeGit := w.repo(polecat.ClonePath)
	current, err := worktreeGit.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if current != prBranch {
		t.Fatalf("worktree HEAD on branch %q, want %q", current, prBranch)
	}

	// The PR commit must be reachable from HEAD — proves we attached to the
	// existing branch rather than starting fresh from main.
	reachable, err := worktreeGit.IsAncestor(prCommit, "HEAD")
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !reachable {
		t.Fatalf("PR commit %s should be reachable from HEAD on resumed branch", prCommit)
	}
}

func TestAddWithOptions_NoFilesAddedToRepo(t *testing.T) {
	t.Parallel()
	checkAddWithOptions_NoFilesAddedToRepo(t, fakeAddBeads)
}

// checkAddWithOptions_NoFilesAddedToRepo is TestAddWithOptions_NoFilesAddedToRepo against the fake bd, or against a real bd on the test Dolt container (the
// integration tier, realAddBeads).
func checkAddWithOptions_NoFilesAddedToRepo(t *testing.T, beadsFor addBeads) {
	// This test verifies the invariant that polecat creation does NOT add any
	// TRACKED files to the repo's directory structure. The user's code should stay pure.
	//
	// After polecat install, `git status` in the worktree should show no
	// untracked files and no modifications. Settings are installed at the shared
	// polecats/.claude/settings.json directory (outside worktrees), so they
	// never appear in any worktree's git status.

	// A clean repo with known files only: .gitignore with .claude/ (Claude
	// Code local state) and .beads/ (the redirect), and no .beads, .claude or
	// CLAUDE.md.
	m, w, mayorRig := addRig(t, beadsFor, map[string]string{
		".gitignore":  ".claude/\n.beads/\n",
		"README.md":   "# Clean Repo\n",
		"src/main.go": "package main\n",
	})

	// Create AGENTS.md in mayor/rig AFTER git commit (NOT tracked in git)
	// This triggers the fallback copy during polecat install
	agentsMDPath := filepath.Join(mayorRig, "AGENTS.md")
	if err := os.WriteFile(agentsMDPath, []byte("# AGENTS\n\nFallback content.\n"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	// Create polecat
	polecat, err := m.AddWithOptions("TestClean", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	// Run git status in worktree - should show nothing except .beads/ (infrastructure)
	// Settings are at polecats/.claude/settings.json (outside worktree) so won't appear
	st, err := w.repo(polecat.ClonePath).CheckUncommittedWork()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}

	// Filter out expected infrastructure files
	var unexpected []string
	for _, line := range append(append([]string{}, st.ModifiedFiles...), st.UntrackedFiles...) {
		// .beads/ is expected - it contains the redirect file for shared beads
		if strings.Contains(line, ".beads") {
			continue
		}
		// CLAUDE.md is expected - provisioned by CreatePolecatCLAUDEmd for gt done instructions
		if strings.Contains(line, "CLAUDE.md") {
			continue
		}
		unexpected = append(unexpected, line)
	}
	if len(unexpected) > 0 {
		t.Errorf("polecat worktree should be clean after install (no files added to repo), but git status shows:\n%s", strings.Join(unexpected, "\n"))
	}
}

func TestAddWithOptions_SettingsInstalledInPolecatsDir(t *testing.T) {
	t.Parallel()
	checkAddWithOptions_SettingsInstalledInPolecatsDir(t, fakeAddBeads)
}

// checkAddWithOptions_SettingsInstalledInPolecatsDir is TestAddWithOptions_SettingsInstalledInPolecatsDir against the fake bd, or against a real bd on the test Dolt container (the
// integration tier, realAddBeads).
func checkAddWithOptions_SettingsInstalledInPolecatsDir(t *testing.T, beadsFor addBeads) {
	// This test verifies that polecat creation installs .claude/settings.json
	// in the SHARED polecats/ parent directory (not inside individual worktrees).
	// Claude Code with --settings supports parent directory settings, and placing
	// them at the polecats/ level avoids polluting individual worktree repos.

	m, _, _ := addRig(t, beadsFor, map[string]string{"README.md": "# Test Repo\n"})

	// Create polecat
	polecat, err := m.AddWithOptions("TestSettings", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}

	// Verify settings.json exists in the SHARED polecats/ parent directory
	// polecats dir is the parent of polecat.ClonePath's parent (ClonePath = polecats/<name>/<rig>)
	polecatsDir := filepath.Dir(filepath.Dir(polecat.ClonePath))
	settingsPath := filepath.Join(polecatsDir, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Errorf("settings.json should exist at %s (shared polecats dir) for Claude Code to find hooks", settingsPath)
	}

	// Verify settings.json does NOT exist inside the worktree (no longer installed there)
	worktreeSettingsPath := filepath.Join(polecat.ClonePath, ".claude", "settings.json")
	if _, err := os.Stat(worktreeSettingsPath); err == nil {
		t.Errorf("settings.json should NOT exist inside worktree at %s (settings are now in shared polecats dir)", worktreeSettingsPath)
	}
}

// TestOverflowNameSessionFormat verifies that overflow names don't create double-prefix.
// Regression test for the double-prefix bug (tr-testrig-N instead of tr-N).
func TestOverflowNameSessionFormat(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create minimal rig
	rigPath := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{
		Name: "testrig",
		Path: rigPath,
	}

	// Create name pool with small size to trigger overflow quickly
	pool := NewNamePoolWithConfig(rigPath, "testrig", "mad-max", nil, 2)
	mgr := &Manager{
		rig:      r,
		namePool: pool,
	}

	// Allocate all themed names
	_, _ = mgr.namePool.Allocate() // furiosa
	_, _ = mgr.namePool.Allocate() // nux

	// Next allocation should be overflow (just a number)
	overflowName, err := mgr.namePool.Allocate()
	if err != nil {
		t.Fatalf("overflow allocation failed: %v", err)
	}

	// Overflow name should be just "3", not "testrig-3"
	if overflowName != "3" {
		t.Errorf("expected overflow name '3', got %s", overflowName)
	}

	// Create session manager
	prefixes := session.NewPrefixRegistry()
	prefixes.Register("tr", "testrig")
	sessMgr := &SessionManager{rig: r, prefixes: prefixes}
	sessionName := sessMgr.SessionName(overflowName)

	// Verify session name is tr-3, NOT tr-testrig-3
	expected := "tr-3"
	if sessionName != expected {
		t.Errorf("expected session name %s, got %s (double-prefix bug!)", expected, sessionName)
	}

	// Verify no double-prefix
	if strings.Contains(sessionName, "testrig-testrig") {
		t.Errorf("double-prefix detected in session name: %s", sessionName)
	}
}

// TestPendingMarkerBlocksReallocation verifies that a .pending reservation file
// written by AllocateName prevents a concurrent reconcile from treating the name
// as available (the TOCTOU fix: hq-ypvza / gt-601kx).
func TestPendingMarkerBlocksReallocation(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "pending-marker-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Use "myrig" which hashes to mad-max theme (furiosa is first name)
	r := &rig.Rig{
		Name: "myrig",
		Path: tmpDir,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	// Simulate AllocateName: create polecats/ dir and write a .pending marker
	// for "furiosa" (as if AllocateName ran but AddWithOptions hasn't yet).
	polecatsDir := filepath.Join(tmpDir, "polecats")
	if err := os.MkdirAll(polecatsDir, 0755); err != nil {
		t.Fatal(err)
	}
	pendingPath := m.pendingPath("furiosa")
	if err := os.WriteFile(pendingPath, []byte("999"), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate a concurrent reconcile (no directories exist, only the marker).
	// reconcilePoolInternal should treat "furiosa" as in-use via the marker.
	m.reconcilePoolInternal()

	// Now allocate — should NOT get furiosa (it's reserved by .pending).
	name, err := m.namePool.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if name == "furiosa" {
		t.Errorf("allocated furiosa despite active .pending marker — TOCTOU race not fixed")
	}
}

// TestStalePendingMarkerIsCleanedUp verifies that cleanupOrphanPolecatState
// removes .pending files older than pendingMaxAge.
func TestStalePendingMarkerIsCleanedUp(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "stale-pending-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	r := &rig.Rig{
		Name: "myrig",
		Path: tmpDir,
	}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	polecatsDir := filepath.Join(tmpDir, "polecats")
	if err := os.MkdirAll(polecatsDir, 0755); err != nil {
		t.Fatal(err)
	}

	pendingPath := m.pendingPath("furiosa")
	if err := os.WriteFile(pendingPath, []byte("999"), 0644); err != nil {
		t.Fatal(err)
	}

	// Backdate the file to simulate a stale marker (older than pendingMaxAge).
	staleTime := time.Now().Add(-(pendingMaxAge + time.Minute))
	if err := os.Chtimes(pendingPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	// cleanupOrphanPolecatState should remove stale markers.
	m.cleanupOrphanPolecatState()

	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Errorf("stale .pending file was not cleaned up by cleanupOrphanPolecatState")
	}
}

func TestCleanupOrphanPolecatStatePreservesUnverifiedBrokenPolecat(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	r := &rig.Rig{Name: "myrig", Path: tmpDir}
	m := newTestManager(r, nil, nil, newNoDatabaseDB())

	polecatDir := filepath.Join(tmpDir, "polecats", "furiosa")
	clonePath := filepath.Join(polecatDir, r.Name)
	if err := os.MkdirAll(clonePath, 0755); err != nil {
		t.Fatal(err)
	}

	m.cleanupOrphanPolecatState()

	if _, err := os.Stat(polecatDir); err != nil {
		t.Fatalf("broken named polecat dir was removed without safety proof: %v", err)
	}
}

func TestCleanupOrphanPolecatStatePreservesOldLayoutWorktree(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	r := &rig.Rig{Name: "myrig", Path: tmpDir}
	m := newTestManager(r, nil, newFakeProbe(), newPolecatDB())

	polecatDir := filepath.Join(tmpDir, "polecats", "furiosa")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(polecatDir, ".git"), []byte("gitdir: /tmp/nonexistent-for-layout-test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(polecatDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("old layout worktree\n"), 0644); err != nil {
		t.Fatal(err)
	}

	m.cleanupOrphanPolecatState()

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("old-layout worktree was removed by orphan cleanup: %v", err)
	}
}

func TestReclaimBrokenIdlePolecatRemovesCleanStructuralFailure(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _ := canonicalRig(t)
	mgr.tmux = newFakeProbe() // no session: the proof that no polecat is live

	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	bd.settleAgent(t, mgr.agentBeadID("toast"), p.Branch)
	if err := os.Remove(filepath.Join(p.ClonePath, ".git")); err != nil {
		t.Fatalf("break worktree .git: %v", err)
	}

	if err := mgr.ReclaimBrokenIdlePolecat("toast"); err != nil {
		t.Fatalf("ReclaimBrokenIdlePolecat: %v", err)
	}
	if _, err := os.Stat(mgr.polecatDir("toast")); !os.IsNotExist(err) {
		t.Fatalf("polecat dir still exists after reclaim, stat err=%v", err)
	}
}

// TestReclaimBrokenIdlePolecatMissingCleanupStatusReclaimable is the gt-2h6
// regression test. A polecat whose worktree directory is gone (not merely
// its .git file) can never self-report cleanup_status again — there is
// nothing left to check — so a missing status must not permanently strand
// the slot the way it did for peridot: "Reclaiming broken idle polecat
// peridot before allocation... was not safe to reclaim: cleanup_status=".
func TestReclaimBrokenIdlePolecatMissingCleanupStatusReclaimable(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _ := canonicalRig(t)
	mgr.tmux = newFakeProbe() // no session: the proof that no polecat is live

	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	// Remove the whole worktree directory, not just .git — peridot's
	// worktree directory itself was gone, which is what leaves
	// cleanup_status permanently unobservable.
	if err := os.RemoveAll(p.ClonePath); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}

	// Swap to a bd that omits cleanup_status entirely, matching a
	// polecat that never ran a fresh cleanup check against the (now gone)
	// worktree.
	bd.settleAgent(t, mgr.agentBeadID("toast"), p.Branch)
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.CleanupStatus = "" })

	if err := mgr.ReclaimBrokenIdlePolecat("toast"); err != nil {
		t.Fatalf("ReclaimBrokenIdlePolecat: %v, want success — a gone worktree with no branch/MR/active work at risk must be reclaimable despite a missing cleanup_status", err)
	}
	if _, err := os.Stat(mgr.polecatDir("toast")); !os.IsNotExist(err) {
		t.Fatalf("polecat dir still exists after reclaim, stat err=%v", err)
	}
}

func TestReclaimBrokenIdlePolecatFailsClosedWithoutSessionEvidence(t *testing.T) {
	t.Parallel()
	mgr, _, _, added, _ := canonicalWithPolecats(t, false, "toast")
	p := added["toast"]
	if err := os.Remove(filepath.Join(p.ClonePath, ".git")); err != nil {
		t.Fatalf("break worktree .git: %v", err)
	}

	err := mgr.ReclaimBrokenIdlePolecat("toast")
	if err == nil || !strings.Contains(err.Error(), "session_state=unverified") {
		t.Fatalf("ReclaimBrokenIdlePolecat error = %v, want session evidence blocker", err)
	}
	if _, statErr := os.Stat(mgr.polecatDir("toast")); statErr != nil {
		t.Fatalf("polecat dir should be preserved after blocked reclaim: %v", statErr)
	}
}

// TestAddWithOptions_RollbackReleasesName verifies that when AddWithOptions fails,
// the allocated name is released back to the pool and the polecat directory is cleaned up.
// Regression test for gt-2vs22: cleanupOnError previously only removed the directory,
// leaking pool names on spawn failure.
func TestAddWithOptions_RollbackReleasesName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create mayor/rig directory structure (acts as repo base)
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// A repo with one commit whose origin is a path with no repository, so
	// the fetch fails and origin/main is never created. This causes
	// AddWithOptions to fail at ref validation, testing rollback.
	w := newWorld()
	w.InitRepo(t, mayorRig)
	w.Commit(t, mayorRig, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, mayorRig)
	w.AddRemote(t, mayorRig, "origin", "/nonexistent/repo")

	r := &rig.Rig{
		Name: "rig",
		Path: root,
	}
	m := newTestManager(r, w, nil, newNoDatabaseDB())

	// Allocate a name (simulates what gt sling does before AddWithOptions)
	name, err := m.AllocateName()
	if err != nil {
		t.Fatalf("AllocateName: %v", err)
	}

	// Verify name is active in pool after allocation
	activeBeforeAdd := m.namePool.ActiveCount()
	if activeBeforeAdd == 0 {
		t.Fatal("expected at least 1 active name after AllocateName")
	}

	// Try to create polecat — should fail because origin/main doesn't exist
	_, err = m.AddWithOptions(name, AddOptions{})
	if err == nil {
		t.Fatal("AddWithOptions should have failed without origin/main ref")
	}

	// Verify name was released back to pool (gt-2vs22 fix)
	activeNames := m.namePool.ActiveNames()
	for _, n := range activeNames {
		if n == name {
			t.Errorf("name %q still active in pool after failed AddWithOptions — rollback didn't release it", name)
		}
	}

	// Verify polecat directory was cleaned up
	polecatDir := m.polecatDir(name)
	if _, err := os.Stat(polecatDir); !os.IsNotExist(err) {
		t.Errorf("polecat directory %s still exists after failed AddWithOptions", polecatDir)
	}

	// Verify pending marker was cleaned up
	pendingPath := m.pendingPath(name)
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Errorf("pending marker %s still exists after failed AddWithOptions", pendingPath)
	}
}

// TestAddWithOptions_RollbackCleansWorktree verifies that when AddWithOptions fails
// AFTER the worktree is created (e.g., agent bead creation fails), the worktree
// registration is cleaned up along with the directory and pool name.
// Regression test for gt-2vs22.
func TestAddWithOptions_RollbackCleansWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create mayor/rig directory structure
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// A repo with a commit and origin/main, so worktree creation succeeds.
	w := newWorld()
	w.InitRepo(t, mayorRig)
	head := w.Commit(t, mayorRig, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, mayorRig)
	w.AddRemote(t, mayorRig, "origin", mayorRig)
	w.SetRef(t, mayorRig, "refs/remotes/origin/main", head)

	// A store that FAILS on agent bead creation.
	bd := newPolecatDB()
	bd.createErr = errors.New("error: database not initialized")

	// Create rig-level .beads directory
	rigBeads := filepath.Join(root, ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	mayorBeads := filepath.Join(mayorRig, ".beads")
	if err := os.MkdirAll(mayorBeads, 0755); err != nil {
		t.Fatalf("mkdir mayor .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeads, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	// Write type-config sentinel so EnsureCustomTypes is a no-op
	_ = os.WriteFile(filepath.Join(mayorBeads, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644)

	r := &rig.Rig{
		Name: "rig",
		Path: root,
	}
	m := newTestManager(r, w, nil, bd)

	// Allocate a name
	name, err := m.AllocateName()
	if err != nil {
		t.Fatalf("AllocateName: %v", err)
	}

	// AddWithOptions should fail at agent bead creation (mock bd fails on create)
	_, err = m.AddWithOptions(name, AddOptions{})
	if err == nil {
		t.Fatal("AddWithOptions should have failed with store failing on create")
	}

	// Verify name was released back to pool
	activeNames := m.namePool.ActiveNames()
	for _, n := range activeNames {
		if n == name {
			t.Errorf("name %q still active in pool after failed AddWithOptions — rollback didn't release it", name)
		}
	}

	// Verify polecat directory was cleaned up
	polecatDir := m.polecatDir(name)
	if _, err := os.Stat(polecatDir); !os.IsNotExist(err) {
		t.Errorf("polecat directory %s still exists after rollback", polecatDir)
	}

	// Verify worktree registration was cleaned up from git.
	// The branch ref may remain (cleaned later by CleanupStaleBranches),
	// but the worktree entry should be removed so git doesn't track a stale path.
	clonePath := filepath.Join(polecatDir, r.Name)
	list, err := w.repo(mayorRig).WorktreeList()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	for _, wt := range list {
		if wt.Path == clonePath {
			t.Errorf("stale worktree entry for %s still registered in git after rollback", clonePath)
		}
	}
}

// TestManagerAgentLifecycleUsesRigLocalBeadsDir verifies that the polecat
// manager's agent-bead lifecycle (create on spawn, reset on nuke) operates on
// the RIG-LOCAL database — the canonical home of rig agent beads (gt-8we).
func TestManagerAgentLifecycleUsesRigLocalBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigName := "gastown"
	rigPath := filepath.Join(townRoot, rigName)
	mayorRig := filepath.Join(rigPath, "mayor", "rig")
	townBeadsDir := filepath.Join(townRoot, ".beads")
	rigBeadsDir := filepath.Join(mayorRig, ".beads")

	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		townBeadsDir,
		rigBeadsDir,
		filepath.Join(rigPath, ".beads"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "gt-", Path: filepath.Join(rigName, "mayor", "rig")},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	for _, dir := range []string{townBeadsDir, rigBeadsDir} {
		if err := os.WriteFile(filepath.Join(dir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
			t.Fatalf("write types sentinel: %v", err)
		}
	}

	bd := newPolecatDB()
	agentID := "gt-gastown-polecat-rust"

	m := newTestManager(&rig.Rig{Name: rigName, Path: rigPath}, nil, nil, bd)
	if err := m.createAgentBeadWithRetry(agentID, &beads.AgentFields{RoleType: "polecat", Rig: rigName, AgentState: "spawning", HookBead: "gt-work"}); err != nil {
		t.Fatalf("createAgentBeadWithRetry: %v", err)
	}
	if err := m.resetAgentBeadForReuse(agentID, "test reset"); err != nil {
		t.Fatalf("resetAgentBeadForReuse: %v", err)
	}

	// Agent beads are rig-local (gt-8we): the store was opened on the rig's
	// own database, and the lifecycle reached it.
	want := beadsSite{workDir: mayorRig, beadsDir: rigBeadsDir}
	if sites := bd.openedSites(); len(sites) != 1 || sites[0] != want {
		t.Fatalf("store opened at %+v, want exactly %+v", sites, want)
	}
	_, fields, err := beads.GetAgentBead(bd, agentID)
	if err != nil {
		t.Fatalf("GetAgentBead after create and reset: %v", err)
	}
	if fields.AgentState != string(beads.AgentStateNuked) || fields.HookBead != "" {
		t.Fatalf("agent bead after reset = state %q hook %q, want nuked and no hook", fields.AgentState, fields.HookBead)
	}
}

// TestAllocateAndAdd_NoDuplicateNames verifies that concurrent AllocateAndAdd
// calls never produce duplicate polecat names (GH#2215). Each goroutine will
// fail at worktree creation (no origin/main), but the allocated names must
// all be unique — the race condition would show as duplicate names.
func TestAllocateAndAdd_NoDuplicateNames(t *testing.T) {
	t.Parallel()
	const concurrency = 20
	root := t.TempDir()

	// Create mayor/rig directory structure
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	// A repo whose origin is not a repository (every fetch fails, as
	// expected).
	w := newWorld()
	w.InitRepo(t, mayorRig)
	w.Commit(t, mayorRig, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, mayorRig)
	w.AddRemote(t, mayorRig, "origin", "/nonexistent/repo")

	r := &rig.Rig{
		Name: "rig",
		Path: root,
	}
	m := newTestManager(r, w, nil, newNoDatabaseDB())

	// Launch concurrent AllocateAndAdd calls. They will fail at worktree
	// creation (no origin/main), but the names they attempt must be unique.
	type result struct {
		name string
		err  error
	}
	results := make(chan result, concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			name, _, err := m.AllocateAndAdd(AddOptions{})
			results <- result{name: name, err: err}
		}()
	}

	// Collect results
	seen := make(map[string]int)
	for i := 0; i < concurrency; i++ {
		r := <-results
		if r.name != "" {
			seen[r.name]++
		}
	}

	// Verify no duplicate names
	for name, count := range seen {
		if count > 1 {
			t.Errorf("name %q allocated %d times — race condition (GH#2215)", name, count)
		}
	}
}

// TestAllocateAndAdd_LeavesPolecatCreatedOutsidePool guards gt-dziey: the name
// the pool draws can already be a polecat directory created outside the pool
// (gt polecat add → AddWithOptions, which takes only the per-polecat lock).
// MkdirAll succeeds on that existing directory, which hides the collision, and
// the rollback path then RemoveAll's it — deleting the other polecat.
func TestAllocateAndAdd_LeavesPolecatCreatedOutsidePool(t *testing.T) {
	t.Parallel()
	mgr, _, _, _ := canonicalRig(t)

	// The name the pool would hand out next.
	taken, err := mgr.AllocateName()
	if err != nil {
		t.Fatalf("AllocateName: %v", err)
	}
	_ = os.Remove(mgr.pendingPath(taken))
	mgr.ReleaseName(taken)

	// The window the manual create lands in: after the pool has drawn the
	// name, before AllocateAndAdd takes the per-polecat lock. Plant the other
	// polecat's worktree, as the manual add would have left it.
	manualWorktree := filepath.Join(mgr.polecatDir(taken), mgr.rig.Name)
	manualReadme := filepath.Join(manualWorktree, "README.md")
	calls := 0
	var hookErr error
	mgr.afterPoolNameAllocated = func(name string) {
		calls++
		if name != taken || hookErr != nil {
			return
		}
		if err := os.MkdirAll(manualWorktree, 0755); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(manualReadme, []byte("manual polecat\n"), 0644)
	}

	got, _, err := mgr.AllocateAndAdd(AddOptions{})
	if hookErr != nil {
		t.Fatalf("planting the polecat at %q: %v", taken, hookErr)
	}
	if _, statErr := os.Stat(manualReadme); statErr != nil {
		t.Fatalf("AllocateAndAdd destroyed the polecat that already held %q: %v (AllocateAndAdd: %v)", taken, statErr, err)
	}
	if calls < 2 {
		t.Fatalf("the pool drew %d name(s) and returned %q; want %q skipped as taken", calls, got, taken)
	}
	if err != nil {
		t.Fatalf("AllocateAndAdd: %v", err)
	}
	if got == taken {
		t.Fatalf("AllocateAndAdd built into %q, a polecat created outside the pool", taken)
	}
}

// TestAllocateAndAdd_GivesUpWhenEveryDrawnNameIsTaken pins the backstop on the
// re-check: the pool is not asked forever, so a rig whose names are all taken
// outside the pool fails the allocation instead of spinning under the pool lock.
func TestAllocateAndAdd_GivesUpWhenEveryDrawnNameIsTaken(t *testing.T) {
	t.Parallel()
	mgr, _, _, _ := canonicalRig(t)

	calls := 0
	mgr.afterPoolNameAllocated = func(name string) {
		calls++
		if err := os.MkdirAll(filepath.Join(mgr.polecatDir(name), mgr.rig.Name), 0755); err != nil {
			t.Errorf("planting %q: %v", name, err)
		}
	}

	if _, _, err := mgr.AllocateAndAdd(AddOptions{}); err == nil {
		t.Fatal("AllocateAndAdd succeeded; want an error when every drawn name is taken")
	}
	if calls < 2 {
		t.Fatalf("the pool drew %d name(s); want the allocation retried before giving up", calls)
	}
}

// TestReuseIdlePolecat_KillsLiveSession verifies that ReuseIdlePolecat kills
// an existing live (non-stale) tmux session instead of returning ErrSessionRunning.
// This is the regression test for the sling-reuse-stale-session bug: idle polecats
// with a live Claude session at a dead ❯ prompt must have their session killed so
// StartSession can create a fresh session with a proper gt prime --hook cycle.
func TestReuseIdlePolecat_KillsLiveSession(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigName := "testreuse"
	rigPath := filepath.Join(townRoot, rigName)
	polecatName := "toast"

	// Create minimal polecat directory structure
	polecatDir := filepath.Join(rigPath, "polecats", polecatName)
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}

	tm := newFakeProbe()
	r := &rig.Rig{Name: rigName, Path: rigPath}
	mgr := newTestManager(r, nil, tm, newNoDatabaseDB())

	// Create a live tmux session (simulates Claude sitting at ❯ after gt done)
	sessionName := session.PolecatSessionName(session.DefaultPrefix, polecatName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, townRoot, "sleep 300", nil); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}

	// Write a fresh heartbeat (simulating a session that just finished gt done
	// but hasn't gone stale yet — this is the exact scenario that previously
	// caused ReuseIdlePolecat to return ErrSessionRunning)
	TouchSessionHeartbeat(townRoot, sessionName)

	// Verify session is alive and heartbeat exists
	running, err := tm.HasSession(sessionName)
	if err != nil || !running {
		t.Fatalf("precondition: session %s should be running", sessionName)
	}
	if hb := ReadSessionHeartbeat(townRoot, sessionName); hb == nil {
		t.Fatal("precondition: heartbeat should exist")
	}

	// Call ReuseIdlePolecat — it will kill the session, then fail on worktree
	// operations (no real git repo). The important thing is it does NOT return
	// ErrSessionRunning.
	_, reuseErr := mgr.ReuseIdlePolecat(polecatName, AddOptions{})

	// Verify it did NOT return ErrSessionRunning (the old buggy behavior)
	if errors.Is(reuseErr, ErrSessionRunning) {
		t.Fatalf("ReuseIdlePolecat returned ErrSessionRunning for live session — " +
			"this is the sling-reuse-stale-session bug: idle polecats with live " +
			"sessions must have their session killed, not rejected")
	}

	// We expect an error from later steps (worktree not found), but not from session handling
	if reuseErr == nil {
		t.Fatal("expected error from worktree operations (test has no real git repo)")
	}
	if !strings.Contains(reuseErr.Error(), "worktree") {
		t.Logf("ReuseIdlePolecat error (expected worktree-related): %v", reuseErr)
	}

	// Verify the session was killed
	running, _ = tm.HasSession(sessionName)
	if running {
		t.Error("session should have been killed by ReuseIdlePolecat")
	}

	// Verify heartbeat was cleaned up
	if hb := ReadSessionHeartbeat(townRoot, sessionName); hb != nil {
		t.Error("heartbeat should have been removed after session kill")
	}
}

func TestRepairWorktreeWithOptions_KillsLiveSession(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigName := "testrepair"
	rigPath := filepath.Join(townRoot, rigName)
	mayorRig := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor rig: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir rig beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mayorRig, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir mayor beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write beads redirect: %v", err)
	}

	w := newWorld()
	w.InitRepo(t, mayorRig)
	head := w.Commit(t, mayorRig, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, mayorRig)
	w.AddRemote(t, mayorRig, "origin", mayorRig)
	w.SetRef(t, mayorRig, "refs/remotes/origin/main", head)
	mayorGit := w.repo(mayorRig)

	polecatName := "toast"
	oldClonePath := filepath.Join(rigPath, "polecats", polecatName, rigName)
	if err := mayorGit.WorktreeAddFromRef(oldClonePath, "old-toast", "HEAD"); err != nil {
		t.Fatalf("create old worktree: %v", err)
	}

	tm := newFakeProbe()
	sessionName := session.PolecatSessionName(session.DefaultPrefix, polecatName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, oldClonePath, "sleep 300", nil); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	TouchSessionHeartbeat(townRoot, sessionName)

	mgr := newTestManager(&rig.Rig{Name: rigName, Path: rigPath}, w, tm, newPolecatDB())
	if _, err := mgr.RepairWorktreeWithOptions(polecatName, true, AddOptions{HookBead: "gt-next"}); err != nil {
		t.Fatalf("RepairWorktreeWithOptions: %v", err)
	}

	running, _ := tm.HasSession(sessionName)
	if running {
		t.Error("session should have been killed by RepairWorktreeWithOptions")
	}
	if hb := ReadSessionHeartbeat(townRoot, sessionName); hb != nil {
		t.Error("heartbeat should have been removed after repair session kill")
	}
}

// TestReuseIdlePolecat_KillsStaleSession verifies that ReuseIdlePolecat also
// handles the stale-session case correctly (regression: the original code path
// that worked before the fix should still work after).
func TestReuseIdlePolecat_KillsStaleSession(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigName := "teststale"
	rigPath := filepath.Join(townRoot, rigName)
	polecatName := "marmalade"

	polecatDir := filepath.Join(rigPath, "polecats", polecatName)
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}

	tm := newFakeProbe() // the pane runs no agent, as "sleep 300" did
	r := &rig.Rig{Name: rigName, Path: rigPath}
	mgr := newTestManager(r, nil, tm, newNoDatabaseDB())

	sessionName := session.PolecatSessionName(session.DefaultPrefix, polecatName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, townRoot, "sleep 300", nil); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}

	// Write a STALE heartbeat (old timestamp)
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-10 * time.Minute).UTC()
	data := []byte(`{"timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","state":"exiting"}`)
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	_, reuseErr := mgr.ReuseIdlePolecat(polecatName, AddOptions{})

	// Should not return ErrSessionRunning
	if errors.Is(reuseErr, ErrSessionRunning) {
		t.Fatal("ReuseIdlePolecat should not return ErrSessionRunning for stale session")
	}

	// Session should be killed
	running, _ := tm.HasSession(sessionName)
	if running {
		t.Error("stale session should have been killed")
	}

	// Heartbeat should be cleaned up
	if hb := ReadSessionHeartbeat(townRoot, sessionName); hb != nil {
		t.Error("heartbeat should have been removed after stale session kill")
	}
}

// TestReuseIdlePolecat_NoSessionNoop verifies that ReuseIdlePolecat proceeds
// normally when there's no existing session (the most common reuse case: session
// was already killed by the Witness or expired).
func TestReuseIdlePolecat_NoSessionNoop(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigName := "testnoop"
	rigPath := filepath.Join(townRoot, rigName)
	polecatName := "jam"

	polecatDir := filepath.Join(rigPath, "polecats", polecatName)
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}

	r := &rig.Rig{Name: rigName, Path: rigPath}
	mgr := newTestManager(r, nil, newFakeProbe(), newNoDatabaseDB())

	// No tmux session, no heartbeat — the common idle case
	_, reuseErr := mgr.ReuseIdlePolecat(polecatName, AddOptions{})

	// Should not return ErrSessionRunning
	if errors.Is(reuseErr, ErrSessionRunning) {
		t.Fatal("ReuseIdlePolecat should not return ErrSessionRunning when no session exists")
	}

	// Error should be from later steps (worktree ops), not session handling
	if reuseErr == nil {
		t.Fatal("expected error from worktree operations")
	}
}

// TestResolveSetupCommandReadsRigRootMergeQueue reproduces gt-egiv finding 1:
// resolveSetupCommand merged repo-committed .gastown/settings.json with
// rig-local settings/config.json only, so a rig configuring setup_command
// exclusively at rig-root onboarding time (gt-me9t) never surfaced it to
// polecat worktree setup.
func TestResolveSetupCommandReadsRigRootMergeQueue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	worktreePath := filepath.Join(rigPath, "polecats", "jasper")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}

	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "merge_queue": {"setup_command": "pnpm install"}
}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "testrig", Path: rigPath, IdentityBeads: newNoDatabaseDB()}
	mgr := &Manager{rig: r, townRoot: tmpDir}

	got := mgr.resolveSetupCommand(worktreePath)
	if got != "pnpm install" {
		t.Errorf("resolveSetupCommand() = %q, want %q (rig-root merge_queue floor invisible to polecat setup)", got, "pnpm install")
	}
}

// TestResolveSetupCommandPrecedence pins the three-tier merge order for
// resolveSetupCommand once it routes through rig.ResolveMergeQueueConfig:
// rig-local settings/config.json has the final say over the repo-committed
// and rig-root layers.
func TestResolveSetupCommandPrecedence(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	worktreePath := filepath.Join(rigPath, "polecats", "jasper")
	gastownDir := filepath.Join(rigPath, "mayor", "rig", ".gastown")
	settingsDir := filepath.Join(rigPath, "settings")
	for _, dir := range []string{gastownDir, settingsDir, worktreePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rigConfig := `{"type": "rig", "version": 1, "name": "testrig", "merge_queue": {"setup_command": "root-setup"}}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	repoSettings := `{"type": "rig-settings", "version": 1, "merge_queue": {"setup_command": "repo-setup"}}`
	if err := os.WriteFile(filepath.Join(gastownDir, "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	localSettings := `{"type": "rig-settings", "version": 1, "merge_queue": {"setup_command": "local-setup"}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{Name: "testrig", Path: rigPath, IdentityBeads: newNoDatabaseDB()}
	mgr := &Manager{rig: r, townRoot: tmpDir}

	got := mgr.resolveSetupCommand(worktreePath)
	if got != "local-setup" {
		t.Errorf("resolveSetupCommand() = %q, want %q (rig-local wins over repo and root)", got, "local-setup")
	}
}

// TestLoadFromBeads_SpawnGraceEndToEnd pins the gt-yteq symptom at the
// level where the witness reads it: a polecat dispatched seconds ago, with
// work hooked but no live tmux session yet, must report spawning rather than
// stalled while its agent bead is inside the spawn grace window, and must
// report stalled the moment the window passes. The unit tests cover
// SpawnGrace itself; this covers the full loadFromBeads derivation through
// the beads client, including the fail-toward-stalled case of an unparseable
// bead timestamp.
//
// loadFromBeads only reaches the session-down state (and therefore the
// spawn grace) when it can PROVE the session is absent: a nil tmux means
// "state unknown" and defaults to alive. A tmux that answers has-session
// with (false, nil) is that proof. Real tmux gives the same answer for a
// socket with no server and for a server without the session; the tmux
// package's session contract pins both.
func TestLoadFromBeads_SpawnGraceEndToEnd(t *testing.T) {
	t.Parallel()
	const (
		agentName   = "basalt"
		rigName     = "testrig"
		assigneeIDT = "testrig/polecats/basalt"
		agentBeadID = "gt-testrig-polecat-basalt"
		hookedID    = "gt-testrig-work"
	)

	now := time.Now().UTC().Truncate(time.Second)
	// 30s is a 10x margin against the 5m window: the only way the fresh
	// case can drift out of the window between test setup and the two
	// loadFromBeads calls inside one subtest is a multi-minute pause.
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339)
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339)

	// A database with the rig-wide state loadFromBeads reads: the agent bead
	// (spawning, dated per case), a hooked work bead, and the assigned issue.
	dbFor := func(t *testing.T, updatedAt string) *polecatDB {
		db := newPolecatDB()
		db.Seed(
			beads.Issue{
				ID: agentBeadID, Title: "agent", Labels: []string{"gt:agent"}, Status: "open", UpdatedAt: updatedAt,
				Description: beads.FormatAgentDescription("agent", &beads.AgentFields{
					RoleType: "polecat", Rig: rigName, AgentState: "spawning", HookBead: hookedID, CleanupStatus: "clean",
				}),
			},
			beads.Issue{ID: hookedID, Title: "work", Status: "hooked", Assignee: assigneeIDT, UpdatedAt: fresh},
		)
		return db
	}

	cases := []struct {
		name      string
		updatedAt string
		wantState State
	}{
		{"fresh dispatch, no session yet", fresh, StateSpawning},
		{"grace window expired reads stalled", stale, StateStalled},
		{"undateable agent bead fails toward stalled", "not-a-time", StateStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for _, dir := range []string{
				filepath.Join(root, "polecats", agentName),
				filepath.Join(root, "mayor", "rig"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			bd := dbFor(t, tc.updatedAt)
			// No session on the fake tmux: the session is provably down.
			mgr := newTestManager(&rig.Rig{Name: rigName, Path: root}, nil, newFakeProbe(), bd)
			mgr.spawnGraceWindow = 5 * time.Minute

			p, err := mgr.loadFromBeads(agentName, nil)
			if err != nil {
				t.Fatalf("loadFromBeads: %v", err)
			}
			if p.Issue != hookedID {
				t.Fatalf("issue = %q, want %q (wrong derivation branch taken)", p.Issue, hookedID)
			}
			if p.State != tc.wantState {
				t.Errorf("state = %q, want %q", p.State, tc.wantState)
			}
			// Direct evaluation of the exact function loadFromBeads calls, on
			// the same bead: it must agree with the state the derivation
			// produced. A mismatch means the derivation consumed something
			// other than the bead this fake returned.
			agentIssue, fields, err := beads.GetAgentBead(mgr.agentBeads(), mgr.agentBeadID(agentName))
			if err != nil {
				t.Fatalf("GetAgentBead: %v", err)
			}
			if fields == nil {
				t.Fatalf("fields nil; the bead the fake returned is not surviving GetAgentBead (issue=%+v)", agentIssue)
			}
			if got := SpawnGrace(fields.AgentState, AgentBeadUpdatedAt(agentIssue), time.Now(), mgr.spawnGraceWindow); got != (p.State == StateSpawning) {
				t.Errorf("SpawnGrace(%q, %v, now, %v) = %v, but loadFromBeads produced %q — the derivation is not consuming this bead's data",
					fields.AgentState, AgentBeadUpdatedAt(agentIssue), mgr.spawnGraceWindow, got, p.State)
			}
		})
	}
}

// A removed polecat must take its intent record with it: townhealth reads
// every record under .runtime/agents, so one that outlives its seat reports
// that seat dead forever (gt-u7voe).
func TestRemoveDeletesThePolecatsIntentRecord(t *testing.T) {
	t.Parallel()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "toast")
	seat := intent.Seat{Rig: "rig", Role: "polecat", Name: "toast"}
	if _, err := intent.Update(mgr.townRoot, seat, func(r *intent.Record) error {
		r.Desired = intent.DesiredPark
		return nil
	}); err != nil {
		t.Fatalf("writing the intent record: %v", err)
	}

	if err := mgr.RemoveWithOptions("toast", RemoveOptions{Force: true}); err != nil {
		t.Fatalf("RemoveWithOptions: %v", err)
	}

	if _, err := os.Stat(mgr.polecatDir("toast")); !os.IsNotExist(err) {
		t.Fatalf("polecat dir survived removal, stat err=%v", err)
	}
	if _, err := os.Stat(seat.Path(mgr.townRoot)); !os.IsNotExist(err) {
		t.Fatalf("intent record survived removal, stat err=%v", err)
	}
}

// A refused removal leaves the record alone: the polecat is still there, so it
// still needs the record.
func TestRemoveKeepsTheIntentRecordWhenTheRemovalIsRefused(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, false, "toast")
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
		f.CleanupStatus = string(CleanupUncommitted)
	})
	seat := intent.Seat{Rig: "rig", Role: "polecat", Name: "toast"}
	if _, err := intent.Update(mgr.townRoot, seat, func(r *intent.Record) error { return nil }); err != nil {
		t.Fatalf("writing the intent record: %v", err)
	}

	if err := mgr.Remove("toast", false); err == nil {
		t.Fatal("Remove of a polecat with uncommitted work succeeded, want a refusal")
	}

	if _, err := os.Stat(mgr.polecatDir("toast")); err != nil {
		t.Fatalf("polecat dir removed by a refused removal: %v", err)
	}
	if _, err := os.Stat(seat.Path(mgr.townRoot)); err != nil {
		t.Fatalf("refused removal deleted the intent record: %v", err)
	}
}
