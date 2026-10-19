package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestCrewRemoveTmuxErrorKeepsWorkspace: a has-session that fails for a reason
// tmux does not recognise as "no session" says nothing about whether the
// member's session is live, so without --force the removal stops there.
func TestCrewRemoveTmuxErrorKeepsWorkspace(t *testing.T) {
	t.Parallel()

	tmuxErr := errors.New("tmux: unexpected failure talking to the server")
	rigPath := t.TempDir()
	crewPath := filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0755); err != nil {
		t.Fatalf("mkdir crew workspace: %v", err)
	}
	env := crewRemoveEnv{
		member: func(rigOverride, name string) (*crew.Manager, *rig.Rig, error) {
			return nil, &rig.Rig{Name: "gastown", Path: rigPath}, nil
		},
		hasSession: func(string) (bool, error) { return false, tmuxErr },
	}

	err := runCrewRemoveWith([]string{"max"}, "gastown", false, false, env)

	if !errors.Is(err, tmuxErr) {
		t.Errorf("error = %v, want it to wrap %q", err, tmuxErr)
	}
	if _, statErr := os.Stat(crewPath); statErr != nil {
		t.Errorf("crew workspace gone after a failed session check: %v", statErr)
	}
}

// TestCrewRemoveForceGoesPastTmuxError: --force skips safety checks, so a
// has-session that fails does not stop it; the removal itself is attempted. The
// workspace is a worktree whose git cannot run (the rig has no mayor clone), so
// the attempt fails on that, not on the session check.
func TestCrewRemoveForceGoesPastTmuxError(t *testing.T) {
	t.Parallel()

	tmuxErr := errors.New("tmux: unexpected failure talking to the server")
	rigPath := t.TempDir()
	crewPath := filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0755); err != nil {
		t.Fatalf("mkdir crew workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(crewPath, ".git"), []byte("gitdir: elsewhere\n"), 0644); err != nil {
		t.Fatalf("write worktree .git file: %v", err)
	}
	env := crewRemoveEnv{
		member: func(rigOverride, name string) (*crew.Manager, *rig.Rig, error) {
			return nil, &rig.Rig{Name: "gastown", Path: rigPath}, nil
		},
		hasSession: func(string) (bool, error) { return false, tmuxErr },
	}

	err := runCrewRemoveWith([]string{"max"}, "gastown", true, false, env)

	if err == nil {
		t.Fatal("error = nil, want the failed worktree removal: the rig has no mayor clone")
	}
	if errors.Is(err, tmuxErr) {
		t.Errorf("error = %v: --force stopped at the session check", err)
	}
}

// A running session still blocks a removal without --force, and leaves the
// workspace where it is.
func TestCrewRemoveRunningSessionKeepsWorkspace(t *testing.T) {
	t.Parallel()

	rigPath := t.TempDir()
	crewPath := filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0755); err != nil {
		t.Fatalf("mkdir crew workspace: %v", err)
	}
	env := crewRemoveEnv{
		member: func(rigOverride, name string) (*crew.Manager, *rig.Rig, error) {
			return nil, &rig.Rig{Name: "gastown", Path: rigPath}, nil
		},
		hasSession: func(string) (bool, error) { return true, nil },
	}

	err := runCrewRemoveWith([]string{"max"}, "gastown", false, false, env)

	if err == nil || !strings.Contains(err.Error(), "session running") {
		t.Errorf("error = %v, want session running", err)
	}
	if _, statErr := os.Stat(crewPath); statErr != nil {
		t.Errorf("crew workspace gone while its session runs: %v", statErr)
	}
}
