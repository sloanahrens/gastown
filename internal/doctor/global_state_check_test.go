package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/state"
)

// withTempState points the state package at a fresh XDG state home for the
// duration of the test; state.Load offers no injection seam, so the env is
// the only way to keep the check off the host's real state file.
func withTempState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// writeCorruptState puts an undecodable state.json where state.Load looks.
func writeCorruptState(t *testing.T) {
	t.Helper()
	dir := state.StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalStateCheck_Run(t *testing.T) {
	tests := []struct {
		name         string
		setUp        func(t *testing.T)
		wantStatus   CheckStatus
		wantMessage  string
		wantDetails  []string
		wantFixHint  string
		detailsAreIn []string
	}{
		{
			name:        "missing state is a warning that does not name the retired --shell flag",
			setUp:       func(t *testing.T) {},
			wantStatus:  StatusWarning,
			wantMessage: "Global state not initialized",
			wantFixHint: "Run from the town root: gt install . --force",
		},
		{
			name: "enabled state is healthy and reports version and machine id",
			setUp: func(t *testing.T) {
				if err := state.Enable("1.2.3"); err != nil {
					t.Fatalf("state.Enable: %v", err)
				}
			},
			wantStatus:   StatusOK,
			wantMessage:  "Global state healthy",
			wantDetails:  []string{"Gas Town: enabled", "Version: 1.2.3"},
			detailsAreIn: []string{"Machine ID: "},
		},
		{
			name: "disabled state is a warning",
			setUp: func(t *testing.T) {
				if err := state.Disable(); err != nil {
					t.Fatalf("state.Disable: %v", err)
				}
			},
			wantStatus:  StatusWarning,
			wantMessage: "Gas Town is disabled globally",
		},
		{
			name:        "unreadable state is an error, never healthy",
			setUp:       writeCorruptState,
			wantStatus:  StatusError,
			wantMessage: "Cannot read global state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempState(t)
			tt.setUp(t)

			got := NewGlobalStateCheck().Run(&CheckContext{})

			if got.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v (message %q)", got.Status, tt.wantStatus, got.Message)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tt.wantMessage)
			}
			if tt.wantFixHint != "" && got.FixHint != tt.wantFixHint {
				t.Errorf("FixHint = %q, want %q", got.FixHint, tt.wantFixHint)
			}
			for _, want := range tt.wantDetails {
				if !containsString(got.Details, want) {
					t.Errorf("Details = %v, want an entry %q", got.Details, want)
				}
			}
			for _, prefix := range tt.detailsAreIn {
				if !anyHasPrefix(got.Details, prefix) {
					t.Errorf("Details = %v, want an entry starting %q", got.Details, prefix)
				}
			}
		})
	}
}

// The check stopped reading the shell startup chain with the hook that wrote
// it (gt-szucfu). HOME and SHELL point at the fixture so the pre-removal check,
// which resolved the rc file from them, would have counted this startup entry.
func TestGlobalStateCheck_IgnoresShellStartupFiles(t *testing.T) {
	withTempState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	if err := state.Enable("1.2.3"); err != nil {
		t.Fatalf("state.Enable: %v", err)
	}
	rc := filepath.Join(home, ".zshrc")
	marker := "# --- Gas Town Integration (managed by gt) ---\nsource hook.sh\n"
	if err := os.WriteFile(rc, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}

	got := NewGlobalStateCheck().Run(&CheckContext{})

	if got.Status != StatusOK || got.Message != "Global state healthy" {
		t.Fatalf("Run = %v %q, want OK healthy", got.Status, got.Message)
	}
	if len(got.Details) != 3 {
		t.Fatalf("Details = %v, want only the three state lines", got.Details)
	}
	if got.Details[0] != "Gas Town: enabled" || got.Details[1] != "Version: 1.2.3" || !strings.HasPrefix(got.Details[2], "Machine ID: ") {
		t.Errorf("Details = %v, want enabled, version and machine id", got.Details)
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

func anyHasPrefix(haystack []string, prefix string) bool {
	for _, s := range haystack {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
