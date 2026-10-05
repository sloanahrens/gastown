package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadSaveBase(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	cfg := DefaultBase()

	if err := home.saveBase(cfg); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	if _, err := os.Stat(home.basePath()); err != nil {
		t.Fatalf("base config file not created: %v", err)
	}

	loaded, err := home.loadBase()
	if err != nil {
		t.Fatalf("LoadBase failed: %v", err)
	}

	if len(loaded.SessionStart) != 1 {
		t.Errorf("expected 1 SessionStart hook, got %d", len(loaded.SessionStart))
	}
	if len(loaded.PreCompact) != 1 {
		t.Errorf("expected 1 PreCompact hook, got %d", len(loaded.PreCompact))
	}
	if len(loaded.UserPromptSubmit) != 1 {
		t.Errorf("expected 1 UserPromptSubmit hook, got %d", len(loaded.UserPromptSubmit))
	}
}

func TestLoadSaveOverride(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	cfg := &HooksConfig{
		PreToolUse: []HookEntry{
			{
				Matcher: "Bash(git push*)",
				Hooks:   []Hook{{Type: "command", Command: "echo blocked && exit 2"}},
			},
		},
	}

	if err := home.saveOverride("crew", cfg); err != nil {
		t.Fatalf("SaveOverride failed: %v", err)
	}

	loaded, err := home.loadOverride("crew")
	if err != nil {
		t.Fatalf("LoadOverride failed: %v", err)
	}

	if len(loaded.PreToolUse) != 1 {
		t.Fatalf("expected 1 PreToolUse hook, got %d", len(loaded.PreToolUse))
	}
	if loaded.PreToolUse[0].Matcher != "Bash(git push*)" {
		t.Errorf("expected matcher 'Bash(git push*)', got %q", loaded.PreToolUse[0].Matcher)
	}
}

func TestLoadOverrideRejectsDuplicateMatchers(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	overridePath := home.overridePath("crew")
	if err := os.MkdirAll(filepath.Dir(overridePath), 0755); err != nil {
		t.Fatalf("creating overrides dir: %v", err)
	}

	raw := `{
  "PreToolUse": [
    {"matcher": "Bash(git push*)", "hooks": [{"type": "command", "command": "first"}]},
    {"matcher": "Bash(git push*)", "hooks": [{"type": "command", "command": "second"}]}
  ]
}`
	if err := os.WriteFile(overridePath, []byte(raw), 0644); err != nil {
		t.Fatalf("writing override: %v", err)
	}

	_, err := home.loadOverride("crew")
	if err == nil {
		t.Fatal("expected duplicate matcher error")
	}
	if !strings.Contains(err.Error(), "duplicate matcher") {
		t.Fatalf("expected duplicate matcher error, got: %v", err)
	}
}

func TestLoadSaveOverrideRigRole(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	cfg := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "echo gastown-crew"}}},
		},
	}

	if err := home.saveOverride("gastown/crew", cfg); err != nil {
		t.Fatalf("SaveOverride failed: %v", err)
	}

	expectedPath := filepath.Join(tmpDir, ".gt", "hooks-overrides", "gastown__crew.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected override file at %s: %v", expectedPath, err)
	}

	loaded, err := home.loadOverride("gastown/crew")
	if err != nil {
		t.Fatalf("LoadOverride failed: %v", err)
	}

	if len(loaded.SessionStart) != 1 {
		t.Fatalf("expected 1 SessionStart hook, got %d", len(loaded.SessionStart))
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	_, err := home.loadBase()
	if err == nil {
		t.Error("expected error loading missing base config")
	}

	_, err = home.loadOverride("crew")
	if err == nil {
		t.Error("expected error loading missing override config")
	}
}

func TestValidTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target string
		valid  bool
	}{
		{"crew", true},
		{"polecats", true},
		{"polecat", true},
		{"mayor", false}, // role retired (gt-rwp7z)
		{"rig", false},
		{"gastown/rig", false},
		{"gastown/crew", true},
		{"sky/polecats", true},
		{"", false},
		{"invalid", false},
		{"gastown/invalid", false},
		{"/crew", false},
		{"gastown/", false},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			if got := ValidTarget(tt.target); got != tt.valid {
				t.Errorf("ValidTarget(%q) = %v, want %v", tt.target, got, tt.valid)
			}
		})
	}
}

func TestNormalizeTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input      string
		normalized string
		valid      bool
	}{
		{"crew", "crew", true},
		{"polecats", "polecats", true},
		{"polecat", "polecats", true},
		{"gastown/polecats", "gastown/polecats", true},
		{"gastown/polecat", "gastown/polecats", true},
		{"mayor", "", false}, // role retired (gt-rwp7z)
		{"invalid", "", false},
		{"gastown/invalid", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := NormalizeTarget(tt.input)
			if ok != tt.valid {
				t.Errorf("NormalizeTarget(%q) valid = %v, want %v", tt.input, ok, tt.valid)
			}
			if got != tt.normalized {
				t.Errorf("NormalizeTarget(%q) = %q, want %q", tt.input, got, tt.normalized)
			}
		})
	}
}

func TestGetApplicableOverrides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target   string
		expected []string
	}{
		{"polecats", []string{"polecats"}},
		{"crew", []string{"crew"}},
		{"gastown/crew", []string{"crew", "gastown/crew"}},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			got := GetApplicableOverrides(tt.target)
			if len(got) != len(tt.expected) {
				t.Fatalf("GetApplicableOverrides(%q) returned %d items, want %d", tt.target, len(got), len(tt.expected))
			}
			for i, v := range got {
				if v != tt.expected[i] {
					t.Errorf("GetApplicableOverrides(%q)[%d] = %q, want %q", tt.target, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestDefaultBase(t *testing.T) {
	t.Parallel()
	cfg := DefaultBase()

	if len(cfg.SessionStart) == 0 {
		t.Error("DefaultBase should have SessionStart hooks")
	}
	if len(cfg.PreCompact) == 0 {
		t.Error("DefaultBase should have PreCompact hooks")
	}
	if len(cfg.UserPromptSubmit) == 0 {
		t.Error("DefaultBase should have UserPromptSubmit hooks")
	}

	found := false
	for _, entry := range cfg.SessionStart {
		for _, h := range entry.Hooks {
			if h.Command != "" {
				found = true
			}
		}
	}
	if !found {
		t.Error("DefaultBase SessionStart should have a command")
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()
	base := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "base-session"}}},
		},
		Stop: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "base-stop"}}},
		},
	}

	override := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "override-session"}}},
		},
		PreToolUse: []HookEntry{
			{Matcher: "Bash(git*)", Hooks: []Hook{{Type: "command", Command: "block-git"}}},
		},
	}

	result := Merge(base, override)

	if len(result.SessionStart) != 1 || result.SessionStart[0].Hooks[0].Command != "override-session" {
		t.Errorf("expected override SessionStart, got %v", result.SessionStart)
	}
	if len(result.Stop) != 1 || result.Stop[0].Hooks[0].Command != "base-stop" {
		t.Errorf("expected base Stop, got %v", result.Stop)
	}
	if len(result.PreToolUse) != 1 || result.PreToolUse[0].Matcher != "Bash(git*)" {
		t.Errorf("expected override PreToolUse, got %v", result.PreToolUse)
	}
	if len(base.PreToolUse) != 0 {
		t.Error("Merge mutated the original base config")
	}
}

// TestMergePerMatcherPreservation is the exact bug scenario from the spec:
// base has PreToolUse with matchers ["Bash(git*)", "Bash(rm*)"], override has
// PreToolUse with matcher ["Bash(git*)"]. The "Bash(rm*)" matcher must be preserved.
func TestMergePerMatcherPreservation(t *testing.T) {
	t.Parallel()
	base := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Bash(git*)", Hooks: []Hook{{Type: "command", Command: "git-guard"}}},
			{Matcher: "Bash(rm*)", Hooks: []Hook{{Type: "command", Command: "rm-guard"}}},
		},
	}
	override := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Bash(git*)", Hooks: []Hook{{Type: "command", Command: "crew-git-guard"}}},
		},
	}

	result := Merge(base, override)

	if len(result.PreToolUse) != 2 {
		t.Fatalf("expected 2 PreToolUse entries (per-matcher merge), got %d", len(result.PreToolUse))
	}

	// Bash(git*) should be replaced by override
	if result.PreToolUse[0].Matcher != "Bash(git*)" {
		t.Errorf("expected first matcher Bash(git*), got %q", result.PreToolUse[0].Matcher)
	}
	if result.PreToolUse[0].Hooks[0].Command != "crew-git-guard" {
		t.Errorf("expected override command for Bash(git*), got %q", result.PreToolUse[0].Hooks[0].Command)
	}

	// Bash(rm*) should be preserved from base
	if result.PreToolUse[1].Matcher != "Bash(rm*)" {
		t.Errorf("expected second matcher Bash(rm*), got %q", result.PreToolUse[1].Matcher)
	}
	if result.PreToolUse[1].Hooks[0].Command != "rm-guard" {
		t.Errorf("expected base command for Bash(rm*), got %q", result.PreToolUse[1].Hooks[0].Command)
	}
}

func TestMergeDifferentMatchersBothIncluded(t *testing.T) {
	t.Parallel()
	base := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Write", Hooks: []Hook{{Type: "command", Command: "write-check"}}},
		},
	}
	override := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Bash", Hooks: []Hook{{Type: "command", Command: "bash-check"}}},
		},
	}

	result := Merge(base, override)

	if len(result.PreToolUse) != 2 {
		t.Fatalf("expected 2 PreToolUse entries, got %d", len(result.PreToolUse))
	}
	if result.PreToolUse[0].Matcher != "Write" {
		t.Errorf("expected base Write matcher first, got %q", result.PreToolUse[0].Matcher)
	}
	if result.PreToolUse[1].Matcher != "Bash" {
		t.Errorf("expected override Bash matcher second, got %q", result.PreToolUse[1].Matcher)
	}
}

func TestMergeExplicitDisable(t *testing.T) {
	t.Parallel()
	base := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Write", Hooks: []Hook{{Type: "command", Command: "write-check"}}},
			{Matcher: "Bash", Hooks: []Hook{{Type: "command", Command: "bash-check"}}},
		},
	}
	override := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Write", Hooks: []Hook{}}, // Explicit disable
		},
	}

	result := Merge(base, override)

	if len(result.PreToolUse) != 1 {
		t.Fatalf("expected 1 PreToolUse entry after disable, got %d", len(result.PreToolUse))
	}
	if result.PreToolUse[0].Matcher != "Bash" {
		t.Errorf("expected Bash matcher to remain, got %q", result.PreToolUse[0].Matcher)
	}
}

func TestMergeEmptyOverride(t *testing.T) {
	t.Parallel()
	base := DefaultBase()
	override := &HooksConfig{}

	result := Merge(base, override)

	if !HooksEqual(base, result) {
		t.Error("empty override should not change base config")
	}
}

func TestComputeExpected(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	base := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "base-cmd"}}},
		},
	}
	if err := home.saveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	crewOverride := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Bash(git*)", Hooks: []Hook{{Type: "command", Command: "crew-guard"}}},
		},
	}
	if err := home.saveOverride("crew", crewOverride); err != nil {
		t.Fatalf("SaveOverride crew failed: %v", err)
	}

	gcOverride := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gastown-crew-session"}}},
		},
	}
	if err := home.saveOverride("gastown/crew", gcOverride); err != nil {
		t.Fatalf("SaveOverride gastown/crew failed: %v", err)
	}

	expected, err := home.computeExpected("gastown/crew")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}

	if len(expected.SessionStart) != 1 || expected.SessionStart[0].Hooks[0].Command != "gastown-crew-session" {
		t.Errorf("expected gastown/crew SessionStart, got %v", expected.SessionStart)
	}
	// On-disk base has no PreToolUse, so DefaultBase's shellExecutingToolMatcher
	// guard entry is backfilled. The crew override adds its own Bash(git*) entry.
	defaultPTU := len(DefaultBase().PreToolUse)
	if len(expected.PreToolUse) != defaultPTU+1 {
		t.Errorf("expected %d PreToolUse (default %d + crew 1), got %d", defaultPTU+1, defaultPTU, len(expected.PreToolUse))
	}
	// Verify crew-guard is present
	hasCrewGuard := false
	for _, e := range expected.PreToolUse {
		if e.Matcher == "Bash(git*)" && e.Hooks[0].Command == "crew-guard" {
			hasCrewGuard = true
		}
	}
	if !hasCrewGuard {
		t.Error("expected crew PreToolUse guard to be present")
	}
}

// TestComputeExpectedBackfillsSessionStart reproduces gt-y22: on-disk base
// created before SessionStart was added to DefaultBase. SessionStart should
// be backfilled from DefaultBase so settings.json files contain startup hooks.
func TestComputeExpectedBackfillsSessionStart(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	// Simulate a stale hooks-base.json that was created before SessionStart existed.
	// It has Stop, PreCompact, UserPromptSubmit but no SessionStart.
	staleBase := &HooksConfig{
		Stop: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gt status"}}},
		},
		PreCompact: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gt prime --hook"}}},
		},
		UserPromptSubmit: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gt mail check --inject"}}},
		},
	}
	if err := home.saveBase(staleBase); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// All targets should get SessionStart backfilled from DefaultBase
	for _, target := range []string{"mayor", "crew", "witness", "gastown/crew"} {
		expected, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s) failed: %v", target, err)
		}
		if len(expected.SessionStart) == 0 {
			t.Errorf("%s: expected SessionStart to be backfilled from DefaultBase, got none", target)
		}
		// Verify the generated hook uses a resolved gt command, not the stale
		// export PATH= marker that causes settings to be treated as out-of-date.
		hasPrime := false
		for _, entry := range expected.SessionStart {
			for _, hook := range entry.Hooks {
				if strings.Contains(hook.Command, "export PATH=") {
					t.Errorf("%s: SessionStart contains stale export PATH marker: %q", target, hook.Command)
				}
				if strings.Contains(hook.Command, "prime --hook") {
					hasPrime = true
				}
			}
		}
		if !hasPrime {
			t.Errorf("%s: expected prime --hook in SessionStart hooks", target)
		}
		// On-disk Stop should be preserved (not overwritten by DefaultBase)
		if len(expected.Stop) == 0 {
			t.Errorf("%s: on-disk Stop should be preserved", target)
		} else if expected.Stop[0].Hooks[0].Command != "gt status" {
			t.Errorf("%s: on-disk Stop should take precedence, got %q", target, expected.Stop[0].Hooks[0].Command)
		}
	}
}

func TestComputeExpectedFailsOnDuplicateOverrideMatcher(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	if err := home.saveBase(DefaultBase()); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	overridePath := home.overridePath("crew")
	if err := os.MkdirAll(filepath.Dir(overridePath), 0755); err != nil {
		t.Fatalf("creating overrides dir: %v", err)
	}

	raw := `{
  "SessionStart": [
    {"matcher": "", "hooks": [{"type": "command", "command": "first"}]},
    {"matcher": "", "hooks": [{"type": "command", "command": "second"}]}
  ]
}`
	if err := os.WriteFile(overridePath, []byte(raw), 0644); err != nil {
		t.Fatalf("writing override: %v", err)
	}

	_, err := home.computeExpected("crew")
	if err == nil {
		t.Fatal("expected ComputeExpected to fail on duplicate matcher")
	}
	if !strings.Contains(err.Error(), "duplicate matcher") {
		t.Fatalf("expected duplicate matcher error, got: %v", err)
	}
}

func TestComputeExpectedNoBase(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	// Mayor should get DefaultBase (no built-in overrides)
	expected, err := home.computeExpected("mayor")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}

	defaultBase := DefaultBase()
	if !HooksEqual(expected, defaultBase) {
		t.Error("expected DefaultBase for mayor when no configs exist")
	}

	// Crew should get DefaultBase + built-in crew override (PreCompact)
	crew, err := home.computeExpected("crew")
	if err != nil {
		t.Fatalf("home.computeExpected(crew) failed: %v", err)
	}
	// Crew has a built-in PreCompact override, so it won't equal bare DefaultBase
	if len(crew.PreCompact) == 0 {
		t.Error("expected crew to have PreCompact hook from DefaultOverrides")
	}
	// But it should still have the base SessionStart hooks
	if len(crew.SessionStart) != len(defaultBase.SessionStart) {
		t.Error("expected crew to inherit SessionStart from DefaultBase")
	}

	// The mayor has no PreToolUse override of its own: it must still carry
	// the ungated base guards from DefaultBase. dangerous-command comes from
	// DefaultBase, so an override that replaced rather than unioned its
	// shellExecutingToolMatcher entry would silently drop it (gt-8ki9).
	mayor, err := home.computeExpected("mayor")
	if err != nil {
		t.Fatalf("home.computeExpected(mayor) failed: %v", err)
	}
	requireUngatedGuardCommand(t, "mayor", mayor, "tap guard pr-workflow")
	requireUngatedGuardCommand(t, "mayor", mayor, "tap guard dangerous-command")
	if len(mayor.SessionStart) != len(defaultBase.SessionStart) {
		t.Error("expected mayor to inherit SessionStart from DefaultBase")
	}
}

// TestBuiltinHooksNeverUseIf pins the gt-3mp1 invariant: no BUILT-IN hook —
// DefaultBase or any DefaultOverrides role — carries an If condition.
//
// The reason is not stylistic. Claude Code's "if" evaluator treats a command
// it cannot statically resolve — a brace group containing a quoted string
// (echo x{"a"}y, any JSON/dict literal), or an argument-position $(...)
// substitution — as matching ANY pattern, so a deny hook gated by a
// leading-* glob fires on unrelated commands. That took out a share of the
// witness's patrol traffic (gt-3mp1). A guard that needs to discriminate on
// the command text must read tool_input.command off stdin instead, which is
// what every built-in guard now does; an If that creeps back into the
// defaults reintroduces the failure, so assert its absence here rather than
// trusting review to notice.
func TestBuiltinHooksNeverUseIf(t *testing.T) {
	t.Parallel()
	check := func(label string, cfg *HooksConfig) {
		t.Helper()
		for _, eventType := range EventTypes {
			for _, entry := range cfg.GetEntries(eventType) {
				for _, h := range entry.Hooks {
					if h.If != "" {
						t.Errorf("%s: %s hook on matcher %q has If=%q; built-in guards must self-filter on tool_input.command instead of an if-glob (gt-3mp1)",
							label, eventType, entry.Matcher, h.If)
					}
				}
			}
		}
	}

	check("DefaultBase", DefaultBase())
	for target, override := range DefaultOverrides() {
		check("DefaultOverrides["+target+"]", override)
	}
}

// requireUngatedGuardCommand asserts that cfg's PreToolUse has a bare
// shellExecutingToolMatcher entry with a Hook whose Command contains
// commandSubstring and whose If field is empty — the shape a self-filtering
// guard (one that reads tool_input.command off stdin and inspects it
// directly, like dangerous-command and patrol-loop) needs, since it must run
// unconditionally rather than depend on an "if" permission-glob to decide
// when it's even invoked (gt-qqfy).
func requireUngatedGuardCommand(t *testing.T, label string, cfg *HooksConfig, commandSubstring string) {
	t.Helper()
	entry, ok := findPreToolUse(cfg, shellExecutingToolMatcher)
	if !ok {
		t.Fatalf("%s: missing bare %q PreToolUse matcher entry", label, shellExecutingToolMatcher)
	}
	for _, h := range entry.Hooks {
		if strings.Contains(h.Command, commandSubstring) && h.If == "" {
			return
		}
	}
	t.Errorf("%s: missing ungated (If=\"\") PreToolUse hook with Command containing %q under matcher %q, got: %+v", label, commandSubstring, shellExecutingToolMatcher, entry.Hooks)
}

// TestComputeExpectedPolecatsGetPolecatPathsGuard pins the gt-hmaf wiring: the
// polecats role must receive the polecat-paths guard on both the
// shellExecutingToolMatcher and the file-writing tool matcher, alongside — not
// instead of — the guards it inherits from the base. A hooks-sync regression
// that dropped it, or a merge change that let a same-matcher override replace
// the base entry, would remove the only protection against one polecat editing
// another's worktree.
func TestComputeExpectedPolecatsGetPolecatPathsGuard(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	polecats, err := home.computeExpected("gastown/polecats")
	if err != nil {
		t.Fatalf("home.computeExpected(gastown/polecats): %v", err)
	}

	const guardCommand = "tap guard polecat-paths"
	shellEntry, ok := findPreToolUse(polecats, shellExecutingToolMatcher)
	if !ok {
		t.Fatalf("polecats missing the base %q PreToolUse entry", shellExecutingToolMatcher)
	}
	hasPolecatPaths := false
	for _, h := range shellEntry.Hooks {
		if strings.Contains(h.Command, guardCommand) {
			hasPolecatPaths = true
			if h.If != "" {
				t.Errorf("polecat-paths must self-filter (If=\"\"), got If=%q", h.If)
			}
		}
	}
	if !hasPolecatPaths {
		t.Errorf("polecats shell entry missing %q, got: %+v", guardCommand, shellEntry.Hooks)
	}
	// The base's own guards share the bare shellExecutingToolMatcher matcher
	// and must survive the union (gt-5ihs, gt-3mp1): pr-workflow, dangerous-command,
	// container-suite and bd-close-invariant are all self-filtering hooks with
	// no If.
	requireUngatedGuardCommand(t, "gastown/polecats", polecats, "tap guard pr-workflow")
	for _, want := range []string{"tap guard dangerous-command", "tap guard container-suite", "tap guard bd-close-invariant"} {
		found := false
		for _, h := range shellEntry.Hooks {
			if strings.Contains(h.Command, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("polecats shell entry lost the inherited %q guard, got: %+v", want, shellEntry.Hooks)
		}
	}

	// One regex matcher covers every tool that writes a file from the model's
	// side (Edit, Write, MultiEdit, NotebookEdit) — four separate matchers
	// would spawn the guard four times per call for no extra coverage.
	fileEntry, ok := findPreToolUse(polecats, "Edit|Write|MultiEdit|NotebookEdit")
	if !ok {
		entries := make([]string, 0, len(polecats.PreToolUse))
		for _, entry := range polecats.PreToolUse {
			entries = append(entries, entry.Matcher)
		}
		t.Fatalf("polecats missing the file-writing guard matcher, have: %v", entries)
	}
	fileGuard := false
	for _, h := range fileEntry.Hooks {
		if strings.Contains(h.Command, guardCommand) {
			fileGuard = true
		}
	}
	if !fileGuard {
		t.Errorf("file-writing matcher missing %q, got: %+v", guardCommand, fileEntry.Hooks)
	}

	// The same override also carries the idle-polecat Stop hook; wiring the
	// guard must not have displaced it.
	if len(polecats.Stop) == 0 {
		t.Error("polecats must keep the polecat-stop-check Stop hook")
	}

	// Other roles must not receive the guard — it is a polecat-only boundary.
	for _, target := range []string{"crew", "mayor", "witness", "refinery", "deacon"} {
		cfg, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s): %v", target, err)
		}
		for _, eventType := range EventTypes {
			for _, entry := range cfg.GetEntries(eventType) {
				for _, h := range entry.Hooks {
					if strings.Contains(h.Command, guardCommand) {
						t.Errorf("%s must not receive the polecat-paths guard (matcher %q)", target, entry.Matcher)
					}
				}
			}
		}
	}
}

// TestComputeExpectedQuestionToolGuardIsScopedToUnattendedRoles pins the
// gt-163k8 wiring: the interactive question tool is denied for the role
// that runs with nobody at the pane, and for nothing else. The guard removes a
// polecat's only interactive channel, so a wiring regression that let it reach
// crew, witness or mayor would silently take a person's question dialog away —
// and one that dropped it from the unattended roles would restore the 4h26m
// park the bead records.
func TestComputeExpectedQuestionToolGuardIsScopedToUnattendedRoles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	const (
		guardCommand = "tap guard question-tool"
		matcher      = "AskUserQuestion"
	)
	for _, target := range []string{"gastown/polecats"} {
		cfg, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s): %v", target, err)
		}
		entry, ok := findPreToolUse(cfg, matcher)
		if !ok {
			t.Fatalf("%s missing the %s PreToolUse matcher", target, matcher)
		}
		found := false
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, guardCommand) {
				found = true
				if h.If != "" {
					t.Errorf("%s question-tool guard must self-filter (If=\"\"), got If=%q", target, h.If)
				}
			}
		}
		if !found {
			t.Errorf("%s %s entry missing %q, got: %+v", target, matcher, guardCommand, entry.Hooks)
		}
	}

	// The attended roles keep their question dialog, on every event.
	for _, target := range []string{"crew", "mayor", "witness", "refinery", "deacon", "boot"} {
		cfg, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s): %v", target, err)
		}
		for _, eventType := range EventTypes {
			for _, entry := range cfg.GetEntries(eventType) {
				for _, h := range entry.Hooks {
					if strings.Contains(h.Command, guardCommand) {
						t.Errorf("%s must not receive the question-tool guard (event %s, matcher %q)",
							target, eventType, entry.Matcher)
					}
				}
			}
		}
	}
}

func findPreToolUse(cfg *HooksConfig, matcher string) (HookEntry, bool) {
	for _, entry := range cfg.PreToolUse {
		if entry.Matcher == matcher {
			return entry, true
		}
	}
	return HookEntry{}, false
}

// TestComputeExpectedPermissionRequestGuardReachesGeneratedSettings pins the
// gt-8stz wiring end to end: DefaultOverrides declares the guard, but the
// generated settings only carry it if merge.go actually propagates the
// PermissionRequest event type. Merge.go's applyOverride/cloneConfig once
// omitted PermissionRequest entirely, so DefaultOverrides' entries never
// reached ComputeExpected's output — a regression this test would have
// caught (gt-8stz review, finding 48f9948f3436).
func TestComputeExpectedPermissionRequestGuardReachesGeneratedSettings(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	const guardCommand = "tap guard permission-request"
	for _, target := range []string{"gastown/polecats"} {
		cfg, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s): %v", target, err)
		}
		if len(cfg.PermissionRequest) == 0 {
			t.Fatalf("%s: generated settings carry no PermissionRequest entries at all", target)
		}
		for _, matcher := range []string{shellExecutingToolMatcher, "Edit|Write|MultiEdit|NotebookEdit"} {
			var entry HookEntry
			found := false
			for _, e := range cfg.PermissionRequest {
				if e.Matcher == matcher {
					entry, found = e, true
				}
			}
			if !found {
				t.Fatalf("%s: PermissionRequest missing the %q matcher, have: %+v", target, matcher, cfg.PermissionRequest)
			}
			hasGuard := false
			for _, h := range entry.Hooks {
				if strings.Contains(h.Command, guardCommand) {
					hasGuard = true
				}
			}
			if !hasGuard {
				t.Errorf("%s: %q matcher missing %q, got: %+v", target, matcher, guardCommand, entry.Hooks)
			}
		}
	}

	// Interactive roles must carry no PermissionRequest entry at all — their
	// prompts stay with the person at the pane (gt-8stz).
	for _, target := range []string{"crew", "mayor", "witness", "refinery", "deacon", "boot"} {
		cfg, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s): %v", target, err)
		}
		if len(cfg.PermissionRequest) != 0 {
			t.Errorf("%s must not receive a PermissionRequest entry, got: %+v", target, cfg.PermissionRequest)
		}
	}
}

func TestComputeExpectedPolecatsKeepUserPromptMailCheck(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	cfg, err := home.computeExpected("polecats")
	if err != nil {
		t.Fatalf("home.computeExpected(polecats): %v", err)
	}
	if len(cfg.UserPromptSubmit) == 0 {
		t.Fatal("polecats should retain UserPromptSubmit mail-check")
	}
}

// TestComputeExpectedBuiltinPlusOnDisk verifies that on-disk overrides layer
// on top of built-in defaults rather than replacing them.
func TestComputeExpectedBuiltinPlusOnDisk(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	// Save an on-disk mayor override that adds a custom SessionStart hook
	customOverride := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "custom-mayor-session"}}},
		},
	}
	if err := home.saveOverride("mayor", customOverride); err != nil {
		t.Fatalf("SaveOverride failed: %v", err)
	}

	expected, err := home.computeExpected("mayor")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}

	// Should have the custom SessionStart from on-disk override
	if len(expected.SessionStart) == 0 {
		t.Error("on-disk SessionStart override should be present")
	} else if expected.SessionStart[0].Hooks[0].Command != "custom-mayor-session" {
		t.Errorf("expected custom-mayor-session, got %q", expected.SessionStart[0].Hooks[0].Command)
	}
}

func TestHooksEqual(t *testing.T) {
	t.Parallel()
	a := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "test"}}},
		},
	}
	b := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "test"}}},
		},
	}
	c := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "different"}}},
		},
	}

	if !HooksEqual(a, b) {
		t.Error("identical configs should be equal")
	}
	if HooksEqual(a, c) {
		t.Error("different configs should not be equal")
	}
	if !HooksEqual(&HooksConfig{}, &HooksConfig{}) {
		t.Error("empty configs should be equal")
	}
}

func TestLoadSettings(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Write raw JSON to test LoadSettings (SettingsJSON uses json:"-" tags)
	settingsJSON := `{
  "editorMode": "vim",
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "test"}]}
    ]
  }
}`
	path := filepath.Join(tmpDir, "settings.json")
	if err := os.WriteFile(path, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if loaded.EditorMode != "vim" {
		t.Errorf("expected editorMode vim, got %q", loaded.EditorMode)
	}
	if len(loaded.Hooks.SessionStart) != 1 {
		t.Errorf("expected 1 SessionStart hook, got %d", len(loaded.Hooks.SessionStart))
	}

	// Test loading non-existent file (should return zero-value)
	missing, err := LoadSettings(filepath.Join(tmpDir, "missing.json"))
	if err != nil {
		t.Fatalf("LoadSettings missing file failed: %v", err)
	}
	if missing.EditorMode != "" || len(missing.Hooks.SessionStart) != 0 {
		t.Error("missing file should return zero-value SettingsJSON")
	}
}

func TestLoadSettingsIntegrityError(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"SessionStart":"bad"}}`), 0644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	_, err := LoadSettings(path)
	if err == nil {
		t.Fatal("expected integrity error for malformed settings")
	}
	if !IsSettingsIntegrityError(err) {
		t.Fatalf("expected SettingsIntegrityError, got %T: %v", err, err)
	}
}

func TestDiscoverTargets(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "testrig", "crew", "alice"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "testrig", "crew", "bob"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "testrig", "witness"), 0755)

	targets, err := DiscoverTargets(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverTargets failed: %v", err)
	}

	if len(targets) != 1 {
		t.Errorf("expected 1 target, got %d", len(targets))
		for _, tgt := range targets {
			t.Logf("  target: %s (key=%s)", tgt.DisplayKey(), tgt.Key)
		}
	}

	found := make(map[string]bool)
	for _, tgt := range targets {
		found[tgt.DisplayKey()] = true
	}

	for _, expected := range []string{"testrig/crew"} {
		if !found[expected] {
			t.Errorf("expected target %q not found", expected)
		}
	}
	// A town may still hold the deleted roles' directories: they are not
	// settings targets (gt-4k3fj.6.1, gt-rwp7z).
	for _, retired := range []string{"mayor", "deacon", "testrig/witness"} {
		if found[retired] {
			t.Errorf("retired role directory %q discovered as a target", retired)
		}
	}
}

func TestDiscoverTargets_RoleNames(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "crew", "alice"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "polecats", "toast"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "witness"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "refinery"), 0755)

	targets, err := DiscoverTargets(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverTargets failed: %v", err)
	}

	// Verify Role field uses singular form (matching RoleSettingsDir conventions)
	roleByKey := make(map[string]string)
	for _, tgt := range targets {
		roleByKey[tgt.Key] = tgt.Role
	}

	expected := map[string]string{
		"rig1/crew":     "crew",
		"rig1/polecats": "polecat",
	}
	for _, retired := range []string{"mayor", "deacon", "rig1/witness", "rig1/refinery"} {
		if _, ok := roleByKey[retired]; ok {
			t.Errorf("retired role directory %q discovered as a target", retired)
		}
	}

	for key, wantRole := range expected {
		gotRole, ok := roleByKey[key]
		if !ok {
			t.Errorf("target %q not found", key)
			continue
		}
		if gotRole != wantRole {
			t.Errorf("target %q: Role = %q, want %q", key, gotRole, wantRole)
		}
	}
}

func TestDiscoverTargets_BootAbsent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon"), 0755)
	// No deacon/dogs/boot directory

	targets, err := DiscoverTargets(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverTargets failed: %v", err)
	}

	for _, tgt := range targets {
		if tgt.Key == "boot" {
			t.Errorf("expected no boot target when deacon/dogs/boot/ absent, got one: %+v", tgt)
		}
	}
}

func TestTargetDisplayKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target   Target
		expected string
	}{
		{Target{Key: "mayor", Role: "mayor"}, "mayor"},
		{Target{Key: "gastown/crew", Rig: "gastown", Role: "crew"}, "gastown/crew"},
	}

	for _, tt := range tests {
		if got := tt.target.DisplayKey(); got != tt.expected {
			t.Errorf("DisplayKey() = %q, want %q", got, tt.expected)
		}
	}
}

func TestGetSetEntries(t *testing.T) {
	t.Parallel()
	cfg := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "test"}}},
		},
	}

	entries := cfg.GetEntries("SessionStart")
	if len(entries) != 1 {
		t.Errorf("expected 1 SessionStart entry, got %d", len(entries))
	}

	entries = cfg.GetEntries("PreToolUse")
	if len(entries) != 0 {
		t.Errorf("expected 0 PreToolUse entries, got %d", len(entries))
	}

	entries = cfg.GetEntries("Unknown")
	if entries != nil {
		t.Errorf("expected nil for unknown event type, got %v", entries)
	}

	cfg.SetEntries("PreToolUse", []HookEntry{
		{Matcher: "Bash(*)", Hooks: []Hook{{Type: "command", Command: "guard"}}},
	})
	if len(cfg.PreToolUse) != 1 {
		t.Errorf("expected 1 PreToolUse entry after SetEntries, got %d", len(cfg.PreToolUse))
	}
}

func TestToMap(t *testing.T) {
	t.Parallel()
	cfg := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "start"}}},
		},
		Stop: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "stop"}}},
		},
	}

	m := cfg.ToMap()
	if len(m) != 2 {
		t.Errorf("expected 2 entries in map, got %d", len(m))
	}
	if _, ok := m["SessionStart"]; !ok {
		t.Error("expected SessionStart in map")
	}
	if _, ok := m["Stop"]; !ok {
		t.Error("expected Stop in map")
	}
	if _, ok := m["PreToolUse"]; ok {
		t.Error("empty PreToolUse should not be in map")
	}
}

func TestAddEntry(t *testing.T) {
	t.Parallel()
	cfg := &HooksConfig{}

	added := cfg.AddEntry("PreToolUse", HookEntry{
		Matcher: "Bash(git*)",
		Hooks:   []Hook{{Type: "command", Command: "guard"}},
	})
	if !added {
		t.Error("expected first entry to be added")
	}
	if len(cfg.PreToolUse) != 1 {
		t.Errorf("expected 1 PreToolUse entry, got %d", len(cfg.PreToolUse))
	}

	added = cfg.AddEntry("PreToolUse", HookEntry{
		Matcher: "Bash(git*)",
		Hooks:   []Hook{{Type: "command", Command: "different"}},
	})
	if added {
		t.Error("expected duplicate matcher to not be added")
	}
	if len(cfg.PreToolUse) != 1 {
		t.Errorf("expected still 1 PreToolUse entry, got %d", len(cfg.PreToolUse))
	}

	added = cfg.AddEntry("PreToolUse", HookEntry{
		Matcher: "Bash(rm*)",
		Hooks:   []Hook{{Type: "command", Command: "block"}},
	})
	if !added {
		t.Error("expected new matcher to be added")
	}
	if len(cfg.PreToolUse) != 2 {
		t.Errorf("expected 2 PreToolUse entries, got %d", len(cfg.PreToolUse))
	}
}

func TestMarshalConfig(t *testing.T) {
	t.Parallel()
	cfg := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "test"}}},
		},
	}

	data, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatalf("MarshalConfig failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("MarshalConfig returned empty data")
	}

	loaded := &HooksConfig{}
	if err := json.Unmarshal(data, loaded); err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}

	if len(loaded.SessionStart) != 1 {
		t.Errorf("round-trip lost SessionStart hooks")
	}
}

// TestNoPreToolUseMatcherContainsParenthesis is a regression guard for
// gt-5ihs: Claude Code's hooks[].matcher matches the TOOL NAME only (exact
// or regex, e.g. "Bash", "Edit|Write") — a permission-rule pattern like
// "Bash(git push*)" written into Matcher never fires. Real Claude Code tool
// names never contain "(", so any PreToolUse Matcher containing one is
// unreachable dead configuration. Command-pattern conditions belong in a
// Hook's If field instead. Checked across DefaultBase, every built-in
// DefaultOverrides role, and ComputeExpected's merged output so a future
// hand-added guard can't reintroduce the bug in any layer.
func TestNoPreToolUseMatcherContainsParenthesis(t *testing.T) {
	t.Parallel()
	assertNoParenMatchers := func(t *testing.T, label string, cfg *HooksConfig) {
		t.Helper()
		for _, entry := range cfg.PreToolUse {
			if strings.Contains(entry.Matcher, "(") {
				t.Errorf("%s: PreToolUse matcher %q contains \"(\" — permission-rule patterns never fire as a Claude Code matcher; put the pattern in a Hook's If field instead", label, entry.Matcher)
			}
		}
	}

	assertNoParenMatchers(t, "DefaultBase", DefaultBase())
	for role, cfg := range DefaultOverrides() {
		assertNoParenMatchers(t, "DefaultOverrides["+role+"]", cfg)
	}

	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}
	for _, target := range []string{"mayor", "crew", "witness", "refinery", "deacon", "polecats", "boot", "gastown/crew", "gastown/witness"} {
		expected, err := home.computeExpected(target)
		if err != nil {
			t.Fatalf("home.computeExpected(%s) failed: %v", target, err)
		}
		assertNoParenMatchers(t, "home.computeExpected("+target+")", expected)
	}
}

// A hooks-base.json written before gt costs was deleted (gt-638go.2) carries
// a Stop hook running "gt costs record". It must not reach the expected
// config, or every sync rewrites a hook that fails on every Stop (gt-31vjc).
func TestComputeExpectedDropsRetiredCommandHooks(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	home := configHome{home: tmpDir}

	staleBase := &HooksConfig{
		Stop: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "/Users/x/.local/bin/gt costs record &"}}},
		},
		PreCompact: []HookEntry{
			{Matcher: "", Hooks: []Hook{
				{Type: "command", Command: "gt mq list"},
				{Type: "command", Command: "gt prime --hook"},
			}},
		},
	}
	if err := home.saveBase(staleBase); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	expected, err := home.computeExpected("mayor")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}
	for _, et := range EventTypes {
		for _, entry := range expected.GetEntries(et) {
			for _, h := range entry.Hooks {
				if runsRetiredGTSubcommand(h.Command) {
					t.Errorf("%s still runs retired command %q", et, h.Command)
				}
			}
		}
	}
	if len(expected.PreCompact) != 1 || len(expected.PreCompact[0].Hooks) != 1 || expected.PreCompact[0].Hooks[0].Command != "gt prime --hook" {
		t.Errorf("PreCompact should keep only the live hook, got %+v", expected.PreCompact)
	}
}

func TestRunsRetiredGTSubcommand(t *testing.T) {
	t.Parallel()
	for cmd, want := range map[string]bool{
		"gt costs record &":                     true,
		"/Users/x/.local/bin/gt costs record &": true,
		"gt mq list":                            true,
		"gt deacon heartbeat":                   true,
		"gt witness status":                     true,
		"gt patrol report":                      true,
		"gt tap guard patrol-loop":              true,
		"gt tap guard boot-sendkeys":            true,
		"gt tap guard dangerous-command":        false,
		"gt tap":                                false,
		"gt prime --hook":                       false,
		"gt":                                    false,
		"echo gt costs":                         false,
		"gtx costs":                             false,
	} {
		if got := runsRetiredGTSubcommand(cmd); got != want {
			t.Errorf("runsRetiredGTSubcommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// TestConfigHomeCascade pins the read cascade: with GT_HOME set, its .gt is
// read (and written) first and ~/.gt is the fallback; a base found in either
// wins over the built-in default, the GT_HOME one over ~/.gt.
func TestConfigHomeCascade(t *testing.T) {
	t.Parallel()
	gtHome, userHome := t.TempDir(), t.TempDir()
	home := configHome{gtHome: gtHome, home: userHome}
	want := []string{filepath.Join(gtHome, ".gt"), filepath.Join(userHome, ".gt")}
	if got := home.configDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("configDirs() = %v, want %v", got, want)
	}
	if got := (configHome{gtHome: userHome, home: userHome}).configDirs(); len(got) != 1 {
		t.Errorf("GT_HOME == HOME: configDirs() = %v, want one dir", got)
	}

	userOnly := configHome{home: userHome}
	fallback := &HooksConfig{SessionStart: []HookEntry{{Matcher: "", Hooks: []Hook{{Type: "command", Command: "from-home"}}}}}
	if err := userOnly.saveBase(fallback); err != nil {
		t.Fatal(err)
	}
	got, err := home.loadBase()
	if err != nil || got.SessionStart[0].Hooks[0].Command != "from-home" {
		t.Fatalf("loadBase() = %+v, %v; want the ~/.gt fallback", got, err)
	}

	primary := &HooksConfig{SessionStart: []HookEntry{{Matcher: "", Hooks: []Hook{{Type: "command", Command: "from-gt-home"}}}}}
	if err := home.saveBase(primary); err != nil {
		t.Fatal(err)
	}
	got, err = home.loadBase()
	if err != nil || got.SessionStart[0].Hooks[0].Command != "from-gt-home" {
		t.Fatalf("loadBase() = %+v, %v; want the $GT_HOME/.gt copy", got, err)
	}
}
