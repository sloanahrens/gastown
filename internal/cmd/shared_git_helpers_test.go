package cmd

// Test helpers shared across this package. They lived in merge-queue test
// files until those were deleted with the merge queue (gt-v4ssj.6).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
)

func writeMQSubmitTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGitForMQSubmitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// contains checks if s contains substr (helper for styled output)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// CodeOfErr is CodeOf with the command's own error, so a caller can tell an
// approve (no error at all) from a request_changes (a SilentExitError).
func CodeOfErr(t *testing.T, cmd *cobra.Command) (int, error) {
	t.Helper()
	err := cmd.Execute()
	// Execute re-adds the default help command to the root it runs from
	// (cobra's InitDefaultHelpCmd removes and adds it on every call), which
	// marks the root unsorted. Sort it again before a parallel test walks
	// the shared tree (TestCommandTreeWalkIsReadOnlyUnderParallelTests).
	presortCommandTree(cmd.Root())
	if err == nil {
		return 0, nil
	}
	if code, ok := IsSilentExit(err); ok {
		return code, err
	}
	t.Errorf("command error: %v", err)
	return 1, err
}

// initOrphanCleanupRepo builds a clone whose origin carries a polecat branch,
// optionally squash-merged into main, so the orphan path runs against a real
// remote rather than a fake. (gt-qjp2)
func initOrphanCleanupRepo(t *testing.T, squashMerge bool) (clone, branch string) {
	t.Helper()
	root, f := cachedGitFixture(t, fmt.Sprintf("orphan-cleanup squash=%v", squashMerge), func(dir string) ([2]string, error) {
		clone, branch := buildOrphanCleanupRepo(t, dir, squashMerge)
		rel, err := filepath.Rel(dir, clone)
		return [2]string{rel, branch}, err
	})
	return filepath.Join(root, f[0]), f[1]
}

// buildOrphanCleanupRepo makes initOrphanCleanupRepo's repos under tmp.
func buildOrphanCleanupRepo(t *testing.T, tmp string, squashMerge bool) (clone, branch string) {
	t.Helper()
	originPath := filepath.Join(tmp, "origin.git")
	runOrphanCleanupGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone = filepath.Join(tmp, "clone")
	runOrphanCleanupGit(t, tmp, "clone", originPath, clone)
	runOrphanCleanupGit(t, clone, "config", "user.email", "polecat@example.com")
	runOrphanCleanupGit(t, clone, "config", "user.name", "Polecat Test")

	writeOrphanCleanupFile(t, clone, "README.md", "seed\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "seed main")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", "main")

	branch = testOrphanBranch
	runOrphanCleanupGit(t, clone, "checkout", "-b", branch)
	writeOrphanCleanupFile(t, clone, "work.txt", "polecat work\n")
	runOrphanCleanupGit(t, clone, "add", "-A")
	runOrphanCleanupGit(t, clone, "commit", "-m", "polecat work")
	runOrphanCleanupGit(t, clone, "push", "-u", "origin", branch)

	if !squashMerge {
		return clone, branch
	}
	runOrphanCleanupGit(t, clone, "checkout", "main")
	runOrphanCleanupGit(t, clone, "merge", "--squash", branch)
	runOrphanCleanupGit(t, clone, "commit", "-m", "merge squash")
	runOrphanCleanupGit(t, clone, "push", "origin", "main")
	return clone, branch
}

func runOrphanCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

const testOrphanBranch = "polecat/test/gt-orphan"

func writeOrphanCleanupFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// stubNoContainersOnce installs the stub below exactly once per test binary.
// Installed from a t.Parallel test, the seam's own runningGateContainers var
// would otherwise be written concurrently — the writes race even though every
// caller installs the same value.
var stubNoContainersOnce sync.Once

// stubNoContainers overrides package slot's docker-ps lookup so tests that
// drive slot.Acquire never shell out to the real docker CLI, which would poll
// for the full gate timeout whenever any dolt/testcontainers/ryuk container is
// up on the host (gt-tuiy). See slot.SetContainerListerForTest.
//
// It installs once and never restores (gt-k317): every caller wants the same
// lister, and a per-test restore raced under t.Parallel.
func stubNoContainers(t *testing.T) {
	t.Helper()
	stubNoContainersOnce.Do(func() {
		slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil })
	})
}
