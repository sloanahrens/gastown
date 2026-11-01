//go:build integration

package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runCrewStaleGit runs git in dir and returns its stdout, failing the test on a
// non-zero exit. It gives git a throwaway identity and no host config, so a
// signing or hook setting cannot decide the fixture.
func runCrewStaleGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Crew Stale",
		"GIT_AUTHOR_EMAIL=crew@example.com",
		"GIT_COMMITTER_NAME=Crew Stale",
		"GIT_COMMITTER_EMAIL=crew@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func writeCrewStaleFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestIntegrationCrewStaleReadsARealClone drives the command's real filesystem
// and git surfaces against a clone: the branch, the commits behind origin/main
// and FETCH_HEAD's time all come from git, and the run leaves the clone exactly
// as it found it.
func TestIntegrationCrewStaleReadsARealClone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	scratch := t.TempDir()
	origin := filepath.Join(scratch, "origin.git")
	seed := filepath.Join(scratch, "seed")
	clone := filepath.Join(town, "gastown", "crew", "dunlin")

	runCrewStaleGit(t, scratch, "init", "-q", "--bare", "--initial-branch=main", origin)
	runCrewStaleGit(t, scratch, "init", "-q", "--initial-branch=main", seed)
	writeCrewStaleFile(t, filepath.Join(seed, "a.txt"), "base\n")
	runCrewStaleGit(t, seed, "add", "a.txt")
	runCrewStaleGit(t, seed, "commit", "-q", "-m", "base")
	runCrewStaleGit(t, seed, "remote", "add", "origin", origin)
	runCrewStaleGit(t, seed, "push", "-q", "origin", "main")

	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatalf("create the crew directory: %v", err)
	}
	runCrewStaleGit(t, scratch, "clone", "-q", origin, clone)

	// origin/main moves ahead and the clone fetches, which is what writes the
	// FETCH_HEAD whose time the report reads.
	writeCrewStaleFile(t, filepath.Join(seed, "b.txt"), "later\n")
	runCrewStaleGit(t, seed, "add", "b.txt")
	runCrewStaleGit(t, seed, "commit", "-q", "-m", "later")
	runCrewStaleGit(t, seed, "push", "-q", "origin", "main")
	runCrewStaleGit(t, clone, "fetch", "-q", "origin")

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fetchHead := filepath.Join(clone, ".git", "FETCH_HEAD")
	fetched := now.Add(-120 * time.Hour)
	if err := os.Chtimes(fetchHead, fetched, fetched); err != nil {
		t.Fatalf("set the mtime of %s: %v", fetchHead, err)
	}
	beforeHead := runCrewStaleGit(t, clone, "rev-parse", "HEAD")
	beforeFetch, err := os.Stat(fetchHead)
	if err != nil {
		t.Fatalf("stat %s: %v", fetchHead, err)
	}

	deps := crewStaleRealDeps()
	deps.findTown = func() (string, error) { return town, nil }
	deps.now = func() time.Time { return now }

	var out bytes.Buffer
	if err := crewStale(&out, deps); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin  main  120h  1\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}

	if after := runCrewStaleGit(t, clone, "rev-parse", "HEAD"); after != beforeHead {
		t.Errorf("HEAD moved from %s to %s: the command changed the clone",
			strings.TrimSpace(beforeHead), strings.TrimSpace(after))
	}
	afterFetch, err := os.Stat(fetchHead)
	if err != nil {
		t.Fatalf("stat %s after the run: %v", fetchHead, err)
	}
	if !afterFetch.ModTime().Equal(beforeFetch.ModTime()) {
		t.Errorf("FETCH_HEAD mtime moved from %v to %v: the command wrote to the clone",
			beforeFetch.ModTime(), afterFetch.ModTime())
	}
}
