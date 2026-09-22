package deacon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestAssigneeToSessionName(t *testing.T) {
	tests := []struct {
		assignee string
		want     string
	}{
		{"deacon", "hq-deacon"},
		{"mayor", "hq-mayor"},
		{"gastown/witness", "gt-witness"},
		{"gastown/refinery", "gt-refinery"},
		{"gastown/polecats/max", "gt-max"},
		{"gastown/crew/joe", "gt-crew-joe"},
		{"", ""},
		{"unknown", ""},
		{"gastown/unknown/agent", ""},
		{"a/b/c/d", ""},
	}

	for _, tt := range tests {
		t.Run(tt.assignee, func(t *testing.T) {
			got := assigneeToSessionName(tt.assignee)
			if got != tt.want {
				t.Errorf("assigneeToSessionName(%q) = %q, want %q", tt.assignee, got, tt.want)
			}
		})
	}
}

func TestAssigneeToWorktreePath_InvalidFormats(t *testing.T) {
	townRoot := t.TempDir()

	tests := []struct {
		name     string
		assignee string
	}{
		{"empty", ""},
		{"single part", "deacon"},
		{"two parts", "gastown/witness"},
		{"four parts", "a/b/c/d"},
		{"unknown agent type", "gastown/unknown/agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assigneeToWorktreePath(townRoot, tt.assignee)
			if got != "" {
				t.Errorf("assigneeToWorktreePath(%q, %q) = %q, want empty", townRoot, tt.assignee, got)
			}
		})
	}
}

func TestAssigneeToWorktreePath_NewStructure(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"

	// Create new-structure worktree: townRoot/testrig/polecats/max/testrig/
	worktreePath := filepath.Join(townRoot, rigName, "polecats", "max", rigName)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}
	// Create .git file (worktree indicator)
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: /fake"), 0644); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/polecats/max")
	if got != worktreePath {
		t.Errorf("assigneeToWorktreePath() = %q, want %q", got, worktreePath)
	}
}

func TestAssigneeToWorktreePath_OldStructure(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"

	// Create old-structure worktree: townRoot/testrig/polecats/max/
	worktreePath := filepath.Join(townRoot, rigName, "polecats", "max")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}
	// Create .git file (worktree indicator)
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: /fake"), 0644); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/polecats/max")
	if got != worktreePath {
		t.Errorf("assigneeToWorktreePath() = %q, want %q", got, worktreePath)
	}
}

func TestAssigneeToWorktreePath_CrewWorker(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"

	// Create crew worktree: townRoot/testrig/crew/joe/testrig/
	worktreePath := filepath.Join(townRoot, rigName, "crew", "joe", rigName)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: /fake"), 0644); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/crew/joe")
	if got != worktreePath {
		t.Errorf("assigneeToWorktreePath() = %q, want %q", got, worktreePath)
	}
}

func TestAssigneeToWorktreePath_NoWorktree(t *testing.T) {
	townRoot := t.TempDir()

	// Directory exists but no .git -> not a worktree
	dirPath := filepath.Join(townRoot, "testrig", "polecats", "max")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		t.Fatal(err)
	}

	got := assigneeToWorktreePath(townRoot, "testrig/polecats/max")
	if got != "" {
		t.Errorf("assigneeToWorktreePath() = %q, want empty (no .git)", got)
	}
}

func TestCheckWorktreeState_CleanRepo(t *testing.T) {
	// Create a real git repo to test against
	tmpDir := t.TempDir()
	townRoot := tmpDir
	rigName := "testrig"

	worktreePath := filepath.Join(townRoot, rigName, "polecats", "max", rigName)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}

	// Initialize a real git repo
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = worktreePath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	result := &StaleHookResult{}
	checkWorktreeState(townRoot, "testrig/polecats/max", result)

	if result.PartialWork {
		t.Error("expected no partial work for clean repo")
	}
	if result.WorktreeDirty {
		t.Error("expected worktree not dirty for clean repo")
	}
	if result.UnpushedCount != 0 {
		t.Errorf("expected 0 unpushed commits, got %d", result.UnpushedCount)
	}
}

func TestCheckWorktreeState_DirtyRepo(t *testing.T) {
	tmpDir := t.TempDir()
	townRoot := tmpDir
	rigName := "testrig"

	worktreePath := filepath.Join(townRoot, rigName, "polecats", "max", rigName)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatal(err)
	}

	// Initialize git repo with a commit, then add uncommitted file
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = worktreePath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	// Create an uncommitted file
	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatal(err)
	}

	result := &StaleHookResult{}
	checkWorktreeState(townRoot, "testrig/polecats/max", result)

	if !result.PartialWork {
		t.Error("expected partial work for dirty repo")
	}
	if !result.WorktreeDirty {
		t.Error("expected worktree dirty")
	}
}

func TestCheckWorktreeState_InvalidAssignee(t *testing.T) {
	townRoot := t.TempDir()

	result := &StaleHookResult{}
	checkWorktreeState(townRoot, "invalid", result)

	// Should not populate any fields for unresolvable assignee
	if result.PartialWork {
		t.Error("expected no partial work for invalid assignee")
	}
	if result.WorktreeError != "" {
		t.Errorf("expected no worktree error, got %q", result.WorktreeError)
	}
}

func TestCheckWorktreeState_NonexistentPath(t *testing.T) {
	townRoot := t.TempDir()

	result := &StaleHookResult{}
	checkWorktreeState(townRoot, "testrig/polecats/ghost", result)

	// Assignee format is valid but path doesn't exist
	if result.PartialWork {
		t.Error("expected no partial work for nonexistent path")
	}
}

func TestDefaultStaleHookConfig(t *testing.T) {
	cfg := DefaultStaleHookConfig()

	if cfg.MaxAge != 1*60*60*1e9 { // 1 hour in nanoseconds
		t.Errorf("MaxAge = %v, want 1h", cfg.MaxAge)
	}
	if cfg.DryRun {
		t.Error("DryRun should default to false")
	}
}

// setupHookStoreTown builds a town root whose routes.jsonl names one rig with
// a store on disk and one rig without, mirroring the live layout where a rig's
// beads live under <rig>/mayor/rig/.beads. Returns (townRoot, rigBeadsDir).
func setupHookStoreTown(t *testing.T) (string, string) {
	t.Helper()

	townRoot := t.TempDir()
	rigBeadsDir := filepath.Join(townRoot, "testrig", "mayor", "rig", ".beads")
	townBeadsDir := filepath.Join(townRoot, ".beads")

	for _, dir := range []string{townBeadsDir, rigBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "hq-cv-", Path: "."},
		{Prefix: "gt-", Path: "testrig/mayor/rig"},
		{Prefix: "gh-", Path: "ghost/mayor/rig"}, // directory never created
	})
	if err != nil {
		t.Fatal(err)
	}

	return townRoot, rigBeadsDir
}

// hookedIssue builds the beads.Issue shape bd list returns for a hooked bead.
func hookedIssue(id, assignee, updatedAt string) *beads.Issue {
	return &beads.Issue{
		ID:        id,
		Title:     "hooked work",
		Status:    beads.StatusHooked,
		Assignee:  assignee,
		UpdatedAt: updatedAt,
	}
}

// stubHookStores replaces the store query and reset hooks for one test,
// returning a pointer to the stores each reset was aimed at.
func stubHookStores(t *testing.T, list func(hookStore) ([]*beads.Issue, error)) *[]hookStore {
	t.Helper()

	originalList, originalUnhook := listStoreHooks, unhookStoreBead
	t.Cleanup(func() {
		listStoreHooks, unhookStoreBead = originalList, originalUnhook
	})
	listStoreHooks = list

	resetStores := &[]hookStore{}
	unhookStoreBead = func(store hookStore, _ string) error {
		*resetStores = append(*resetStores, store)
		return nil
	}

	return resetStores
}

func TestDiscoverHookStores(t *testing.T) {
	townRoot, rigBeadsDir := setupHookStoreTown(t)

	stores, err := discoverHookStores(townRoot)
	if err != nil {
		t.Fatalf("discoverHookStores() error = %v", err)
	}

	want := []hookStore{
		{Name: "town", BeadsDir: filepath.Join(townRoot, ".beads")},
		{Name: "testrig", BeadsDir: rigBeadsDir},
	}
	if len(stores) != len(want) {
		t.Fatalf("discoverHookStores() = %v, want %v", stores, want)
	}
	for i, store := range stores {
		if store != want[i] {
			t.Errorf("store[%d] = %v, want %v", i, store, want[i])
		}
	}
}

// TestScanStaleHooksSearchesRigStores is the regression for gt-hdph: a bead
// hooked in a rig store must be found when it is not in the town store, and
// the unhook must land in the store that holds it.
func TestScanStaleHooksSearchesRigStores(t *testing.T) {
	townRoot, rigBeadsDir := setupHookStoreTown(t)

	var listed []string
	resetStores := stubHookStores(t, func(store hookStore) ([]*beads.Issue, error) {
		listed = append(listed, store.Name)
		if store.Name != "testrig" {
			return nil, nil
		}
		return []*beads.Issue{hookedIssue("gt-fr3r", "testrig/polecats/garnet",
			time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339))}, nil
	})

	result, err := ScanStaleHooks(townRoot, &StaleHookConfig{MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("ScanStaleHooks() error = %v", err)
	}

	if got, want := listed, []string{"town", "testrig"}; !slices.Equal(got, want) {
		t.Errorf("stores queried = %v, want %v", got, want)
	}
	if result.TotalHooked != 1 || result.StaleCount != 1 || result.Unhooked != 1 {
		t.Errorf("TotalHooked/StaleCount/Unhooked = %d/%d/%d, want 1/1/1",
			result.TotalHooked, result.StaleCount, result.Unhooked)
	}
	if len(result.Results) != 1 || result.Results[0].Store != "testrig" {
		t.Fatalf("Results = %+v, want one result tagged store=testrig", result.Results)
	}
	if len(*resetStores) != 1 || (*resetStores)[0].BeadsDir != rigBeadsDir {
		t.Errorf("reset targeted %v, want the rig store %s", *resetStores, rigBeadsDir)
	}
	if len(result.StoresSearched) != 2 {
		t.Errorf("StoresSearched = %v, want both stores named", result.StoresSearched)
	}
}

func TestScanStaleHooksReportsStoresSearched(t *testing.T) {
	townRoot, rigBeadsDir := setupHookStoreTown(t)

	resetStores := stubHookStores(t, func(hookStore) ([]*beads.Issue, error) { return nil, nil })

	result, err := ScanStaleHooks(townRoot, &StaleHookConfig{MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("ScanStaleHooks() error = %v", err)
	}

	if result.TotalHooked != 0 || len(result.Results) != 0 {
		t.Errorf("expected no hooked beads, got %d", result.TotalHooked)
	}
	if resetStoresRun := *resetStores; len(resetStoresRun) != 0 {
		t.Errorf("unhooked %v, want nothing", resetStoresRun)
	}
	joined := strings.Join(result.StoresSearched, " ")
	for _, want := range []string{"town", "testrig", rigBeadsDir} {
		if !strings.Contains(joined, want) {
			t.Errorf("StoresSearched = %v, want it to name %q", result.StoresSearched, want)
		}
	}
}

func TestScanStaleHooksStoreErrorDoesNotAbortScan(t *testing.T) {
	townRoot, _ := setupHookStoreTown(t)

	resetStores := stubHookStores(t, func(store hookStore) ([]*beads.Issue, error) {
		if store.Name == "town" {
			return nil, errors.New("dolt unreachable")
		}
		return []*beads.Issue{hookedIssue("gt-abc", "testrig/polecats/garnet",
			time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339))}, nil
	})

	result, err := ScanStaleHooks(townRoot, &StaleHookConfig{MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("ScanStaleHooks() error = %v", err)
	}

	if result.TotalHooked != 1 || result.Unhooked != 1 {
		t.Errorf("TotalHooked/Unhooked = %d/%d, want 1/1", result.TotalHooked, result.Unhooked)
	}
	if resetStoresRun := *resetStores; len(resetStoresRun) != 1 {
		t.Errorf("unhooked %v, want the reachable store's bead", resetStoresRun)
	}
	if got := result.StoreErrors["town"]; !strings.Contains(got, "dolt unreachable") {
		t.Errorf("StoreErrors = %v, want the town store's failure recorded", result.StoreErrors)
	}
}

func TestScanStaleHooksFailsWhenNoStoreAnswers(t *testing.T) {
	townRoot, _ := setupHookStoreTown(t)

	resetStores := stubHookStores(t, func(hookStore) ([]*beads.Issue, error) {
		return nil, errors.New("dolt unreachable")
	})

	_, err := ScanStaleHooks(townRoot, &StaleHookConfig{MaxAge: time.Hour})
	if err == nil {
		t.Fatal("ScanStaleHooks() error = nil, want failure when no store answers")
	}
	if resetStoresRun := *resetStores; len(resetStoresRun) != 0 {
		t.Errorf("unhooked %v behind a failed scan, want nothing", resetStoresRun)
	}
}

func TestScanStaleHooksAgeFallbackNeedsParsedTimestamp(t *testing.T) {
	townRoot, _ := setupHookStoreTown(t)

	stubHookStores(t, func(store hookStore) ([]*beads.Issue, error) {
		if store.Name != "testrig" {
			return nil, nil
		}
		// No assignee: session liveness can't be checked, so both beads fall
		// back to age. Only the parseable one is stale — an unreadable
		// timestamp must not pass for infinitely old (it would unhook live work).
		return []*beads.Issue{
			hookedIssue("gt-unreadable", "", "not-a-timestamp"),
			hookedIssue("gt-aged", "", time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339)),
		}, nil
	})

	result, err := ScanStaleHooks(townRoot, &StaleHookConfig{MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("ScanStaleHooks() error = %v", err)
	}

	if result.TotalHooked != 2 || result.StaleCount != 1 {
		t.Fatalf("TotalHooked/StaleCount = %d/%d, want 2/1", result.TotalHooked, result.StaleCount)
	}
	if result.Results[0].BeadID != "gt-aged" {
		t.Errorf("stale bead = %s, want gt-aged", result.Results[0].BeadID)
	}
	if result.Results[0].Age != "3h0m0s" {
		t.Errorf("Age = %q, want %q", result.Results[0].Age, "3h0m0s")
	}
}

func TestStaleHookResult_PartialWorkFields(t *testing.T) {
	result := &StaleHookResult{
		BeadID:        "gt-abc",
		Title:         "test bead",
		Assignee:      "gastown/polecats/max",
		PartialWork:   true,
		WorktreeDirty: true,
		UnpushedCount: 3,
	}

	if !result.PartialWork {
		t.Error("PartialWork should be true")
	}
	if !result.WorktreeDirty {
		t.Error("WorktreeDirty should be true")
	}
	if result.UnpushedCount != 3 {
		t.Errorf("UnpushedCount = %d, want 3", result.UnpushedCount)
	}
}
