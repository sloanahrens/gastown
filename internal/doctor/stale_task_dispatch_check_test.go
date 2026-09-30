package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

func TestStaleTaskDispatchCheck_Clean(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a mayor settings.json without task-dispatch
	mayorDir := filepath.Join(tmpDir, "mayor", ".claude")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash(gh pr create*)",
        "hooks": [{"type": "command", "command": "gt tap guard pr-workflow"}]
      }
    ]
  }
}
`
	if err := os.WriteFile(filepath.Join(mayorDir, "settings.json"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleTaskDispatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for clean settings, got %v: %s", result.Status, result.Message)
	}
}

func TestStaleTaskDispatchCheck_Stale(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a mayor settings.json WITH stale task-dispatch
	mayorDir := filepath.Join(tmpDir, "mayor", ".claude")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Task",
        "hooks": [{"type": "command", "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gt tap guard task-dispatch"}]
      },
      {
        "matcher": "Bash(gh pr create*)",
        "hooks": [{"type": "command", "command": "gt tap guard pr-workflow"}]
      }
    ]
  }
}
`
	if err := os.WriteFile(filepath.Join(mayorDir, "settings.json"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleTaskDispatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for stale settings, got %v: %s", result.Status, result.Message)
	}
	if len(check.staleTargets) != 1 {
		t.Errorf("expected 1 stale target, got %d", len(check.staleTargets))
	}
}

func TestStaleTaskDispatchCheck_Fix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a mayor settings.json WITH stale task-dispatch
	mayorDir := filepath.Join(tmpDir, "mayor", ".claude")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Task",
        "hooks": [{"type": "command", "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gt tap guard task-dispatch"}]
      }
    ]
  }
}
`
	settingsPath := filepath.Join(mayorDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleTaskDispatchCheck()
	check.computeExpected = func(string) (*hooks.HooksConfig, error) { return hooks.DefaultBase(), nil }
	ctx := &CheckContext{TownRoot: tmpDir}

	// Run to detect
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v", result.Status)
	}

	// Fix
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify the fix removed the stale entry
	fixed, err := hooks.LoadSettings(settingsPath)
	if err != nil {
		t.Fatalf("failed to load fixed settings: %v", err)
	}

	if containsTaskDispatch(&fixed.Hooks) {
		t.Error("fixed settings still contain task-dispatch")
	}

	// Re-run check should pass
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}

// TestStaleTaskDispatchCheck_FixConvergesWithOverride verifies that Fix()
// converges even when an on-disk hooks-override re-injects the task-dispatch
// command via ComputeExpected.
func TestStaleTaskDispatchCheck_FixConvergesWithOverride(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a mayor settings.json WITH stale task-dispatch
	mayorDir := filepath.Join(tmpDir, "mayor", ".claude")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Task",
        "hooks": [{"type": "command", "command": "gt tap guard task-dispatch"}]
      }
    ]
  }
}
`
	settingsPath := filepath.Join(mayorDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleTaskDispatchCheck()
	// A mayor hooks-override that re-injects task-dispatch.
	check.computeExpected = func(string) (*hooks.HooksConfig, error) {
		return &hooks.HooksConfig{PreToolUse: []hooks.HookEntry{
			{Matcher: "Task", Hooks: []hooks.Hook{{Type: "command", Command: "gt tap guard task-dispatch"}}},
		}}, nil
	}
	ctx := &CheckContext{TownRoot: tmpDir}

	// Run to detect
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v", result.Status)
	}

	// Fix should converge even though override re-injects task-dispatch
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify the fix stripped the stale entry despite the override
	fixed, err := hooks.LoadSettings(settingsPath)
	if err != nil {
		t.Fatalf("failed to load fixed settings: %v", err)
	}

	if containsTaskDispatch(&fixed.Hooks) {
		t.Error("fixed settings still contain task-dispatch despite stripTaskDispatch")
	}

	// Re-run check should pass
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}

func TestContainsTaskDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		config   hooks.HooksConfig
		expected bool
	}{
		{
			name:     "empty config",
			config:   hooks.HooksConfig{},
			expected: false,
		},
		{
			name: "no task-dispatch",
			config: hooks.HooksConfig{
				PreToolUse: []hooks.HookEntry{
					{Matcher: "Bash(gh pr create*)", Hooks: []hooks.Hook{{Type: "command", Command: "gt tap guard pr-workflow"}}},
				},
			},
			expected: false,
		},
		{
			name: "has task-dispatch",
			config: hooks.HooksConfig{
				PreToolUse: []hooks.HookEntry{
					{Matcher: "Task", Hooks: []hooks.Hook{{Type: "command", Command: "gt tap guard task-dispatch"}}},
				},
			},
			expected: true,
		},
		{
			name: "has task-dispatch with path setup",
			config: hooks.HooksConfig{
				PreToolUse: []hooks.HookEntry{
					{Matcher: "Task", Hooks: []hooks.Hook{{Type: "command", Command: "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gt tap guard task-dispatch"}}},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsTaskDispatch(&tt.config); got != tt.expected {
				t.Errorf("containsTaskDispatch() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestStripTaskDispatch(t *testing.T) {
	t.Parallel()
	cfg := &hooks.HooksConfig{
		PreToolUse: []hooks.HookEntry{
			{Matcher: "Task", Hooks: []hooks.Hook{{Type: "command", Command: "gt tap guard task-dispatch"}}},
			{Matcher: "Bash(gh pr create*)", Hooks: []hooks.Hook{{Type: "command", Command: "gt tap guard pr-workflow"}}},
		},
	}

	stripped := stripTaskDispatch(cfg)

	if containsTaskDispatch(stripped) {
		t.Error("stripTaskDispatch did not remove task-dispatch entry")
	}

	// Should preserve the pr-workflow entry
	if len(stripped.PreToolUse) != 1 {
		t.Errorf("expected 1 PreToolUse entry after strip, got %d", len(stripped.PreToolUse))
	}
	if stripped.PreToolUse[0].Matcher != "Bash(gh pr create*)" {
		t.Errorf("wrong remaining entry: %s", stripped.PreToolUse[0].Matcher)
	}
}
