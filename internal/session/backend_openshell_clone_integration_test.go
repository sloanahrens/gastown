//go:build integration

package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationCloneScriptLeavesOriginMainForGTDone runs the clone script the
// backend sends into a sandbox, with real git, against a local remote. gt done
// compares the polecat branch with origin/main, which a single-branch clone
// does not have, so the script must leave refs/remotes/origin/main behind and
// rev-list must run against it. A remote without main, or a missing branch,
// must fail the script.
func TestIntegrationCloneScriptLeavesOriginMainForGTDone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		branches []string // created on the remote in order; the first is its seed branch
		branch   string   // the branch the clone asks for
		wantOK   bool
	}{
		{"main and the polecat branch", []string{"main", openshellTestBranch}, openshellTestBranch, true},
		{"remote has no main", []string{openshellTestBranch}, openshellTestBranch, false},
		{"the branch is missing", []string{"main"}, openshellTestBranch, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			remote := cloneReplayRemote(t, tc.branches)
			dest := filepath.Join(t.TempDir(), "clone")
			cmd := exec.Command("sh", "-c", cloneScript, "sh", tc.branch, remote, dest)
			cmd.Env = cloneReplayEnv()
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("clone script: err = %v, want success = %v; output:\n%s", err, tc.wantOK, out)
			}
			if !tc.wantOK {
				return
			}
			cloneReplayGit(t, dest, "rev-parse", "--verify", "refs/remotes/origin/main")
			if n := cloneReplayGit(t, dest, "rev-list", "--count", "origin/main..HEAD"); n != "1" {
				t.Fatalf("origin/main..HEAD counts %s commits, want the one polecat commit", n)
			}
		})
	}
}

// cloneReplayRemote builds a bare remote holding the given branches, each one
// commit past the one before it, and returns its file:// URL.
func cloneReplayRemote(t *testing.T, branches []string) string {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	cloneReplayGit(t, root, "init", "-q", "-b", branches[0], seed)
	cloneReplayGit(t, seed, "commit", "-q", "--allow-empty", "-m", branches[0])
	for _, b := range branches[1:] {
		cloneReplayGit(t, seed, "checkout", "-q", "-b", b)
		cloneReplayGit(t, seed, "commit", "-q", "--allow-empty", "-m", b)
	}
	remote := filepath.Join(root, "remote.git")
	cloneReplayGit(t, root, "clone", "-q", "--bare", seed, remote)
	return "file://" + remote
}

// cloneReplayGit runs git in dir and returns its trimmed output. A failure
// fails the test with the output attached.
func cloneReplayGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = cloneReplayEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cloneReplayEnv is the environment for the replay: the host's PATH and a
// placeholder identity, with no global or system git config read.
func cloneReplayEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"FORGEJO_TOKEN=replay-token",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=replay",
		"GIT_AUTHOR_EMAIL=replay@example.invalid",
		"GIT_COMMITTER_NAME=replay",
		"GIT_COMMITTER_EMAIL=replay@example.invalid",
	}
}
