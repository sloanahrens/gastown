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
			if err := home.InstallForRole("claude", dir, dir, tt.role, ".claude", "settings.json", "claude", true); err != nil {
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
			if err := InstallForRole("claude", dir, dir, tt.role, ".claude", "settings.json", "claude", true); err != nil {
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
			if err := InstallForRole("claude", dir, dir, tt.role, ".claude", "settings.json", "claude", true); err != nil {
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

			if err := InstallForRole("claude", dir, dir, tt.role, ".claude", "settings.json", "claude", true); err != nil {
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

			if err := home.installForRole("claude", dir, dir, "polecat", ".claude", "settings.json", "claude", true); err != nil {
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

	if err := home.installForRole("claude", settingsDir, settingsDir, "polecat", ".claude", "settings.json", "claude", true); err != nil {
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
	err := home.installForRole("claude", dir, dir, "polecat", ".claude", "settings.json", "claude", true)
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

	err := home.installForRole("claude", dir, dir, "polecat", ".claude", "settings.json", "claude", true)
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

func TestInstallForRole_RoleAgnostic(t *testing.T) {
	t.Parallel()
	// OpenCode, Pi, OMP have single templates
	tests := []struct {
		provider  string
		hooksDir  string
		hooksFile string
	}{
		{"opencode", ".opencode/plugins", "gastown.js"},
		{"pi", ".pi/extensions", "gastown-hooks.js"},
		{"omp", ".omp/hooks", "gastown-hook.ts"},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			dir := t.TempDir()
			err := InstallForRole(tt.provider, dir, dir, "polecat", tt.hooksDir, tt.hooksFile, tt.provider, false)
			if err != nil {
				t.Fatalf("InstallForRole(%s): %v", tt.provider, err)
			}

			path := filepath.Join(dir, tt.hooksDir, tt.hooksFile)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Fatalf("%s not created", tt.hooksFile)
			}
		})
	}
}

func TestOpenCodeTemplateFailureDiagnostics(t *testing.T) {
	t.Parallel()
	template, err := templateFS.ReadFile("templates/opencode/gastown.js")
	if err != nil {
		t.Fatalf("read opencode template: %v", err)
	}
	content := string(template)
	for _, want := range []string{
		"command: ${cmd}",
		"exit_code:",
		"exit code 124",
		"timeout:",
		"stdout_tail:",
		"stderr_tail:",
		"timeout 10s ${gtCommand()} dolt status 2>&1",
		"dolt_status_tail:",
		"suggested_recovery:",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("opencode template missing diagnostic field %q", want)
		}
	}
}

func TestOpenCodeTemplateUsesHookPrime(t *testing.T) {
	t.Parallel()
	template, err := templateFS.ReadFile("templates/opencode/gastown.js")
	if err != nil {
		t.Fatalf("read opencode template: %v", err)
	}
	content := string(template)
	for _, want := range []string{
		"GT_HOOK_SOURCE=",
		"GT_SESSION_ID=",
		"prime --hook",
		"gt prime --hook",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("opencode template missing hook prime field %q", want)
		}
	}
	if strings.Contains(content, "gt mail check --inject") {
		t.Fatal("opencode template should let gt prime --hook handle mail injection")
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

	if err := home.InstallForRole("claude", dir, dir, "mayor", ".claude", "settings.json", "claude", true); err != nil {
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

func TestInstallForRole_UpgradesStaleExportPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".opencode/plugins", "gastown.js")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)

	// Write a stale file with the legacy "export PATH=" pattern
	os.WriteFile(hooksPath, []byte(`export PATH=/usr/local/bin:$PATH && gt hook`), 0644)

	err := InstallForRole("opencode", dir, dir, "crew", ".opencode/plugins", "gastown.js", "opencode", false)
	if err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	got, _ := os.ReadFile(hooksPath)
	if strings.Contains(string(got), "export PATH=") {
		t.Error("stale export PATH pattern was not upgraded")
	}
	// Should now match the current template after placeholder substitution.
	template, _ := resolveAndSubstitute("opencode", "gastown.js", "crew")
	if string(got) != string(template) {
		t.Error("upgraded file does not match current template")
	}
}

func TestInstallForRole_UpgradesStaleOpenCodePrimeHook(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".opencode/plugins", "gastown.js")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)

	os.WriteFile(hooksPath, []byte(`// Gas Town OpenCode plugin: hooks SessionStart/Compaction via events.
export const GasTown = async ({ $ }) => {
  await $`+"`"+`gt prime`+"`"+`
}`), 0644)

	if err := InstallForRole("opencode", dir, dir, "crew", ".opencode/plugins", "gastown.js", "opencode", false); err != nil {
		t.Fatalf("InstallForRole: %v", err)
	}

	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read upgraded hook: %v", err)
	}
	if strings.Contains(string(got), "captureRun(\"gt prime\")") || strings.Contains(string(got), "$`gt prime`") {
		t.Fatal("stale bare gt prime was not upgraded")
	}
	if !strings.Contains(string(got), "prime --hook") {
		t.Fatal("upgraded OpenCode hook does not run prime --hook")
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

	if err := InstallForRole("claude", dir, dir, "crew", ".claude", "settings.json", "claude", true); err != nil {
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

	if err := InstallForRole("claude", dir, dir, "crew", ".claude", "settings.json", "claude", true); err != nil {
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

func TestOpenCodeTemplateUsesHookModeAndCompoundRoles(t *testing.T) {
	t.Parallel()
	content, err := resolveAndSubstitute("opencode", "gastown.js", "polecat")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	s := string(content)
	for _, want := range []string{
		"prime --hook",
		"GT_HOOK_SOURCE=",
		"GT_SESSION_ID=",
		`parts[1] === "polecats"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("OpenCode template missing %q", want)
		}
	}
	if strings.Contains(s, "{{GT_BIN}}") {
		t.Fatal("OpenCode template contains unresolved {{GT_BIN}}")
	}
	if strings.Contains(s, "mail check --inject") {
		t.Fatal("OpenCode template should not duplicate startup mail injection")
	}
}

func TestSyncForRole_UpdatesStaleContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".opencode/plugins", "gastown.js")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)
	os.WriteFile(hooksPath, []byte("stale-content"), 0644)

	result, err := SyncForRole("opencode", dir, dir, "crew", ".opencode/plugins", "gastown.js", "opencode", false)
	if err != nil {
		t.Fatalf("SyncForRole: %v", err)
	}
	if result != SyncUpdated {
		t.Errorf("expected SyncUpdated, got %d", result)
	}

	got, _ := os.ReadFile(hooksPath)
	if string(got) == "stale-content" {
		t.Error("stale file was not updated")
	}

	// Should match the template after placeholder substitution.
	template, _ := resolveAndSubstitute("opencode", "gastown.js", "crew")
	if string(got) != string(template) {
		t.Error("updated file does not match current template")
	}
}

func TestSyncForRole_SkipsMatchingContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".opencode/plugins", "gastown.js")
	os.MkdirAll(filepath.Dir(hooksPath), 0755)

	// Write the actual installed template content — should report unchanged.
	template, _ := resolveAndSubstitute("opencode", "gastown.js", "crew")
	os.WriteFile(hooksPath, template, 0644)

	result, err := SyncForRole("opencode", dir, dir, "crew", ".opencode/plugins", "gastown.js", "opencode", false)
	if err != nil {
		t.Fatalf("SyncForRole: %v", err)
	}
	if result != SyncUnchanged {
		t.Errorf("expected SyncUnchanged, got %d", result)
	}
}

func TestSyncForRole_CreatesNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".opencode/plugins", "gastown.js")

	result, err := SyncForRole("opencode", dir, dir, "polecat", ".opencode/plugins", "gastown.js", "opencode", false)
	if err != nil {
		t.Fatalf("SyncForRole: %v", err)
	}
	if result != SyncCreated {
		t.Errorf("expected SyncCreated, got %d", result)
	}

	if _, err := os.Stat(hooksPath); os.IsNotExist(err) {
		t.Error("file was not created")
	}
}

func TestSyncForRole_EmptyProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	result, err := SyncForRole("", dir, dir, "crew", ".opencode/plugins", "gastown.js", "", false)
	if err != nil {
		t.Fatalf("expected nil error for empty provider, got: %v", err)
	}
	if result != SyncUnchanged {
		t.Errorf("expected SyncUnchanged for empty provider, got %d", result)
	}
}

func TestSyncForRole_InvalidProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := SyncForRole("nonexistent-provider", dir, dir, "crew", ".test", "settings.json", "nonexistent-provider", false)
	if err == nil {
		t.Error("expected error for invalid provider")
	}
}

func TestSyncForRole_WriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A regular file where the parent directory should be keeps MkdirAll
	// from creating the hooks dir.
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, nil, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := SyncForRole("opencode", notADir, notADir, "crew", ".opencode/plugins", "gastown.js", "opencode", false)
	if err == nil {
		t.Error("expected error when the hooks dir cannot be created")
	}
}

func TestSyncForRole_JSONWhitespaceInsensitive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// First, create the file via SyncForRole
	result, err := SyncForRole("gemini", dir, dir, "crew", ".gemini", "settings.json", "gemini", false)
	if err != nil {
		t.Fatalf("initial SyncForRole: %v", err)
	}
	if result != SyncCreated {
		t.Fatalf("expected SyncCreated, got %d", result)
	}

	// Read the canonical file, reformat with different whitespace
	targetPath := filepath.Join(dir, ".gemini", "settings.json")
	original, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("reading created file: %v", err)
	}

	// Reformat with different whitespace by round-tripping through json.MarshalIndent.
	// This changes indentation structure without corrupting string values (safe on Windows
	// where strings.ReplaceAll(":", " : ") would corrupt drive letters like C: → C :).
	var parsed interface{}
	if err := json.Unmarshal(original, &parsed); err != nil {
		t.Fatalf("parsing original JSON: %v", err)
	}
	reformatted, err := json.MarshalIndent(parsed, "", "    ")
	if err != nil {
		t.Fatalf("reformatting JSON: %v", err)
	}
	if string(original) == string(reformatted) {
		t.Fatal("reformatted content should differ from original bytes")
	}
	if err := os.WriteFile(targetPath, reformatted, 0600); err != nil {
		t.Fatalf("writing reformatted file: %v", err)
	}

	// SyncForRole should treat this as unchanged (structurally equal JSON)
	result, err = SyncForRole("gemini", dir, dir, "crew", ".gemini", "settings.json", "gemini", false)
	if err != nil {
		t.Fatalf("SyncForRole after reformat: %v", err)
	}
	if result != SyncUnchanged {
		t.Errorf("expected SyncUnchanged for whitespace-only JSON difference, got %d", result)
	}
}

func TestSyncForRole_GeminiWithGTBinSubstitution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	result, err := SyncForRole("gemini", dir, dir, "polecat", ".gemini", "settings.json", "gemini", false)
	if err != nil {
		t.Fatalf("SyncForRole: %v", err)
	}
	if result != SyncCreated {
		t.Errorf("expected SyncCreated, got %d", result)
	}

	got, _ := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	// Verify {{GT_BIN}} was substituted (should not appear in output)
	if strings.Contains(string(got), "{{GT_BIN}}") {
		t.Error("{{GT_BIN}} placeholder was not substituted")
	}
	// Verify the resolved binary path is present (JSON-escaped for Windows compatibility).
	gtBin := resolveGTBinary()
	gtBinJSON := strings.ReplaceAll(gtBin, `\`, `\\`)
	if !strings.Contains(string(got), gtBinJSON) {
		t.Errorf("expected resolved gt binary %q in output", gtBin)
	}
}

func TestInstallForRole_SettingsDirVsWorkDir(t *testing.T) {
	t.Parallel()
	settingsDir := t.TempDir()
	workDir := t.TempDir()

	// Claude uses settingsDir (useSettingsDir=true)
	err := InstallForRole("claude", settingsDir, workDir, "crew", ".claude", "settings.json", "claude", true)
	if err != nil {
		t.Fatalf("InstallForRole (claude): %v", err)
	}
	if _, err := os.Stat(filepath.Join(settingsDir, ".claude", "settings.json")); os.IsNotExist(err) {
		t.Error("claude: file not in settingsDir")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("claude: file should not be in workDir")
	}

	// OpenCode uses workDir (useSettingsDir=false)
	err = InstallForRole("opencode", settingsDir, workDir, "polecat", ".opencode/plugins", "gastown.js", "opencode", false)
	if err != nil {
		t.Fatalf("InstallForRole (opencode): %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".opencode/plugins", "gastown.js")); os.IsNotExist(err) {
		t.Error("opencode: file not in workDir")
	}
}

func TestInstallForRole_EmptyProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := InstallForRole("", dir, dir, "crew", ".claude", "settings.json", "", false)
	if err != nil {
		t.Fatalf("expected nil error for empty provider, got: %v", err)
	}
}

func TestInstallForRole_Permissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// JSON files should get 0600
	err := InstallForRole("claude", dir, dir, "crew", ".claude", "settings.json", "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, ".claude", "settings.json"))
	if info.Mode().Perm() != 0600 {
		t.Errorf("JSON file perm = %o, want 0600", info.Mode().Perm())
	}

	// Non-JSON files should get 0644
	dir2 := t.TempDir()
	err = InstallForRole("pi", dir2, dir2, "polecat", ".pi/extensions", "gastown-hooks.js", "pi", false)
	if err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(filepath.Join(dir2, ".pi/extensions", "gastown-hooks.js"))
	if info.Mode().Perm() != 0644 {
		t.Errorf("JS file perm = %o, want 0644", info.Mode().Perm())
	}
}

func TestInstallForRole_CursorRoleAware(t *testing.T) {
	t.Parallel()
	// Cursor uses hooks-autonomous.json / hooks-interactive.json naming
	dir := t.TempDir()
	err := InstallForRole("cursor", dir, dir, "polecat", ".cursor", "hooks.json", "cursor", false)
	if err != nil {
		t.Fatalf("InstallForRole(cursor, polecat): %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, ".cursor", "hooks.json"))
	want, err := resolveAndSubstitute("cursor", "hooks-autonomous.json", "polecat")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(got) != string(want) {
		t.Error("cursor autonomous: content mismatch")
	}

	dir2 := t.TempDir()
	err = InstallForRole("cursor", dir2, dir2, "crew", ".cursor", "hooks.json", "cursor", false)
	if err != nil {
		t.Fatalf("InstallForRole(cursor, crew): %v", err)
	}

	got, _ = os.ReadFile(filepath.Join(dir2, ".cursor", "hooks.json"))
	want, err = resolveAndSubstitute("cursor", "hooks-interactive.json", "crew")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(got) != string(want) {
		t.Error("cursor interactive: content mismatch")
	}
}

func TestInstallForRole_GeminiRoleAware(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := InstallForRole("gemini", dir, dir, "polecat", ".gemini", "settings.json", "gemini", false)
	if err != nil {
		t.Fatalf("InstallForRole(gemini, polecat): %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	want, _ := templateFS.ReadFile("templates/gemini/settings-autonomous.json")
	// Gemini templates contain {{GT_BIN}} which gets resolved at install time.
	// Apply the same substitution (with JSON escaping) to the expected content for comparison.
	gtBin := resolveGTBinary()
	gtBinJSON := strings.ReplaceAll(gtBin, `\`, `\\`)
	wantResolved := strings.ReplaceAll(string(want), "{{GT_BIN}}", gtBinJSON)
	if string(got) != wantResolved {
		t.Error("gemini autonomous: content mismatch")
	}
}

func TestInstallForRole_CodexRoleAware(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := InstallForRole("codex", dir, dir, "crew", ".codex", "hooks.json", "codex", false)
	if err != nil {
		t.Fatalf("InstallForRole(codex, crew): %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	want, err := resolveAndSubstitute("codex", "hooks-interactive.json", "crew")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(got) != string(want) {
		t.Error("codex interactive: content mismatch")
	}

	dir2 := t.TempDir()
	err = InstallForRole("codex", dir2, dir2, "polecat", ".codex", "hooks.json", "codex", false)
	if err != nil {
		t.Fatalf("InstallForRole(codex, polecat): %v", err)
	}

	got, _ = os.ReadFile(filepath.Join(dir2, ".codex", "hooks.json"))
	want, err = resolveAndSubstitute("codex", "hooks-autonomous.json", "polecat")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(got) != string(want) {
		t.Error("codex autonomous: content mismatch")
	}
}

func TestInstallForRole_CopilotRoleAware(t *testing.T) {
	t.Parallel()
	// Copilot uses gastown-autonomous.json / gastown-interactive.json naming
	dir := t.TempDir()
	err := InstallForRole("copilot", dir, dir, "polecat", ".github/hooks", "gastown.json", "copilot", false)
	if err != nil {
		t.Fatalf("InstallForRole(copilot, polecat): %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, ".github/hooks", "gastown.json"))
	want, err := resolveAndSubstitute("copilot", "gastown-autonomous.json", "polecat")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(got) != string(want) {
		t.Error("copilot autonomous: content mismatch")
	}

	dir2 := t.TempDir()
	err = InstallForRole("copilot", dir2, dir2, "crew", ".github/hooks", "gastown.json", "copilot", false)
	if err != nil {
		t.Fatalf("InstallForRole(copilot, crew): %v", err)
	}

	got, _ = os.ReadFile(filepath.Join(dir2, ".github/hooks", "gastown.json"))
	want, err = resolveAndSubstitute("copilot", "gastown-interactive.json", "crew")
	if err != nil {
		t.Fatalf("resolveAndSubstitute: %v", err)
	}
	if string(got) != string(want) {
		t.Error("copilot interactive: content mismatch")
	}
}

func TestComputeExpectedTemplate_Gemini(t *testing.T) {
	t.Parallel()
	// Autonomous role should get settings-autonomous.json template
	content, err := ComputeExpectedTemplate("gemini", "settings.json", "polecat")
	if err != nil {
		t.Fatalf("ComputeExpectedTemplate: %v", err)
	}

	// Should contain resolved gt binary path, not {{GT_BIN}}
	if strings.Contains(string(content), "{{GT_BIN}}") {
		t.Error("expected {{GT_BIN}} to be resolved")
	}

	// Should contain GT_HOOK_SOURCE=compact (from autonomous template)
	if !strings.Contains(string(content), "GT_HOOK_SOURCE=compact") {
		t.Error("expected GT_HOOK_SOURCE=compact in autonomous template")
	}

	if strings.Contains(string(content), `"context"`) || strings.Contains(string(content), `"fileName"`) {
		t.Error("Gemini template should not force context.fileName; GEMINI.md overlays must remain loadable")
	}

	// Interactive role should get settings-interactive.json template
	interactiveContent, err := ComputeExpectedTemplate("gemini", "settings.json", "crew")
	if err != nil {
		t.Fatalf("ComputeExpectedTemplate(crew): %v", err)
	}

	// Interactive template should NOT contain GT_HOOK_SOURCE=compact
	if strings.Contains(string(interactiveContent), "GT_HOOK_SOURCE=compact") {
		t.Error("interactive template should not contain GT_HOOK_SOURCE=compact")
	}
}

func TestTemplateContentEqual(t *testing.T) {
	t.Parallel()
	// Same JSON, different formatting
	a := []byte(`{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"test"}]}]}}`)
	b := []byte(`{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "test"
          }
        ]
      }
    ]
  }
}`)

	if !TemplateContentEqual(a, b) {
		t.Error("expected structurally equal JSON to match")
	}

	// Different content
	c := []byte(`{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"different"}]}]}}`)
	if TemplateContentEqual(a, c) {
		t.Error("expected different JSON to not match")
	}

	// Invalid JSON
	invalid := []byte(`not json`)
	if TemplateContentEqual(a, invalid) {
		t.Error("expected invalid JSON to not match")
	}
}
