//go:build integration

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
			cmd := exec.Command("sh", "-c", cloneScript, "sh", tc.branch, remote, dest, strconv.Itoa(sandboxCloneDepth))
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

// TestIntegrationCloneScriptPinsNoPolecatBranch checks the fetch refspec the
// clone script leaves behind, then deletes the polecat branch on the remote the
// way a landing does. The bare fetch that gt done runs before a rebase must
// still succeed: the configured refspec names main alone.
func TestIntegrationCloneScriptPinsNoPolecatBranch(t *testing.T) {
	t.Parallel()
	remote := cloneReplayRemote(t, []string{"main", openshellTestBranch})
	dest := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("sh", "-c", cloneScript, "sh", openshellTestBranch, remote, dest, strconv.Itoa(sandboxCloneDepth))
	cmd.Env = cloneReplayEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone script: %v\n%s", err, out)
	}
	fetchRefspecs := cloneReplayGit(t, dest, "config", "--get-all", "remote.origin.fetch")
	if strings.Contains(fetchRefspecs, openshellTestBranch) {
		t.Fatalf("remote.origin.fetch pins the polecat branch:\n%s", fetchRefspecs)
	}
	if !strings.Contains(fetchRefspecs, "+refs/heads/main:refs/remotes/origin/main") {
		t.Fatalf("remote.origin.fetch lost the main refspec:\n%s", fetchRefspecs)
	}
	cloneReplayGit(t, strings.TrimPrefix(remote, "file://"), "update-ref", "-d", "refs/heads/"+openshellTestBranch)
	cloneReplayGit(t, dest, "fetch", "origin")
}

// TestIntegrationShallowCloneKeepsWhatGTDoneNeeds clones a repository whose
// history is deeper than the clone depth, with main having moved past the
// commit the polecat branch forked from, and runs what gt done does with
// history: merge-base against origin/main, an ancestry check, the commit count,
// a rebase onto origin/main, patch-id, and pushing the new commits. A depth
// shorter than the fork distance puts the fork point outside the shallow
// history, and the script then refetches in full rather than hand gt done a
// clone without a merge-base.
func TestIntegrationShallowCloneKeepsWhatGTDoneNeeds(t *testing.T) {
	t.Parallel()
	const (
		mainCommits  = 1500 // deeper than sandboxCloneDepth
		forkDistance = 20   // landings on main since the polecat branch forked
		polecatWork  = 3
	)
	remote, forkSHA := cloneDeepRemote(t, mainCommits, forkDistance, polecatWork)

	clone := func(t *testing.T, depth int) (string, error) {
		dest := filepath.Join(t.TempDir(), "clone")
		cmd := exec.Command("sh", "-c", cloneScript, "sh", openshellTestBranch, remote, dest, strconv.Itoa(depth))
		cmd.Env = cloneReplayEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return dest, fmt.Errorf("%w: %s", err, out)
		}
		return dest, nil
	}

	t.Run("sandbox depth", func(t *testing.T) {
		t.Parallel()
		dest, err := clone(t, sandboxCloneDepth)
		if err != nil {
			t.Fatalf("clone script: %v", err)
		}
		// The shallow file names where each history was cut. --is-shallow-repository
		// is already true on main's shallow fetch alone, so the branch's own
		// boundary is what catches a clone that dropped --depth.
		boundary := cloneReplayGit(t, strings.TrimPrefix(remote, "file://"), "rev-parse",
			fmt.Sprintf("%s~%d", openshellTestBranch, sandboxCloneDepth-1))
		shallow, err := os.ReadFile(filepath.Join(dest, ".git", "shallow"))
		if err != nil || !strings.Contains(string(shallow), boundary) {
			t.Fatalf("shallow file = %q (err %v), want the branch's depth boundary %s", shallow, err, boundary)
		}
		// origin/main's window starts exactly at the depth: a dropped --depth on
		// the fetch fails this count, not a size bound.
		if got := cloneReplayGit(t, dest, "rev-list", "--count", "origin/main"); got != strconv.Itoa(sandboxCloneDepth) {
			t.Errorf("origin/main holds %s commits, want exactly the depth %d", got, sandboxCloneDepth)
		}
		// The first fetch is shallow too: a full clone followed by a shallow
		// fetch of main still holds every object, which is the cost to avoid.
		counts := cloneReplayGit(t, dest, "count-objects", "-v")
		objects := 0
		for _, line := range strings.Split(counts, "\n") {
			if k, v, ok := strings.Cut(line, ": "); ok && (k == "count" || k == "in-pack") {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					t.Fatalf("count-objects line %q: %v", line, err)
				}
				objects += n
			}
		}
		if limit := 3*(sandboxCloneDepth+polecatWork) + 100; objects > limit {
			t.Errorf("the clone holds %d objects, want at most %d (three per commit at the depth)", objects, limit)
		}
		if got := cloneReplayGit(t, dest, "merge-base", "HEAD", "origin/main"); got != forkSHA {
			t.Errorf("merge-base = %s, want the fork point %s", got, forkSHA)
		}
		cloneReplayGit(t, dest, "merge-base", "--is-ancestor", forkSHA, "HEAD")
		if got := cloneReplayGit(t, dest, "rev-list", "--count", "origin/main..HEAD"); got != strconv.Itoa(polecatWork) {
			t.Errorf("origin/main..HEAD = %s commits, want %d", got, polecatWork)
		}
		if got := cloneReplayGit(t, dest, "rev-list", "--count", "HEAD..origin/main"); got != strconv.Itoa(forkDistance) {
			t.Errorf("HEAD..origin/main = %s commits, want %d", got, forkDistance)
		}
		cloneReplayGit(t, dest, "rebase", "origin/main")
		if got := cloneReplayGit(t, dest, "rev-list", "--count", "origin/main..HEAD"); got != strconv.Itoa(polecatWork) {
			t.Errorf("after the rebase origin/main..HEAD = %s commits, want %d", got, polecatWork)
		}
		cloneReplayGit(t, dest, "merge-base", "--is-ancestor", "origin/main", "HEAD")
		diff := cloneReplayGit(t, dest, "diff", "origin/main..HEAD")
		patch := exec.Command("git", "patch-id", "--stable")
		patch.Dir = dest
		patch.Env = cloneReplayEnv()
		patch.Stdin = strings.NewReader(diff + "\n")
		if out, err := patch.CombinedOutput(); err != nil || len(strings.Fields(string(out))) < 2 {
			t.Errorf("patch-id --stable: err = %v, output %q", err, out)
		}
		cloneReplayGit(t, dest, "push", "-q", "origin", "HEAD:refs/heads/polecat/pushed")
		if got := cloneReplayGit(t, filepath.Join(strings.TrimPrefix(remote, "file://")), "rev-parse", "refs/heads/polecat/pushed"); got != cloneReplayGit(t, dest, "rev-parse", "HEAD") {
			t.Errorf("the remote's polecat/pushed = %s, want the pushed HEAD", got)
		}
	})

	t.Run("a depth shorter than the fork distance still yields the merge-base", func(t *testing.T) {
		t.Parallel()
		dest, err := clone(t, forkDistance/2)
		if err != nil {
			t.Fatalf("clone script: %v", err)
		}
		if got := cloneReplayGit(t, dest, "merge-base", "HEAD", "origin/main"); got != forkSHA {
			t.Errorf("merge-base = %s, want the fork point %s: a depth that stops short of the fork point must refetch in full", got, forkSHA)
		}
	})
}

// cloneDeepRemote builds a bare remote whose main has mainCommits commits and
// whose polecat branch forked forkDistance commits before main's tip and holds
// polecatWork commits of its own. It returns the remote's file:// URL and the
// fork point's SHA.
func cloneDeepRemote(t *testing.T, mainCommits, forkDistance, polecatWork int) (string, string) {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	cloneReplayGit(t, root, "init", "-q", "-b", "main", seed)
	var stream strings.Builder
	for i := 1; i <= mainCommits; i++ {
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\ncommitter replay <replay@example.invalid> %d +0000\ndata <<EOM\nmain %d\nEOM\n", i, 1700000000+i, i)
		if i > 1 {
			fmt.Fprintf(&stream, "from :%d\n", i-1)
		}
		fmt.Fprintf(&stream, "M 100644 inline main.txt\ndata <<EOM\nline %d\nEOM\n\n", i)
	}
	imp := exec.Command("git", "fast-import", "--quiet")
	imp.Dir = seed
	imp.Env = cloneReplayEnv()
	imp.Stdin = strings.NewReader(stream.String())
	if out, err := imp.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %v\n%s", err, out)
	}
	cloneReplayGit(t, seed, "checkout", "-q", "main")
	forkSHA := cloneReplayGit(t, seed, "rev-parse", fmt.Sprintf("main~%d", forkDistance))
	cloneReplayGit(t, seed, "checkout", "-q", "-b", openshellTestBranch, forkSHA)
	for i := 1; i <= polecatWork; i++ {
		name := fmt.Sprintf("polecat%d.txt", i)
		if err := os.WriteFile(filepath.Join(seed, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cloneReplayGit(t, seed, "add", name)
		cloneReplayGit(t, seed, "commit", "-q", "-m", name)
	}
	remote := filepath.Join(root, "remote.git")
	cloneReplayGit(t, root, "clone", "-q", "--bare", seed, remote)
	return "file://" + remote, forkSHA
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
