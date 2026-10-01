//go:build integration

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Wiring guards whose production collaborator is a real process: each leaves
// one seam nil and proves the collaborator behind it ran. The unit tier's
// guards that need no process are in wiring_guard_test.go.

// TestIntegrationListOriginBranchesReadsTheRigOrigin guards listOriginBranches' nil
// path: it lists the polecat branches on the rig's real origin remote.
func TestIntegrationListOriginBranchesReadsTheRigOrigin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	wireGit(t, origin, "init", "--bare")

	clone := filepath.Join(townRoot, "gt", "mayor", "rig")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	const branch = "polecat/basalt/gt-issue1+abc123"
	wireGit(t, clone, "init")
	wireGit(t, clone, "config", "user.email", "test@test.com")
	wireGit(t, clone, "config", "user.name", "Test")
	wireGit(t, clone, "remote", "add", "origin", origin)
	wireGit(t, clone, "checkout", "-b", branch)
	wireGit(t, clone, "commit", "--allow-empty", "-m", "work")
	wireGit(t, clone, "push", "origin", branch)

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, nil, 10*time.Minute, nil, nil, nil)
	got, err := m.listOriginBranches(filepath.Join(townRoot, "gt"))
	if err != nil {
		t.Fatalf("listOriginBranches: %v", err)
	}
	if want := []string{branch}; !reflect.DeepEqual(got, want) {
		t.Errorf("listOriginBranches = %v, want %v from the rig's origin", got, want)
	}
}

// wireGit runs git in dir for a wiring guard's fixture, isolated from the
// host's git config.
func wireGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
