//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

// TestIntegrationResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips runs
// dead-holder recovery end to end on real git: the worktree state read
// (git's own unpushed-commit judgement, which gitfake approximates by
// ancestry) and the preserve push.
func TestIntegrationResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue1", Title: "Held by dead session", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtf, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue1+abc123"
	worktreePath, originPath := newRealDeadHolderWorktree(t, townRoot, "gt", "basalt", branch)
	localTip := runDeadHolderGit(t, worktreePath, "rev-parse", "HEAD")

	// The rig-level shared-repo listing (survivingBranchFor's source) has
	// nothing — the branch was never pushed, so it cannot appear there.

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unpushed work",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue1") {
		t.Errorf("expected no sling for gt-issue1 (dead holder has unpushed work), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2 after skipping the preserved issue, got: %q", logContent)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation for a successful preserve, got: %v", escalated)
	}

	remoteTip := runDeadHolderGit(t, originPath, "rev-parse", branch)
	if remoteTip != localTip {
		t.Errorf("expected %s pushed to origin at %s, got %s", branch, localTip, remoteTip)
	}
}

// runDeadHolderGit runs a git command in dir, failing the test on error.
// GIT_CONFIG_GLOBAL/GIT_CONFIG_NOSYSTEM isolate it from the host's global and
// system git config — a signing key, hooksPath, or init.defaultBranch set on
// the developer's machine must not change whether these commits/pushes
// succeed.
func runDeadHolderGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRealDeadHolderWorktree creates a bare "origin" remote and a worktree cloned
// from it, checked out on the given generated polecat branch — the layout
// assigneeToWorktreePath resolves for assignee "<rig>/polecats/<name>".
// Returns the worktree path and the bare repo path.
func newRealDeadHolderWorktree(t *testing.T, townRoot, rig, name, branch string) (worktreePath, originPath string) {
	t.Helper()

	originPath = filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(originPath, 0755); err != nil {
		t.Fatalf("mkdir origin: %v", err)
	}
	runDeadHolderGit(t, originPath, "init", "--bare")

	worktreePath = filepath.Join(townRoot, rig, "polecats", name, rig)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	runDeadHolderGit(t, worktreePath, "init")
	runDeadHolderGit(t, worktreePath, "config", "user.email", "test@test.com")
	runDeadHolderGit(t, worktreePath, "config", "user.name", "Test")
	runDeadHolderGit(t, worktreePath, "remote", "add", "origin", originPath)
	runDeadHolderGit(t, worktreePath, "checkout", "-b", branch)
	runDeadHolderGit(t, worktreePath, "commit", "--allow-empty", "-m", "initial")

	return worktreePath, originPath
}
