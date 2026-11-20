//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// originGit runs git in dir with the child's environment fixed: no global or
// system config, and a commit identity passed on the command line, so the test
// does not depend on the host's git setup.
func originGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=nux", "-c", "user.email=nux@gastown.local", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// A rig's bare repository is cloned from a real origin, so the remote that
// answers the read is the one clone configured. When origin holds the seat's
// branch at its tip, the read answers that tip.
func TestIntegrationNukeSeatOriginTipRealOriginHoldingTheTipAnswersTheTip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	rigPath := filepath.Join(root, "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating the rig: %v", err)
	}

	originGit(t, root, "init", "-q", "--bare", origin)
	originGit(t, root, "init", "-q", seed)
	originGit(t, seed, "commit", "-q", "--allow-empty", "-m", "seat work")
	tip := originGit(t, seed, "rev-parse", "HEAD")
	originGit(t, seed, "push", "-q", origin, "HEAD:refs/heads/"+preserveBranch)
	originGit(t, root, "clone", "-q", "--bare", origin, filepath.Join(rigPath, ".repo.git"))

	got, err := rigOriginTip(rigPath)(preserveBranch)
	if err != nil {
		t.Fatalf("rigOriginTip: %v", err)
	}
	if got != tip {
		t.Fatalf("tip = %q, want origin's %q", got, tip)
	}
}

// Origin holding only an older commit, on refs other than the seat's branch,
// does not answer the seat's tip: the read asks for the seat's branch by name.
func TestIntegrationNukeSeatOriginTipRealOriginHoldingOnlyAnOlderCommitDoesNotAnswerTheSeatTip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	rigPath := filepath.Join(root, "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating the rig: %v", err)
	}

	originGit(t, root, "init", "-q", "--bare", origin)
	originGit(t, root, "init", "-q", seed)
	originGit(t, seed, "commit", "-q", "--allow-empty", "-m", "older work")
	older := originGit(t, seed, "rev-parse", "HEAD")
	originGit(t, seed, "push", "-q", origin, "HEAD:refs/heads/main")
	originGit(t, seed, "commit", "-q", "--allow-empty", "-m", "seat work")
	originGit(t, seed, "push", "-q", origin, "HEAD:refs/heads/polecat/other")
	originGit(t, root, "clone", "-q", "--bare", origin, filepath.Join(rigPath, ".repo.git"))

	got, err := rigOriginTip(rigPath)(preserveBranch)
	if err != nil {
		t.Fatalf("rigOriginTip: %v", err)
	}
	if got != "" {
		t.Fatalf("tip = %q, want none: origin holds only %q, on other refs", got, older)
	}
}

// An origin that cannot be reached is an error, never an empty tip: an empty
// tip reads as origin lacking the seat's branch, which would let the nuke go on.
func TestIntegrationNukeSeatOriginTipRealUnreachableOriginIsAnErrorNotOnOrigin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	rigPath := filepath.Join(root, "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating the rig: %v", err)
	}

	originGit(t, root, "init", "-q", "--bare", origin)
	originGit(t, root, "init", "-q", seed)
	originGit(t, seed, "commit", "-q", "--allow-empty", "-m", "seat work")
	originGit(t, seed, "push", "-q", origin, "HEAD:refs/heads/"+preserveBranch)
	gitDir := filepath.Join(rigPath, ".repo.git")
	originGit(t, root, "clone", "-q", "--bare", origin, gitDir)
	originGit(t, gitDir, "remote", "set-url", "origin", filepath.Join(root, "missing.git"))

	got, err := rigOriginTip(rigPath)(preserveBranch)
	if err == nil {
		t.Fatalf("tip = %q with no error, want an error: origin does not exist", got)
	}
	if got != "" {
		t.Fatalf("tip = %q with an error, want none", got)
	}
}
