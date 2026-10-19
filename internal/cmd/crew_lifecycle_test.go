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
// member's session is live. It was read as "no session", which skipped the
// running-session guard and the kill and removed the workspace under a session
// that might be running. With or without --force, the removal now stops there.
func TestCrewRemoveTmuxErrorKeepsWorkspace(t *testing.T) {
	t.Parallel()

	tmuxErr := errors.New("tmux: unexpected failure talking to the server")

	for _, force := range []bool{false, true} {
		name := "without --force"
		if force {
			name = "with --force"
		}
		t.Run(name, func(t *testing.T) {
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
				hasSession: func(string) (bool, error) { return false, tmuxErr },
			}

			err := runCrewRemoveWith([]string{"max"}, "gastown", force, false, env)

			if !errors.Is(err, tmuxErr) {
				t.Errorf("error = %v, want it to wrap %q", err, tmuxErr)
			}
			if _, statErr := os.Stat(crewPath); statErr != nil {
				t.Errorf("crew workspace gone after a failed session check: %v", statErr)
			}
		})
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
