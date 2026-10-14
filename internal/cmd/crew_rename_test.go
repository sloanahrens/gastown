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

// renameTestManager returns a crew manager over a temp rig holding one crew
// workspace per name. Rename touches only the filesystem, so no git is wired.
func renameTestManager(t *testing.T, names ...string) (*crew.Manager, string) {
	t.Helper()
	rigPath := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(rigPath, "crew", n), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return crew.NewManager(&rig.Rig{Name: "test-rig", Path: rigPath}, nil, nil), rigPath
}

func TestCrewRenameStopsOnlyAfterChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		oldName  string
		newName  string
		wantErr  string // substring; "" means the rename succeeds
		wantStop bool
	}{
		{"valid rename stops the old session", "emma", "carol", "", true},
		{"new name already exists", "emma", "bob", "'bob' already exists", false},
		{"invalid new name", "emma", "../evil", "invalid new name '../evil'", false},
		{"empty new name", "emma", "", "invalid new name", false},
		{"old name missing", "ghost", "carol", "'ghost' not found", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, rigPath := renameTestManager(t, "emma", "bob")
			stopped := false
			err := renameCrewWorkspace(mgr, tt.oldName, tt.newName, func() error {
				stopped = true
				return nil
			})

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("renameCrewWorkspace: %v", err)
				}
				if _, statErr := os.Stat(filepath.Join(rigPath, "crew", tt.newName)); statErr != nil {
					t.Errorf("new workspace missing after rename: %v", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(rigPath, "crew", tt.oldName)); !os.IsNotExist(statErr) {
					t.Errorf("old workspace still present after rename (stat err = %v)", statErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("renameCrewWorkspace error = %v, want one containing %q", err, tt.wantErr)
			}
			if stopped != tt.wantStop {
				t.Errorf("old session stopped = %v, want %v", stopped, tt.wantStop)
			}
		})
	}
}

// A failed stop aborts the rename: the workspace keeps its old name.
func TestCrewRenameStopFailureLeavesWorkspace(t *testing.T) {
	t.Parallel()
	mgr, rigPath := renameTestManager(t, "emma")
	stopErr := errors.New("tmux refused")
	err := renameCrewWorkspace(mgr, "emma", "carol", func() error { return stopErr })
	if !errors.Is(err, stopErr) {
		t.Fatalf("error = %v, want the stop error", err)
	}
	if _, statErr := os.Stat(filepath.Join(rigPath, "crew", "emma")); statErr != nil {
		t.Errorf("old workspace gone after failed stop: %v", statErr)
	}
}
