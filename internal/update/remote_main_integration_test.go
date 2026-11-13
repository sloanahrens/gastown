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
	full := append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)
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

// bareFixture makes a bare origin whose main holds one commit, a seed clone
// that pushes to it, and a rig's bare repository whose origin is that origin
// and whose refs/heads/main was last set to the first commit, so it lags
// origin once the seed pushes again. It returns the seed and the bare repository.
func bareFixture(t *testing.T) (seed, bare string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed = filepath.Join(root, "seed")
	bare = filepath.Join(root, "gastown", ".repo.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-q", "-b", "main", seed)
	gitIn(t, seed, "remote", "add", "origin", origin)
	first := commitFile(t, seed, "a")
	gitIn(t, seed, "push", "-q", "origin", "main")
	gitIn(t, root, "init", "-q", "--bare", bare)
	gitIn(t, bare, "remote", "add", "origin", origin)
	gitIn(t, bare, "fetch", "-q", "origin", "main")
	gitIn(t, bare, "update-ref", "refs/heads/main", first)
	return seed, bare
}

// TestIntegrationRemoteMainSHAReadsOriginNotBareRef drives the production
// ExecRun through RemoteMainSHA on real git. origin's main moves on without a
// fetch, so the bare repository's refs/heads/main is old; the answer is
// origin's tip.
func TestIntegrationRemoteMainSHAReadsOriginNotBareRef(t *testing.T) {
	t.Parallel()
	seed, bare := bareFixture(t)
	tip := commitFile(t, seed, "b")
	gitIn(t, seed, "push", "-q", "origin", "main")
	if stale := gitIn(t, bare, "rev-parse", "refs/heads/main"); stale == tip {
		t.Fatalf("bare refs/heads/main already at %s; the fixture did not lag origin", tip)
	}

	sha, err := RemoteMainSHA(context.Background(), ExecRun, bare)
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
	bare := filepath.Join(root, "gastown", ".repo.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "init", "-q", "--bare", bare)
	gitIn(t, bare, "remote", "add", "origin", origin)

	sha, err := RemoteMainSHA(context.Background(), ExecRun, bare)
	if err == nil {
		t.Fatalf("RemoteMainSHA() = %q, want an error for an origin with no main", sha)
	}
	if sha != "" {
		t.Errorf("RemoteMainSHA() sha = %q, want empty on error", sha)
	}
}

// TestIntegrationRemoteMainSHARefusesBareThatIsNotARepository: git ls-remote
// against a --git-dir that holds no repository exits nonzero, which is an
// error, not a blank sha.
func TestIntegrationRemoteMainSHARefusesBareThatIsNotARepository(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "gastown", ".repo.git")
	sha, err := RemoteMainSHA(context.Background(), ExecRun, bare)
	if err == nil {
		t.Fatalf("RemoteMainSHA() = %q, want an error outside a repository", sha)
	}
	if sha != "" {
		t.Errorf("RemoteMainSHA() sha = %q, want empty on error", sha)
	}
}
