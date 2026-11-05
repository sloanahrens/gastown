//go:build integration

package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git with args in dir and returns its trimmed stdout. A failed
// command stops the test.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=test", "-c", "user.email=test@test.com"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile adds a file named name to dir, commits it, and returns the new HEAD.
func commitFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-q", "-m", "add "+name)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// originFixture makes a bare origin whose main holds one commit, a seed clone
// that pushes to it, and a checkout cloned from it. It returns the seed and
// the checkout.
func originFixture(t *testing.T) (seed, checkout string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed = filepath.Join(root, "seed")
	checkout = filepath.Join(root, "checkout")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-q", "-b", "main", seed)
	gitIn(t, seed, "remote", "add", "origin", origin)
	commitFile(t, seed, "a")
	gitIn(t, seed, "push", "-q", "origin", "main")
	gitIn(t, root, "clone", "-q", origin, checkout)
	return seed, checkout
}

// TestIntegrationRemoteMainSHAReadsOriginNotCheckoutRef drives the production
// ExecRun through RemoteMainSHA on real git. origin's main moves on without a
// fetch, so the checkout's origin/main lags; the answer is origin's tip.
func TestIntegrationRemoteMainSHAReadsOriginNotCheckoutRef(t *testing.T) {
	t.Parallel()
	seed, checkout := originFixture(t)
	tip := commitFile(t, seed, "b")
	gitIn(t, seed, "push", "-q", "origin", "main")
	if stale := gitIn(t, checkout, "rev-parse", "origin/main"); stale == tip {
		t.Fatalf("checkout origin/main already at %s; the fixture did not lag origin", tip)
	}

	sha, err := RemoteMainSHA(context.Background(), ExecRun, checkout)
	if err != nil {
		t.Fatalf("RemoteMainSHA: %v", err)
	}
	if sha != tip {
		t.Errorf("RemoteMainSHA() = %s, want origin's tip %s", sha, tip)
	}
}

// TestIntegrationRemoteMainSHARefusesOriginWithoutMain: a real ls-remote that
// names no refs/heads/main exits 0 with no output, which is an error, not a
// blank sha.
func TestIntegrationRemoteMainSHARefusesOriginWithoutMain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	checkout := filepath.Join(root, "checkout")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-q", "-b", "main", checkout)
	gitIn(t, checkout, "remote", "add", "origin", origin)

	sha, err := RemoteMainSHA(context.Background(), ExecRun, checkout)
	if err == nil {
		t.Fatalf("RemoteMainSHA() = %q, want an error for an origin with no main", sha)
	}
	if sha != "" {
		t.Errorf("RemoteMainSHA() sha = %q, want empty on error", sha)
	}
}

// TestIntegrationRemoteMainSHARefusesRepoRootThatIsNotARepository: git
// ls-remote in a directory with no repository exits nonzero, which is an
// error, not a blank sha.
func TestIntegrationRemoteMainSHARefusesRepoRootThatIsNotARepository(t *testing.T) {
	t.Parallel()
	sha, err := RemoteMainSHA(context.Background(), ExecRun, t.TempDir())
	if err == nil {
		t.Fatalf("RemoteMainSHA() = %q, want an error outside a repository", sha)
	}
	if sha != "" {
		t.Errorf("RemoteMainSHA() sha = %q, want empty on error", sha)
	}
}
