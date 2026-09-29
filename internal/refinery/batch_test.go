package refinery

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/rig"
)

// testGitRepo creates a bare repo + working clone with an initial commit.
// Returns the working dir, a cleanup func, and a git.Git for the working dir.
//
// The repo is a copy of one built once per test binary (gitRepoTemplate):
// building it takes nine git processes, about 2 s each time on a loaded host,
// and some 145 tests start from it.
func testGitRepo(t *testing.T) (workDir string, g *gitpkg.Git, cleanup func()) {
	t.Helper()
	tmpl, err := gitRepoTemplate()
	if err != nil {
		t.Fatalf("building the template git repo: %v", err)
	}
	tmpDir := t.TempDir()
	if err := copyTree(tmpl, tmpDir); err != nil {
		t.Fatalf("copying the template git repo: %v", err)
	}
	workDir = filepath.Join(tmpDir, "work")
	// The clone's origin is the template's bare repo; point it at the copy.
	cfgPath := filepath.Join(workDir, ".git", "config")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	fixed := strings.ReplaceAll(string(cfg), filepath.Join(tmpl, "origin.git"), filepath.Join(tmpDir, "origin.git"))
	if fixed == string(cfg) {
		t.Fatalf("template clone config names no template origin:\n%s", cfg)
	}
	if err := os.WriteFile(cfgPath, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}

	g = gitpkg.NewGit(workDir)

	return workDir, g, func() {} // t.TempDir handles cleanup
}

var (
	gitRepoTemplateOnce sync.Once
	gitRepoTemplateDir  string
	gitRepoTemplateErr  error
)

// gitRepoTemplate returns the directory holding testGitRepo's starting
// state, building it on first use: origin.git, a bare repo whose main has one
// commit (README.md), and work, its clone with main checked out and tracking
// origin/main. It lives in the hermetic sandbox's home, which TestMain
// removes.
func gitRepoTemplate() (string, error) {
	gitRepoTemplateOnce.Do(func() {
		gitRepoTemplateDir, gitRepoTemplateErr = buildGitRepoTemplate()
	})
	return gitRepoTemplateDir, gitRepoTemplateErr
}

func buildGitRepoTemplate() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(home, "refinery-git-template-")
	if err != nil {
		return "", err
	}
	bareDir := filepath.Join(dir, "origin.git")
	workDir := filepath.Join(dir, "work")
	steps := []struct {
		dir  string
		args []string
	}{
		{dir, []string{"init", "--bare", "--initial-branch=main", bareDir}},
		{dir, []string{"clone", bareDir, workDir}},
		{workDir, []string{"config", "user.email", "test@test.com"}},
		{workDir, []string{"config", "user.name", "Test"}},
		{workDir, []string{"checkout", "-b", "main"}},
		{workDir, []string{"add", "."}},
		{workDir, []string{"commit", "-m", "initial commit"}},
		{workDir, []string{"push", "-u", "origin", "main"}},
	}
	for i, step := range steps {
		if i == 5 {
			if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
				return "", err
			}
		}
		cmd := exec.Command("git", step.args...)
		cmd.Dir = step.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %v\n%s", step.args, err, out)
		}
	}
	return dir, nil
}

// copyTree copies the directory tree src into dst, keeping file modes.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
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

// pushBranch publishes branch on origin. The gt-sda9 pre-gate assertion reads
// origin rather than the shared local ref, so a test that stages a merge from a
// branch needs it published first.
func pushBranch(t *testing.T, workDir, branch string) {
	t.Helper()
	run(t, workDir, "git", "push", "-u", "origin", branch)
}

// originBranchTip returns the SHA origin holds for branch, or "" when origin
// has no such branch. Head resolution reads origin, so a test that has to know
// whether a submission is published asks here rather than of the local ref.
func originBranchTip(t *testing.T, workDir, branch string) string {
	t.Helper()
	fields := strings.Fields(run(t, workDir, "git", "ls-remote", "origin", "refs/heads/"+branch))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
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
	e.notifier = notifyfake.New() // never a live gt; read it with recorderOf
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

// advanceBranchLocally adds a commit to branch and leaves the worktree back on
// main, without pushing — the shape a polecat's in-progress rework leaves
// behind in a shared .repo.git.
func advanceBranchLocally(t *testing.T, workDir, branch string) string {
	t.Helper()
	run(t, workDir, "git", "checkout", branch)
	writeFile(t, workDir, "later.txt", "not submitted\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: later")
	head := run(t, workDir, "git", "rev-parse", "refs/heads/"+branch)
	run(t, workDir, "git", "checkout", "main")
	return head
}

// TestBuildRebaseStack_EjectsAdvancedSourceBranch is the gt-qlim8 regression at
// the stacking level: the shared local ref for a member has moved past the head
// its MR recorded, and origin never received the branch. The local ref is not
// the authority on what was submitted, so this is drift — and drift is one
// member's problem. It is dropped from the stack and left queued, whereas
// before the fix the whole batch aborted on it.
func TestBuildRebaseStack_EjectsAdvancedSourceBranch(t *testing.T) {
	// Not t.Parallel(): fakeBDAndGt uses t.Setenv.
	fakeBDAndGt(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "feature-advanced"
	createFeatureBranch(t, workDir, branch, "a.txt", "submitted\n")
	commit := run(t, workDir, "git", "rev-parse", branch)
	advanceBranchLocally(t, workDir, branch)

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{{
		ID:          "gt-mr-advanced",
		Branch:      branch,
		Target:      "main",
		SourceIssue: "gt-src",
		CommitSHA:   commit,
	}}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("a drifted member must not abort the batch: %v", err)
	}
	if len(stacked) != 0 {
		t.Errorf("expected the drifted member to stay out of the stack, got %v", stackedIDs(stacked))
	}
	if len(conflicts) != 1 || conflicts[0].ID != "gt-mr-advanced" {
		t.Errorf("expected gt-mr-advanced ejected from the batch, got %v", stackedIDs(conflicts))
	}
}

// TestBuildRebaseStack_MergesSubmissionWhenLocalRefDrifted is the other half of
// gt-qlim8: the local ref for a member has moved off the recorded head, but
// origin still carries the submission (the worktree kept working and never
// pushed, a superseding resubmission, a nuked worktree). Origin decides, so the
// member stacks the recorded submission — the head that was actually submitted,
// reviewed and gated — instead of being refused, and the unsubmitted work on
// the local ref stays out of the merge.
func TestBuildRebaseStack_MergesSubmissionWhenLocalRefDrifted(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	branch := "feature-drifted"
	createFeatureBranch(t, workDir, branch, "a.txt", "submitted\n")
	submitted := run(t, workDir, "git", "rev-parse", branch)
	pushBranch(t, workDir, branch)
	drifted := advanceBranchLocally(t, workDir, branch)
	if drifted == submitted {
		t.Fatal("test setup: the local branch did not move")
	}
	// Origin still points at the submission, not at the drifted ref.
	if tip := originBranchTip(t, workDir, branch); tip != submitted {
		t.Fatalf("test setup: origin/%s = %s, want the submitted head %s", branch, tip, submitted)
	}

	e := newTestEngineer(t, workDir, g)
	batch := []*MRInfo{{
		ID:          "gt-mr-drifted",
		Branch:      branch,
		Target:      "main",
		SourceIssue: "gt-src",
		CommitSHA:   submitted,
	}}

	stacked, conflicts, err := e.BuildRebaseStack(context.Background(), batch, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stacked) != 1 || len(conflicts) != 0 {
		t.Fatalf("expected the submission stacked, got stacked=%v conflicts=%v", stackedIDs(stacked), stackedIDs(conflicts))
	}

	// The stack carries the submitted content, not the local ref's later work.
	content, readErr := os.ReadFile(filepath.Join(workDir, "a.txt"))
	if readErr != nil {
		t.Fatalf("read a.txt from the stack: %v", readErr)
	}
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "submitted\n" {
		t.Errorf("a.txt on the stack = %q, want the submitted content", got)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "later.txt")); !os.IsNotExist(statErr) {
		t.Errorf("later.txt is on the stack: unsubmitted local work must not be merged")
	}
}

// TestProcessBatch_LandsHealthyMembersPastAStaleOne is the gt-qlim8 acceptance
// case end to end: one batch member's recorded head is stale — the branch was
// rewritten on origin after submission, so origin no longer carries it — and
// the healthy member behind it must still land. Before the fix the stale
// member aborted the whole batch, so nothing landed.
func TestProcessBatch_LandsHealthyMembersPastAStaleOne(t *testing.T) {
	// Not t.Parallel(): fakeBDAndGt uses t.Setenv.
	fakeBDAndGt(t)
	workDir, _, cleanup := testGitRepo(t)
	defer cleanup()

	const stale = "feature-rewritten"
	createFeatureBranch(t, workDir, stale, "shared.txt", "submitted\n")
	recorded := run(t, workDir, "git", "rev-parse", "refs/heads/"+stale)
	pushBranch(t, workDir, stale)

	// Rewrite the branch on origin: a disjoint history, so the recorded head is
	// neither origin's tip nor reachable from it.
	run(t, workDir, "git", "checkout", "-B", stale, "main")
	writeFile(t, workDir, "shared.txt", "rewritten\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "feat: rewritten")
	run(t, workDir, "git", "push", "--force", "origin", stale)
	run(t, workDir, "git", "checkout", "main")

	createFeatureBranch(t, workDir, "feature-good", "good.txt", "good\n")
	goodHead := run(t, workDir, "git", "rev-parse", "refs/heads/feature-good")
	pushBranch(t, workDir, "feature-good")

	store := newPrepushStore(
		prepushIssue("gt-src-stale", ""),
		prepushIssue("gt-src-good", ""),
		prepushMRIssue("gt-mr-stale", stale, "main", "gt-src-stale", recorded),
		prepushMRIssue("gt-mr-good", "feature-good", "main", "gt-src-good", goodHead),
	)
	e := newPrepushEngineer(t, workDir, store)
	batch := []*MRInfo{
		{ID: "gt-mr-stale", Branch: stale, Target: "main", SourceIssue: "gt-src-stale", CommitSHA: recorded},
		{ID: "gt-mr-good", Branch: "feature-good", Target: "main", SourceIssue: "gt-src-good", CommitSHA: goodHead},
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("a stale member must not fail the batch: %v", result.Error)
	}
	if len(result.Merged) != 1 || result.Merged[0].ID != "gt-mr-good" {
		t.Fatalf("expected only gt-mr-good merged, got %v", stackedIDs(result.Merged))
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].ID != "gt-mr-stale" {
		t.Fatalf("expected gt-mr-stale dropped from the batch, got %v", stackedIDs(result.Conflicts))
	}

	// The healthy member reached origin's main.
	verifyDir := filepath.Join(filepath.Dir(workDir), "verify")
	run(t, filepath.Dir(workDir), "git", "clone", filepath.Join(filepath.Dir(workDir), "origin.git"), verifyDir)
	if _, statErr := os.Stat(filepath.Join(verifyDir, "good.txt")); statErr != nil {
		t.Errorf("good.txt missing from origin's main after the batch: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(verifyDir, "shared.txt")); !os.IsNotExist(statErr) {
		t.Errorf("the rewritten branch landed on origin's main; only the recorded submission may")
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

// TestProcessBatch_MultipleMRs_NotifiesWitnessMergedPerMR is the gt-9gjl
// regression: gt mq batch run's multi-MR fast-forward path must emit the
// documented MERGED notification (mail-protocol.md: Refinery -> Witness)
// once per MR in the batch, not zero times for the whole batch. Without it
// the witness never completes each polecat's cleanup wisp, so batched
// polecat worktrees are never reaped.
func TestProcessBatch_MultipleMRs_NotifiesWitnessMergedPerMR(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "polecat/a/gt-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "polecat/b/gt-b", "b.txt", "hello b\n")
	createFeatureBranch(t, workDir, "polecat/c/gt-c", "c.txt", "hello c\n")

	e := newTestEngineer(t, workDir, g)
	var notified []string
	e.notifyMergedFn = func(mr *MRInfo, mergeCommit string) {
		notified = append(notified, mr.ID)
		if mergeCommit == "" {
			t.Errorf("notify for %s got an empty merge commit", mr.ID)
		}
	}

	// IDs keep the "mr-" prefix and SourceIssue stays empty so
	// isSyntheticMergeMechanicsMR recognizes these as test MRs and skips the
	// real bd close, matching the sibling tests in this file (this workDir
	// has no .beads database).
	batch := []*MRInfo{
		{ID: "mr-a", Branch: "polecat/a/gt-a", Target: "main", Worker: "polecats/a", CreatedAt: time.Now()},
		{ID: "mr-b", Branch: "polecat/b/gt-b", Target: "main", Worker: "polecats/b", CreatedAt: time.Now()},
		{ID: "mr-c", Branch: "polecat/c/gt-c", Target: "main", Worker: "polecats/c", CreatedAt: time.Now()},
	}

	result := e.ProcessBatch(context.Background(), batch, "main", DefaultBatchConfig())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Merged) != 3 {
		t.Fatalf("expected 3 merged, got %d", len(result.Merged))
	}

	if len(notified) != 3 {
		t.Fatalf("expected 3 witness MERGED notifications (one per batched MR), got %d: %v", len(notified), notified)
	}
	for _, id := range []string{"mr-a", "mr-b", "mr-c"} {
		found := false
		for _, n := range notified {
			if n == id {
				found = true
			}
		}
		if !found {
			t.Errorf("expected MR %s to be notified, notified=%v", id, notified)
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

// TestProcessBatch_RealignsWhenLocalTargetAheadOfOrigin covers gt-u093 fix
// (b): a leftover commit on the shared local main — from a prior run that
// exited before restoreTargetToOrigin, or another agent's routine commit
// (gt-032w) — must never turn into a rig-wide halt. ProcessBatch realigns
// target to origin up front and proceeds with the batch; every stack build
// below was going to discard the leftover commit anyway.
func TestProcessBatch_RealignsWhenLocalTargetAheadOfOrigin(t *testing.T) {
	t.Parallel()
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	createFeatureBranch(t, workDir, "feature-a", "a.txt", "hello a\n")
	createFeatureBranch(t, workDir, "feature-b", "b.txt", "hello b\n")

	e := newTestEngineer(t, workDir, g)
	originMain := run(t, workDir, "git", "rev-parse", "origin/main")

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

	if result.Error != nil {
		t.Fatalf("expected ProcessBatch to realign and proceed, got error: %v", result.Error)
	}
	if len(result.Merged) != 2 {
		t.Fatalf("expected both MRs merged after realignment, got %v", stackedIDs(result.Merged))
	}

	if err := exec.Command("git", "-C", workDir, "merge-base", "--is-ancestor", leftoverSHA, "origin/main").Run(); err == nil {
		t.Errorf("leftover commit %s is still reachable from origin/main — it should have been discarded by realignment", leftoverSHA)
	}
	if err := exec.Command("git", "-C", workDir, "merge-base", "--is-ancestor", originMain, "origin/main").Run(); err != nil {
		t.Errorf("origin/main's prior tip %s is not an ancestor of the landed batch", originMain)
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
