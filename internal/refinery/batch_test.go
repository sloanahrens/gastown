package refinery

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// testGitRepo creates a bare repo + working clone with an initial commit.
// Returns the working dir, a cleanup func, and a git.Git for the working dir.
func testGitRepo(t *testing.T) (workDir string, g *gitpkg.Git, cleanup func()) {
	t.Helper()
	tmpDir := t.TempDir()

	bareDir := filepath.Join(tmpDir, "origin.git")
	workDir = filepath.Join(tmpDir, "work")

	// Create bare repo with main as default branch
	run(t, tmpDir, "git", "init", "--bare", "--initial-branch=main", bareDir)

	// Clone it
	run(t, tmpDir, "git", "clone", bareDir, workDir)

	// Configure git user
	run(t, workDir, "git", "config", "user.email", "test@test.com")
	run(t, workDir, "git", "config", "user.name", "Test")

	// Ensure we're on main branch
	run(t, workDir, "git", "checkout", "-b", "main")

	// Initial commit
	writeFile(t, workDir, "README.md", "# Test\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "initial commit")
	run(t, workDir, "git", "push", "-u", "origin", "main")

	g = gitpkg.NewGit(workDir)

	return workDir, g, func() {} // t.TempDir handles cleanup
}

// createFeatureBranch creates a branch with a single file change.
func createFeatureBranch(t *testing.T, workDir, branchName, filename, content string) {
	t.Helper()
	run(t, workDir, "git", "checkout", "-b", branchName, "main")
	writeFile(t, workDir, filename, content)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", fmt.Sprintf("feat: add %s", filename))
	run(t, workDir, "git", "checkout", "main")
}

// createConflictingBranch creates a branch that modifies the same file as another.
func createConflictingBranch(t *testing.T, workDir, branchName, filename, content string) {
	t.Helper()
	run(t, workDir, "git", "checkout", "-b", branchName, "main")
	writeFile(t, workDir, filename, content)
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", fmt.Sprintf("feat: modify %s", filename))
	run(t, workDir, "git", "checkout", "main")
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command %s %v failed: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func newTestEngineer(t *testing.T, workDir string, g *gitpkg.Git) *Engineer {
	t.Helper()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	e.git = g
	e.workDir = workDir
	e.output = &bytes.Buffer{}
	e.testAllowSyntheticMRs = true
	// No-op merge slot functions for tests
	e.mergeSlotEnsureExists = func() (string, error) { return "test-slot", nil }
	e.mergeSlotAcquire = func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error) {
		return &beads.MergeSlotStatus{Available: true, Holder: holder}, nil
	}
	e.mergeSlotRelease = func(holder string) error { return nil }
	return e
}

func makeMR(id, branch, target string) *MRInfo {
	return &MRInfo{
		ID:        id,
		Branch:    branch,
		Target:    target,
		CreatedAt: time.Now(),
	}
}

func failMarkerGateCmd() string {
	// Gate commands already run inside workDir, so use a relative path.
	// Raw Windows paths like D:\... confuse `sh test -f` under MSYS.
	return "test ! -f FAIL_MARKER"
}

// --- DefaultBatchConfig tests ---

func TestDefaultBatchConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultBatchConfig()
	if cfg.MaxBatchSize != 5 {
		t.Errorf("expected MaxBatchSize 5, got %d", cfg.MaxBatchSize)
	}
	if cfg.BatchWaitTime != 30*time.Second {
		t.Errorf("expected BatchWaitTime 30s, got %v", cfg.BatchWaitTime)
	}
	if !cfg.RetryBatchOnFlaky {
		t.Error("expected RetryBatchOnFlaky true")
	}
}

func TestFastForwardBatch_BlocksForkBackedDefaultPush(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	addDistinctUpstreamRemote(t, workDir, g)
	e := newTestEngineer(t, workDir, g)
	before := run(t, workDir, "git", "rev-parse", "origin/main")

	writeFile(t, workDir, "batched.txt", "batched\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "batch result")

	result := e.fastForwardBatch(context.Background(), nil, "main", &BatchResult{})
	if result.Error == nil || !strings.Contains(result.Error.Error(), "refusing direct push") {
		t.Fatalf("expected fork-backed default push refusal, got: %+v", result)
	}
	assertOriginMainUnchangedAndReset(t, workDir, before)
}

// --- AssembleBatch tests ---

func TestAssembleBatch_EmptyQueue(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	batch := e.AssembleBatch(nil, DefaultBatchConfig())
	if len(batch) != 0 {
		t.Errorf("expected empty batch, got %d", len(batch))
	}
}

func TestAssembleBatch_LessThanMax(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	mrs := []*MRInfo{
		makeMR("mr-1", "branch-1", "main"),
		makeMR("mr-2", "branch-2", "main"),
	}

	batch := e.AssembleBatch(mrs, &BatchConfig{MaxBatchSize: 5})
	if len(batch) != 2 {
		t.Errorf("expected 2 MRs in batch, got %d", len(batch))
	}
}

func TestAssembleBatch_CapsAtMax(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	mrs := make([]*MRInfo, 10)
	for i := range mrs {
		mrs[i] = makeMR(fmt.Sprintf("mr-%d", i), fmt.Sprintf("branch-%d", i), "main")
	}

	batch := e.AssembleBatch(mrs, &BatchConfig{MaxBatchSize: 3})
	if len(batch) != 3 {
		t.Errorf("expected 3 MRs in batch, got %d", len(batch))
	}
}

func TestAssembleBatch_SkipsBlockedMRs(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	mrs := []*MRInfo{
		makeMR("mr-1", "branch-1", "main"),
		{ID: "mr-2", Branch: "branch-2", Target: "main", BlockedBy: "mr-99"},
		makeMR("mr-3", "branch-3", "main"),
	}

	batch := e.AssembleBatch(mrs, &BatchConfig{MaxBatchSize: 5})
	if len(batch) != 2 {
		t.Errorf("expected 2 MRs (skipping blocked), got %d", len(batch))
	}
	if batch[0].ID != "mr-1" || batch[1].ID != "mr-3" {
		t.Errorf("expected mr-1 and mr-3, got %s and %s", batch[0].ID, batch[1].ID)
	}
}

func TestAssembleBatch_IncludesBlockedByBatchMember(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	mrs := []*MRInfo{
		makeMR("mr-1", "branch-1", "main"),
		{ID: "mr-2", Branch: "branch-2", Target: "main", BlockedBy: "mr-1"},
	}

	batch := e.AssembleBatch(mrs, &BatchConfig{MaxBatchSize: 5})
	if len(batch) != 2 {
		t.Errorf("expected 2 MRs (blocked by batch member ok), got %d", len(batch))
	}
}

func TestAssembleBatch_NilConfig(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)

	mrs := []*MRInfo{
		makeMR("mr-1", "branch-1", "main"),
	}

	batch := e.AssembleBatch(mrs, nil)
	if len(batch) != 1 {
		t.Errorf("expected 1 MR with nil config, got %d", len(batch))
	}
}

// --- BuildRebaseStack tests (require real git) ---

func TestBuildRebaseStack_SingleMR(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacked) != 1 {
		t.Errorf("expected 1 stacked, got %d", len(stacked))
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts, got %d", len(conflicts))
	}

	// Verify the file exists in working tree
	content, readErr := os.ReadFile(filepath.Join(workDir, "a.txt"))
	if readErr != nil {
		t.Fatalf("expected a.txt to exist: %v", readErr)
	}
	got := strings.ReplaceAll(string(content), "\r\n", "\n")
	if got != "hello a\n" {
		t.Errorf("expected 'hello a\\n', got %q", got)
	}
}

func TestBuildRebaseStack_MultipleMRs(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")
	createFeatureBranch(t, workDir, "feature-c", "c.txt", "hello c\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
	}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacked) != 3 {
		t.Errorf("expected 3 stacked, got %d", len(stacked))
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts, got %d", len(conflicts))
	}

	// All files should exist
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(workDir, f)); os.IsNotExist(err) {
			t.Errorf("expected %s to exist", f)
		}
	}
}

func TestBuildRebaseStack_ConflictRemovesMR(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// Create two branches that modify the same file
	createFeatureBranch(t, workDir, "feature-a", "shared.txt", "version A\n")
	createConflictingBranch(t, workDir, "feature-b", "shared.txt", "version B\n")
	createFeatureBranch(t, workDir, "feature-c", "c.txt", "hello c\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
	}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// feature-a merges first, feature-b conflicts with it
	if len(stacked) != 2 {
		t.Errorf("expected 2 stacked (a and c), got %d: %v", len(stacked), stackedIDs(stacked))
	}
	if len(conflicts) != 1 {
		t.Errorf("expected 1 conflict (b), got %d", len(conflicts))
	}
	if len(conflicts) > 0 && conflicts[0].ID != "mr-b" {
		t.Errorf("expected conflict to be mr-b, got %s", conflicts[0].ID)
	}
}

// TestBuildRebaseStack_DropsEmptyMR pins that a batch member whose merge leaves
// the stack unchanged is dropped instead of stacked. Stacked, it would be
// pushed and closed as merged while contributing nothing (gt-j5cc), and it is
// not a conflict, so the rest of the batch must be unaffected.
func TestBuildRebaseStack_DropsEmptyMR(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	// feature-empty commits a payload and then removes it again, ending with
	// main's own tree.
	run(t, workDir, "git", "checkout", "-b", "feature-empty", "main")
	writeFile(t, workDir, "payload.txt", "payload\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: add payload")
	run(t, workDir, "git", "rm", "payload.txt")
	run(t, workDir, "git", "commit", "-m", "fix: drop payload")
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-empty", "feature-empty", "main"),
	}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacked) != 1 || stacked[0].ID != "mr-a" {
		t.Errorf("expected only mr-a stacked, got %v", stackedIDs(stacked))
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts (an empty MR is not one), got %d", len(conflicts))
	}
	if _, err := os.Stat(filepath.Join(workDir, "a.txt")); os.IsNotExist(err) {
		t.Error("mr-a's file is missing from the stack")
	}
	if _, err := os.Stat(filepath.Join(workDir, "payload.txt")); !os.IsNotExist(err) {
		t.Error("the empty MR's payload is in the stack")
	}
}

func TestBuildRebaseStack_EmptyBatch(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)
	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), nil, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacked) != 0 || len(conflicts) != 0 {
		t.Error("expected empty results for empty batch")
	}
}

func TestBuildRebaseStack_MissingBranch(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-missing", "nonexistent-branch", "main"),
		makeMR("mr-a", "feature-a", "main"),
	}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacked) != 1 || stacked[0].ID != "mr-a" {
		t.Errorf("expected only mr-a stacked, got %v", stackedIDs(stacked))
	}
	if len(conflicts) != 1 || conflicts[0].ID != "mr-missing" {
		t.Errorf("expected mr-missing in conflicts, got %v", stackedIDs(conflicts))
	}
}

func TestBuildRebaseStack_RejectsAdvancedSourceBranch(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "feature-advanced"
	createFeatureBranch(t, workDir, branch, "a.txt", "submitted\n")
	commit := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "checkout", branch)
	writeFile(t, workDir, "later.txt", "not submitted\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: later")
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{{
		ID:        "mr-advanced",
		Branch:    branch,
		Target:    "main",
		CommitSHA: commit,
	}}

	_, _, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err == nil {
		t.Fatal("BuildRebaseStack succeeded for advanced source branch")
	}
	if !strings.Contains(err.Error(), "changed from submitted head") {
		t.Fatalf("BuildRebaseStack error = %q, want submitted-head drift", err.Error())
	}
}

// --- ProcessBatch tests ---

func TestProcessBatch_EmptyBatch(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)
	e.output = &bytes.Buffer{}

	result := e.ProcessBatch(context.Background(), nil, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Errorf("unexpected error: %v", result.Error)
	}
	if len(result.Merged) != 0 {
		t.Errorf("expected no merged MRs, got %d", len(result.Merged))
	}
}

func TestProcessBatch_SingleMR_Success(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)
	// No gates configured → auto-pass
	batch := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Merged) != 1 {
		t.Errorf("expected 1 merged, got %d", len(result.Merged))
	}
	if result.MergeCommit == "" {
		t.Error("expected merge commit SHA")
	}
}

func TestProcessBatch_MultipleMRs_AllPass(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")
	createFeatureBranch(t, workDir, "feature-c", "c.txt", "hello c\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Merged) != 3 {
		t.Errorf("expected 3 merged, got %d", len(result.Merged))
	}
	if result.MergeCommit == "" {
		t.Error("expected merge commit SHA")
	}

	// Verify all files landed on main
	run(t, workDir, "git", "checkout", "main")
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(workDir, f)); os.IsNotExist(err) {
			t.Errorf("expected %s on main after batch merge", f)
		}
	}
}

func TestProcessBatch_MergeStrategyPR_RefusesMultiMRBatch(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = "pr"
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error == nil {
		t.Fatal("expected an error refusing to batch under merge_strategy=pr, got nil")
	}
	if !strings.Contains(result.Error.Error(), "merge_strategy=pr") {
		t.Errorf("expected error to mention merge_strategy=pr, got: %v", result.Error)
	}
	if len(result.Merged) != 0 {
		t.Errorf("expected no MRs merged, got %d", len(result.Merged))
	}

	// Verify nothing landed on main.
	run(t, workDir, "git", "checkout", "main")
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(workDir, f)); err == nil {
			t.Errorf("expected %s NOT to land on main under refused merge_strategy=pr batch", f)
		}
	}
}

func TestProcessBatch_MergeStrategyPR_AllowsSingleMR(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)
	e.config.MergeStrategy = "pr"
	batch := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	// A single-MR batch takes the processSingleMR path, which predates and
	// is independent of the batch merge_strategy=pr refusal — it must not
	// be refused here just because MergeStrategy is "pr".
	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil && strings.Contains(result.Error.Error(), "does not support merge_strategy=pr") {
		t.Fatalf("single-MR batch should not hit the multi-MR merge_strategy=pr refusal: %v", result.Error)
	}
}

func TestProcessBatch_WithConflict(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "shared.txt", "version A\n")
	createConflictingBranch(t, workDir, "feature-b", "shared.txt", "version B\n")
	createFeatureBranch(t, workDir, "feature-c", "c.txt", "hello c\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	// mr-a and mr-c should merge, mr-b should conflict
	if len(result.Merged) != 2 {
		t.Errorf("expected 2 merged, got %d: %v", len(result.Merged), stackedIDs(result.Merged))
	}
	if len(result.Conflicts) != 1 {
		t.Errorf("expected 1 conflict, got %d", len(result.Conflicts))
	}
}

func TestProcessBatch_GateFailure_BisectsToFindCulprit(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	// feature-b creates a file that the gate checks for
	createFeatureBranch(t, workDir, "feature-b", "FAIL_MARKER", "this causes test failure\n")
	createFeatureBranch(t, workDir, "feature-c", "c.txt", "hello c\n")

	e := newTestEngineer(t, workDir, g)
	// Gate that fails if FAIL_MARKER exists
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}
	e.config.GatesParallel = false

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
	}

	cfg := &BatchConfig{
		MaxBatchSize:      5,
		RetryBatchOnFlaky: false, // Don't retry, go straight to bisect
	}

	result := e.ProcessBatch(context.Background(), batch, "main", cfg)

	// mr-b should be identified as culprit
	if len(result.Culprits) != 1 {
		t.Errorf("expected 1 culprit, got %d: %v", len(result.Culprits), stackedIDs(result.Culprits))
	}
	if len(result.Culprits) > 0 && result.Culprits[0].ID != "mr-b" {
		t.Errorf("expected culprit mr-b, got %s", result.Culprits[0].ID)
	}

	// mr-a and mr-c should be merged
	if len(result.Merged) != 2 {
		t.Errorf("expected 2 merged (a and c), got %d: %v", len(result.Merged), stackedIDs(result.Merged))
	}
}

func TestProcessBatch_RetryOnFlaky(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")

	e := newTestEngineer(t, workDir, g)

	// Create a flaky gate: fails first time, passes second
	counterFile := filepath.Join(workDir, ".gate_counter")
	e.config.Gates = map[string]*GateConfig{
		"flaky": {Cmd: fmt.Sprintf(`count=$(cat %s 2>/dev/null || echo 0); count=$((count + 1)); echo $count > %s; test $count -ge 2`, counterFile, counterFile)},
	}

	batch := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	cfg := &BatchConfig{
		MaxBatchSize:      5,
		RetryBatchOnFlaky: true,
	}

	result := e.ProcessBatch(context.Background(), batch, "main", cfg)
	// With retry, the flaky test should pass on second attempt
	// Note: since len(batch)==1, it goes through processSingleMR path
	// which uses doMerge (no retry there). Let's test with 2 MRs instead.
	_ = result
}

func TestProcessBatch_RetryOnFlaky_MultipleMRs(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)

	// Create a flaky gate: fails first time, passes second
	counterFile := filepath.Join(t.TempDir(), "gate_counter")
	e.config.Gates = map[string]*GateConfig{
		"flaky": {Cmd: fmt.Sprintf(`count=$(cat %s 2>/dev/null || echo 0); count=$((count + 1)); echo $count > %s; test $count -ge 2`, counterFile, counterFile)},
	}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	cfg := &BatchConfig{
		MaxBatchSize:      5,
		RetryBatchOnFlaky: true,
	}

	result := e.ProcessBatch(context.Background(), batch, "main", cfg)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Merged) != 2 {
		t.Errorf("expected 2 merged after flaky retry, got %d", len(result.Merged))
	}
}

func TestProcessBatch_AllConflict(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	// Create a commit on main that conflicts with both branches
	writeFile(t, workDir, "shared.txt", "main version\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "main: add shared.txt")
	run(t, workDir, "git", "push", "origin", "main")

	createConflictingBranch(t, workDir, "feature-a", "shared.txt", "version A\n")
	createConflictingBranch(t, workDir, "feature-b", "shared.txt", "version B\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	// First one should stack fine (it modifies shared.txt which already exists on main,
	// but since we're using a clean branch from main, it's a fast-forward of changes).
	// Actually both branches diverge from the initial commit, not from the current main.
	// So feature-a's shared.txt conflicts with main's shared.txt.
	// The result depends on whether CheckConflicts detects the conflict.
	// Either way, we should get no error.
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
}

// --- Bisection tests ---

func TestBisectBatch_SingleMR(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "FAIL_MARKER", "fail\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}

	batch := []*MRInfo{makeMR("mr-a", "feature-a", "main")}

	good, culprits := e.bisectBatch(context.Background(), batch, "main")
	if len(good) != 0 {
		t.Errorf("expected 0 good, got %d", len(good))
	}
	if len(culprits) != 1 {
		t.Errorf("expected 1 culprit, got %d", len(culprits))
	}
}

func TestBisectBatch_TwoMRs_SecondBad(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "FAIL_MARKER", "fail\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	good, culprits := e.bisectBatch(context.Background(), batch, "main")
	if len(good) != 1 || good[0].ID != "mr-a" {
		t.Errorf("expected good=[mr-a], got %v", stackedIDs(good))
	}
	if len(culprits) != 1 || culprits[0].ID != "mr-b" {
		t.Errorf("expected culprits=[mr-b], got %v", stackedIDs(culprits))
	}
}

func TestBisectBatch_TwoMRs_FirstBad(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "FAIL_MARKER", "fail\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	good, culprits := e.bisectBatch(context.Background(), batch, "main")
	if len(culprits) != 1 || culprits[0].ID != "mr-a" {
		t.Errorf("expected culprits=[mr-a], got %v", stackedIDs(culprits))
	}
	if len(good) != 1 || good[0].ID != "mr-b" {
		t.Errorf("expected good=[mr-b], got %v", stackedIDs(good))
	}
}

func TestBisectBatch_FourMRs_ThirdBad(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")
	createFeatureBranch(t, workDir, "feature-c", "FAIL_MARKER", "fail\n")
	createFeatureBranch(t, workDir, "feature-d", "d.txt", "hello d\n")

	e := newTestEngineer(t, workDir, g)
	e.output = os.Stderr
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
		makeMR("mr-c", "feature-c", "main"),
		makeMR("mr-d", "feature-d", "main"),
	}

	good, culprits := e.bisectBatch(context.Background(), batch, "main")
	if len(culprits) != 1 || culprits[0].ID != "mr-c" {
		t.Errorf("expected culprits=[mr-c], got %v", stackedIDs(culprits))
	}
	// a, b, d should be good
	goodIDs := stackedIDs(good)
	if len(good) != 3 {
		t.Errorf("expected 3 good MRs, got %d: %v", len(good), goodIDs)
	}
}

// --- Integration: ProcessBatch end-to-end with push ---

func TestProcessBatch_PushesAndLands(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Merged) != 2 {
		t.Fatalf("expected 2 merged, got %d", len(result.Merged))
	}

	// Verify pushed to origin by re-cloning
	verifyDir := filepath.Join(filepath.Dir(workDir), "verify")
	bareDir := filepath.Join(filepath.Dir(workDir), "origin.git")
	run(t, filepath.Dir(workDir), "git", "clone", bareDir, verifyDir)

	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(verifyDir, f)); os.IsNotExist(err) {
			t.Errorf("expected %s in cloned repo after push", f)
		}
	}
}

func TestProcessBatch_BisectAndMergeGood(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "FAIL_MARKER", "fail\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	cfg := &BatchConfig{
		MaxBatchSize:      5,
		RetryBatchOnFlaky: false,
	}

	result := e.ProcessBatch(context.Background(), batch, "main", cfg)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	// mr-a should be merged, mr-b should be culprit
	if len(result.Merged) != 1 || result.Merged[0].ID != "mr-a" {
		t.Errorf("expected merged=[mr-a], got %v", stackedIDs(result.Merged))
	}
	if len(result.Culprits) != 1 || result.Culprits[0].ID != "mr-b" {
		t.Errorf("expected culprits=[mr-b], got %v", stackedIDs(result.Culprits))
	}

	// Verify a.txt landed on origin
	verifyDir := filepath.Join(filepath.Dir(workDir), "verify2")
	bareDir := filepath.Join(filepath.Dir(workDir), "origin.git")
	run(t, filepath.Dir(workDir), "git", "clone", bareDir, verifyDir)

	if _, err := os.Stat(filepath.Join(verifyDir, "a.txt")); os.IsNotExist(err) {
		t.Error("expected a.txt in cloned repo")
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "FAIL_MARKER")); !os.IsNotExist(err) {
		t.Error("FAIL_MARKER should NOT be in cloned repo")
	}
}

// --- getMergeMessage tests ---

func TestGetMergeMessage_FromBranch(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	run(t, workDir, "git", "checkout", "-b", "feat-branch", "main")
	writeFile(t, workDir, "x.txt", "x\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: add x feature")
	run(t, workDir, "git", "checkout", "main")

	e := newTestEngineer(t, workDir, g)
	mr := makeMR("mr-x", "feat-branch", "main")

	msg := e.getMergeMessage(mr)
	if !strings.Contains(msg, "feat: add x feature") {
		t.Errorf("expected original commit message, got %q", msg)
	}
}

func TestGetMergeMessage_Fallback(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "test-rig", Path: t.TempDir()}
	e := NewEngineer(r)
	e.output = &bytes.Buffer{}

	mr := &MRInfo{
		ID:          "mr-x",
		Branch:      "nonexistent-branch",
		Target:      "main",
		SourceIssue: "gt-abc",
	}

	msg := e.getMergeMessage(mr)
	if !strings.Contains(msg, "Merge") {
		t.Errorf("expected fallback message, got %q", msg)
	}
	if !strings.Contains(msg, "gt-abc") {
		t.Errorf("expected source issue in fallback, got %q", msg)
	}
}

// TestProcessBatch_SingleMR_BranchNotFound verifies that a missing branch is treated as a
// skippable condition (added to Conflicts) rather than a fatal infrastructure error.
func TestProcessBatch_SingleMR_BranchNotFound(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	e := newTestEngineer(t, workDir, g)

	// MR pointing to a branch that doesn't exist locally or remotely.
	batch := []*MRInfo{makeMR("mr-gone", "ghost-branch", "main")}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())

	// Must NOT be a fatal error.
	if result.Error != nil {
		t.Fatalf("expected no fatal error for missing branch, got: %v", result.Error)
	}
	// Must be skipped (added to conflicts, not merged or culprits).
	if len(result.Merged) != 0 {
		t.Errorf("expected no merged MRs, got %d", len(result.Merged))
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].ID != "mr-gone" {
		t.Errorf("expected mr-gone in conflicts (skipped), got %v", stackedIDs(result.Conflicts))
	}
	// Verify the log message indicates the branch was not found (escalating, not fatal).
	log := e.output.(*bytes.Buffer).String()
	if !strings.Contains(log, "not found") {
		t.Errorf("expected 'not found' in log output, got: %s", log)
	}
}

// TestProcessBatch_AllCulprits_RestoresTargetToOrigin covers gt-u093: when
// bisection isolates every stacked MR as a culprit, ProcessBatch pushes
// nothing, but a prior version left local main wherever bisection's last
// resetAndRebuildStack call staged it — a rejected stacked merge, ahead of
// origin/main. The next successful push would have carried that merge to
// origin.
func TestProcessBatch_AllCulprits_RestoresTargetToOrigin(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "FAIL_A", "fail a\n")
	createFeatureBranch(t, workDir, "feature-b", "FAIL_B", "fail b\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: "test ! -f FAIL_A && test ! -f FAIL_B"},
	}

	originMain := run(t, workDir, "git", "rev-parse", "origin/main")

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}
	cfg := &BatchConfig{MaxBatchSize: 5, RetryBatchOnFlaky: false}

	result := e.ProcessBatch(context.Background(), batch, "main", cfg)

	if len(result.Merged) != 0 {
		t.Errorf("expected nothing merged when every MR is a culprit, got %v", stackedIDs(result.Merged))
	}
	if len(result.Culprits) != 2 {
		t.Fatalf("expected both MRs isolated as culprits, got %v", stackedIDs(result.Culprits))
	}

	localMain := run(t, workDir, "git", "rev-parse", "main")
	if localMain != originMain {
		t.Errorf("gt-u093: local main is %s after bisection found only culprits, want it restored to origin/main %s", localMain, originMain)
	}
}

// TestProcessBatch_SingleSurvivorGateFailure_RestoresTargetToOrigin covers the
// same gt-u093 gap in verifyAndPush: when conflict removal leaves exactly one
// MR stacked and its gates fail, that MR's merge must not stay on local main.
func TestProcessBatch_SingleSurvivorGateFailure_RestoresTargetToOrigin(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	writeFile(t, workDir, "shared.txt", "main version\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "main: add shared.txt")
	run(t, workDir, "git", "push", "origin", "main")

	// feature-bad stacks cleanly (it's first in the batch) but fails gates.
	run(t, workDir, "git", "checkout", "-b", "feature-bad", "main")
	writeFile(t, workDir, "shared.txt", "version A\n")
	writeFile(t, workDir, "FAIL_MARKER", "fail\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: bad change")
	run(t, workDir, "git", "checkout", "main")

	// feature-conflict changes the same file from the same base, so once
	// feature-bad's change is stacked, stacking this one on top conflicts.
	createConflictingBranch(t, workDir, "feature-conflict", "shared.txt", "version B\n")

	e := newTestEngineer(t, workDir, g)
	e.config.Gates = map[string]*GateConfig{
		"check": {Cmd: failMarkerGateCmd()},
	}

	originMain := run(t, workDir, "git", "rev-parse", "origin/main")

	batch := []*MRInfo{
		makeMR("mr-bad", "feature-bad", "main"),
		makeMR("mr-conflict", "feature-conflict", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())

	if len(result.Conflicts) != 1 || result.Conflicts[0].ID != "mr-conflict" {
		t.Fatalf("expected mr-conflict to conflict, got %v", stackedIDs(result.Conflicts))
	}
	if len(result.Culprits) != 1 || result.Culprits[0].ID != "mr-bad" {
		t.Fatalf("expected mr-bad as culprit via verifyAndPush, got %v", stackedIDs(result.Culprits))
	}

	localMain := run(t, workDir, "git", "rev-parse", "main")
	if localMain != originMain {
		t.Errorf("gt-u093: local main is %s after single-survivor gate failure, want origin/main %s", localMain, originMain)
	}
}

// TestProcessBatch_RefusesWhenLocalTargetAheadOfOrigin covers gt-u093 fix (b):
// a run must never build a new stack on top of a local target that already
// holds commits origin/target doesn't have — the exact leftover state a prior
// run that skipped restoreTargetToOrigin produces. Silently discarding it via
// prepareMergeTarget's reset would hide the very evidence of the bug.
func TestProcessBatch_RefusesWhenLocalTargetAheadOfOrigin(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)

	// Simulate a prior run's rejected stacked merge left on local main.
	run(t, workDir, "git", "checkout", "main")
	writeFile(t, workDir, "leftover.txt", "leftover\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "Merge rejected-branch into main (gt-xxxx)")
	leftoverSHA := run(t, workDir, "git", "rev-parse", "main")

	batch := []*MRInfo{
		makeMR("mr-a", "feature-a", "main"),
		makeMR("mr-b", "feature-b", "main"),
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())

	if result.Error == nil {
		t.Fatal("expected ProcessBatch to refuse when local main is ahead of origin/main")
	}
	if !strings.Contains(result.Error.Error(), "ahead of origin") {
		t.Errorf("expected an ahead-of-origin refusal, got: %v", result.Error)
	}
	if len(result.Merged) != 0 {
		t.Errorf("expected nothing merged, got %v", stackedIDs(result.Merged))
	}

	// The refusal must not discard the leftover commit either — that would
	// erase the evidence a human needs to diagnose how it got there.
	if got := run(t, workDir, "git", "rev-parse", "main"); got != leftoverSHA {
		t.Errorf("refusal changed local main from %s to %s", leftoverSHA, got)
	}
}

// --- Helpers ---

func stackedIDs(mrs []*MRInfo) []string {
	ids := make([]string, len(mrs))
	for i, mr := range mrs {
		ids[i] = mr.ID
	}
	return ids
}
