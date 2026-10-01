package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every Claude role's install writes the managed hooks gt hooks sync writes
// for its key, not a static template (gt-4k3fj.8.3).
func TestInstallForRole_ClaudeRolesGetManagedHooks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ role, sub, key string }{
		{"crew", "crew", "gastown/crew"},
		{"polecat", "polecats", "gastown/polecats"},
		{"mayor", "mayor", "mayor"},
	} {
		t.Run(tt.role, func(t *testing.T) {
			t.Parallel()
			home := HomeAt(t.TempDir())
			dir := filepath.Join(t.TempDir(), "gastown", tt.sub)
			if err := home.InstallForRole(dir, tt.role); err != nil {
				t.Fatalf("InstallForRole: %v", err)
			}
			got, err := LoadSettings(filepath.Join(dir, ".claude", "settings.json"))
			if err != nil {
				t.Fatalf("LoadSettings: %v", err)
			}
			want, err := home.ComputeExpected(tt.key)
			if err != nil {
				t.Fatalf("ComputeExpected(%s): %v", tt.key, err)
			}
			if !HooksEqual(want, &got.Hooks) {
				t.Errorf("hooks differ from the managed set for %s", tt.key)
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

// An existing Claude settings file has its hooks replaced by the managed set
// and keeps every other field (gt-4k3fj.8.3).
func TestInstallForRole_SyncsExistingKeepsOtherFields(t *testing.T) {
	t.Parallel()
	home := HomeAt(t.TempDir())
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte(`{"model":"custom","hooks":{"Stop":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := home.InstallForRole(dir, "mayor"); err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	data, _ := os.ReadFile(hooksPath)
	if !strings.Contains(string(data), `"model": "custom"`) {
		t.Errorf("existing model field was dropped:\n%s", data)
	}
	if err := home.CheckManagedClaudeSettings(Target{Path: hooksPath, Key: "mayor"}); err != nil {
		t.Errorf("after install: %v", err)
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
