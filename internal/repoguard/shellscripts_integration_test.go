//go:build integration

package repoguard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestIntegrationNoNewShellScripts holds every tracked .sh file to
// shell-scripts.txt. The listing runs real git, so the test lives in the
// integration tier: the unit tier runs no git (internal/testpolicy/realgit.txt,
// docs/testing.md "Seams for external tools"). The allowlist is the single
// source of truth, compared in both directions, so a new script has no line
// and a line whose script is gone fails.
func TestIntegrationNoNewShellScripts(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	allowed, err := ReadAllowlist("shell-scripts.txt")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckShellScripts(func() ([]string, error) { return trackedShellScripts(root) }, allowed)
	if err != nil {
		t.Fatalf("listing the tracked .sh files: %v", err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

// TestIntegrationTrackedShellScriptsFailsOutsideGit: a listing that cannot see
// the tree is an error, never an empty tree, so the guard cannot pass by
// failing to read git.
func TestIntegrationTrackedShellScriptsFailsOutsideGit(t *testing.T) {
	t.Parallel()
	if got, err := trackedShellScripts(t.TempDir()); err == nil {
		t.Fatalf("trackedShellScripts(non-repo) = %q, want an error", got)
	}
}

// trackedShellScripts is the real lister: every .sh file git tracks under
// root, sorted. It lives in this integration-tagged file because the unit tier
// runs no git; the guard itself takes a Lister so its unit tests inject a fake.
func trackedShellScripts(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--", "*.sh").Output() //nolint:gosec // G204: fixed argv; root is the repository under test
	if err != nil {
		return nil, fmt.Errorf("git ls-files '*.sh' in %s: %w", root, err)
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	sort.Strings(files)
	return files, nil
}

// repoRoot is the gastown checkout this package sits in. The test reads the
// checked-in allowlist and the tree's tracked scripts, both relative to it.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
