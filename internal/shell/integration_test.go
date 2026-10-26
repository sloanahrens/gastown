// ABOUTME: Tests for shell integration install/remove functionality.
// ABOUTME: Verifies RC file manipulation and hook script creation.

package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellHookEnabledRule checks the enabled check the emitted hook ships
// with applies the rule state.IsEnabled applies: GASTOWN_DISABLED=1 wins, then
// GASTOWN_ENABLED=1, then the state file at the XDG path (gt-1tjks). The unit
// tier refuses to start a shell (internal/testutil/unittier), so the rule is
// checked in the emitted source rather than by running it.
func TestShellHookEnabledRule(t *testing.T) {
	t.Parallel()

	if !strings.Contains(shellHookScript, shellHookEnabledFunc) {
		t.Fatal("shellHookScript no longer embeds shellHookEnabledFunc")
	}

	tests := []struct {
		name string
		rule string
		want bool
	}{
		{"disabled is 1, not any non-empty value", `[[ "${GASTOWN_DISABLED:-}" == "1" ]] && return 1`, true},
		{"enabled is 1, not any non-empty value", `[[ "${GASTOWN_ENABLED:-}" == "1" ]] && return 0`, true},
		{"state file follows XDG_STATE_HOME with the old default", `"${XDG_STATE_HOME:-$HOME/.local/state}/gastown/state.json"`, true},
		{"non-emptiness of GASTOWN_DISABLED decides nothing", `-n "$GASTOWN_DISABLED"`, false},
		{"non-emptiness of GASTOWN_ENABLED decides nothing", `-n "$GASTOWN_ENABLED"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Contains(shellHookScript, tt.rule); got != tt.want {
				t.Errorf("shellHookScript contains %q = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}

func TestDetectShell(t *testing.T) {
	t.Parallel()
	tests := []struct {
		shellEnv string
		want     string
	}{
		{"/bin/zsh", "zsh"},
		{"/usr/bin/zsh", "zsh"},
		{"/bin/bash", "bash"},
		{"/usr/bin/bash", "bash"},
		{"", "zsh"},
	}

	for _, tt := range tests {
		t.Run(tt.shellEnv, func(t *testing.T) {
			t.Parallel()
			if got := detectShell(tt.shellEnv); got != tt.want {
				t.Errorf("detectShell(%q) = %q, want %q", tt.shellEnv, got, tt.want)
			}
		})
	}

	// DetectShell reads $SHELL from the process environment.
	//testpolicy:allow no-host-env — DetectShell reads the developer's SHELL by design; the test compares it with the same read
	if got, want := DetectShell(), detectShell(os.Getenv("SHELL")); got != want {
		t.Errorf("DetectShell() = %q, want %q", got, want)
	}
}

func TestRCFilePath(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-host-env — the rc file paths are joined under the real home; the test checks the join, not what is in the home
	home, _ := os.UserHomeDir()

	tests := []struct {
		shell string
		want  string
	}{
		{"zsh", filepath.Join(home, ".zshrc")},
		{"bash", filepath.Join(home, ".bashrc")},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			got := RCFilePath(tt.shell)
			if got != tt.want {
				t.Errorf("RCFilePath(%q) = %q, want %q", tt.shell, got, tt.want)
			}
		})
	}
}

func TestAddRemoveFromRCFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rcPath := filepath.Join(tmpDir, ".zshrc")

	originalContent := "# existing content\nalias foo=bar\n"
	if err := os.WriteFile(rcPath, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	if err := addToRCFile(rcPath); err != nil {
		t.Fatalf("addToRCFile() error = %v", err)
	}

	data, err := os.ReadFile(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, markerStart) {
		t.Error("RC file should contain start marker")
	}
	if !strings.Contains(content, markerEnd) {
		t.Error("RC file should contain end marker")
	}
	if !strings.Contains(content, "shell-hook.sh") {
		t.Error("RC file should source shell-hook.sh")
	}
	if !strings.Contains(content, "# existing content") {
		t.Error("RC file should preserve original content")
	}

	if err := removeFromRCFile(rcPath); err != nil {
		t.Fatalf("removeFromRCFile() error = %v", err)
	}

	data, err = os.ReadFile(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	content = string(data)

	if strings.Contains(content, markerStart) {
		t.Error("RC file should not contain start marker after removal")
	}
	if strings.Contains(content, markerEnd) {
		t.Error("RC file should not contain end marker after removal")
	}
	if !strings.Contains(content, "# existing content") {
		t.Error("RC file should preserve original content after removal")
	}
}

func TestUpdateRCFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rcPath := filepath.Join(tmpDir, ".zshrc")

	if err := addToRCFile(rcPath); err != nil {
		t.Fatalf("initial addToRCFile() error = %v", err)
	}

	if err := addToRCFile(rcPath); err != nil {
		t.Fatalf("second addToRCFile() error = %v", err)
	}

	data, _ := os.ReadFile(rcPath)
	content := string(data)

	startCount := strings.Count(content, markerStart)
	if startCount != 1 {
		t.Errorf("RC file has %d start markers, want 1", startCount)
	}
}
