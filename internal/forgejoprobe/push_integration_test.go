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

// TestIntegrationPushRunsTheRigHook builds a rig repository whose core.hooksPath
// is .githooks, a committed pre-push that refuses land/*, and a local bare
// directory as the Forgejo remote. The push from the worktree of the rig
// repository is refused by that hook, and the worktree is gone after Cleanup.
// Nothing touches the network or the real town.
func TestIntegrationPushRunsTheRigHook(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()

	src := filepath.Join(root, "src")
	runGit(t, root, "init", "--quiet", src)
	if err := os.MkdirAll(filepath.Join(src, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".githooks", "pre-push"), []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".githooks/pre-push")
	runGit(t, src, "-c", "user.name=probe", "-c", "user.email=probe@example.invalid", "commit", "--quiet", "-m", "rig hooks")
	sha := runGit(t, src, "rev-parse", "HEAD")

	bare := filepath.Join(root, "rig", ".repo.git")
	runGit(t, root, "clone", "--quiet", "--bare", src, bare)
	runGit(t, bare, "config", "core.hooksPath", ".githooks")

	remote := filepath.Join(root, "forgejo.git")
	runGit(t, root, "init", "--quiet", "--bare", remote)

	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	var logs []string
	p := &Pusher{
		Git:        NewGit(),
		Bare:       bare,
		Copy:       filepath.Join(root, "copy"),
		Work:       work,
		GitURL:     remote,
		MainBranch: "main",
		Branch:     "land/probe-rig",
		Log:        func(msg string) { logs = append(logs, msg) },
	}

	err := p.Push(sha)
	if err == nil || !strings.Contains(err.Error(), "pre-push: refusing refs/heads/land/probe-rig") {
		t.Fatalf("Push = %v, want the rig hook's refusal", err)
	}
	if err := p.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(work, "wt")); !os.IsNotExist(statErr) {
		t.Errorf("worktree still on disk after Cleanup: %v", statErr)
	}
	if list := runGit(t, bare, "worktree", "list", "--porcelain"); strings.Contains(list, filepath.Join(work, "wt")) {
		t.Errorf("worktree still registered in the rig repository:\n%s", list)
	}
}
