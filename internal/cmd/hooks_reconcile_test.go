package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

// TestStripInterimHook pins the removal rule: only a Hook whose Command
// references interimHookMarker is dropped; every other hook on the same
// matcher, and every matcher that never carried the interim hook, is left
// untouched.
func TestStripInterimHook(t *testing.T) {
	t.Parallel()

	interim := hooks.Hook{Type: "command", Command: "/Users/sloan/.claude/hooks/no-root-scan.sh"}
	dangerous := hooks.Hook{Type: "command", Command: "/Users/sloan/.local/bin/gt tap guard dangerous-command"}
	prWorkflow := hooks.Hook{Type: "command", Command: "/Users/sloan/.local/bin/gt tap guard pr-workflow", If: "Bash(gh pr create*)"}

	t.Run("removes the interim hook alongside others on the same matcher", func(t *testing.T) {
		entries := []hooks.HookEntry{
			{Matcher: "Bash", Hooks: []hooks.Hook{prWorkflow, interim, dangerous}},
		}
		result, removed := stripInterimHook(entries)
		if !removed {
			t.Fatalf("stripInterimHook() removed=false, want true")
		}
		if len(result) != 1 || len(result[0].Hooks) != 2 {
			t.Fatalf("stripInterimHook() = %+v, want one entry with 2 hooks", result)
		}
		for _, h := range result[0].Hooks {
			if h.Command == interim.Command {
				t.Fatalf("interim hook still present: %+v", result)
			}
		}
	})

	t.Run("drops the whole entry when the interim hook was its only hook", func(t *testing.T) {
		entries := []hooks.HookEntry{
			{Matcher: "Bash", Hooks: []hooks.Hook{interim}},
			{Matcher: "Edit|Write", Hooks: []hooks.Hook{dangerous}},
		}
		result, removed := stripInterimHook(entries)
		if !removed {
			t.Fatalf("stripInterimHook() removed=false, want true")
		}
		if len(result) != 1 || result[0].Matcher != "Edit|Write" {
			t.Fatalf("stripInterimHook() = %+v, want only the Edit|Write entry left", result)
		}
	})

	t.Run("no-op when nothing references the interim hook", func(t *testing.T) {
		entries := []hooks.HookEntry{
			{Matcher: "Bash", Hooks: []hooks.Hook{prWorkflow, dangerous}},
		}
		result, removed := stripInterimHook(entries)
		if removed {
			t.Fatalf("stripInterimHook() removed=true, want false")
		}
		if len(result) != 1 || len(result[0].Hooks) != 2 {
			t.Fatalf("stripInterimHook() = %+v, want the entry unchanged", result)
		}
	})

	t.Run("no-op on empty input", func(t *testing.T) {
		result, removed := stripInterimHook(nil)
		if removed || len(result) != 0 {
			t.Fatalf("stripInterimHook(nil) = (%+v, %v), want (nil, false)", result, removed)
		}
	})
}

// TestReconcileTarget exercises the file-level wiring: a settings.json
// carrying the interim hook is rewritten with it removed (leaving every
// other field and hook intact), a dry run reports the change without
// writing, and a file that never had the interim hook is reported
// unchanged.
func TestReconcileTarget(t *testing.T) {
	t.Parallel()

	writeSettings := func(t *testing.T, dir, body string) string {
		t.Helper()
		path := filepath.Join(dir, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	const withInterim = `{
  "theme": "dark",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "/Users/sloan/.local/bin/gt tap guard dangerous-command"},
          {"type": "command", "command": "/Users/sloan/.claude/hooks/no-root-scan.sh"}
        ]
      }
    ]
  }
}`

	t.Run("removes the interim hook and preserves other fields", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeSettings(t, dir, withInterim)
		target := hooks.Target{Path: path, Key: "test/target"}

		changed, err := reconcileTarget(target, false)
		if err != nil {
			t.Fatalf("reconcileTarget: %v", err)
		}
		if !changed {
			t.Fatalf("reconcileTarget() changed=false, want true")
		}

		got, err := hooks.LoadSettings(path)
		if err != nil {
			t.Fatalf("LoadSettings after reconcile: %v", err)
		}
		entries := got.Hooks.PreToolUse
		if len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Fatalf("PreToolUse after reconcile = %+v, want one entry with one hook", entries)
		}
		if entries[0].Hooks[0].Command != "/Users/sloan/.local/bin/gt tap guard dangerous-command" {
			t.Fatalf("surviving hook = %q, want the dangerous-command guard", entries[0].Hooks[0].Command)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(raw), `"theme": "dark"`) {
			t.Errorf("theme field lost from settings.json: %s", raw)
		}
	})

	t.Run("dry run reports change without writing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeSettings(t, dir, withInterim)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		target := hooks.Target{Path: path, Key: "test/target"}

		changed, err := reconcileTarget(target, true)
		if err != nil {
			t.Fatalf("reconcileTarget: %v", err)
		}
		if !changed {
			t.Fatalf("reconcileTarget() changed=false, want true (dry run still reports the pending change)")
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("dry run modified the file on disk")
		}
	})

	t.Run("unchanged when the interim hook is absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const clean = `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/Users/sloan/.local/bin/gt tap guard dangerous-command"}]}
    ]
  }
}`
		path := writeSettings(t, dir, clean)
		target := hooks.Target{Path: path, Key: "test/target"}

		changed, err := reconcileTarget(target, false)
		if err != nil {
			t.Fatalf("reconcileTarget: %v", err)
		}
		if changed {
			t.Fatalf("reconcileTarget() changed=true, want false")
		}
	})

	t.Run("unchanged when the settings file does not exist", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := hooks.Target{Path: filepath.Join(dir, ".claude", "settings.json"), Key: "test/target"}

		changed, err := reconcileTarget(target, false)
		if err != nil {
			t.Fatalf("reconcileTarget: %v", err)
		}
		if changed {
			t.Fatalf("reconcileTarget() changed=true, want false for a missing file")
		}
	})

	// Unlike 'gt hooks sync' (hooks.SyncManagedClaudeSettings), reconcile's
	// job is removing one hook entry, not normalizing the rest of the file —
	// it must never force permission/onboarding defaults onto a file that
	// deliberately carries something else.
	t.Run("does not force Claude prompt defaults onto an operator-customized file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const customized = `{
  "skipDangerousModePermissionPrompt": false,
  "permissions": {"defaultMode": "default"},
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "/Users/sloan/.local/bin/gt tap guard dangerous-command"},
          {"type": "command", "command": "/Users/sloan/.claude/hooks/no-root-scan.sh"}
        ]
      }
    ]
  }
}`
		path := writeSettings(t, dir, customized)
		target := hooks.Target{Path: path, Key: "test/target"}

		changed, err := reconcileTarget(target, false)
		if err != nil {
			t.Fatalf("reconcileTarget: %v", err)
		}
		if !changed {
			t.Fatalf("reconcileTarget() changed=false, want true")
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(raw), `"skipDangerousModePermissionPrompt": false`) {
			t.Errorf("skipDangerousModePermissionPrompt was overwritten: %s", raw)
		}
		if !strings.Contains(string(raw), `"defaultMode": "default"`) {
			t.Errorf("permissions.defaultMode was overwritten: %s", raw)
		}
		if strings.Contains(string(raw), "no-root-scan.sh") {
			t.Errorf("interim hook still present: %s", raw)
		}
	})
}
