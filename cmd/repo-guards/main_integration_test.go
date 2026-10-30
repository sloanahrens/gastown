//go:build integration

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegrationRepoGuardsSubdirectory runs repo-guards on a real repository
// with a tracked .beads/issues.jsonl. The file is only visible from the top
// level: a subdirectory root must be refused with exit 2, never read as clean.
func TestIntegrationRepoGuardsSubdirectory(t *testing.T) {
	scrubGitEnv(t)
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	writeFile(t, root, "go.mod", "module example.com/x\n\ngo 1.26\n")
	writeFile(t, root, ".beads/issues.jsonl", "{}\n")
	gitIn(t, root, "add", ".beads/issues.jsonl")
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if got := run([]string{sub}, liveEnv(), &stdout, &stderr); got != 2 {
		t.Errorf("subdirectory exit = %d, want 2; stderr %q", got, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("subdirectory stdout = %q, want none", stdout.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("is not the top level of its repository")) {
		t.Errorf("subdirectory stderr = %q, want the top-level refusal", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if got := run([]string{root}, liveEnv(), &stdout, &stderr); got != 1 {
		t.Errorf("top-level exit = %d, want 1; stderr %q", got, stderr.String())
	}
	if stderr.String() != issuesStderr {
		t.Errorf("top-level stderr = %q, want %q", stderr.String(), issuesStderr)
	}
}

// scrubGitEnv gives the test the environment a plain git user has. A git hook
// sets GIT_DIR and GIT_INDEX_FILE, which would point the temporary repository
// at the caller's. t.Setenv restores each variable when the test ends, so the
// test is not parallel.
func scrubGitEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// gitIn runs git in root. Its output is not shown on failure, as in the
// repoguard integration tests.
func gitIn(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v", args[0], err)
	}
}

// writeFile writes rel under root, creating its directory.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
