package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	preserveTestBranch = "polecat/bullet/gt-1"
	preserveTestTip    = "1111111111111111111111111111111111111111"
)

// preserveTestRepo is a bare rig repository under a test root, opened the way
// nuke opens it (NewGitWithDir) and driven by a scripted runner.
type preserveTestRepo struct {
	s  *scripted
	g  *Git
	ri string // the test root
	// bare, seat and prefix are the repository, the seat's worktree, and the
	// --git-dir prefix NewGitWithDir puts on the repository's own commands.
	bare, seat, prefix string
}

func newPreserveTestRepo(t *testing.T) preserveTestRepo {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, ".repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("make the bare repository: %v", err)
	}
	s := newScripted(nil)
	// The seat's checkout has to exist on disk: a handle refuses to run in a
	// directory that is gone, as git would.
	seat := filepath.Join(root, "polecats", "bullet", "gastown")
	if err := os.MkdirAll(seat, 0o755); err != nil {
		t.Fatalf("make the seat's worktree: %v", err)
	}
	// Every preserve run starts here: the branch's tip, the refresh of the
	// remote's state, and the reachability read that decides whether to push.
	s.on("--git-dir="+bare+" rev-parse refs/heads/"+preserveTestBranch, ok(preserveTestTip))
	s.on("--git-dir="+bare+" fetch --prune origin", ok(""))
	s.on("--git-dir="+bare+" branch -r --contains "+preserveTestTip, ok(""))
	// Every push reads the remote's default branch first; the preserve branch
	// is never it, so the call ends there.
	s.on("symbolic-ref refs/remotes/origin/HEAD", ok("refs/remotes/origin/main\n"))
	return preserveTestRepo{
		s: s, g: &Git{gitDir: bare, exec: s.run},
		ri: root, bare: bare,
		seat:   seat,
		prefix: "--git-dir=" + bare + " ",
	}
}

// workRoot is the directory a temporary preserve worktree may be made in.
func (r preserveTestRepo) workRoot() string { return filepath.Join(r.ri, ".runtime", "preserve") }

// onlyWorktreeOn scripts a listing with one worktree, the seat's, on branch.
func (r preserveTestRepo) onlyWorktreeOn(branch string) {
	r.s.on(r.prefix+"worktree list --porcelain",
		ok("worktree "+r.seat+"\nHEAD "+preserveTestTip+"\nbranch refs/heads/"+branch+"\n"))
}

// remoteHolds scripts what a push and its verification read: the push itself,
// and the remote tip VerifyPushedCommit compares.
func (r preserveTestRepo) remoteHolds(remote, branch string) {
	r.s.on("push "+remote+" *", ok(""))
	r.s.on("remote get-url "+remote, ok("/srv/"+remote+".git\n"))
	r.s.on("remote get-url --push "+remote, ok("/srv/"+remote+".git\n"))
	r.s.on("ls-remote --heads "+remote+" "+branch, ok(preserveTestTip+"\trefs/heads/"+branch+"\n"))
}

// pushes returns the joined argv of every push the step sent, in order.
func (r preserveTestRepo) pushes() []string {
	var out []string
	for _, c := range r.s.calls {
		if len(c.args) > 0 && c.args[0] == "push" {
			out = append(out, strings.Join(c.args, " "))
		}
	}
	return out
}

// tempWorktreeDir returns the directory of the worktree the step added, or ""
// when it added none.
func (r preserveTestRepo) tempWorktreeDir() string {
	for _, c := range r.s.calls {
		for i := 1; i+1 < len(c.args); i++ {
			if c.args[i-1] == "worktree" && c.args[i] == "add" {
				return c.args[i+1]
			}
		}
	}
	return ""
}

// wantClearedEnv fails unless every command the step ran had GIT_DIR and
// GIT_WORK_TREE taken out of its environment (rpkg #769): an inherited GIT_DIR
// points these commands at another repository.
func (r preserveTestRepo) wantClearedEnv(t *testing.T) {
	t.Helper()
	for _, c := range r.s.calls {
		for _, key := range preserveUnsetEnv {
			if !containsKey(c.unset, key) {
				t.Fatalf("git %s ran without clearing %s (unset %v)", strings.Join(c.args, " "), key, c.unset)
			}
		}
	}
}

// A tip the remote already holds — merged into main, or pushed by an earlier
// run — is not pushed again: the branch is safe to delete, and a push here
// would only wait on the network to learn what it was already told.
func TestPreserveBranchSkipsAPushTheRemoteAlreadyHolds(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.s.on(r.prefix+"branch -r --contains "+preserveTestTip, ok("  origin/main\n"))

	result, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot())
	if err != nil {
		t.Fatalf("PreserveBranch: %v", err)
	}
	if result.RemoteRef != "origin/main" || !result.AlreadyThere {
		t.Fatalf("result = %+v, want origin/main and AlreadyThere", result)
	}
	if pushes := r.pushes(); len(pushes) != 0 {
		t.Fatalf("pushes = %q, want none: the remote already holds the tip", pushes)
	}
	r.wantClearedEnv(t)
	r.s.noUnscripted(t)
}

// The seat's own worktree stands on the branch, so the push runs from it: that
// is the checkout whose HEAD the pre-push guard reads (gt-7cps5).
func TestPreserveBranchPushesFromTheWorktreeStandingOnTheBranch(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.onlyWorktreeOn(preserveTestBranch)
	r.s.on("rev-parse --abbrev-ref HEAD", ok(preserveTestBranch+"\n"))
	r.remoteHolds("origin", preserveTestBranch)

	result, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot())
	if err != nil {
		t.Fatalf("PreserveBranch: %v (calls %q)", err, r.s.sent())
	}
	if result.RemoteRef != "origin/"+preserveTestBranch {
		t.Fatalf("RemoteRef = %q, want origin/%s", result.RemoteRef, preserveTestBranch)
	}
	push, found := r.s.sentCall("push origin " + preserveTestBranch + ":" + preserveTestBranch)
	if !found {
		t.Fatalf("calls = %q, want a push of the branch under its own name", r.s.sent())
	}
	if push.dir != r.seat {
		t.Fatalf("the push ran in %q, want the seat's worktree %q", push.dir, r.seat)
	}
	if dir := r.tempWorktreeDir(); dir != "" {
		t.Fatalf("a temporary worktree was added at %q; the seat's own checkout stands on the branch", dir)
	}
	r.wantClearedEnv(t)
	r.s.noUnscripted(t)
}

// No worktree stands on the branch (the seat was reset, or its checkout is on
// main). The push then comes from a temporary worktree of the branch, under the
// work root it was given, and that worktree is removed once the push is done.
func TestPreserveBranchAddsAndRemovesATemporaryWorktreeWhenNoCheckoutHasTheBranch(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.onlyWorktreeOn("main")
	r.s.on("rev-parse --abbrev-ref HEAD", ok("main\n"))
	r.s.on(r.prefix+"worktree prune", ok(""))
	r.s.on(r.prefix+"worktree add *", ok(""))
	r.s.on(r.prefix+"worktree remove *", ok(""))
	r.remoteHolds("origin", preserveTestBranch)

	if _, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot()); err != nil {
		t.Fatalf("PreserveBranch: %v", err)
	}
	temp := r.tempWorktreeDir()
	if temp == "" {
		t.Fatalf("calls = %q, want a worktree of the branch to be added", r.s.sent())
	}
	if !strings.HasPrefix(temp, r.workRoot()+string(filepath.Separator)) {
		t.Fatalf("temporary worktree at %q, want it under the work root %q", temp, r.workRoot())
	}
	push, found := r.s.sentCall("push origin " + preserveTestBranch + ":" + preserveTestBranch)
	if !found {
		t.Fatalf("calls = %q, want a push of the branch", r.s.sent())
	}
	if push.dir != temp {
		t.Fatalf("the push ran in %q, want the temporary worktree %q", push.dir, temp)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temporary worktree %q still exists (stat: %v), want it removed", temp, err)
	}
	if !r.s.hasSent(r.prefix + "worktree remove " + temp + " --force") {
		t.Fatalf("calls = %q, want the temporary worktree removed", r.s.sent())
	}
	if want := r.prefix + "worktree add " + temp + " -- " + preserveTestBranch; !r.s.hasSent(want) {
		t.Fatalf("calls = %q, want %q: the branch is named after --, never parsed as an option", r.s.sent(), want)
	}
	r.wantClearedEnv(t)
	r.s.noUnscripted(t)
}

// A push that fails under the branch's name and on the fallback ref leaves
// nothing preserved, and the temporary worktree still goes: a failed preserve
// must not leave the rig littered with checkouts.
func TestPreserveBranchRemovesTheTemporaryWorktreeWhenThePushFails(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.onlyWorktreeOn("main")
	r.s.on("rev-parse --abbrev-ref HEAD", ok("main\n"))
	r.s.on(r.prefix+"worktree prune", ok(""))
	r.s.on(r.prefix+"worktree add *", ok(""))
	r.s.on(r.prefix+"worktree remove *", ok(""))
	r.s.on("push origin *", fail(1, "error: failed to push some refs\n"))

	_, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot())
	if !errors.Is(err, ErrBranchNotPreserved) {
		t.Fatalf("err = %v, want it to wrap ErrBranchNotPreserved: the pushes ran and neither left the tip on the remote", err)
	}
	temp := r.tempWorktreeDir()
	if temp == "" {
		t.Fatalf("calls = %q, want a worktree of the branch to be added", r.s.sent())
	}
	if _, statErr := os.Stat(temp); !os.IsNotExist(statErr) {
		t.Fatalf("temporary worktree %q still exists (stat: %v), want it removed on the error path", temp, statErr)
	}
	want := preserveTestBranch + "-" + preserveTestTip[:7]
	if pushes := r.pushes(); len(pushes) != 2 || !strings.Contains(pushes[1], ":"+want) {
		t.Fatalf("pushes = %q, want the branch and its fallback ref %s", pushes, want)
	}
	r.wantClearedEnv(t)
	r.s.noUnscripted(t)
}

// A worktree that cannot be added leaves no directory behind, and no push.
func TestPreserveBranchRemovesTheDirectoryWhenTheWorktreeCannotBeAdded(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.onlyWorktreeOn("main")
	r.s.on("rev-parse --abbrev-ref HEAD", ok("main\n"))
	r.s.on(r.prefix+"worktree prune", ok(""))
	r.s.on(r.prefix+"worktree add *", fail(128, "fatal: '"+preserveTestBranch+"' is already used by worktree at /elsewhere\n"))
	r.s.on(r.prefix+"worktree remove *", ok(""))

	_, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot())
	if err == nil {
		t.Fatal("PreserveBranch = nil error, want the worktree add's failure")
	}
	if !strings.Contains(err.Error(), "already used by worktree") {
		t.Fatalf("err = %v, want it to carry the worktree add's refusal", err)
	}
	entries, readErr := os.ReadDir(r.workRoot())
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read the work root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("work root holds %d entries after a failed add, want none", len(entries))
	}
	if pushes := r.pushes(); len(pushes) != 0 {
		t.Fatalf("pushes = %q, want none after a failed worktree add", pushes)
	}
	r.wantClearedEnv(t)
	r.s.noUnscripted(t)
}

// Each failure before the push reports itself and pushes nothing: a preserve
// that cannot read the branch, the remote or the worktrees has not preserved
// anything, and its caller must not read it as one.
func TestPreserveBranchFailsBeforeThePush(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		script func(*preserveTestRepo)
		want   string
	}{
		{
			name: "the branch cannot be resolved",
			script: func(r *preserveTestRepo) {
				r.s.on(r.prefix+"rev-parse refs/heads/"+preserveTestBranch, fail(128, "fatal: ambiguous argument\n"))
			},
			want: "resolve refs/heads/",
		},
		{
			name: "the branch resolves to no commit",
			script: func(r *preserveTestRepo) {
				r.s.on(r.prefix+"rev-parse refs/heads/"+preserveTestBranch, ok(""))
			},
			want: "empty sha",
		},
		{
			name: "the remote cannot be fetched",
			script: func(r *preserveTestRepo) {
				r.s.on(r.prefix+"fetch --prune origin", fail(128, "fatal: could not read from remote\n"))
			},
			want: "fetch origin before preserve",
		},
		{
			name: "the worktrees cannot be listed",
			script: func(r *preserveTestRepo) {
				r.s.on(r.prefix+"worktree list --porcelain", fail(128, "fatal: not a git repository\n"))
			},
			want: "list worktrees",
		},
		{
			name: "the worktrees cannot be pruned",
			script: func(r *preserveTestRepo) {
				r.onlyWorktreeOn("main")
				r.s.on("rev-parse --abbrev-ref HEAD", ok("main\n"))
				r.s.on(r.prefix+"worktree prune", fail(128, "fatal: cannot prune\n"))
			},
			want: "prune worktrees",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newPreserveTestRepo(t)
			tt.script(&r)
			if _, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot()); err == nil {
				t.Fatalf("PreserveBranch = nil error, want %q", tt.want)
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to name %q", err, tt.want)
			} else if errors.Is(err, ErrBranchNotPreserved) {
				t.Fatalf("err = %v, want no ErrBranchNotPreserved: no push ran, so nothing was learned about the remote", err)
			}
			if pushes := r.pushes(); len(pushes) != 0 {
				t.Fatalf("pushes = %q, want none when the preserve failed before pushing", pushes)
			}
			r.wantClearedEnv(t)
		})
	}
}

// A work root that cannot be made leaves nothing to push from, and the caller
// hears it instead of a silent success.
func TestPreserveBranchFailsWhenTheWorkRootCannotBeMade(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.onlyWorktreeOn("main")
	r.s.on("rev-parse --abbrev-ref HEAD", ok("main\n"))
	r.s.on(r.prefix+"worktree prune", ok(""))
	// A regular file where the work root should be: MkdirAll cannot make it.
	blocked := filepath.Join(r.ri, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("make the blocking file: %v", err)
	}

	if _, err := r.g.PreserveBranch(preserveTestBranch, "origin", filepath.Join(blocked, "preserve")); err == nil {
		t.Fatal("PreserveBranch = nil error, want the work root's failure")
	} else if !strings.Contains(err.Error(), "temporary worktree under") {
		t.Fatalf("err = %v, want it to name the temporary worktree's directory", err)
	}
	if pushes := r.pushes(); len(pushes) != 0 {
		t.Fatalf("pushes = %q, want none with no worktree to push from", pushes)
	}
	r.wantClearedEnv(t)
}

// A push whose tip the remote does not end up holding is not a preserve: the
// step fails even though git exited 0.
func TestPreserveBranchFailsWhenTheRemoteDoesNotHoldThePushedTip(t *testing.T) {
	t.Parallel()
	r := newPreserveTestRepo(t)
	r.onlyWorktreeOn(preserveTestBranch)
	r.s.on("rev-parse --abbrev-ref HEAD", ok(preserveTestBranch+"\n"))
	r.s.on("push origin *", ok(""))
	r.s.on("remote get-url origin", ok("git@example.com:rig.git\n"))
	r.s.on("remote get-url --push origin", ok("git@example.com:rig.git\n"))
	r.s.on("ls-remote --heads origin "+preserveTestBranch,
		ok("2222222222222222222222222222222222222222\trefs/heads/"+preserveTestBranch+"\n"))

	_, err := r.g.PreserveBranch(preserveTestBranch, "origin", r.workRoot())
	if err == nil {
		t.Fatal("PreserveBranch = nil error, want the verification's failure")
	}
	if !strings.Contains(err.Error(), "verified_push_failed") {
		t.Fatalf("err = %v, want git's verification failure", err)
	}
	if !errors.Is(err, ErrBranchNotPreserved) {
		t.Fatalf("err = %v, want it to wrap ErrBranchNotPreserved: the push exited 0 but the tip is not on the remote", err)
	}
}

// callEnv is what makes WithoutEnv real: a key is taken out of the environment
// git is started with rather than exported empty, because an empty GIT_DIR
// makes git read the current directory as the repository.
func TestCallEnvRemovesUnsetKeysAndAppendsExtras(t *testing.T) {
	t.Parallel()
	env := callEnv([]string{"GT_TEST_EXTRA=1"}, []string{"PATH"})
	if envHas(env, "PATH") {
		t.Fatalf("callEnv kept PATH after unsetting it: %v", env)
	}
	if !envHas(env, "GT_TEST_EXTRA") {
		t.Fatalf("callEnv dropped the extra variable: %v", env)
	}
	if !envHas(callEnv(nil, nil), "PATH") {
		t.Fatal("callEnv dropped PATH with nothing to unset: the process environment must pass through")
	}
}

// envHas reports whether env holds key.
func envHas(env []string, key string) bool {
	for _, entry := range env {
		if k, _, _ := strings.Cut(entry, "="); k == key {
			return true
		}
	}
	return false
}
