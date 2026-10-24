//go:build integration

package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestIntegrationPushOrAcceptRemoteAhead runs the push step against real git: a
// host clone pushes to a local remote, and a sandbox clone may have moved the
// branch on the remote first. The unit tier pins the same decisions against a
// fake; this pins them against the refusal git actually prints.
func TestIntegrationPushOrAcceptRemoteAhead(t *testing.T) {
	t.Parallel()
	t.Run("first push of a new branch is accepted", func(t *testing.T) {
		t.Parallel()
		remote := cloneReplayRemote(t, []string{"main", "b"})
		host := pushReplayHost(t, remote, "main")
		cloneReplayGit(t, host, "checkout", "-q", "-b", "feature")
		cloneReplayGit(t, host, "commit", "-q", "--allow-empty", "-m", "feature")
		if err := pushOrAcceptRemoteAhead(pushReplayGit(host), "feature"); err != nil {
			t.Fatalf("first push of a new branch: %v", err)
		}
	})
	t.Run("a remote ahead of the host is accepted", func(t *testing.T) {
		t.Parallel()
		remote := cloneReplayRemote(t, []string{"main", "b"})
		host := pushReplayHost(t, remote, "b")
		pushReplaySandboxCommit(t, remote, "b")
		g := pushReplayGit(host)
		if err := g.Push("origin", "b", false); err == nil || !strings.Contains(err.Error(), "[rejected]") {
			t.Fatalf("setup: a plain push = %v, want git's [rejected] refusal", err)
		}
		if err := pushOrAcceptRemoteAhead(g, "b"); err != nil {
			t.Fatalf("remote ahead: %v", err)
		}
	})
	t.Run("a diverged remote is refused and left alone", func(t *testing.T) {
		t.Parallel()
		remote := cloneReplayRemote(t, []string{"main", "b"})
		host := pushReplayHost(t, remote, "b")
		cloneReplayGit(t, host, "commit", "-q", "--allow-empty", "-m", "host work")
		sandboxTip := pushReplaySandboxCommit(t, remote, "b")
		err := pushOrAcceptRemoteAhead(pushReplayGit(host), "b")
		if err == nil || !strings.Contains(err.Error(), "[rejected]") {
			t.Fatalf("diverged push = %v, want the push's own [rejected] refusal", err)
		}
		if tip := pushReplayRemoteTip(t, host, "b"); tip != sandboxTip {
			t.Errorf("remote b = %s, want the sandbox's %s: nothing may be forced", tip, sandboxTip)
		}
	})
}

// pushReplayHost clones remote into a fresh host repo and checks out branch.
func pushReplayHost(t *testing.T, remote, branch string) string {
	t.Helper()
	host := filepath.Join(t.TempDir(), "host")
	cloneReplayGit(t, filepath.Dir(host), "clone", "-q", remote, host)
	cloneReplayGit(t, host, "checkout", "-q", branch)
	return host
}

// pushReplaySandboxCommit clones remote as the sandbox does, commits on branch
// and pushes it, leaving the remote's branch ahead of any host clone made
// before. It returns the sandbox's new tip.
func pushReplaySandboxCommit(t *testing.T, remote, branch string) string {
	t.Helper()
	sandbox := pushReplayHost(t, remote, branch)
	cloneReplayGit(t, sandbox, "commit", "-q", "--allow-empty", "-m", "sandbox work")
	cloneReplayGit(t, sandbox, "push", "-q", "origin", branch)
	return cloneReplayGit(t, sandbox, "rev-parse", "HEAD")
}

// pushReplayGit is the push step's view of a host clone: the git dir of the
// clone, with no work tree, as pushToForgejo builds it.
func pushReplayGit(host string) *git.Git {
	return git.NewGitWithDir(filepath.Join(host, ".git"), "")
}

// pushReplayRemoteTip returns the sha the remote holds for branch.
func pushReplayRemoteTip(t *testing.T, host, branch string) string {
	t.Helper()
	fields := strings.Fields(cloneReplayGit(t, host, "ls-remote", "origin", "refs/heads/"+branch))
	if len(fields) == 0 {
		t.Fatalf("remote has no branch %s", branch)
	}
	return fields[0]
}
