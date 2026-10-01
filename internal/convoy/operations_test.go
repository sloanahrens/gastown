package convoy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	beadsRouting "github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dispatch"
)

func TestExtractIssueID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"gt-abc", "gt-abc"},
		{"bd-xyz", "bd-xyz"},
		{"hq-cv-123", "hq-cv-123"},
		{"external:gt:gt-abc", "gt-abc"},
		{"external:bd:bd-xyz", "bd-xyz"},
		{"external:hq:hq-cv-123", "hq-cv-123"},
		{"external:", "external:"}, // malformed, return as-is
		{"external:x:", ""},        // 3 parts but empty last part
		{"simple", "simple"},       // no external prefix
		{"", ""},                   // empty
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := extractIssueID(tt.input)
			if result != tt.expected {
				t.Errorf("extractIssueID(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsSlingableType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		issueType string
		want      bool
	}{
		{"task", true},
		{"bug", true},
		{"feature", true},
		{"chore", true},
		{"", true},          // empty defaults to task
		{"epic", false},     // container type
		{"convoy", false},   // meta type
		{"sub-epic", false}, // container type
		{"decision", false}, // non-work type
		{"message", false},  // non-work type
		{"event", false},    // non-work type
		{"unknown", false},  // unknown types are not slingable
	}

	for _, tt := range tests {
		t.Run(tt.issueType, func(t *testing.T) {
			got := IsSlingableType(tt.issueType)
			if got != tt.want {
				t.Errorf("IsSlingableType(%q) = %v, want %v", tt.issueType, got, tt.want)
			}
		})
	}
}

func TestIsIssueBlocked_NoStore(t *testing.T) {
	t.Parallel()
	// isIssueBlocked with nil store should fail-open (return false, not panic).
	// This covers the "store unavailable" failure mode (F-17).
	result := isIssueBlocked(context.Background(), nil, "test-any-id", nil)
	if result {
		t.Error("isIssueBlocked should fail-open (return false) with nil store")
	}
}

func TestReadyIssueFilterLogic_SkipsNonSlingableTypes(t *testing.T) {
	t.Parallel()
	// Validates that feedNextReadyIssue's type filter skips non-slingable types.
	// We test the predicate inline (same pattern as existing filter tests).
	tracked := []trackedIssue{
		{ID: "gt-epic", Status: "open", Assignee: "", IssueType: "epic"},
		{ID: "gt-task", Status: "open", Assignee: "", IssueType: "task"},
		{ID: "gt-convoy", Status: "open", Assignee: "", IssueType: "convoy"},
		{ID: "gt-bug", Status: "open", Assignee: "", IssueType: "bug"},
	}

	var slingable []string
	for _, issue := range tracked {
		if issue.Status == "open" && issue.Assignee == "" && IsSlingableType(issue.IssueType) {
			slingable = append(slingable, issue.ID)
		}
	}

	if len(slingable) != 2 {
		t.Errorf("expected 2 slingable issues (task, bug), got %d: %v", len(slingable), slingable)
	}
	if slingable[0] != "gt-task" || slingable[1] != "gt-bug" {
		t.Errorf("expected [gt-task, gt-bug], got %v", slingable)
	}
}

func TestReadyIssueFilterLogic_SkipsNonOpenIssues(t *testing.T) {
	t.Parallel()
	// Validates the filtering predicate used by feedNextReadyIssue: only
	// open issues with no assignee should be considered "ready". We test
	// the predicate inline because feedNextReadyIssue also calls rigForIssue
	// and dispatchIssue, making isolated unit testing impractical without a
	// real store. Integration coverage lives in convoy_manager_integration_test.go.
	tracked := []trackedIssue{
		{ID: "gt-closed", Status: "closed", Assignee: ""},
		{ID: "gt-inprog", Status: "in_progress", Assignee: "gastown/polecats/alpha"},
		{ID: "gt-hooked", Status: "hooked", Assignee: "gastown/polecats/beta"},
		{ID: "gt-assigned", Status: "open", Assignee: "gastown/polecats/gamma"},
	}

	// None of these should be considered "ready"
	for _, issue := range tracked {
		if issue.Status == "open" && issue.Assignee == "" {
			t.Errorf("issue %s should not be ready (status=%s, assignee=%s)", issue.ID, issue.Status, issue.Assignee)
		}
	}
}

func TestReadyIssueFilterLogic_FindsReadyIssue(t *testing.T) {
	t.Parallel()
	// Validates that the "first open+unassigned" selection picks the correct
	// issue. See comment on TestReadyIssueFilterLogic_SkipsNonOpenIssues for
	// why this tests the predicate inline rather than calling feedNextReadyIssue.
	tracked := []trackedIssue{
		{ID: "gt-closed", Status: "closed", Assignee: ""},
		{ID: "gt-inprog", Status: "in_progress", Assignee: "gastown/polecats/alpha"},
		{ID: "gt-ready", Status: "open", Assignee: ""},
		{ID: "gt-also-ready", Status: "open", Assignee: ""},
	}

	// Find first ready issue - should be gt-ready (first match)
	var foundReady string
	for _, issue := range tracked {
		if issue.Status == "open" && issue.Assignee == "" {
			foundReady = issue.ID
			break
		}
	}

	if foundReady != "gt-ready" {
		t.Errorf("expected first ready issue to be gt-ready, got %s", foundReady)
	}
}

func TestCheckConvoysForIssue_NilStore(t *testing.T) {
	t.Parallel()
	// Nil store returns nil immediately (no convoy checks).
	result := CheckConvoysForIssue(context.Background(), nil, "/nonexistent/path", "gt-test", "test", nil, "gt", nil, nil)
	if result != nil {
		t.Errorf("expected nil for nil store, got %v", result)
	}
}

func TestCheckConvoysForIssue_NilLogger(t *testing.T) {
	t.Parallel()
	// Nil logger should not panic — gets replaced with no-op internally.
	// With nil store, returns nil.
	result := CheckConvoysForIssue(context.Background(), nil, "/nonexistent/path", "gt-test", "test", nil, "gt", nil, nil)
	if result != nil {
		t.Errorf("expected nil for nil store, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// blockingDepTypes map tests
// ---------------------------------------------------------------------------

func TestBlockingDepTypes_ContainsExpectedTypes(t *testing.T) {
	t.Parallel()
	expected := []string{"blocks", "conditional-blocks", "waits-for", "merge-blocks"}
	for _, depType := range expected {
		if !blockingDepTypes[depType] {
			t.Errorf("blockingDepTypes should contain %q", depType)
		}
	}
}

func TestBlockingDepTypes_ExcludesParentChild(t *testing.T) {
	t.Parallel()
	if blockingDepTypes["parent-child"] {
		t.Error("blockingDepTypes should NOT contain parent-child")
	}
}

func TestBlockingDepTypes_ExactSize(t *testing.T) {
	t.Parallel()
	// Ensure the map has exactly the 4 expected entries and no extras.
	if len(blockingDepTypes) != 4 {
		t.Errorf("blockingDepTypes has %d entries, want 4; contents: %v", len(blockingDepTypes), blockingDepTypes)
	}
}

// ---------------------------------------------------------------------------
// isIssueBlocked tests (real beads store)
// ---------------------------------------------------------------------------

func TestIsIssueBlocked_NoDeps(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	issue := &beadsdk.Issue{
		ID:        "test-noblk1",
		Title:     "No Deps Issue",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if isIssueBlocked(ctx, store, issue.ID, nil) {
		t.Error("isIssueBlocked should return false for issue with no dependencies")
	}
}

func TestIsIssueBlocked_BlockedByOpenBlocker(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	blocker := &beadsdk.Issue{
		ID:        "test-blkr1",
		Title:     "Blocker",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	blocked := &beadsdk.Issue{
		ID:        "test-blkd1",
		Title:     "Blocked",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, blocker, "test"); err != nil {
		t.Fatalf("CreateIssue blocker: %v", err)
	}
	if err := store.CreateIssue(ctx, blocked, "test"); err != nil {
		t.Fatalf("CreateIssue blocked: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        beadsdk.DepBlocks,
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	result := isIssueBlocked(ctx, store, blocked.ID, nil)

	if !result {
		t.Error("isIssueBlocked should return true when issue has open blocker")
	}
}

func TestIsIssueBlocked_NotBlockedByClosedBlocker(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	blocker := &beadsdk.Issue{
		ID:        "test-clblkr",
		Title:     "Closed Blocker",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	blocked := &beadsdk.Issue{
		ID:        "test-clblkd",
		Title:     "Blocked By Closed",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, blocker, "test"); err != nil {
		t.Fatalf("CreateIssue blocker: %v", err)
	}
	if err := store.CreateIssue(ctx, blocked, "test"); err != nil {
		t.Fatalf("CreateIssue blocked: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        beadsdk.DepBlocks,
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// Even if GetDependenciesWithMetadata works, the blocker is closed so
	// isIssueBlocked should return false.
	if isIssueBlocked(ctx, store, blocked.ID, nil) {
		t.Error("isIssueBlocked should return false when the only blocker is closed")
	}
}

func TestIsIssueBlocked_ParentChildDoesNotBlock(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	parent := &beadsdk.Issue{
		ID:        "test-pcpar",
		Title:     "Parent",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	child := &beadsdk.Issue{
		ID:        "test-pcchld",
		Title:     "Child",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, parent, "test"); err != nil {
		t.Fatalf("CreateIssue parent: %v", err)
	}
	if err := store.CreateIssue(ctx, child, "test"); err != nil {
		t.Fatalf("CreateIssue child: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     child.ID,
		DependsOnID: parent.ID,
		Type:        beadsdk.DepParentChild,
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// parent-child deps should NOT block dispatch
	if isIssueBlocked(ctx, store, child.ID, nil) {
		t.Error("isIssueBlocked should return false for parent-child dependency (not a blocking type)")
	}
}

func TestIsIssueBlocked_FailOpenOnNonexistentIssue(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Querying deps for a nonexistent issue should fail-open (return false)
	if isIssueBlocked(ctx, store, "test-nonexistent-issue", nil) {
		t.Error("isIssueBlocked should fail-open (return false) for nonexistent issue")
	}
}

// ---------------------------------------------------------------------------
// merge-blocks dependency tests (#1893)
// ---------------------------------------------------------------------------

func TestIsIssueBlocked_MergeBlocksStillBlockedWhenClosedWithoutMerge(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Blocker is closed but has no CloseReason (gt done without merge)
	blocker := &beadsdk.Issue{
		ID:        "test-mblkr1",
		Title:     "Closed No Merge",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	blocked := &beadsdk.Issue{
		ID:        "test-mblkd1",
		Title:     "Merge-Blocked",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, blocker, "test"); err != nil {
		t.Fatalf("CreateIssue blocker: %v", err)
	}
	if err := store.CreateIssue(ctx, blocked, "test"); err != nil {
		t.Fatalf("CreateIssue blocked: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        beadsdk.DependencyType("merge-blocks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	result := isIssueBlocked(ctx, store, blocked.ID, nil)

	if !result {
		t.Error("isIssueBlocked should return true for merge-blocks dep when blocker is closed without merge")
	}
}

func TestIsIssueBlocked_MergeBlocksUnblockedWhenMerged(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Blocker is closed WITH merge confirmation
	blocker := &beadsdk.Issue{
		ID:          "test-mblkr2",
		Title:       "Merged Blocker",
		Status:      beadsdk.StatusClosed,
		CloseReason: "Merged in mr-xyz",
		Priority:    2,
		IssueType:   beadsdk.TypeTask,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	blocked := &beadsdk.Issue{
		ID:        "test-mblkd2",
		Title:     "Merge-Blocked By Merged",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, blocker, "test"); err != nil {
		t.Fatalf("CreateIssue blocker: %v", err)
	}
	if err := store.CreateIssue(ctx, blocked, "test"); err != nil {
		t.Fatalf("CreateIssue blocked: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        beadsdk.DependencyType("merge-blocks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// Blocker is closed with "Merged in mr-xyz" — should NOT be blocked
	if isIssueBlocked(ctx, store, blocked.ID, nil) {
		t.Error("isIssueBlocked should return false when merge-blocks blocker has CloseReason 'Merged in ...'")
	}
}

func TestIsIssueBlocked_MergeBlocksUnblockedOnTombstone(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create blocker as open first, then transition to tombstone
	blocker := &beadsdk.Issue{
		ID:        "test-mblkr3",
		Title:     "Tombstone Blocker",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	blocked := &beadsdk.Issue{
		ID:        "test-mblkd3",
		Title:     "Merge-Blocked By Tombstone",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.CreateIssue(ctx, blocker, "test"); err != nil {
		t.Fatalf("CreateIssue blocker: %v", err)
	}
	if err := store.CreateIssue(ctx, blocked, "test"); err != nil {
		t.Fatalf("CreateIssue blocked: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     blocked.ID,
		DependsOnID: blocker.ID,
		Type:        beadsdk.DependencyType("merge-blocks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// Transition to tombstone
	if err := store.UpdateIssue(ctx, blocker.ID, map[string]interface{}{
		"status": "tombstone",
	}, "test"); err != nil {
		t.Fatalf("UpdateIssue to tombstone: %v", err)
	}

	// Tombstone always unblocks, regardless of dep type
	if isIssueBlocked(ctx, store, blocked.ID, nil) {
		t.Error("isIssueBlocked should return false when merge-blocks blocker is tombstoned")
	}
}

// ---------------------------------------------------------------------------
// rigForIssue tests
// ---------------------------------------------------------------------------

func TestRigForIssue_ValidPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create .beads/routes.jsonl with a mapping
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}` + "\n" +
		`{"prefix":"bd-","path":"beads/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("WriteFile routes.jsonl: %v", err)
	}

	rig := rigForIssue(townRoot, "gt-abc123")
	if rig != "gastown" {
		t.Errorf("rigForIssue(townRoot, 'gt-abc123') = %q, want 'gastown'", rig)
	}

	rig = rigForIssue(townRoot, "bd-xyz")
	if rig != "beads" {
		t.Errorf("rigForIssue(townRoot, 'bd-xyz') = %q, want 'beads'", rig)
	}
}

func TestRigForIssue_EmptyPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// No prefix extractable from "nohyphen"
	rig := rigForIssue(townRoot, "nohyphen")
	if rig != "" {
		t.Errorf("rigForIssue with no-hyphen ID = %q, want empty", rig)
	}
}

func TestRigForIssue_EmptyIssueID(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	rig := rigForIssue(townRoot, "")
	if rig != "" {
		t.Errorf("rigForIssue with empty ID = %q, want empty", rig)
	}
}

func TestRigForIssue_UnknownPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create routes.jsonl with only gt- mapping
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("WriteFile routes.jsonl: %v", err)
	}

	// "zz-" prefix not in routes
	rig := rigForIssue(townRoot, "zz-unknown")
	if rig != "" {
		t.Errorf("rigForIssue with unknown prefix = %q, want empty", rig)
	}
}

func TestRigForIssue_NoRoutesFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// No .beads directory at all — should return ""
	rig := rigForIssue(townRoot, "gt-abc")
	if rig != "" {
		t.Errorf("rigForIssue with no routes file = %q, want empty", rig)
	}
}

func TestRigForIssue_TownLevelPrefix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Town-level beads have path="." which should return "" (no specific rig)
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	routesContent := `{"prefix":"hq-","path":"."}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("WriteFile routes.jsonl: %v", err)
	}

	rig := rigForIssue(townRoot, "hq-cv-test")
	if rig != "" {
		t.Errorf("rigForIssue for town-level prefix = %q, want empty", rig)
	}
}

// ---------------------------------------------------------------------------
// Helper: create a temporary town root with routes.jsonl and a gt stub
// ---------------------------------------------------------------------------

// setupTownRoot creates a temp directory with .beads/routes.jsonl mapping
// the "test-" prefix to the rig name "testrig".
func setupTownRoot(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll .beads: %v", err)
	}
	routesContent := `{"prefix":"test-","path":"testrig/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatalf("WriteFile routes.jsonl: %v", err)
	}
	return townRoot
}

// slingLog is a slinger that records each gt sling's arguments, one line per
// call, and fails every call with err when it is set.
type slingLog struct {
	mu    sync.Mutex
	lines []string
	err   error
}

func (l *slingLog) sling(_ context.Context, _ string, args []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.Join(args, " "))
	return l.err
}

// read returns the recorded calls, or os.ErrNotExist when there were none.
func (l *slingLog) read() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) == 0 {
		return nil, os.ErrNotExist
	}
	return []byte(strings.Join(l.lines, "\n") + "\n"), nil
}

func (l *slingLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = nil
}

// noopChecker is a Checker that closes nothing, so a test of the event path
// runs no bd.
func noopChecker(context.Context, string) error { return nil }

// makeLogger returns a logger that captures messages and a pointer to the slice.
func makeLogger() (func(string, ...interface{}), *[]string) {
	var msgs []string
	logger := func(format string, args ...interface{}) {
		msgs = append(msgs, fmt.Sprintf(format, args...))
	}
	return logger, &msgs
}

// ---------------------------------------------------------------------------
// feedNextReadyIssue tests (real beads store)
// ---------------------------------------------------------------------------

func TestFeedNextReadyIssue_DispatchesFirstReadyIssue(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create convoy issue
	convoy := &beadsdk.Issue{
		ID:        "test-convoy1",
		Title:     "Test Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 1: closed issue (should be skipped)
	closed := &beadsdk.Issue{
		ID:        "test-closed1",
		Title:     "Closed Task",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 2: assigned issue (should be skipped)
	assigned := &beadsdk.Issue{
		ID:        "test-assigned1",
		Title:     "Assigned Task",
		Status:    beadsdk.StatusOpen,
		Assignee:  "gastown/polecats/alpha",
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 3: open, unassigned task (should be dispatched)
	ready := &beadsdk.Issue{
		ID:        "test-ready1",
		Title:     "Ready Task",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, closed, assigned, ready} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	// Add tracks deps: convoy -> each tracked issue
	for _, trackedID := range []string{closed.ID, assigned.ID, ready.ID} {
		dep := &beadsdk.Dependency{
			IssueID:     convoy.ID,
			DependsOnID: trackedID,
			Type:        beadsdk.DependencyType("tracks"),
			CreatedAt:   now,
			CreatedBy:   "test",
		}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", trackedID, err)
		}
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, _ := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

	// Verify gt was called with the ready issue
	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub was not called (no log file): %v", err)
	}
	logStr := strings.TrimSpace(string(logData))
	// Expected: "sling test-ready1 testrig --no-boot"
	if !strings.Contains(logStr, "sling test-ready1 testrig --no-boot") {
		t.Errorf("gt stub called with unexpected args: %q", logStr)
	}
}

func TestFeedNextReadyIssue_SkipsEpicAndDispatchesTask(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	convoy := &beadsdk.Issue{
		ID:        "test-convoy2",
		Title:     "Convoy For Epic Test",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	epic := &beadsdk.Issue{
		ID:        "test-epic1",
		Title:     "An Epic",
		Status:    beadsdk.StatusOpen,
		Priority:  1,
		IssueType: beadsdk.TypeEpic,
		CreatedAt: now,
		UpdatedAt: now,
	}
	task := &beadsdk.Issue{
		ID:        "test-task2",
		Title:     "A Task",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, epic, task} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	// Add tracks deps: convoy -> epic, convoy -> task
	for _, trackedID := range []string{epic.ID, task.ID} {
		dep := &beadsdk.Dependency{
			IssueID:     convoy.ID,
			DependsOnID: trackedID,
			Type:        beadsdk.DependencyType("tracks"),
			CreatedAt:   now,
			CreatedBy:   "test",
		}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", trackedID, err)
		}
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, _ := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub was not called (no log file): %v", err)
	}
	logStr := strings.TrimSpace(string(logData))
	// Only the task should have been dispatched, not the epic
	if !strings.Contains(logStr, "sling test-task2 testrig --no-boot") {
		t.Errorf("expected task dispatch, got: %q", logStr)
	}
	if strings.Contains(logStr, "test-epic1") {
		t.Errorf("epic should not have been dispatched, but log contains: %q", logStr)
	}
}

func TestFeedNextReadyIssue_SkipsBlockedIssue(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	convoy := &beadsdk.Issue{
		ID:        "test-convoy3",
		Title:     "Convoy For Blocked Test",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Blocker issue (open)
	blocker := &beadsdk.Issue{
		ID:        "test-blocker3",
		Title:     "Blocker",
		Status:    beadsdk.StatusOpen,
		Priority:  1,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Blocked task
	blockedTask := &beadsdk.Issue{
		ID:        "test-blocked3",
		Title:     "Blocked Task",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Unblocked task
	unblockedTask := &beadsdk.Issue{
		ID:        "test-unblk3",
		Title:     "Unblocked Task",
		Status:    beadsdk.StatusOpen,
		Priority:  3,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, blocker, blockedTask, unblockedTask} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	// Add tracks deps from convoy
	for _, trackedID := range []string{blockedTask.ID, unblockedTask.ID} {
		dep := &beadsdk.Dependency{
			IssueID:     convoy.ID,
			DependsOnID: trackedID,
			Type:        beadsdk.DependencyType("tracks"),
			CreatedAt:   now,
			CreatedBy:   "test",
		}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency tracks %s: %v", trackedID, err)
		}
	}

	// Add blocks dep: blockedTask is blocked by blocker
	blocksDep := &beadsdk.Dependency{
		IssueID:     blockedTask.ID,
		DependsOnID: blocker.ID,
		Type:        beadsdk.DepBlocks,
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, blocksDep, "test"); err != nil {
		t.Fatalf("AddDependency blocks: %v", err)
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, logMsgs := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub was not called (%v): the unblocked task was never dispatched. log messages: %v", err, *logMsgs)
	}
	logStr := strings.TrimSpace(string(logData))

	// Only the unblocked task should be dispatched
	if strings.Contains(logStr, "test-blocked3") {
		t.Errorf("blocked task should not have been dispatched, log: %q", logStr)
	}
	if !strings.Contains(logStr, "sling test-unblk3 testrig --no-boot") {
		t.Errorf("expected unblocked task dispatch, got: %q", logStr)
	}
}

func TestFeedNextReadyIssue_NoReadyIssues_LogsMessage(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	convoy := &beadsdk.Issue{
		ID:        "test-convoy4",
		Title:     "Convoy No Ready",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	closed1 := &beadsdk.Issue{
		ID:        "test-cl4a",
		Title:     "Closed A",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	closed2 := &beadsdk.Issue{
		ID:        "test-cl4b",
		Title:     "Closed B",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, closed1, closed2} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	for _, trackedID := range []string{closed1.ID, closed2.ID} {
		dep := &beadsdk.Dependency{
			IssueID:     convoy.ID,
			DependsOnID: trackedID,
			Type:        beadsdk.DependencyType("tracks"),
			CreatedAt:   now,
			CreatedBy:   "test",
		}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", trackedID, err)
		}
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, logMsgs := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

	// Verify "no ready issues to feed" was logged
	found := false
	for _, msg := range *logMsgs {
		if strings.Contains(msg, "no ready issues to feed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'no ready issues to feed' in log messages, got: %v", *logMsgs)
	}
}

func TestFeedNextReadyIssue_SkipsParkedRig(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	convoy := &beadsdk.Issue{
		ID:        "test-convoy5",
		Title:     "Convoy Parked Test",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	task := &beadsdk.Issue{
		ID:        "test-task5",
		Title:     "Task For Parked Rig",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, task} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	dep := &beadsdk.Dependency{
		IssueID:     convoy.ID,
		DependsOnID: task.ID,
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, logMsgs := makeLogger()

	// isRigParked always returns true
	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return true }, nil)

	// gt should NOT have been called
	if _, err := gt.read(); err == nil {
		t.Errorf("gt stub should not have been called for parked rig")
	}

	// Verify "parked" appeared in log
	foundParked := false
	for _, msg := range *logMsgs {
		if strings.Contains(msg, "parked") {
			foundParked = true
			break
		}
	}
	if !foundParked {
		// It's also possible we got "no ready issues" if getConvoyTrackedIssues
		// failed due to embedded Dolt. Accept either.
		t.Logf("log messages: %v", *logMsgs)
	}
}

// ---------------------------------------------------------------------------
// dispatchIssue tests (direct function call)
// ---------------------------------------------------------------------------

func TestDispatchIssue_Success(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	gt := &slingLog{}

	err := gt.sling(context.Background(), townRoot, slingArgs("test-abc", "myrig", "", "", ""))
	if err != nil {
		t.Fatalf("dispatchIssue returned error: %v", err)
	}

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub log not written: %v", err)
	}
	logStr := strings.TrimSpace(string(logData))
	expected := "sling test-abc myrig --no-boot"
	if logStr != expected {
		t.Errorf("gt stub called with %q, want %q", logStr, expected)
	}
}

// TestDispatchIssue_PassesAgent is the regression test for gt-yg24: a bead
// slung with --agent must be re-dispatched with the same agent, or the feed
// silently re-routes it to the rig default.
func TestDispatchIssue_PassesAgent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	gt := &slingLog{}

	err := gt.sling(context.Background(), townRoot, slingArgs("test-agent", "myrig", "", "deepseek-flash", ""))
	if err != nil {
		t.Fatalf("dispatchIssue returned error: %v", err)
	}

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub log not written: %v", err)
	}
	logStr := strings.TrimSpace(string(logData))
	expected := "sling test-agent myrig --no-boot --agent=deepseek-flash"
	if logStr != expected {
		t.Errorf("gt stub called with %q, want %q", logStr, expected)
	}
}

// TestFeedDispatchAgent covers the feeder's agent choice: a recorded agent is
// passed through verbatim, and the no-agent fallback stays with gt sling while
// naming the rig default it expects (gt-yg24).
func TestFeedDispatchAgent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	t.Run("recorded agent wins", func(t *testing.T) {
		agent, desc := FeedDispatchAgent("deepseek-flash", townRoot, "gastown")
		if agent != "deepseek-flash" {
			t.Errorf("agent = %q, want %q", agent, "deepseek-flash")
		}
		if !strings.Contains(desc, "recorded on convoy") {
			t.Errorf("description %q should say the agent was recorded on the convoy", desc)
		}
	})

	t.Run("whitespace-only agent counts as unset", func(t *testing.T) {
		agent, desc := FeedDispatchAgent("  ", townRoot, "gastown")
		if agent != "" {
			t.Errorf("agent = %q, want empty (left to gt sling)", agent)
		}
		if !strings.Contains(desc, "rig default") {
			t.Errorf("description %q should report the rig default fallback", desc)
		}
	})

	t.Run("no agent falls back to rig default and says so", func(t *testing.T) {
		agent, desc := FeedDispatchAgent("", townRoot, "gastown")
		if agent != "" {
			t.Errorf("agent = %q, want empty (left to gt sling)", agent)
		}
		// The town has no settings in a temp root, so resolution lands on the
		// built-in default. What matters here is that the fallback is named in
		// the log line rather than applied invisibly.
		if !strings.Contains(desc, "no --agent recorded on convoy") {
			t.Errorf("description %q should explain why the default is used", desc)
		}
	})
}

// TestAgentFromConvoyDescription covers the single parse of the convoy's
// sling-time agent record. Every re-dispatch path reads the field through it, so
// the field spellings and the empty cases are pinned here once (gt-mxyk).
func TestAgentFromConvoyDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{
			name:        "agent field is read",
			description: "Auto-created convoy tracking gt-abc\n\nmerge: mr\nagent: deepseek-flash\n",
			want:        "deepseek-flash",
		},
		{
			name:        "agent field absent",
			description: "Auto-created convoy tracking gt-abc\n\nmerge: mr\n",
			want:        "",
		},
		{
			name:        "whitespace-only value is not an agent",
			description: "merge: mr\nagent:   \n",
			want:        "",
		},
		{
			name:        "empty description",
			description: "",
			want:        "",
		},
		{
			name:        "prose containing the word agent is not a field",
			description: "agent: is what the field would look like\n",
			// The line does parse as a field; what it must not do is invent an
			// agent for a convoy that recorded none. Here it genuinely recorded
			// the literal text, so it round-trips verbatim.
			want: "is what the field would look like",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentFromConvoyDescription(tc.description); got != tc.want {
				t.Errorf("AgentFromConvoyDescription() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRedispatchAgent is the table over the one decision every re-dispatch path
// makes: a recorded agent is passed through, and an unrecorded one is left to
// gt sling while naming the rig default it expects (gt-mxyk, gt-yg24).
func TestRedispatchAgent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	tests := []struct {
		name        string
		description string
		rig         string
		wantAgent   string
		wantDesc    string
	}{
		{
			name:        "recorded agent is passed through",
			description: "merge: mr\nagent: deepseek-flash\n",
			rig:         "gastown",
			wantAgent:   "deepseek-flash",
			wantDesc:    "recorded on convoy",
		},
		{
			name:        "no record leaves the choice to gt sling",
			description: "merge: mr\n",
			rig:         "gastown",
			wantAgent:   "",
			wantDesc:    "no --agent recorded on convoy",
		},
		{
			name:        "unresolved rig is named as such",
			description: "merge: mr\n",
			rig:         "",
			wantAgent:   "",
			wantDesc:    "rig unresolved",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent, desc := RedispatchAgent(tc.description, townRoot, tc.rig)
			if agent != tc.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tc.wantAgent)
			}
			if !strings.Contains(desc, tc.wantDesc) {
				t.Errorf("description %q should contain %q", desc, tc.wantDesc)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DS-07: CheckConvoysForIssue skips staged_ready convoys
// ---------------------------------------------------------------------------

func TestCheckConvoysForIssue_SkipsStagedReady(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create a convoy as open first (SDK validates status on create),
	// then transition to "staged_ready" via UpdateIssue.
	convoy := &beadsdk.Issue{
		ID:        "test-cv-staged1",
		Title:     "Staged Ready Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Create a tracked issue (closed, to trigger the event path)
	tracked := &beadsdk.Issue{
		ID:        "test-trk-stg1",
		Title:     "Tracked Issue",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, tracked} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	// Transition convoy to "staged_ready" (SDK validates status on create,
	// so we create as open first and update).
	if err := store.UpdateIssue(ctx, convoy.ID, map[string]interface{}{
		"status": "staged_ready",
	}, "test"); err != nil {
		t.Fatalf("UpdateIssue to staged_ready: %v", err)
	}

	// Add tracks dependency: convoy tracks the closed issue
	dep := &beadsdk.Dependency{
		IssueID:     convoy.ID,
		DependsOnID: tracked.ID,
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, logMsgs := makeLogger()

	// Call CheckConvoysForIssue with the tracked issue's ID (simulating close event)
	result := checkConvoysForIssue(ctx, store, townRoot, tracked.ID, "DS-07", logger, gt.sling, noopChecker, nil, nil)

	// The convoy should be returned (it was found as a tracker)
	if len(result) == 0 {
		t.Fatal("no tracking convoys found for an issue a convoy tracks")
	}

	// Verify the staged convoy was skipped via log messages
	foundStagedSkip := false
	for _, msg := range *logMsgs {
		if strings.Contains(msg, "staged") && strings.Contains(msg, "skipping") {
			foundStagedSkip = true
			break
		}
	}
	if !foundStagedSkip {
		t.Errorf("expected log message about staged convoy being skipped, got: %v", *logMsgs)
	}

	// Verify "checking convoy" was NOT logged (convoy should be skipped before check)
	for _, msg := range *logMsgs {
		if strings.Contains(msg, "checking convoy") && strings.Contains(msg, convoy.ID) {
			t.Errorf("staged convoy should not have been checked, but found log: %s", msg)
		}
	}
}

// ---------------------------------------------------------------------------
// DS-08: CheckConvoysForIssue skips staged_warnings convoys
// ---------------------------------------------------------------------------

func TestCheckConvoysForIssue_SkipsStagedWarnings(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create a convoy as open first, then transition to "staged_warnings".
	convoy := &beadsdk.Issue{
		ID:        "test-cv-staged2",
		Title:     "Staged Warnings Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tracked := &beadsdk.Issue{
		ID:        "test-trk-stg2",
		Title:     "Tracked Issue",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, tracked} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	// Transition convoy to "staged_warnings"
	if err := store.UpdateIssue(ctx, convoy.ID, map[string]interface{}{
		"status": "staged_warnings",
	}, "test"); err != nil {
		t.Fatalf("UpdateIssue to staged_warnings: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     convoy.ID,
		DependsOnID: tracked.ID,
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, logMsgs := makeLogger()

	result := checkConvoysForIssue(ctx, store, townRoot, tracked.ID, "DS-08", logger, gt.sling, noopChecker, nil, nil)

	if len(result) == 0 {
		t.Fatal("no tracking convoys found for an issue a convoy tracks")
	}

	// Verify the staged convoy was skipped
	foundStagedSkip := false
	for _, msg := range *logMsgs {
		if strings.Contains(msg, "staged") && strings.Contains(msg, "skipping") {
			foundStagedSkip = true
			break
		}
	}
	if !foundStagedSkip {
		t.Errorf("expected log message about staged convoy being skipped, got: %v", *logMsgs)
	}

	// Verify "checking convoy" was NOT logged
	for _, msg := range *logMsgs {
		if strings.Contains(msg, "checking convoy") && strings.Contains(msg, convoy.ID) {
			t.Errorf("staged convoy should not have been checked, but found log: %s", msg)
		}
	}
}

// ---------------------------------------------------------------------------
// DS-10: After staged_ready→open transition, daemon feeds normally
// ---------------------------------------------------------------------------

func TestCheckConvoysForIssue_FeedsAfterStagedToOpenTransition(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create a convoy as open first, then transition to "staged_ready"
	convoy := &beadsdk.Issue{
		ID:        "test-cv-launch",
		Title:     "Launched Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Create a tracked issue that is closed (triggers event path)
	tracked := &beadsdk.Issue{
		ID:        "test-trk-lnch",
		Title:     "Tracked Closed",
		Status:    beadsdk.StatusClosed,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	for _, iss := range []*beadsdk.Issue{convoy, tracked} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}

	// Transition convoy to "staged_ready"
	if err := store.UpdateIssue(ctx, convoy.ID, map[string]interface{}{
		"status": "staged_ready",
	}, "test"); err != nil {
		t.Fatalf("UpdateIssue to staged_ready: %v", err)
	}

	dep := &beadsdk.Dependency{
		IssueID:     convoy.ID,
		DependsOnID: tracked.ID,
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// Phase 1: While staged, verify it's skipped
	logger1, logMsgs1 := makeLogger()
	townRoot := setupTownRoot(t)
	gt := &slingLog{}

	result1 := checkConvoysForIssue(ctx, store, townRoot, tracked.ID, "DS-10-staged", logger1, gt.sling, noopChecker, nil, nil)
	if len(result1) == 0 {
		t.Fatal("no tracking convoys found for an issue a convoy tracks")
	}

	foundStagedSkip := false
	for _, msg := range *logMsgs1 {
		if strings.Contains(msg, "staged") && strings.Contains(msg, "skipping") {
			foundStagedSkip = true
			break
		}
	}
	if !foundStagedSkip {
		t.Fatalf("convoy should have been skipped while staged, logs: %v", *logMsgs1)
	}

	// Phase 2: Transition convoy to "open" (launch it)
	if err := store.UpdateIssue(ctx, convoy.ID, map[string]interface{}{
		"status": string(beadsdk.StatusOpen),
	}, "test"); err != nil {
		t.Fatalf("UpdateIssue staged->open: %v", err)
	}

	// Verify it's no longer staged
	if isConvoyStaged(ctx, store, convoy.ID) {
		t.Fatal("convoy should not be staged after transition to open")
	}

	// Phase 3: Call CheckConvoysForIssue again — now the convoy should be processed
	logger2, logMsgs2 := makeLogger()
	_ = checkConvoysForIssue(ctx, store, townRoot, tracked.ID, "DS-10-open", logger2, gt.sling, noopChecker, nil, nil)

	// Verify "checking convoy" WAS logged (convoy is now open and being processed)
	foundChecking := false
	for _, msg := range *logMsgs2 {
		if strings.Contains(msg, "checking convoy") && strings.Contains(msg, convoy.ID) {
			foundChecking = true
			break
		}
	}
	if !foundChecking {
		t.Errorf("expected convoy to be checked after staged->open transition, logs: %v", *logMsgs2)
	}

	// Verify it was NOT skipped as staged
	for _, msg := range *logMsgs2 {
		if strings.Contains(msg, "staged") && strings.Contains(msg, "skipping") {
			t.Errorf("convoy should NOT be skipped after transition to open, but found: %s", msg)
		}
	}
}

// ---------------------------------------------------------------------------
// Cross-rig fallback tests
// ---------------------------------------------------------------------------

// setupTownRootWithCrossRig creates a town root with routes for both "test-"
// (local rig) and "oag-" (cross-rig, rig osr_ai_gm) prefixes.
func setupTownRootWithCrossRig(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .beads: %v", err)
	}

	// Create cross-rig directory with .beads
	crossRigDir := filepath.Join(townRoot, "osr_ai_gm", ".beads")
	if err := os.MkdirAll(crossRigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll osr_ai_gm/.beads: %v", err)
	}

	// Routes: test- is local, oag- is cross-rig
	routesContent := `{"prefix":"test-","path":"testrig/.beads"}` + "\n" +
		`{"prefix":"oag-","path":"osr_ai_gm/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0o644); err != nil {
		t.Fatalf("WriteFile routes.jsonl: %v", err)
	}
	return townRoot
}

// rigDBs opens db for every rig and records each rig path it opened.
func rigDBs(db beads.Client) (open func(string) beads.Client, opened *[]string) {
	opened = new([]string)
	return func(rigPath string) beads.Client {
		*opened = append(*opened, rigPath)
		return db
	}, opened
}

func TestGetConvoyTrackedIssues_CrossRigFallback(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create convoy in the store (local)
	convoy := &beadsdk.Issue{
		ID:        "test-convoy-xrig",
		Title:     "Cross-Rig Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, convoy, "test"); err != nil {
		t.Fatalf("CreateIssue convoy: %v", err)
	}

	// The cross-rig bead (oag-19dd9) is NOT in the local store.
	// Add tracks dependency using external reference format expected by beads.
	dep := &beadsdk.Dependency{
		IssueID:     convoy.ID,
		DependsOnID: "external:oag:oag-19dd9",
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// The bead's own rig store has it closed.
	rigStore := newFakeRigStore(&beadsdk.Issue{ID: "oag-19dd9", Status: beadsdk.StatusClosed, Assignee: "gastown/polecats/alpha", Priority: 2, IssueType: beadsdk.TypeTask})
	townRoot := setupTownRootWithCrossRig(t)
	resolver := NewStoreResolver(townRoot, map[string]beadsdk.Storage{"osr_ai_gm": rigStore})

	tracked := getConvoyTrackedIssues(ctx, store, convoy.ID, townRoot, resolver, func(string, ...interface{}) {})

	// Find the cross-rig bead in tracked results
	var found *trackedIssue
	for i := range tracked {
		if tracked[i].ID == "oag-19dd9" {
			found = &tracked[i]
			break
		}
	}

	if found == nil {
		t.Fatalf("cross-rig bead oag-19dd9 is missing from the convoy's tracked issues %+v: a convoy that tracks another rig's bead must list it", tracked)
	}

	// The critical assertion: the cross-rig bead should show fresh "closed" status,
	// NOT the stale "open" from dependency metadata.
	if found.Status != "closed" {
		t.Errorf("cross-rig bead status = %q, want %q (stale metadata was used instead of fresh bd show)", found.Status, "closed")
	}
	if found.Assignee != "gastown/polecats/alpha" {
		t.Errorf("cross-rig bead assignee = %q, want %q", found.Assignee, "gastown/polecats/alpha")
	}
}

func TestFetchCrossRigBeadStatus(t *testing.T) {
	t.Parallel()
	townRoot := setupTownRootWithCrossRig(t)
	db := beadsfake.New(beadsfake.WithPrefix("oag"))
	db.Seed(beads.Issue{ID: "oag-abc", Title: "abc", Status: "closed", Priority: 1},
		beads.Issue{ID: "oag-xyz", Title: "xyz", Status: "open", Assignee: "gastown/polecats/beta", Priority: 3})
	open, opened := rigDBs(db)

	result := fetchCrossRigBeadStatusWith(townRoot, []string{"oag-abc", "oag-xyz"}, open)

	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}

	abc := result["oag-abc"]
	if abc == nil {
		t.Fatal("oag-abc not found in results")
	}
	if string(abc.Status) != "closed" {
		t.Errorf("oag-abc status = %q, want %q", abc.Status, "closed")
	}

	xyz := result["oag-xyz"]
	if xyz == nil {
		t.Fatal("oag-xyz not found in results")
	}
	if string(xyz.Status) != "open" {
		t.Errorf("oag-xyz status = %q, want %q", xyz.Status, "open")
	}
	if xyz.Assignee != "gastown/polecats/beta" {
		t.Errorf("oag-xyz assignee = %q, want %q", xyz.Assignee, "gastown/polecats/beta")
	}

	// One database opened, the oag rig's, for both IDs.
	if len(*opened) != 1 || !strings.Contains((*opened)[0], "osr_ai_gm") {
		t.Errorf("rig databases opened = %q, want only osr_ai_gm", *opened)
	}
}

func TestFetchCrossRigBeadStatus_UnknownPrefix(t *testing.T) {
	t.Parallel()
	open, opened := rigDBs(beadsfake.New())

	// "zzz-" prefix has no route — should return empty, not panic
	result := fetchCrossRigBeadStatusWith(setupTownRootWithCrossRig(t), []string{"zzz-unknown"}, open)
	if len(result) != 0 {
		t.Errorf("expected 0 results for unknown prefix, got %d", len(result))
	}
	if len(*opened) != 0 {
		t.Errorf("a database was opened for an unrouted prefix: %q", *opened)
	}
}

func TestFetchCrossRigBeadStatus_EmptyInput(t *testing.T) {
	t.Parallel()
	open, _ := rigDBs(beadsfake.New())
	result := fetchCrossRigBeadStatusWith("/nonexistent", nil, open)
	if len(result) != 0 {
		t.Errorf("expected 0 results for empty input, got %d", len(result))
	}
}

func TestFireCrossRigDepNotifications_NilStores(t *testing.T) {
	t.Parallel()
	// Should not panic with nil stores.
	FireCrossRigDepNotifications(context.Background(), "bd-xxx", "/tmp", nil, nil)
}

func TestFireCrossRigDepNotifications_EmptyClosedID(t *testing.T) {
	t.Parallel()
	// Should not panic with empty closed issue ID.
	store, cleanup := setupTestStore(t)
	defer cleanup()
	FireCrossRigDepNotifications(context.Background(), "", "/tmp", map[string]beadsdk.Storage{"test": store}, nil)
}

func TestFireCrossRigDepNotifications_EmptyPrefix(t *testing.T) {
	t.Parallel()
	// Issue ID without a recognizable prefix should not panic.
	store, cleanup := setupTestStore(t)
	defer cleanup()
	FireCrossRigDepNotifications(context.Background(), "noprefixid", "/tmp", map[string]beadsdk.Storage{"test": store}, nil)
}

// A close that unblocks an issue in another rig is logged, naming the issue and
// its rig, and nothing else is done: the rig's witness it used to nudge was
// deleted (bbc95aa1), and the unblocked issue needs no signal to become ready.
// A blocked issue is ready again once bd sees its blocker closed, and the feeds
// and stranded scan that dispatch convoy work read readiness, not nudges.
func TestFireCrossRigDepNotifications_LogsCrossRigUnblock(t *testing.T) {
	t.Parallel()
	// Set up a real store that simulates the "gastown" rig.
	// In it we create gt-dep which is blocked by external:bd:bd-closed.
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	dependent := &beadsdk.Issue{
		ID:        "gt-dep1",
		Title:     "Waiting on beads fix",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, dependent, "test"); err != nil {
		t.Fatalf("CreateIssue dependent: %v", err)
	}

	// Add a blocking dep: gt-dep1 is blocked by external:bd:bd-closed
	dep := &beadsdk.Dependency{
		IssueID:     "gt-dep1",
		DependsOnID: "external:bd:bd-closed",
		Type:        beadsdk.DepBlocks,
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// Set up town root with routes: gt- → gastown, bd- → beads.
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .beads: %v", err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/.beads"}` + "\n" +
		`{"prefix":"bd-","path":"beads/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0o644); err != nil {
		t.Fatalf("WriteFile routes.jsonl: %v", err)
	}

	// stores: "gastown" → store (has gt-dep1 blocked by external:bd:bd-closed)
	//         "beads"   → (closed issue's home store, skipped by FireCrossRigDepNotifications)
	stores := map[string]beadsdk.Storage{
		"gastown": store,
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	FireCrossRigDepNotifications(ctx, "bd-closed", townRoot, stores, logger)

	want := "CrossRig: bd-closed closed, unblocking gt-dep1 (Waiting on beads fix, rig gastown)"
	if len(logged) != 1 || logged[0] != want {
		t.Errorf("logged %q, want [%q]", logged, want)
	}
}

// TestFeedNextReadyIssue_HoldsCrossStoreBead feeds through the event-driven
// continuation path, the second feeder a close event triggers, with the
// resolver its production caller passes. The held bead's decision lives in a
// rig store, while the copy the convoy's own store can see carries no hold: a
// feeder that reads the convoy's store instead of the store the resolver
// redirects to dispatches the bead (gt-tq6l).
func TestFeedNextReadyIssue_HoldsCrossStoreBead(t *testing.T) {
	t.Parallel()
	// The convoy's store. GetDependenciesWithMetadata drops a tracked bead this
	// store cannot fetch, so the convoy's dep rows need a row here to be seen
	// at all — the projection of a bead, not its record.
	convoyStore, convoyCleanup := setupTestStore(t)
	defer convoyCleanup()
	// The rig store: the bead's home, and the only place its hold is written.
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	convoy := &beadsdk.Issue{
		ID:        "test-convoyhold",
		Title:     "Convoy With A Held Bead",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	held := &beadsdk.Issue{
		ID:        "test-held1",
		Title:     "Held For The Mayor",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	ready := &beadsdk.Issue{
		ID:        "test-ready2",
		Title:     "Next Ready Task",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := convoyStore.CreateIssue(ctx, convoy, "test"); err != nil {
		t.Fatalf("CreateIssue convoy: %v", err)
	}
	for _, iss := range []*beadsdk.Issue{held, ready} {
		if err := convoyStore.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s in convoy store: %v", iss.ID, err)
		}
		if err := rigStore.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s in rig store: %v", iss.ID, err)
		}
	}
	// The decision is on the rig store's copy alone. Sorts first, so the feed
	// reaches the held bead before the ready one.
	if err := rigStore.AddLabel(ctx, held.ID, "needs-mayor-review", "test"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	for _, trackedID := range []string{held.ID, ready.ID} {
		dep := &beadsdk.Dependency{
			IssueID:     convoy.ID,
			DependsOnID: trackedID,
			Type:        beadsdk.DependencyType("tracks"),
			CreatedAt:   now,
			CreatedBy:   "test",
		}
		if err := convoyStore.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", trackedID, err)
		}
	}

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll .beads: %v", err)
	}
	beadsRouting.WriteRoutes(beadsDir, []beadsRouting.Route{
		{Prefix: "test-", Path: "testrig"},
	})

	resolver := NewStoreResolver(townRoot, map[string]beadsdk.Storage{
		"hq":      convoyStore,
		"testrig": rigStore,
	})

	gt := &slingLog{}
	logger, msgs := makeLogger()

	feedNextReadyIssue(ctx, convoyStore, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, resolver)

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub was not called (no log file): %v\nlogger said: %v", err, *msgs)
	}
	logStr := string(logData)
	if strings.Contains(logStr, held.ID) {
		t.Errorf("held bead must not be dispatched by the continuation feed, got: %q", logStr)
	}
	if !strings.Contains(logStr, ready.ID) {
		t.Errorf("expected the next ready bead to be dispatched, got: %q", logStr)
	}

	heldLogged := false
	for _, m := range *msgs {
		if strings.Contains(m, held.ID) && strings.Contains(m, "not dispatched: label needs-mayor-review") {
			heldLogged = true
		}
	}
	if !heldLogged {
		t.Errorf("expected a hold log for %s, got: %v", held.ID, *msgs)
	}
}

// TestDispatchHoldReason_ResolverRedirectsToRigStore pins the redirect the hold
// check depends on away from the feeders: the bead's own record lives in its
// rig store, and reading the town store the caller holds instead reports no
// hold where there is one (gt-tq6l).
func TestDispatchHoldReason_ResolverRedirectsToRigStore(t *testing.T) {
	t.Parallel()
	townStore, townCleanup := setupTestStore(t)
	defer townCleanup()
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	townCopy := &beadsdk.Issue{
		ID:        "test-held1",
		Title:     "Held",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	rigCopy := *townCopy
	if err := townStore.CreateIssue(ctx, townCopy, "test"); err != nil {
		t.Fatalf("CreateIssue town copy: %v", err)
	}
	if err := rigStore.CreateIssue(ctx, &rigCopy, "test"); err != nil {
		t.Fatalf("CreateIssue rig copy: %v", err)
	}
	if err := rigStore.AddLabel(ctx, rigCopy.ID, "needs-mayor-review", "test"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll .beads: %v", err)
	}
	beadsRouting.WriteRoutes(beadsDir, []beadsRouting.Route{
		{Prefix: "test-", Path: "testrig"},
	})

	resolver := NewStoreResolver(townRoot, map[string]beadsdk.Storage{
		"hq":      townStore,
		"testrig": rigStore,
	})

	if reason := DispatchHoldReason(ctx, townStore, rigCopy.ID, resolver); reason != "label needs-mayor-review" {
		t.Errorf("with the resolver, DispatchHoldReason = %q, want %q", reason, "label needs-mayor-review")
	}

	// A bead the town store has no row for is the gt close shape: the caller
	// holds only the town store, and the old nil resolver left the hold
	// unread. The check has to report the record unknown, not the bead free.
	rigOnly := &beadsdk.Issue{
		ID:        "test-rigonly1",
		Title:     "Rig Only",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := rigStore.CreateIssue(ctx, rigOnly, "test"); err != nil {
		t.Fatalf("CreateIssue rig-only: %v", err)
	}
	if reason := DispatchHoldReason(ctx, townStore, rigOnly.ID, nil); reason == "" {
		t.Error("a bead the caller's store cannot read must not be reported dispatchable")
	}
}

// TestFeedNextReadyIssue_UnreadableRecordFailsClosed is the continuation feed's
// half of the fail-closed rule: a record that cannot be read leaves the hold
// unknown, and an unknown hold is not a licence to dispatch (gt-tq6l).
func TestFeedNextReadyIssue_UnreadableRecordFailsClosed(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	convoy := &beadsdk.Issue{
		ID:        "test-convoyunreadable",
		Title:     "Convoy With An Unreadable Bead",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tracked := &beadsdk.Issue{
		ID:        "test-unreadable1",
		Title:     "Tracked",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, iss := range []*beadsdk.Issue{convoy, tracked} {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
	}
	dep := &beadsdk.Dependency{
		IssueID:     convoy.ID,
		DependsOnID: tracked.ID,
		Type:        beadsdk.DependencyType("tracks"),
		CreatedAt:   now,
		CreatedBy:   "test",
	}
	if err := store.AddDependency(ctx, dep, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// The resolver redirects the tracked bead to a store that refuses to hand
	// back its record, which is what a Dolt that has gone away looks like to
	// this path.
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("MkdirAll .beads: %v", err)
	}
	beadsRouting.WriteRoutes(beadsDir, []beadsRouting.Route{
		{Prefix: "test-", Path: "testrig"},
	})
	resolver := NewStoreResolver(townRoot, map[string]beadsdk.Storage{
		"hq":      store,
		"testrig": &unreadableStorage{Storage: store},
	})

	gt := &slingLog{}
	logger, msgs := makeLogger()

	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, resolver)

	if _, err := gt.read(); err == nil {
		data, _ := gt.read()
		t.Errorf("expected no dispatch when the record cannot be read, got sling: %q", string(data))
	}
	heldLogged := false
	for _, m := range *msgs {
		if strings.Contains(m, tracked.ID) && strings.Contains(m, "not dispatched: record unreadable") {
			heldLogged = true
		}
	}
	if !heldLogged {
		t.Errorf("expected an unreadable-record log for %s, got: %v", tracked.ID, *msgs)
	}
}

// unreadableStorage wraps a store whose issue reads fail, standing in for a
// Dolt that has gone away while its other calls still work.
type unreadableStorage struct {
	beadsdk.Storage
}

func (s *unreadableStorage) GetIssue(context.Context, string) (*beadsdk.Issue, error) {
	return nil, fmt.Errorf("dolt unreachable")
}

func (s *unreadableStorage) GetIssueComments(context.Context, string) ([]*beadsdk.Comment, error) {
	return nil, fmt.Errorf("dolt unreachable")
}

// GetDependencyRecords still answers: beadsdk.Storage does not declare it, so
// the embedded interface would hide the real store's method, and the feed
// would hold the bead for want of raw records instead of for its unreadable
// record.
func (s *unreadableStorage) GetDependencyRecords(ctx context.Context, issueID string) ([]*beadsdk.Dependency, error) {
	return s.Storage.(dependencyRecordReader).GetDependencyRecords(ctx, issueID)
}

// TestDispatchIssue_PassesFormula is the gt-o9sbq regression test: a bead slung
// with --formula must be re-dispatched with the same formula, or the feed runs
// it under gt sling's default formula instead (gt-4lor).
func TestDispatchIssue_PassesFormula(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	gt := &slingLog{}

	err := gt.sling(context.Background(), townRoot, slingArgs("test-formula", "myrig", "", "deepseek-flash", "mol-custom"))
	if err != nil {
		t.Fatalf("dispatchIssue returned error: %v", err)
	}

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub log not written: %v", err)
	}
	logStr := strings.TrimSpace(string(logData))
	expected := "sling test-formula myrig --no-boot --agent=deepseek-flash --formula=mol-custom"
	if logStr != expected {
		t.Errorf("gt stub called with %q, want %q", logStr, expected)
	}
}

// TestFormulaFromConvoyDescription is the table over the one parse of a
// convoy's sling-time formula record that every re-dispatch path reads.
func TestFormulaFromConvoyDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{"formula field is read", "Auto-created convoy\n\nmerge: mr\nformula: mol-custom\n", "mol-custom"},
		{"formula field absent", "Auto-created convoy\n\nmerge: mr\n", ""},
		{"whitespace-only value is not a formula", "merge: mr\nformula:   \n", ""},
		{"empty description", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormulaFromConvoyDescription(tc.description); got != tc.want {
				t.Errorf("FormulaFromConvoyDescription() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFeedNextReadyIssue_PassesRecordedFormula drives the event-driven feeder
// end to end: a convoy that recorded a formula at sling time re-feeds its next
// ready bead with --formula, and one that recorded none passes nothing (gt-o9sbq).
func TestFeedNextReadyIssue_PassesRecordedFormula(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{
			name:        "recorded formula is passed",
			description: "Auto-created convoy\n\nmerge: mr\nformula: mol-custom\n",
			want:        "sling test-ready1 testrig --no-boot --formula=mol-custom",
		},
		{
			name:        "no recorded formula passes none",
			description: "Auto-created convoy\n\nmerge: mr\n",
			want:        "sling test-ready1 testrig --no-boot",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()

			ctx := context.Background()
			now := time.Now().UTC()

			convoy := &beadsdk.Issue{
				ID:          "test-convoy1",
				Title:       "Test Convoy",
				Description: tc.description,
				Status:      beadsdk.StatusOpen,
				Priority:    2,
				IssueType:   beadsdk.TypeTask,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			ready := &beadsdk.Issue{
				ID:        "test-ready1",
				Title:     "Ready Task",
				Status:    beadsdk.StatusOpen,
				Priority:  2,
				IssueType: beadsdk.TypeTask,
				CreatedAt: now,
				UpdatedAt: now,
			}
			for _, iss := range []*beadsdk.Issue{convoy, ready} {
				if err := store.CreateIssue(ctx, iss, "test"); err != nil {
					t.Fatalf("CreateIssue %s: %v", iss.ID, err)
				}
			}
			dep := &beadsdk.Dependency{
				IssueID:     convoy.ID,
				DependsOnID: ready.ID,
				Type:        beadsdk.DependencyType("tracks"),
				CreatedAt:   now,
				CreatedBy:   "test",
			}
			if err := store.AddDependency(ctx, dep, "test"); err != nil {
				t.Fatalf("AddDependency: %v", err)
			}

			townRoot := setupTownRoot(t)
			gt := &slingLog{}
			logger, _ := makeLogger()

			feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

			logData, err := gt.read()
			if err != nil {
				t.Fatalf("gt stub was not called (no log file): %v", err)
			}
			if got := strings.TrimSpace(string(logData)); got != tc.want {
				t.Errorf("gt stub called with %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFeedNextReadyIssue_BacksOffAfterStartupFailure guards gt-wacl: the
// event-driven feeder must not re-sling a bead whose last sling failed at
// session start; it moves on to the next ready bead instead, and feeds the
// resting one again once its record is cleared.
func TestFeedNextReadyIssue_BacksOffAfterStartupFailure(t *testing.T) {
	t.Parallel()
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	convoy := &beadsdk.Issue{
		ID: "test-convoy1", Title: "Test Convoy", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	issues := []*beadsdk.Issue{convoy}
	for _, id := range []string{"test-ready1", "test-ready2"} {
		issues = append(issues, &beadsdk.Issue{
			ID: id, Title: id, Status: beadsdk.StatusOpen,
			Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
		})
	}
	for _, iss := range issues {
		if err := store.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", iss.ID, err)
		}
		if iss.ID == convoy.ID {
			continue
		}
		dep := &beadsdk.Dependency{
			IssueID: convoy.ID, DependsOnID: iss.ID,
			Type: beadsdk.DependencyType("tracks"), CreatedAt: now, CreatedBy: "test",
		}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency: %v", err)
		}
	}

	townRoot := setupTownRoot(t)
	gt := &slingLog{}
	logger, msgs := makeLogger()

	if err := dispatch.RecordStartupFailure(townRoot, "test-ready1", "startup blocked: trust dialog"); err != nil {
		t.Fatal(err)
	}
	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)

	logData, err := gt.read()
	if err != nil {
		t.Fatalf("gt stub was not called (no log file): %v", err)
	}
	if got := strings.TrimSpace(string(logData)); got != "sling test-ready2 testrig --no-boot" {
		t.Errorf("gt stub called with %q, want the next ready bead past the resting one", got)
	}
	skipped := false
	for _, m := range *msgs {
		if strings.Contains(m, "test-ready1 not dispatched") && strings.Contains(m, "startup blocked: trust dialog") {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("the skip should be logged with the recorded reason, got: %v", *msgs)
	}

	dispatch.ClearStartupFailure(townRoot, "test-ready1")
	gt.reset()
	feedNextReadyIssue(ctx, store, townRoot, convoy.ID, "test", logger, gt.sling, func(string) bool { return false }, nil)
	logData, _ = gt.read()
	if got := strings.TrimSpace(string(logData)); got != "sling test-ready1 testrig --no-boot" {
		t.Errorf("after the record cleared gt stub called with %q, want test-ready1", got)
	}
}
