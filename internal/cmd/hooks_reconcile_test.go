package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

// TestStripInterimHook pins the removal rule: only a hook whose command
// references interimHookMarker is dropped; every other hook on the same
// matcher, and every matcher that never carried the interim hook, is left
// untouched.
func TestStripInterimHook(t *testing.T) {
	t.Parallel()

	const (
		interim    = `{"type": "command", "command": "/Users/sloan/.claude/hooks/no-root-scan.sh"}`
		dangerous  = `{"type": "command", "command": "/Users/sloan/.local/bin/gt tap guard dangerous-command"}`
		prWorkflow = `{"type": "command", "command": "/Users/sloan/.local/bin/gt tap guard pr-workflow", "if": "Bash(gh pr create*)"}`
	)
	entry := func(matcher string, hookJSON ...string) json.RawMessage {
		return json.RawMessage(`{"matcher": "` + matcher + `", "hooks": [` + strings.Join(hookJSON, ",") + `]}`)
	}
	commands := func(t *testing.T, e json.RawMessage) []string {
		t.Helper()
		var parsed struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(e, &parsed); err != nil {
			t.Fatalf("unmarshal entry %s: %v", e, err)
		}
		var cmds []string
		for _, h := range parsed.Hooks {
			cmds = append(cmds, h.Command)
		}
		return cmds
	}

	t.Run("removes the interim hook alongside others on the same matcher", func(t *testing.T) {
		result, removed, err := stripInterimHook([]json.RawMessage{entry("Bash", prWorkflow, interim, dangerous)})
		if err != nil {
			t.Fatalf("stripInterimHook: %v", err)
		}
		if !removed {
			t.Fatalf("stripInterimHook() removed=false, want true")
		}
		if len(result) != 1 {
			t.Fatalf("stripInterimHook() = %s, want one entry", result)
		}
		cmds := commands(t, result[0])
		if len(cmds) != 2 {
			t.Fatalf("surviving commands = %v, want 2", cmds)
		}
		for _, c := range cmds {
			if strings.Contains(c, interimHookMarker) {
				t.Fatalf("interim hook still present: %v", cmds)
			}
		}
		if !strings.Contains(string(result[0]), `"if":"Bash(gh pr create*)"`) {
			t.Errorf("surviving hook lost its if key: %s", result[0])
		}
	})

	t.Run("drops the whole entry when the interim hook was its only hook", func(t *testing.T) {
		result, removed, err := stripInterimHook([]json.RawMessage{
			entry("Bash", interim),
			entry("Edit|Write", dangerous),
		})
		if err != nil {
			t.Fatalf("stripInterimHook: %v", err)
		}
		if !removed {
			t.Fatalf("stripInterimHook() removed=false, want true")
		}
		if len(result) != 1 || !strings.Contains(string(result[0]), "Edit|Write") {
			t.Fatalf("stripInterimHook() = %s, want only the Edit|Write entry left", result)
		}
	})

	t.Run("no-op when nothing references the interim hook", func(t *testing.T) {
		in := []json.RawMessage{entry("Bash", prWorkflow, dangerous)}
		result, removed, err := stripInterimHook(in)
		if err != nil {
			t.Fatalf("stripInterimHook: %v", err)
		}
		if removed {
			t.Fatalf("stripInterimHook() removed=true, want false")
		}
		if len(result) != 1 || string(result[0]) != string(in[0]) {
			t.Fatalf("stripInterimHook() = %s, want the entry byte-identical", result)
		}
	})

	t.Run("keeps an entry with no hooks array untouched", func(t *testing.T) {
		bare := json.RawMessage(`{"matcher": "Bash"}`)
		result, removed, err := stripInterimHook([]json.RawMessage{bare})
		if err != nil {
			t.Fatalf("stripInterimHook: %v", err)
		}
		if removed || len(result) != 1 || string(result[0]) != string(bare) {
			t.Fatalf("stripInterimHook(bare) = (%s, %v), want the entry untouched", result, removed)
		}
	})

	t.Run("no-op on empty input", func(t *testing.T) {
		result, removed, err := stripInterimHook(nil)
		if err != nil {
			t.Fatalf("stripInterimHook: %v", err)
		}
		if removed || len(result) != 0 {
			t.Fatalf("stripInterimHook(nil) = (%s, %v), want (nil, false)", result, removed)
		}
	})

	// An entry stripInterimHook cannot read might carry the interim hook, so
	// passing it through would report the target unchanged after a removal
	// that never happened (gt-hbjly).
	t.Run("errors on an entry that cannot be inspected", func(t *testing.T) {
		bad := []json.RawMessage{
			json.RawMessage(`"not an object"`),
			json.RawMessage(`{"matcher": "Bash", "hooks": "not an array"}`),
			json.RawMessage(`{"matcher": "Bash", "hooks": [{"type": "command", "command": 42}]}`),
		}
		for _, e := range bad {
			result, removed, err := stripInterimHook([]json.RawMessage{e})
			if err == nil {
				t.Errorf("stripInterimHook(%s) err = nil, want an error", e)
				continue
			}
			if removed || result != nil {
				t.Errorf("stripInterimHook(%s) = (%s, %v) beside the error, want no result", e, result, removed)
			}
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

	// The typed hooks.HooksConfig has no Notification field and hooks.Hook has
	// no timeout, so a typed round-trip dropped both whenever reconcile removed
	// the interim hook from a file that carried them (gt-wqvxe).
	t.Run("keeps hook events and hook keys the typed config lacks", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const rich = `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "/Users/sloan/.local/bin/gt tap guard dangerous-command", "timeout": 30},
          {"type": "command", "command": "/Users/sloan/.claude/hooks/no-root-scan.sh"}
        ]
      }
    ],
    "Notification": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/Users/sloan/.local/bin/notify"}]}
    ]
  }
}`
		path := writeSettings(t, dir, rich)
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
		var got struct {
			Hooks map[string][]struct {
				Hooks []map[string]any `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal rewritten settings: %v\n%s", err, raw)
		}
		if _, ok := got.Hooks["Notification"]; !ok {
			t.Errorf("Notification event lost from settings.json: %s", raw)
		}
		pre := got.Hooks["PreToolUse"]
		if len(pre) != 1 || len(pre[0].Hooks) != 1 {
			t.Fatalf("PreToolUse after reconcile = %+v, want one entry with one hook", pre)
		}
		if timeout, _ := pre[0].Hooks[0]["timeout"].(float64); timeout != 30 {
			t.Errorf("timeout key lost from the surviving hook: %s", raw)
		}
		if strings.Contains(string(raw), "no-root-scan.sh") {
			t.Errorf("interim hook still present: %s", raw)
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

		// The dry run goes through the same os.IsNotExist narrowing, and the
		// absence still has to read as "nothing to do", not an error (gt-hbjly).
		for _, dryRun := range []bool{false, true} {
			changed, err := reconcileTarget(target, dryRun)
			if err != nil {
				t.Fatalf("reconcileTarget(dryRun=%v): %v", dryRun, err)
			}
			if changed {
				t.Fatalf("reconcileTarget(dryRun=%v) changed=true, want false for a missing file", dryRun)
			}
		}
	})

	// The two other early returns: a settings.json with no "hooks" key, and
	// one whose hooks carry no PreToolUse event (gt-hbjly).
	t.Run("unchanged when settings carries nothing to reconcile", func(t *testing.T) {
		t.Parallel()
		bodies := map[string]string{
			"no hooks key":  `{"theme": "dark"}`,
			"no PreToolUse": `{"hooks": {"Notification": [{"matcher": "", "hooks": [{"type": "command", "command": "/Users/sloan/.local/bin/notify"}]}]}}`,
		}
		for name, body := range bodies {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				path := writeSettings(t, dir, body)
				target := hooks.Target{Path: path, Key: "test/target"}

				changed, err := reconcileTarget(target, false)
				if err != nil {
					t.Fatalf("reconcileTarget: %v", err)
				}
				if changed {
					t.Fatalf("reconcileTarget() changed=true, want false")
				}
			})
		}
	})

	// A PreToolUse entry this cannot read is a fail-closed integrity error,
	// never a silent "unchanged" (gt-hbjly).
	t.Run("uninspectable PreToolUse entry is an integrity error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		const weird = `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": 42}]}
    ]
  }
}`
		path := writeSettings(t, dir, weird)
		target := hooks.Target{Path: path, Key: "test/target"}

		changed, err := reconcileTarget(target, false)
		if !hooks.IsSettingsIntegrityError(err) {
			t.Fatalf("reconcileTarget() err = %v, want a settings integrity error", err)
		}
		if changed {
			t.Fatalf("reconcileTarget() changed=true, want false beside the error")
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile: %v", readErr)
		}
		if string(raw) != weird {
			t.Errorf("settings.json was rewritten despite the error: %s", raw)
		}
	})

	t.Run("malformed settings is an integrity error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeSettings(t, dir, `{"hooks": [`)
		target := hooks.Target{Path: path, Key: "test/target"}

		_, err := reconcileTarget(target, false)
		if !hooks.IsSettingsIntegrityError(err) {
			t.Fatalf("reconcileTarget() err = %v, want a settings integrity error", err)
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
