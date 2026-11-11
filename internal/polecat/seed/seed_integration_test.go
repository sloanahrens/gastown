//go:build integration

package seed

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The unit tier runs no real git (internal/testpolicy/gitfree.txt), so this
// test, which runs the production Git seam against a real bare repo, is in the
// integration tier. The gate does not run it.
func TestIntegrationStageAndGuardReadARealBareRepo(t *testing.T) {
	t.Parallel()
	const rig = "devops"
	const gomod = "module example.com/devops\n"
	town := t.TempDir()
	bare := filepath.Join(town, rig, ".repo.git")
	commit := makeBareRepo(t, bare, gomod)

	dest := t.TempDir()
	sources := []Source{{Rig: rig, Commit: commit}}
	if err := Stage(dest, town, "gastown", "", sources, ExecGit); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, rig, "go.mod"))
	if err != nil {
		t.Fatalf("staged go.mod: %v", err)
	}
	if string(got) != gomod {
		t.Errorf("staged go.mod = %q, want the committed %q", got, gomod)
	}

	notes, err := Guard(sources, "gastown", town, ExecGit)
	if err != nil {
		t.Fatalf("Guard on a pin equal to main = %v, want nil", err)
	}
	if len(notes) != 0 {
		t.Errorf("Guard on a pin equal to main noted %v, want none", notes)
	}

	const unknown = "1111111111111111111111111111111111111111"
	_, err = Guard([]Source{{Rig: rig, Commit: unknown}}, "gastown", town, ExecGit)
	if err == nil || !strings.Contains(err.Error(), bare) {
		t.Errorf("Guard on a pin the bare repo lacks = %v, want an error naming %s", err, bare)
	}
}

// makeBareRepo commits go.mod in a work repository, then clones it bare to
// bare, the way a rig's .repo.git is made. It returns the commit.
func makeBareRepo(t *testing.T, bare, gomod string) string {
	t.Helper()
	work := t.TempDir()
	gitRun(t, "", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	gitRun(t, work, "add", "go.mod")
	gitRun(t, work, "commit", "-q", "-m", "seed")
	commit := gitRun(t, work, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	gitRun(t, "", "clone", "-q", "--bare", work, bare)
	return commit
}

// gitRun runs git with args in dir ("" for the test's directory) under a
// config that reads no user or system settings, and returns its trimmed
// stdout.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@example.invalid",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@example.invalid",
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String())
}
