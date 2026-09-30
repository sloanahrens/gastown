//go:build integration

package daemon

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationCheckpointWorktreeRefusesRevertOfMergedWork runs the gt-2bp8
// case on real git, whose write-tree and history the revert guard reads: a
// shared-worktree reset left keep.txt holding stale pre-merge content even
// though this branch's own HEAD (and origin/main) already moved past it, and
// real WIP work sits right alongside the pollution — exactly the mixed shape
// checkpoint_dog must separate. The auto-checkpoint must refuse to commit
// rather than bake the stale content into the branch as a "real" change.
func TestIntegrationCheckpointWorktreeRefusesRevertOfMergedWork(t *testing.T) {
	t.Parallel()
	polecat := newRealCheckpointRevertScenario(t)

	// The bug: working-tree pollution reintroduces the PRE-merge content for a
	// file this session never meant to touch.
	if err := os.WriteFile(filepath.Join(polecat, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("pollute keep.txt: %v", err)
	}
	// Real work, staged alongside the pollution — the checkpoint must be able
	// to refuse the revert without the presence of genuine WIP work fooling it
	// into committing anyway.
	if err := os.WriteFile(filepath.Join(polecat, "wip.txt"), []byte("real work in progress\n"), 0o644); err != nil {
		t.Fatalf("write wip.txt: %v", err)
	}

	beforeHead := mustRunGit(t, polecat, "rev-parse", "HEAD")

	alerts := &checkpointAlertRecorder{}
	d := &Daemon{
		logger:                log.New(io.Discard, "", 0),
		checkpointRevertAlert: alerts.alert,
	}
	if d.checkpointWorktree(polecat, "rig", "polecat") {
		t.Fatal("checkpointWorktree created a checkpoint that reverts already-merged work")
	}

	if afterHead := mustRunGit(t, polecat, "rev-parse", "HEAD"); afterHead != beforeHead {
		t.Fatalf("checkpointWorktree advanced HEAD to %s, want unchanged %s", afterHead, beforeHead)
	}

	if len(alerts.calls) != 1 {
		t.Fatalf("expected exactly one revert-guard escalation, got %d: %+v", len(alerts.calls), alerts.calls)
	}
	if !strings.Contains(alerts.calls[0].message, "keep.txt") {
		t.Errorf("escalation message missing the reverted path keep.txt: %s", alerts.calls[0].message)
	}
}

// newRealCheckpointRevertScenario reproduces, with real git, the shared-worktree-reuse shape from
// gt-2bp8: a bare "origin", a seed checkout that stands in for main, and a
// polecat checkout on its own branch that has already fast-forwarded through
// another polecat's merge — so origin/main and the branch's own ancestry both
// show keep.txt at its post-merge content ("keep\nmain touch\n"). Returns the
// polecat checkout's path; the caller decides what to do to its working tree
// from there.
func newRealCheckpointRevertScenario(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")

	mustRunGit(t, "", "init", "--bare", remote)
	// Point the bare repo's HEAD at main explicitly: git init's default branch
	// name is host-configurable, and the clones below check out whatever HEAD
	// names.
	mustRunGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	mustRunGit(t, "", "clone", remote, seed)
	mustRunGit(t, seed, "config", "user.email", "seed@example.com")
	mustRunGit(t, seed, "config", "user.name", "Seed")
	if err := os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	mustRunGit(t, seed, "add", "-A")
	mustRunGit(t, seed, "commit", "-m", "base")
	mustRunGit(t, seed, "push", "origin", "main")

	// The polecat worktree is cut here, at the base commit.
	mustRunGit(t, "", "clone", remote, polecat)
	mustRunGit(t, polecat, "config", "user.email", "polecat@example.com")
	mustRunGit(t, polecat, "config", "user.name", "Polecat")
	mustRunGit(t, polecat, "switch", "-c", "polecat/turquoise/gt-test")

	// A different polecat's work merges into main while this worktree sits at
	// the base commit — the gt-wprt/gt-rv8h merges from the incident report.
	if err := os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("keep\nmain touch\n"), 0o644); err != nil {
		t.Fatalf("advance keep.txt: %v", err)
	}
	mustRunGit(t, seed, "add", "-A")
	mustRunGit(t, seed, "commit", "-m", "merged: other polecat's work")
	mustRunGit(t, seed, "push", "origin", "main")

	// The reused worktree picks up the merge, so its own ancestry already
	// contains it — exactly what a shared-worktree reset leaves behind.
	mustRunGit(t, polecat, "fetch", "origin")
	mustRunGit(t, polecat, "merge", "--ff-only", "origin/main")

	return polecat
}

// mustRunGit runs git in workDir and returns its trimmed stdout.
func mustRunGit(t *testing.T, workDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}
