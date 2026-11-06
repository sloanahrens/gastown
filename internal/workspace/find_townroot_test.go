package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/workspace"
)

// TestFindTownRootFromANonexistentCallerPath pins gt-yjzepk.1: town resolution
// from an explicit start directory walks lexically past ancestors this process
// cannot see, so a caller path that only exists where the caller stands (a Mac
// crew worktree in a pod) still resolves the town above it. It also pins that
// generic workspace resolution ignores GT_CALLER_CWD: the variable is a
// statement about identity, not a workspace resolver input.
func TestFindTownRootFromANonexistentCallerPath(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "crew"), 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	// The crew worktree and the directories below it are not present here.
	callerPath := filepath.Join(townRoot, "gastown", "crew", "eagle", "gastown")

	if got := beads.FindTownRoot(callerPath); got != townRoot {
		t.Fatalf("beads.FindTownRoot(%q) = %q, want the town root %q", callerPath, got, townRoot)
	}
	if got, err := workspace.Find(callerPath); err != nil || got != townRoot {
		t.Fatalf("workspace.Find(%q) = %q, %v, want the town root %q", callerPath, got, err, townRoot)
	}
}

// TestWorkspaceFindIgnoresTheCallerCwd pins that the generic resolvers do not
// read GT_CALLER_CWD. The caller path is a real town; the process directory is
// not. Resolving from the process directory must not land on the caller's
// town, or a guard that resolves the workspace through Find could be steered
// by the variable.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestWorkspaceFindIgnoresTheCallerCwd(t *testing.T) {
	callerTown := t.TempDir()
	if err := os.MkdirAll(filepath.Join(callerTown, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(callerTown, "mayor", "town.json"), []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}

	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, callerTown)

	got, err := workspace.FindFromCwd()
	if err != nil {
		t.Fatalf("FindFromCwd: %v", err)
	}
	if got == callerTown {
		t.Fatalf("FindFromCwd resolved the caller's town %q from GT_CALLER_CWD; the generic resolver must ignore it", got)
	}
}
