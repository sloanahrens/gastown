//go:build integration

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestIntegrationGateLandingsReadsLandingCommits drives the landing reader
// against a throwaway repository: the landing commits it finds, their diffs,
// the non-landing commit it skips, and --last.
func TestIntegrationGateLandingsReadsLandingCommits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := gateTestEnv(t)
	gateTestCommit(t, dir, env, "a.txt", "init")
	gateTestCommit(t, dir, env, "b.txt", "land: polecat/a/gt-1 (aaa) onto main (bbb)")
	gateTestCommit(t, dir, env, "c.txt", "chore: not a landing")
	gateTestCommit(t, dir, env, "d.txt", "land: polecat/a/gt-2 (ccc) onto main (ddd)")

	landings, err := gateLandings(context.Background(), dir, "HEAD", 10)
	if err != nil {
		t.Fatalf("gateLandings: %v", err)
	}
	if len(landings) != 2 {
		t.Fatalf("landings = %d, want 2: only the land: subjects are landings", len(landings))
	}
	if !strings.Contains(landings[0].Subject, "gt-2") || !strings.Contains(landings[1].Subject, "gt-1") {
		t.Errorf("subjects = %q, %q, want newest first", landings[0].Subject, landings[1].Subject)
	}
	if len(landings[0].SHA) != 40 {
		t.Errorf("sha = %q, want the full 40-character sha", landings[0].SHA)
	}
	if !strings.Contains(landings[0].NameStatus, "d.txt") {
		t.Errorf("gt-2 diff = %q, want d.txt", landings[0].NameStatus)
	}
	if landings[0].Unreadable != "" {
		t.Errorf("gt-2 unreadable = %q, want a readable diff", landings[0].Unreadable)
	}

	one, err := gateLandings(context.Background(), dir, "HEAD", 1)
	if err != nil {
		t.Fatalf("gateLandings --last 1: %v", err)
	}
	if len(one) != 1 {
		t.Errorf("--last 1 read %d landings, want 1", len(one))
	}
}

// TestIntegrationGateLandingsMarksAParentlessDiff pins the fallback input: a
// landing commit with no parent cannot be diffed, so the reader says so and the
// model runs everything.
func TestIntegrationGateLandingsMarksAParentlessDiff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := gateTestEnv(t)
	gateTestCommit(t, dir, env, "a.txt", "land: the root landing")

	landings, err := gateLandings(context.Background(), dir, "HEAD", 1)
	if err != nil {
		t.Fatalf("gateLandings: %v", err)
	}
	if len(landings) != 1 {
		t.Fatalf("landings = %d, want 1", len(landings))
	}
	if landings[0].Unreadable == "" {
		t.Error("a root landing's diff must be marked unreadable, never read as no change")
	}
}

// TestIntegrationGateLandingsRefusesAnOptionRef pins the --base guard.
func TestIntegrationGateLandingsRefusesAnOptionRef(t *testing.T) {
	t.Parallel()
	if _, err := gateLandings(context.Background(), t.TempDir(), "--all", 1); err == nil {
		t.Fatal("a --base beginning with - must be refused, not passed to git")
	}
}

// TestIntegrationGateRepoRootRefusesADirectoryWithoutGit covers the refusal a
// plain directory gets, so the model never reads a repository it did not find.
func TestIntegrationGateRepoRootRefusesADirectoryWithoutGit(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := gateRepoRoot(context.Background()); err == nil {
		t.Fatal("gateRepoRoot outside a work tree = nil error, want a refusal")
	}
}

// gateTestEnv is the environment a throwaway repository's git runs in: an
// empty HOME, so the host's git config, identity and signing never apply.
func gateTestEnv(t *testing.T) []string {
	t.Helper()
	return testutil.CleanGTEnv("HOME=" + t.TempDir())
}

// gateTestCommit writes subject as file's content and commits it,
// initializing the repository on the first call.
func gateTestCommit(t *testing.T, dir string, env []string, file, subject string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		gateTestGit(t, dir, env, "init", "-q")
		gateTestGit(t, dir, env, "config", "user.name", "gate-test")
		gateTestGit(t, dir, env, "config", "user.email", "gate-test@example.test")
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(subject), 0o644); err != nil {
		t.Fatalf("writing %s: %v", file, err)
	}
	gateTestGit(t, dir, env, "add", file)
	gateTestGit(t, dir, env, "commit", "-q", "-m", subject)
}

// gateTestGit runs one git command in dir.
func gateTestGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
