//go:build integration

package buildsrc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// emptyTree is the sha of git's empty tree, which every fixture commit uses.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// absentSHA is a well-formed sha that no fixture repo has.
const absentSHA = "0123456789abcdef0123456789abcdef01234567"

// The tests below run real git against real temporary repos: the clone, the
// fetch and the checkout are the ones a production call makes.

// runGit runs git for a fixture and returns its trimmed stdout. Fixtures commit
// with a fixed identity so no host configuration is read for them.
func runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// seedBare makes a bare repo at bare with one commit on main and returns it.
func seedBare(t *testing.T, bare string) string {
	t.Helper()
	runGit(t, "init", "--bare", "--quiet", "--initial-branch=main", bare)
	first := commit(t, bare, "", "first")
	runGit(t, "--git-dir="+bare, "update-ref", "refs/heads/main", first)
	return first
}

// commit writes a commit on top of parent ("" for a root) into the bare repo
// and returns its sha. No ref is moved.
func commit(t *testing.T, bare, parent, msg string) string {
	t.Helper()
	args := []string{"--git-dir=" + bare, "commit-tree", emptyTree, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return runGit(t, args...)
}

// advance moves ref in bare to a new commit on top of its current tip and
// returns the new sha.
func advance(t *testing.T, bare, ref, msg string) string {
	t.Helper()
	tip := runGit(t, "--git-dir="+bare, "rev-parse", "--verify", ref)
	next := commit(t, bare, tip, msg)
	runGit(t, "--git-dir="+bare, "update-ref", ref, next)
	return next
}

// headOf returns the commit the checkout at dir has, and whether HEAD is
// detached.
func headOf(t *testing.T, dir string) (sha string, detached bool) {
	t.Helper()
	sha = runGit(t, "-C", dir, "rev-parse", "HEAD")
	detached = runGit(t, "-C", dir, "rev-parse", "--abbrev-ref", "HEAD") == "HEAD"
	return sha, detached
}

func TestIntegrationEnsureFollowsBareMainAcrossCalls(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	bare := filepath.Join(town, "gastown", ".repo.git")
	first := seedBare(t, bare)

	dir1, err := Ensure(context.Background(), town, "gastown", "")
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if got, detached := headOf(t, dir1); got != first || !detached {
		t.Fatalf("first call HEAD = %s (detached %v), want detached at %s", got, detached, first)
	}

	second := advance(t, bare, "refs/heads/main", "second")
	dir2, err := Ensure(context.Background(), town, "gastown", "")
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if dir2 != dir1 {
		t.Fatalf("second call returned %s, want the same directory %s", dir2, dir1)
	}
	if got, _ := headOf(t, dir2); got != second {
		t.Fatalf("second call HEAD = %s, want the new bare main %s", got, second)
	}
}

func TestIntegrationEnsureChecksOutNamedCommitDetached(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	bare := filepath.Join(town, "gastown", ".repo.git")
	first := seedBare(t, bare)
	// side is off main, so only a fetch by sha can bring it in.
	side := commit(t, bare, first, "side")
	runGit(t, "--git-dir="+bare, "update-ref", "refs/heads/side", side)

	dir, err := Ensure(context.Background(), town, "gastown", side)
	if err != nil {
		t.Fatalf("Ensure at rev: %v", err)
	}
	if got, detached := headOf(t, dir); got != side || !detached {
		t.Fatalf("HEAD = %s (detached %v), want detached at %s", got, detached, side)
	}
}

func TestIntegrationEnsureUnknownRevErrorsNamingIt(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	seedBare(t, filepath.Join(town, "gastown", ".repo.git"))

	_, err := Ensure(context.Background(), town, "gastown", absentSHA)
	if err == nil {
		t.Fatalf("Ensure at an absent sha returned no error")
	}
	if !strings.Contains(err.Error(), absentSHA) {
		t.Fatalf("error %q does not name the sha %s", err, absentSHA)
	}
}

// cloneCounter runs real git and counts the clones it is asked for.
type cloneCounter struct {
	mu     sync.Mutex
	clones int
}

func (c *cloneCounter) run(ctx context.Context, timeout time.Duration, dir string, args ...string) error {
	if len(args) > 0 && args[0] == "clone" {
		c.mu.Lock()
		c.clones++
		c.mu.Unlock()
	}
	return execGit(ctx, timeout, dir, args...)
}

// childFixtureEnv names the fixture repo for TestIntegrationExecGitChildProcess.
const childFixtureEnv = "BUILDSRC_EXECGIT_FIXTURE"

// TestIntegrationExecGitIgnoresCallerRepoEnv starts the test binary as a child
// whose environment points GIT_DIR at an empty repo. The child's execGit must
// still see the fixture repo at dir, which holds only if execGit drops the
// caller's GIT_DIR. The child runs in its own process because the variable has
// to be in the environment execGit reads.
func TestIntegrationExecGitIgnoresCallerRepoEnv(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	fixture := filepath.Join(town, "fixture")
	runGit(t, "init", "--quiet", "--initial-branch=main", fixture)
	runGit(t, "-C", fixture, "commit", "--quiet", "--allow-empty", "-m", "first")
	empty := filepath.Join(town, "empty.git")
	runGit(t, "init", "--bare", "--quiet", empty)

	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrationExecGitChildProcess$")
	cmd.Env = append(os.Environ(), childFixtureEnv+"="+fixture, "GIT_DIR="+empty)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
}

// TestIntegrationExecGitChildProcess is the child half of
// TestIntegrationExecGitIgnoresCallerRepoEnv. Run on its own it does nothing.
func TestIntegrationExecGitChildProcess(t *testing.T) {
	fixture := os.Getenv(childFixtureEnv)
	if fixture == "" {
		t.Skip("only run as the child of TestIntegrationExecGitIgnoresCallerRepoEnv")
	}
	if err := execGit(context.Background(), time.Minute, fixture, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		t.Fatalf("execGit in the fixture repo: %v", err)
	}
}

func TestIntegrationConcurrentCallsCloneOnce(t *testing.T) {
	t.Parallel()
	town := newTown(t)
	first := seedBare(t, filepath.Join(town, "gastown", ".repo.git"))
	counter := &cloneCounter{}

	const callers = 2
	start := make(chan struct{})
	dirs := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = ensure(context.Background(), counter.run, town, "gastown", "")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if dirs[0] != dirs[1] {
		t.Fatalf("callers got %s and %s, want one directory", dirs[0], dirs[1])
	}
	if counter.clones != 1 {
		t.Fatalf("clones = %d, want 1", counter.clones)
	}
	if got, _ := headOf(t, dirs[0]); got != first {
		t.Fatalf("HEAD = %s, want main %s", got, first)
	}
}
