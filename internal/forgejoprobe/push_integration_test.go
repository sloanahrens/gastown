//go:build integration

package forgejoprobe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookScript is the rig's pre-push: it refuses any push of a land/* branch, with
// the text the probe must surface.
const hookScript = `#!/bin/sh
while read local_ref local_sha remote_ref remote_sha; do
  case "$remote_ref" in
    refs/heads/land/*) echo "pre-push: refusing $remote_ref: land/* belongs to the landing worker" >&2; exit 1 ;;
  esac
done
exit 0
`

// acceptHookScript is a rig's pre-push that lets every push through. It reads
// the refs git sends it, as a real hook does.
const acceptHookScript = "#!/bin/sh\ncat >/dev/null\nexit 0\n"

// runGit runs git in dir for the integration fixtures and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := NewGit()
	out, err := cmd.Run(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out)
}

// rigFixture is a throwaway world for one push: a source whose .githooks/pre-push
// is the hook, the rig repository cloned from it with core.hooksPath set to
// .githooks (absent when the fixture has no rig repository), the copy cloned
// from it on main, and a local bare directory as the Forgejo remote.
type rigFixture struct {
	bare, copy, remote, work, sha string
}

// newRigFixture builds the rigFixture under t.TempDir(). Nothing touches the
// network or the real town.
func newRigFixture(t *testing.T, hook string, withRig bool) rigFixture {
	t.Helper()
	root := t.TempDir()

	src := filepath.Join(root, "src")
	runGit(t, root, "init", "--quiet", "--initial-branch", "main", src)
	hooks := filepath.Join(src, ".githooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".githooks/pre-push")
	runGit(t, src, "-c", "user.name=probe", "-c", "user.email=probe@example.invalid", "commit", "--quiet", "-m", "rig hooks")

	f := rigFixture{
		bare:   filepath.Join(root, "rig", ".repo.git"),
		copy:   filepath.Join(root, "copy"),
		remote: filepath.Join(root, "forgejo.git"),
		work:   filepath.Join(root, "work"),
		sha:    runGit(t, src, "rev-parse", "HEAD"),
	}
	if withRig {
		runGit(t, root, "clone", "--quiet", "--bare", src, f.bare)
		runGit(t, f.bare, "config", "core.hooksPath", ".githooks")
	}
	runGit(t, root, "clone", "--quiet", src, f.copy)
	runGit(t, root, "init", "--quiet", "--bare", f.remote)
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// pusher is the Pusher over the fixture, its log lines collected into logs.
func (f rigFixture) pusher(logs *[]string) *Pusher {
	return &Pusher{
		Git:        NewGit(),
		Bare:       f.bare,
		Copy:       f.copy,
		Work:       f.work,
		GitURL:     f.remote,
		MainBranch: "main",
		Branch:     "land/probe-rig",
		Log:        func(msg string) { *logs = append(*logs, msg) },
	}
}

// TestIntegrationPushRunsTheRigHook pushes through real git against the three
// shapes of the rig repository: a pre-push that refuses land/*, one that accepts
// it, and no rig repository at all. Each subtest owns its fixture, and each
// asserts both the verdict and the remote's ref, so a pass cannot come from a
// push that never ran.
func TestIntegrationPushRunsTheRigHook(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	t.Run("a refusing hook fails the push with its text and the worktree is gone after Cleanup", func(t *testing.T) {
		f := newRigFixture(t, hookScript, true)
		var logs []string
		p := f.pusher(&logs)
		mode, err := p.Push(f.sha)
		if err == nil || !strings.Contains(err.Error(), "pre-push: refusing refs/heads/land/probe-rig") {
			t.Fatalf("Push = %v, want the rig hook's refusal", err)
		}
		if mode.HooksExercised() {
			t.Errorf("mode %v says the hooks were exercised by a refused push", mode)
		}
		if _, err := NewGit().Run(f.remote, nil, "rev-parse", "--verify", "--quiet", "refs/heads/land/probe-rig"); err == nil {
			t.Errorf("the refused push left land/probe-rig on the remote")
		}
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(f.work, "wt")); !os.IsNotExist(statErr) {
			t.Errorf("worktree still on disk after Cleanup: %v", statErr)
		}
		if list := runGit(t, f.bare, "worktree", "list", "--porcelain"); strings.Contains(list, filepath.Join(f.work, "wt")) {
			t.Errorf("worktree still registered in the rig repository:\n%s", list)
		}
	})

	t.Run("an accepting hook lets the push land from the worktree, which is gone after Cleanup", func(t *testing.T) {
		f := newRigFixture(t, acceptHookScript, true)
		var logs []string
		p := f.pusher(&logs)
		mode, err := p.Push(f.sha)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if mode != PushedFromWorktree || !mode.HooksExercised() {
			t.Errorf("mode = %v, want PushedFromWorktree, exercised", mode)
		}
		if got := runGit(t, f.remote, "rev-parse", "--verify", "refs/heads/land/probe-rig"); got != f.sha {
			t.Errorf("remote land/probe-rig = %s, want the pushed %s", got, f.sha)
		}
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(f.work, "wt")); !os.IsNotExist(statErr) {
			t.Errorf("worktree still on disk after Cleanup: %v", statErr)
		}
	})

	t.Run("with no rig repository the push comes from the clone, warns, and exercises no hook", func(t *testing.T) {
		f := newRigFixture(t, hookScript, false)
		var logs []string
		p := f.pusher(&logs)
		mode, err := p.Push(f.sha)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if mode != PushedFromClone || mode.HooksExercised() {
			t.Errorf("mode = %v, want PushedFromClone, not exercised", mode)
		}
		if len(logs) != 1 || !strings.HasPrefix(logs[0], "warning: no rig repository at ") {
			t.Errorf("log = %q, want the no-rig-repository warning", logs)
		}
		if got := runGit(t, f.remote, "rev-parse", "--verify", "refs/heads/land/probe-rig"); got != f.sha {
			t.Errorf("remote land/probe-rig = %s, want the pushed %s", got, f.sha)
		}
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
	})
}
