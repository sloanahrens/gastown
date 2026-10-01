package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallForRole_RoleAware(t *testing.T) {
	t.Parallel()
	// Claude's only autonomous role, "polecat", is exercised separately
	// (TestInstallForRole_PolecatClaudeSettingsUseManagedHooks): it routes
	// through the JSON merge path, not the static template compared here
	// (gt-8stz).
	tests := []struct {
		name     string
		role     string
		wantFile string // expected template used
	}{
		{"interactive crew", "crew", "settings-interactive.json"},
		{"interactive mayor", "mayor", "settings-interactive.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			err := InstallForRole(dir, tt.role)
			if err != nil {
				t.Fatalf("InstallForRole: %v", err)
			}

			path := filepath.Join(dir, ".claude", "settings.json")
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Fatal("settings.json not created")
			}

			// Verify content matches resolved template (with {{GT_BIN}} substituted)
			got, _ := os.ReadFile(path)
			want, err := renderTemplate(tt.role)
			if err != nil {
				t.Fatalf("resolveAndSubstitute: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("content mismatch: got %d bytes, want %d bytes (from %s)", len(got), len(want), tt.wantFile)
			}
		})
	}
}

func TestInstallForRole_ClaudeSettingsSuppressStartupPrompts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		role string
	}{
		{"autonomous", "polecat"},
		{"interactive", "crew"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := InstallForRole(dir, tt.role); err != nil {
				t.Fatalf("InstallForRole: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
			if err != nil {
				t.Fatalf("read settings: %v", err)
			}
			if strings.Contains(string(data), "export PATH=") {
				t.Fatal("claude settings contain stale export PATH marker")
			}
			if strings.Contains(string(data), "{{GT_BIN}}") {
				t.Fatal("claude settings contain unresolved {{GT_BIN}} placeholder")
			}

			var settings map[string]any
			if err := json.Unmarshal(data, &settings); err != nil {
				t.Fatalf("settings are not valid JSON: %v", err)
			}
			if got, ok := settings["skipDangerousModePermissionPrompt"].(bool); !ok || !got {
				t.Fatalf("skipDangerousModePermissionPrompt = %v, want true", settings["skipDangerousModePermissionPrompt"])
			}
			if got, ok := settings["hasCompletedOnboarding"].(bool); !ok || !got {
				t.Fatalf("hasCompletedOnboarding = %v, want true", settings["hasCompletedOnboarding"])
			}
			if got := settings["theme"]; got != "dark" {
				t.Fatalf("theme = %v, want dark", got)
			}
			permissions, ok := settings["permissions"].(map[string]any)
			if !ok {
				t.Fatalf("permissions = %T, want object", settings["permissions"])
			}
			if got := permissions["defaultMode"]; got != "bypassPermissions" {
				t.Fatalf("permissions.defaultMode = %v, want bypassPermissions", got)
			}
		})
	}
}

func TestInstallForRole_ClaudeCurrentTemplatePreservesExistingSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		role string
	}{
		{"autonomous", "polecat"},
		{"interactive", "crew"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			settingsPath := filepath.Join(dir, ".claude", "settings.json")
			if err := InstallForRole(dir, tt.role); err != nil {
				t.Fatalf("initial InstallForRole: %v", err)
			}

			data, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatalf("read settings: %v", err)
			}
			var settings map[string]any
			if err := json.Unmarshal(data, &settings); err != nil {
				t.Fatalf("unmarshal settings: %v", err)
			}
			settings["customSentinel"] = true
			updated, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				t.Fatalf("marshal settings: %v", err)
			}
			if err := os.WriteFile(settingsPath, append(updated, '\n'), 0600); err != nil {
				t.Fatalf("write settings: %v", err)
			}

			if err := InstallForRole(dir, tt.role); err != nil {
				t.Fatalf("second InstallForRole: %v", err)
			}

			data, err = os.ReadFile(settingsPath)
			if err != nil {
				t.Fatalf("read settings after second install: %v", err)
			}
			if err := json.Unmarshal(data, &settings); err != nil {
				t.Fatalf("unmarshal settings after second install: %v", err)
			}
			if got, ok := settings["customSentinel"].(bool); !ok || !got {
				t.Fatalf("customSentinel = %v, want true", settings["customSentinel"])
			}
		})
	}
}

// TestInstallForRole_PolecatClaudeSettingsUseManagedHooks pins gt-8stz: a
// polecat's settings.json is produced by the JSON merge path
// (SyncManagedClaudeSettings), same as boot/dog, so a hook config change
// (e.g. the PermissionRequest guard) reaches an existing settings file
// instead of the install being a silent no-op.
func TestInstallForRole_PolecatClaudeSettingsUseManagedHooks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing bool
	}{
		{name: "creates managed settings"},
		{name: "updates existing pre-fix settings", existing: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			home := configHome{home: dir}
			settingsPath := filepath.Join(dir, ".claude", "settings.json")

			if tt.existing {
				// Simulate the exact production drift: a settings.json written
				// before the PermissionRequest guard existed, carrying no such
				// entry, plus a customized field that must survive the sync.
				if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
					t.Fatalf("creating settings dir: %v", err)
				}
				stale := `{"customSentinel":true,"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"gt tap guard dangerous-command"}]}]}}`
				if err := os.WriteFile(settingsPath, []byte(stale), 0600); err != nil {
					t.Fatalf("writing existing settings: %v", err)
				}
			}

			if err := home.installForRole(dir, "polecat"); err != nil {
				t.Fatalf("InstallForRole: %v", err)
			}

			settings, err := LoadSettings(settingsPath)
			if err != nil {
				t.Fatalf("LoadSettings: %v", err)
			}
			foundGuard := false
			for _, entry := range settings.Hooks.PermissionRequest {
				for _, h := range entry.Hooks {
					if strings.Contains(h.Command, "tap guard permission-request") {
						foundGuard = true
					}
				}
			}
			if !foundGuard {
				t.Fatalf("polecat install did not carry the PermissionRequest guard, got: %+v", settings.Hooks.PermissionRequest)
			}
			if !HasClaudePromptDefaults(settings) {
				t.Fatal("polecat managed settings missing Claude prompt defaults")
			}
			if tt.existing {
				if raw, ok := settings.Extra["customSentinel"]; !ok || string(raw) != "true" {
					t.Fatalf("customSentinel not preserved: %s", raw)
				}
			}
		})
	}
}

// TestInstallForRole_PolecatUsesRigScopedOverrideKey pins the install-path
// regression found reviewing gt-wisp-4nns: polecat settings are shared per
// rig (config.RoleSettingsDir joins rigPath+"polecats"), and DiscoverTargets/
// GetApplicableOverrides manage the file under the rig-scoped key
// "<rig>/polecats" so a ~/.gt/hooks-overrides/<rig>__polecats.json override
// applies. Resolving the bare "polecats" key instead would silently drop
// that override on every polecat spawn, and gt hooks sync would put it back
// — the file would flip between the two on every spawn/sync cycle.
func TestInstallForRole_PolecatUsesRigScopedOverrideKey(t *testing.T) {
	t.Parallel()
	home := configHome{home: t.TempDir()}

	override := &HooksConfig{
		PreToolUse: []HookEntry{
			{
				Matcher: "Bash",
				Hooks: []Hook{
					{Type: "command", Command: "gt tap guard rig-marker"},
				},
			},
		},
	}
	if err := home.saveOverride("gastown/polecats", override); err != nil {
		t.Fatalf("SaveOverride: %v", err)
	}

	// settingsDir must look like <rig>/polecats for the rig-scoped key
	// derivation (filepath.Base(filepath.Dir(settingsDir))) to find "gastown",
	// matching what config.RoleSettingsDir produces for a real polecat spawn.
	rigRoot := t.TempDir()
	settingsDir := filepath.Join(rigRoot, "gastown", "polecats")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("mkdir settingsDir: %v", err)
	}

	if err := home.installForRole(settingsDir, "polecat"); err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	settings, err := LoadSettings(filepath.Join(settingsDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	found := false
	for _, entry := range settings.Hooks.PreToolUse {
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, "tap guard rig-marker") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("rig-scoped override was not applied — polecat install used the bare \"polecats\" key instead of \"<rig>/polecats\"")
	}
}

// TestInstallForRole_PolecatFailsClosedOnUnparseableBaseConfig pins the
// mayor-scoped decision on gt-8stz: a malformed ~/.gt/hooks-base.json must
// abort a polecat install with an actionable error naming the file, rather
// than silently falling back to a template that could be missing hooks a
// rig-scoped override added. This file controls a security guard's own
// settings, so installing on unreadable config (or leaving a stale copy in
// place with no signal) is worse than blocking the spawn with a clear error.
func TestInstallForRole_PolecatFailsClosedOnUnparseableBaseConfig(t *testing.T) {
	t.Parallel()
	home := configHome{home: t.TempDir()}

	basePath := home.basePath()
	if err := os.MkdirAll(filepath.Dir(basePath), 0755); err != nil {
		t.Fatalf("mkdir .gt: %v", err)
	}
	if err := os.WriteFile(basePath, []byte(`{not valid json`), 0644); err != nil {
		t.Fatalf("write malformed base: %v", err)
	}

	dir := t.TempDir()
	err := home.installForRole(dir, "polecat")
	if err == nil {
		t.Fatal("expected error from malformed hooks-base.json, got nil")
	}
	if !strings.Contains(err.Error(), basePath) {
		t.Errorf("error does not name the broken file %q: %v", basePath, err)
	}
}

// TestInstallForRole_PolecatFailsClosedOnCorruptExistingSettings is the
// existing-settings.json counterpart: a corrupt settings.json for an
// already-spawned polecat must abort the install rather than proceed as if
// the file did not exist.
func TestInstallForRole_PolecatFailsClosedOnCorruptExistingSettings(t *testing.T) {
	t.Parallel()
	home := configHome{home: t.TempDir()}

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{not valid json`), 0600); err != nil {
		t.Fatalf("write corrupt settings: %v", err)
	}

	err := home.installForRole(dir, "polecat")
	if err == nil {
		t.Fatal("expected error from corrupt existing settings.json, got nil")
	}
	if !strings.Contains(err.Error(), settingsPath) {
		t.Errorf("error does not name the broken file %q: %v", settingsPath, err)
	}
	if !IsSettingsIntegrityError(err) {
		t.Errorf("expected a SettingsIntegrityError in the chain, got: %v", err)
	}
}

func TestInstallForRole_SkipsExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)
	os.WriteFile(hooksPath, []byte("custom"), 0644)

	err := InstallForRole(dir, "crew")
	if err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	got, _ := os.ReadFile(hooksPath)
	if string(got) != "custom" {
		t.Error("existing file was overwritten")
	}
}

func TestInstallForRole_UpgradesStaleExportPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)

	// Write a stale file with the legacy "export PATH=" pattern
	os.WriteFile(hooksPath, []byte(`export PATH=/usr/local/bin:$PATH && gt hook`), 0644)

	err := InstallForRole(dir, "crew")
	if err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	got, _ := os.ReadFile(hooksPath)
	if strings.Contains(string(got), "export PATH=") {
		t.Error("stale export PATH pattern was not upgraded")
	}
	// Should now match the current template after placeholder substitution.
	template, _ := renderTemplate("crew")
	if string(got) != string(template) {
		t.Error("upgraded file does not match current template")
	}
}

// TestInstallForRole_UpgradesStaleParenMatcher pins the gt-5ihs follow-up
// (gt-wisp-db27 finding 3): needsUpgrade must recognize a PreToolUse
// matcher written as a permission-rule pattern (e.g. "Bash(gh pr
// create*)") as stale, so an agent scaffolded from an old settings.json
// gets auto-upgraded to the bare-tool-name + "if" layout instead of being
// left with dead guards forever.
func TestInstallForRole_UpgradesStaleParenMatcher(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)

	stale := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash(gh pr create*)",
        "hooks": [{"type": "command", "command": "gt tap guard pr-workflow"}]
      }
    ]
  }
}`
	os.WriteFile(hooksPath, []byte(stale), 0644)

	if err := InstallForRole(dir, "crew"); err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read upgraded settings: %v", err)
	}
	if hasParenPreToolUseMatcher(got) {
		t.Error("stale paren-style PreToolUse matcher was not upgraded")
	}
}

// TestInstallForRole_UpgradesStaleBareBashMatcher pins gt-ly9c4: the shipped
// Claude templates wrote their PreToolUse guards on a bare "Bash" matcher, so
// an agent scaffolded before the fix has a settings.json whose matcher names
// Bash but not Monitor — invisible to Monitor, which carries the same
// tool_input.command shape (gt-vx2mm). needsUpgrade's paren check cannot see
// that shape, so such a file was judged current and never upgraded. Catch it
// and rewrite from the template.
func TestInstallForRole_UpgradesStaleBareBashMatcher(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)

	stale := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "gt tap guard pr-workflow", "if": "Bash(gh pr create*)"},
          {"type": "command", "command": "gt tap guard dangerous-command"}
        ]
      }
    ]
  }
}`
	os.WriteFile(hooksPath, []byte(stale), 0644)

	if err := InstallForRole(dir, "crew"); err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read upgraded settings: %v", err)
	}
	if hasBareBashPreToolUseMatcher(got) {
		t.Error("stale bare \"Bash\" PreToolUse matcher was not upgraded (gt-ly9c4)")
	}
	if !strings.Contains(string(got), `"Bash|Monitor"`) {
		t.Errorf("upgraded settings do not match Monitor; got:\n%s", got)
	}
	if strings.Contains(string(got), `"if"`) {
		t.Errorf("upgraded settings still carry an If field (gt-3mp1); got:\n%s", got)
	}
}

// TestNeedsUpgradeIgnoresCurrentMatchers guards the other direction: a
// settings.json that already routes its shell guards through
// shellExecutingToolMatcher (and a non-Bash matcher such as the Edit|Write
// family) must NOT be treated as stale, or every install would clobber a
// customised file.
func TestNeedsUpgradeIgnoresCurrentMatchers(t *testing.T) {
	t.Parallel()
	current := []byte(`{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash|Monitor", "hooks": [{"type": "command", "command": "gt tap guard pr-workflow"}]},
      {"matcher": "Edit|Write|MultiEdit|NotebookEdit", "hooks": [{"type": "command", "command": "gt tap guard polecat-paths"}]}
    ]
  }
}`)
	if needsUpgrade(current) {
		t.Error("needsUpgrade flagged an up-to-date settings.json as stale")
	}

	// A Monitor-only matcher is equally current, and "BashOutput" is a
	// different tool name — neither may be mistaken for a Bash matcher.
	for _, matcher := range []string{"Monitor", "BashOutput", "BashOutput|Monitor"} {
		content := []byte(`{"hooks":{"PreToolUse":[{"matcher":"` + matcher + `","hooks":[{"type":"command","command":"gt tap guard pr-workflow"}]}]}}`)
		if needsUpgrade(content) {
			t.Errorf("needsUpgrade flagged matcher %q as a bare-Bash matcher", matcher)
		}
	}
}

func TestInstallForRole_Permissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Settings files should get 0600
	err := InstallForRole(dir, "crew")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, ".claude", "settings.json"))
	if info.Mode().Perm() != 0600 {
		t.Errorf("JSON file perm = %o, want 0600", info.Mode().Perm())
	}
}
