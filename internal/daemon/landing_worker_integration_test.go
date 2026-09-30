//go:build integration

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landworker"
)

// TestIntegrationPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot runs the real
// post-land runner against a real repository, with a stub gt whose "slot
// run" only strips its own arguments. The runner's shell and gt run inside
// internal/land's CommandGate, which has no seam this package can reach.
func TestIntegrationPostLandRunUsesAWorktreeAtTheLandedCommitUnderTheSlot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	lwGit(t, root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "marker"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lwGit(t, repo, "add", ".")
	lwGit(t, repo, "commit", "-q", "-m", "landed")
	commit := lwGit(t, repo, "rev-parse", "HEAD")
	stub := filepath.Join(root, "gt")
	slotLog := filepath.Join(root, "slot.log")
	script := "#!/bin/sh\necho \"$@\" >> " + slotLog + "\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := postLandRun(repo, filepath.Join(root, "work"), filepath.Join(root, "logs"), stub, "gastown", time.Minute)

	res := run(context.Background(), "cat marker && echo slow tier failed && exit 3", landworker.PostLand{BeadID: "gt-a", Commit: commit})
	if res.Err != nil || res.ExitCode != 3 || !strings.Contains(res.Tail, "landed") || !strings.Contains(res.Tail, "slow tier failed") {
		t.Fatalf("red run: %+v", res)
	}
	res = run(context.Background(), "test -f marker", landworker.PostLand{BeadID: "gt-a", Commit: commit})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("green run: %+v", res)
	}
	data, err := os.ReadFile(slotLog)
	if err != nil || strings.Count(string(data), "slot run --role gastown/post-land --") != 2 {
		t.Fatalf("slot wrapper calls: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "work")); len(entries) != 0 {
		t.Fatalf("worktrees left behind: %v", entries)
	}
}
