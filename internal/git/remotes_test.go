package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const noUpstream = "error: No such remote 'upstream'\n"

// splitRemotes answers the remote lookups of a polecat clone whose origin
// fetches from upstream and pushes to a fork.
func splitRemotes() *scripted {
	return newScripted(map[string]reply{
		"remote get-url origin":                 ok("/srv/upstream.git\n"),
		"remote get-url --push origin":          ok("/srv/fork.git\n"),
		"remote get-url upstream":               fail(2, noUpstream),
		"symbolic-ref refs/remotes/origin/HEAD": ok("refs/remotes/origin/main\n"),
	})
}

// plainRemotes answers for a clone whose origin fetches and pushes to one URL.
func plainRemotes() *scripted {
	return newScripted(map[string]reply{
		"remote get-url origin":                 ok("/srv/origin.git\n"),
		"remote get-url --push origin":          ok("/srv/origin.git\n"),
		"remote get-url upstream":               fail(2, noUpstream),
		"symbolic-ref refs/remotes/origin/HEAD": ok("refs/remotes/origin/main\n"),
	})
}

func TestForkBackedDefaultPushGuardSplitPushURL(t *testing.T) {
	t.Parallel()
	s := splitRemotes()
	g := newTestGit(t, s)
	if !g.ForkBackedRemote("origin") {
		t.Fatal("split push URL should be detected as fork-backed")
	}
	if got := g.CleanDefaultBranchBaseRef("origin", "main"); got != "origin/main" {
		t.Fatalf("CleanDefaultBranchBaseRef = %q, want origin/main", got)
	}
	for _, refspec := range []string{"main", "refs/heads/main", "feature:main", "feature:refs/heads/main", "HEAD:main", "+feature:main", ":main"} {
		if err := g.RefuseForkBackedDefaultPush("origin", refspec, "main"); err == nil {
			t.Fatalf("RefuseForkBackedDefaultPush(%q) = nil, want refusal", refspec)
		}
	}
	if err := g.PushWithEnv("origin", "HEAD:main", false, []string{"GT_INTEGRATION_LAND=1"}); err == nil {
		t.Fatal("GT_INTEGRATION_LAND must not bypass the fork default-branch guard")
	}
	for _, c := range s.sent() {
		if strings.HasPrefix(c, "push") {
			t.Fatalf("a refused push reached git: %q", c)
		}
	}
	// A feature branch still pushes.
	s.on("push origin polecat/fork-policy:polecat/fork-policy", ok(""))
	if err := g.Push("origin", "polecat/fork-policy:polecat/fork-policy", false); err != nil {
		t.Fatalf("feature branch push: %v", err)
	}
}

func TestForkBackedDefaultPushGuardOriginForkWithUpstream(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"remote get-url origin":                 ok("https://github.com/fork/repo.git\n"),
		"remote get-url --push origin":          ok("https://github.com/fork/repo.git\n"),
		"remote get-url upstream":               ok("git@github.com:upstream/repo.git\n"),
		"symbolic-ref refs/remotes/origin/HEAD": ok("refs/remotes/origin/main\n"),
	})
	g := newTestGit(t, s)
	if !g.ForkBackedRemote("origin") {
		t.Fatal("origin fork plus a distinct upstream should be fork-backed")
	}
	if got := g.CleanDefaultBranchBaseRef("origin", "main"); got != "upstream/main" {
		t.Fatalf("CleanDefaultBranchBaseRef = %q, want upstream/main", got)
	}
	if err := g.Push("origin", "feature:main", false); err == nil {
		t.Fatal("Push should block feature-to-default in fork/upstream topology")
	}
}

// An upstream that is the same repository as origin, spelled differently,
// does not make origin a fork.
func TestForkBackedRemoteComparesNormalizedURLs(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"remote get-url origin":        ok("https://github.com/Owner/Repo.git\n"),
		"remote get-url --push origin": ok("git@github.com:owner/repo\n"),
		"remote get-url upstream":      ok("ssh://git@github.com/owner/repo.git/\n"),
	})
	if newTestGit(t, s).ForkBackedRemote("origin") {
		t.Fatal("the same repository behind three URL spellings is not a fork")
	}
}

func TestForkBackedDefaultPushGuardAllowsNormalDefaultPush(t *testing.T) {
	t.Parallel()
	s := plainRemotes().on("push origin main", ok(""))
	g := newTestGit(t, s)
	if g.ForkBackedRemote("origin") {
		t.Fatal("plain origin should not be fork-backed")
	}
	if err := g.Push("origin", "main", false); err != nil {
		t.Fatalf("normal default push: %v", err)
	}
	if c, _ := s.sentCall("push origin main"); c.timeout != pushTimeout {
		t.Errorf("push timeout = %v, want %v", c.timeout, pushTimeout)
	}
}

// PushWithEnv hands its environment to git, and so to the pre-push hook
// (GT_INTEGRATION_LAND=1 is for the hook); force adds --force.
func TestPushWithEnvPassesEnvironment(t *testing.T) {
	t.Parallel()
	s := plainRemotes().on("push origin feature --force", ok(""))
	env := []string{"GT_INTEGRATION_LAND=1"}
	if err := newTestGit(t, s).PushWithEnv("origin", "feature", true, env); err != nil {
		t.Fatalf("PushWithEnv: %v", err)
	}
	c, found := s.sentCall("push origin feature --force")
	if !found || !reflect.DeepEqual(c.env, env) || c.timeout != pushTimeout {
		t.Fatalf("push call = %+v (found %v), want env %v and timeout %v", c, found, env, pushTimeout)
	}
}

// A call that runs out its timeout reports a timeout, not git's output.
func TestTimedOutCallReportsTimeout(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	g := &Git{workDir: t.TempDir(), exec: func(c gitCall) (string, string, error) {
		s.run(c)
		return "", "", errTimedOut
	}}
	err := g.FetchRefspecWithTimeout("origin", "+refs/heads/main:refs/remotes/origin/main", 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "git fetch timed out after 3s (remote may be unreachable)") {
		t.Fatalf("error = %v, want the timeout message", err)
	}
	var ge *GitError
	if errors.As(err, &ge) {
		t.Fatalf("a timeout must not look like git's own failure: %+v", ge)
	}
	if c := s.calls[0]; c.timeout != 3*time.Second || strings.Join(c.args, " ") != "fetch origin +refs/heads/main:refs/remotes/origin/main" {
		t.Fatalf("call = %+v", c)
	}
	err = g.PushWithEnv("origin", "feature", false, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1m0s") {
		t.Fatalf("PushWithEnv error = %v, want the push timeout message", err)
	}
}

func TestFetchDefaultBranchWithTimeoutFetchesRemoteDefault(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"symbolic-ref refs/remotes/origin/HEAD": fail(128, "fatal: ref refs/remotes/origin/HEAD is not a symbolic ref\n"),
		"rev-parse --verify origin/master":      fail(128, "fatal: Needed a single revision\n"),
		"rev-parse --verify origin/main":        ok("5e06d1a1792be1930476f5a334963d4de44e8ea3\n"),
		"fetch origin main":                     ok(""),
	})
	if err := newTestGit(t, s).FetchDefaultBranchWithTimeout("origin", time.Minute); err != nil {
		t.Fatalf("FetchDefaultBranchWithTimeout: %v", err)
	}
	if c, _ := s.sentCall("fetch origin main"); c.timeout != time.Minute {
		t.Fatalf("fetch timeout = %v", c.timeout)
	}
}

// git config --unset-all exits 5 when the key is absent: clearing a push URL
// that was never set is a no-op. Any other failure is reported.
func TestClearPushURL(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"config --unset-all remote.origin.pushurl": fail(5, "")})
	g := newTestGit(t, s)
	if err := g.ClearPushURL("origin"); err != nil {
		t.Fatalf("ClearPushURL with no push URL: %v", err)
	}
	s.on("config --unset-all remote.fork.pushurl", fail(3, "error: could not lock config file .git/config: File exists\n"))
	if err := g.ClearPushURL("fork"); err == nil {
		t.Fatal("ClearPushURL must report a failure other than the absent key")
	}
}

func TestUpstreamRemote(t *testing.T) {
	t.Parallel()
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		s := newScripted(map[string]reply{"remote get-url upstream": fail(2, noUpstream)})
		g := newTestGit(t, s)
		if has, err := g.HasUpstreamRemote(); err != nil || has {
			t.Fatalf("HasUpstreamRemote = %v, %v", has, err)
		}
		if url, err := g.GetUpstreamURL(); err != nil || url != "" {
			t.Fatalf("GetUpstreamURL = %q, %v", url, err)
		}
		s.on("remote add upstream https://example.com/u.git", ok(""))
		if err := g.AddUpstreamRemote("https://example.com/u.git"); err != nil || !s.hasSent("remote add upstream https://example.com/u.git") {
			t.Fatalf("AddUpstreamRemote = %v; calls %q", err, s.sent())
		}
	})
	t.Run("same URL is a no-op", func(t *testing.T) {
		t.Parallel()
		s := newScripted(map[string]reply{"remote get-url upstream": ok("https://example.com/u.git\n")})
		if err := newTestGit(t, s).AddUpstreamRemote("https://example.com/u.git"); err != nil {
			t.Fatal(err)
		}
		for _, c := range s.sent() {
			if c != "remote get-url upstream" {
				t.Fatalf("a no-op add changed the remote: %q", s.sent())
			}
		}
	})
	t.Run("different URL is updated", func(t *testing.T) {
		t.Parallel()
		s := newScripted(map[string]reply{
			"remote get-url upstream":                           ok("https://example.com/u.git\n"),
			"remote set-url upstream https://example.com/v.git": ok(""),
		})
		if err := newTestGit(t, s).AddUpstreamRemote("https://example.com/v.git"); err != nil {
			t.Fatal(err)
		}
		s.noUnscripted(t)
	})
	t.Run("other failure is reported", func(t *testing.T) {
		t.Parallel()
		s := newScripted(map[string]reply{"remote get-url upstream": fail(128, "fatal: not a git repository (or any of the parent directories): .git\n")})
		if _, err := newTestGit(t, s).HasUpstreamRemote(); err == nil {
			t.Fatal("HasUpstreamRemote must not read a broken repo as no upstream")
		}
	})
}

// With a split push URL, the push-side queries ask the push URL; without one,
// they ask the remote by name.
func TestPushRemoteBranchTipAsksThePushTarget(t *testing.T) {
	t.Parallel()
	s := splitRemotes().on("ls-remote --heads /srv/fork.git polecat/x", ok("5e06d1a1792be1930476f5a334963d4de44e8ea3\trefs/heads/polecat/x\n"))
	tip, err := newTestGit(t, s).PushRemoteBranchTip("origin", "polecat/x")
	if err != nil || tip != "5e06d1a1792be1930476f5a334963d4de44e8ea3" {
		t.Fatalf("PushRemoteBranchTip = %q, %v", tip, err)
	}
	s = plainRemotes().on("ls-remote --heads origin polecat/x", ok(""))
	if tip, err := newTestGit(t, s).PushRemoteBranchTip("origin", "polecat/x"); err != nil || tip != "" {
		t.Fatalf("PushRemoteBranchTip (absent) = %q, %v", tip, err)
	}
}

// parseLSRemoteTip picks the exact branch: ls-remote matches patterns by
// suffix, so polecat/x also lists refs/heads/other/polecat/x.
func TestParseLSRemoteTipMatchesExactBranch(t *testing.T) {
	t.Parallel()
	out := "1111111111111111111111111111111111111111\trefs/heads/other/polecat/x\n2222222222222222222222222222222222222222\trefs/heads/polecat/x\n"
	if got := parseLSRemoteTip(out, "polecat/x"); got != "2222222222222222222222222222222222222222" {
		t.Fatalf("parseLSRemoteTip = %q", got)
	}
}

func TestVerifyPushedCommit(t *testing.T) {
	t.Parallel()
	const pushed = "5e06d1a1792be1930476f5a334963d4de44e8ea3"
	for _, tt := range []struct {
		name, commit string
		lsRemote     reply
		wantErr      string
	}{
		{"tip is the commit", pushed, ok(pushed + "\trefs/heads/main\n"), ""},
		{"tip moved", pushed, ok("1111111111111111111111111111111111111111\trefs/heads/main\n"), "not on origin/main (remote tip 11111111"},
		{"branch missing", pushed, ok(""), "missing after push"},
		{"empty commit", " ", ok(""), "empty commit"},
		{"remote unreadable", pushed, fail(128, "fatal: '/srv/origin.git' does not appear to be a git repository\n"), "unable to read origin/main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := plainRemotes().on("ls-remote --heads origin main", tt.lsRemote)
			err := newTestGit(t, s).VerifyPushedCommit("origin", "main", tt.commit)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyPushedCommit: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "verified_push_failed") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want verified_push_failed ... %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeleteRemoteBranchIfAtUsesALease(t *testing.T) {
	t.Parallel()
	const old = "5e06d1a1792be1930476f5a334963d4de44e8ea3"
	s := newScripted(map[string]reply{
		"push --force-with-lease=refs/heads/polecat/x:" + old + " origin :refs/heads/polecat/x": fail(1, " ! [rejected]        polecat/x (stale info)\nerror: failed to push some refs to '/srv/origin.git'\n"),
	})
	if err := newTestGit(t, s).DeleteRemoteBranchIfAt("origin", "polecat/x", old); err == nil {
		t.Fatal("a rejected lease must be reported")
	}
	s.noUnscripted(t)
}

// git branch -r also prints the symbolic origin/HEAD entry, which names no
// branch of its own.
func TestRemoteRefsContaining(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"branch -r --contains abc": ok("  origin/HEAD -> origin/main\n  origin/main\n  origin/polecat/x\n"),
		"branch -r --contains def": ok(""),
	})
	g := newTestGit(t, s)
	if refs, err := g.RemoteRefsContaining("abc"); err != nil || !reflect.DeepEqual(refs, []string{"origin/main", "origin/polecat/x"}) {
		t.Fatalf("RemoteRefsContaining(abc) = %q, %v", refs, err)
	}
	if refs, err := g.RemoteRefsContaining("def"); err != nil || len(refs) != 0 {
		t.Fatalf("RemoteRefsContaining(def) = %q, %v", refs, err)
	}
	if _, err := g.RemoteRefsContaining("  "); err == nil {
		t.Fatal("an empty sha must fail rather than report nothing reachable")
	}
	s.noUnscripted(t)
}
