package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Wiring guards: each test below leaves one seam nil and proves the
// production collaborator behind it actually ran. The rest of the package
// replaces these seams, so without a guard a seam whose nil path stopped
// calling the real thing would pass every other test.

// TestListOriginBranchesReadsTheRigOrigin guards listOriginBranches' nil
// path: it lists the polecat branches on the rig's real origin remote.
func TestListOriginBranchesReadsTheRigOrigin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	runDeadHolderGit(t, origin, "init", "--bare")

	clone := filepath.Join(townRoot, "gt", "mayor", "rig")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	const branch = "polecat/basalt/gt-issue1+abc123"
	runDeadHolderGit(t, clone, "init")
	runDeadHolderGit(t, clone, "config", "user.email", "test@test.com")
	runDeadHolderGit(t, clone, "config", "user.name", "Test")
	runDeadHolderGit(t, clone, "remote", "add", "origin", origin)
	runDeadHolderGit(t, clone, "checkout", "-b", branch)
	runDeadHolderGit(t, clone, "commit", "--allow-empty", "-m", "work")
	runDeadHolderGit(t, clone, "push", "origin", branch)

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, "gt", 10*time.Minute, nil, nil, nil)
	got, err := m.listOriginBranches(filepath.Join(townRoot, "gt"))
	if err != nil {
		t.Fatalf("listOriginBranches: %v", err)
	}
	if want := []string{branch}; !reflect.DeepEqual(got, want) {
		t.Errorf("listOriginBranches = %v, want %v from the rig's origin", got, want)
	}
}

// TestDogSessionsDefaultsToTheRealSessionManager guards dogSessions' nil path:
// the handler drives a *dog.SessionManager on the town's tmux, for this town
// and this dog manager, not a stand-in.
func TestDogSessionsDefaultsToTheRealSessionManager(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mgr := dog.NewManager(townRoot, nil)
	d := &Daemon{config: &Config{TownRoot: townRoot}}
	got := d.dogSessions(mgr)
	want := dog.NewSessionManager(tmux.NewTmux(), townRoot, mgr)
	if !reflect.DeepEqual(got, dogSessions(want)) {
		t.Fatalf("dogSessions() = %#v, want %#v when no test seam is set", got, want)
	}
}
