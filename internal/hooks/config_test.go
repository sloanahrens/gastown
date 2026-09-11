package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setTestHome sets HOME (and USERPROFILE on Windows) so that
// os.UserHomeDir() returns tmpDir on all platforms.
func setTestHome(t *testing.T, tmpDir string) {
	t.Helper()
	t.Setenv("HOME", tmpDir)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmpDir)
	}
}

func TestLoadSaveBase(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	cfg := DefaultBase()

	if err := SaveBase(cfg); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	if _, err := os.Stat(BasePath()); err != nil {
		t.Fatalf("base config file not created: %v", err)
	}

	loaded, err := LoadBase()
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
	if len(loaded.Stop) != 1 {
		t.Errorf("expected 1 Stop hook, got %d", len(loaded.Stop))
	}
}

func TestLoadSaveOverride(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	cfg := &HooksConfig{
		PreToolUse: []HookEntry{
			{
				Matcher: "Bash(git push*)",
				Hooks:   []Hook{{Type: "command", Command: "echo blocked && exit 2"}},
			},
		},
	}

	if err := SaveOverride("crew", cfg); err != nil {
		t.Fatalf("SaveOverride failed: %v", err)
	}

	loaded, err := LoadOverride("crew")
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
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	overridePath := OverridePath("crew")
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

	_, err := LoadOverride("crew")
	if err == nil {
		t.Fatal("expected duplicate matcher error")
	}
	if !strings.Contains(err.Error(), "duplicate matcher") {
		t.Fatalf("expected duplicate matcher error, got: %v", err)
	}
}

func TestLoadSaveOverrideRigRole(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	cfg := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "echo gastown-crew"}}},
		},
	}

	if err := SaveOverride("gastown/crew", cfg); err != nil {
		t.Fatalf("SaveOverride failed: %v", err)
	}

	expectedPath := filepath.Join(tmpDir, ".gt", "hooks-overrides", "gastown__crew.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected override file at %s: %v", expectedPath, err)
	}

	loaded, err := LoadOverride("gastown/crew")
	if err != nil {
		t.Fatalf("LoadOverride failed: %v", err)
	}

	if len(loaded.SessionStart) != 1 {
		t.Fatalf("expected 1 SessionStart hook, got %d", len(loaded.SessionStart))
	}
}

func TestLoadMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	_, err := LoadBase()
	if err == nil {
		t.Error("expected error loading missing base config")
	}

	_, err = LoadOverride("crew")
	if err == nil {
		t.Error("expected error loading missing override config")
	}
}

func TestValidTarget(t *testing.T) {
	tests := []struct {
		target string
		valid  bool
	}{
		{"crew", true},
		{"witness", true},
		{"refinery", true},
		{"polecats", true},
		{"polecat", true},
		{"mayor", true},
		{"deacon", true},
		{"rig", false},
		{"gastown/rig", false},
		{"gastown/crew", true},
		{"beads/witness", true},
		{"sky/polecats", true},
		{"wyvern/refinery", true},
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
		{"mayor", "mayor", true},
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
	tests := []struct {
		target   string
		expected []string
	}{
		{"mayor", []string{"mayor"}},
		{"crew", []string{"crew"}},
		{"gastown/crew", []string{"crew", "gastown/crew"}},
		{"beads/witness", []string{"witness", "beads/witness"}},
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
	if len(cfg.Stop) == 0 {
		t.Error("DefaultBase should have Stop hooks")
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
	base := DefaultBase()
	override := &HooksConfig{}

	result := Merge(base, override)

	if !HooksEqual(base, result) {
		t.Error("empty override should not change base config")
	}
}

func TestComputeExpected(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	base := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "base-cmd"}}},
		},
	}
	if err := SaveBase(base); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	crewOverride := &HooksConfig{
		PreToolUse: []HookEntry{
			{Matcher: "Bash(git*)", Hooks: []Hook{{Type: "command", Command: "crew-guard"}}},
		},
	}
	if err := SaveOverride("crew", crewOverride); err != nil {
		t.Fatalf("SaveOverride crew failed: %v", err)
	}

	gcOverride := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gastown-crew-session"}}},
		},
	}
	if err := SaveOverride("gastown/crew", gcOverride); err != nil {
		t.Fatalf("SaveOverride gastown/crew failed: %v", err)
	}

	expected, err := ComputeExpected("gastown/crew")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}

	if len(expected.SessionStart) != 1 || expected.SessionStart[0].Hooks[0].Command != "gastown-crew-session" {
		t.Errorf("expected gastown/crew SessionStart, got %v", expected.SessionStart)
	}
	// On-disk base has no PreToolUse, so DefaultBase's 3 pr-workflow guards are
	// backfilled. The crew override adds Bash(git*), making 4 total.
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
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	// Simulate a stale hooks-base.json that was created before SessionStart existed.
	// It has Stop, PreCompact, UserPromptSubmit but no SessionStart.
	staleBase := &HooksConfig{
		Stop: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gt costs record"}}},
		},
		PreCompact: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gt prime --hook"}}},
		},
		UserPromptSubmit: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "gt mail check --inject"}}},
		},
	}
	if err := SaveBase(staleBase); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	// All targets should get SessionStart backfilled from DefaultBase
	for _, target := range []string{"mayor", "crew", "witness", "gastown/crew"} {
		expected, err := ComputeExpected(target)
		if err != nil {
			t.Fatalf("ComputeExpected(%s) failed: %v", target, err)
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
		} else if expected.Stop[0].Hooks[0].Command != "gt costs record" {
			t.Errorf("%s: on-disk Stop should take precedence, got %q", target, expected.Stop[0].Hooks[0].Command)
		}
	}
}

func TestComputeExpectedFailsOnDuplicateOverrideMatcher(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	if err := SaveBase(DefaultBase()); err != nil {
		t.Fatalf("SaveBase failed: %v", err)
	}

	overridePath := OverridePath("crew")
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

	_, err := ComputeExpected("crew")
	if err == nil {
		t.Fatal("expected ComputeExpected to fail on duplicate matcher")
	}
	if !strings.Contains(err.Error(), "duplicate matcher") {
		t.Fatalf("expected duplicate matcher error, got: %v", err)
	}
}

func TestComputeExpectedNoBase(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	// Mayor should get DefaultBase (no built-in overrides)
	expected, err := ComputeExpected("mayor")
	if err != nil {
		t.Fatalf("ComputeExpected failed: %v", err)
	}

	defaultBase := DefaultBase()
	if !HooksEqual(expected, defaultBase) {
		t.Error("expected DefaultBase for mayor when no configs exist")
	}

	// Crew should get DefaultBase + built-in crew override (PreCompact)
	crew, err := ComputeExpected("crew")
	if err != nil {
		t.Fatalf("ComputeExpected(crew) failed: %v", err)
	}
	// Crew has a built-in PreCompact override, so it won't equal bare DefaultBase
	if len(crew.PreCompact) == 0 {
		t.Error("expected crew to have PreCompact hook from DefaultOverrides")
	}
	// But it should still have the base SessionStart hooks
	if len(crew.SessionStart) != len(defaultBase.SessionStart) {
		t.Error("expected crew to inherit SessionStart from DefaultBase")
	}

	// Witness should get DefaultBase + built-in patrol-loop guard (gt-e47hxn,
	// gt-qqfy). Post gt-5ihs, every PreToolUse Bash guard shares the bare
	// "Bash" tool-name matcher — Claude Code's matcher only ever matches the
	// tool name. patrol-loop self-filters on tool_input.command (gt-qqfy),
	// so it needs no If — witness must have exactly one PreToolUse entry
	// (matcher "Bash") whose Hooks accumulate the base guards (pr-workflow
	// x3, dangerous-command x1) AND witness's own ungated patrol-loop hook.
	witness, err := ComputeExpected("witness")
	if err != nil {
		t.Fatalf("ComputeExpected(witness) failed: %v", err)
	}
	requireUngatedGuardCommand(t, "witness", witness, "tap guard patrol-loop")
	requireIfConditions(t, "witness", witness, prWorkflowIfConditions)
	if len(witness.SessionStart) != len(defaultBase.SessionStart) {
		t.Error("expected witness to inherit SessionStart from DefaultBase")
	}

	// Deacon should get DefaultBase + the same ungated patrol-loop guard;
	// the guard itself applies the anti-batch-loop checks (for/seq, while
	// true, while :) only when it detects the deacon role (gt-qqfy).
	deacon, err := ComputeExpected("deacon")
	if err != nil {
		t.Fatalf("ComputeExpected(deacon) failed: %v", err)
	}
	requireUngatedGuardCommand(t, "deacon", deacon, "tap guard patrol-loop")
	requireIfConditions(t, "deacon", deacon, prWorkflowIfConditions)
	if len(deacon.SessionStart) != len(defaultBase.SessionStart) {
		t.Error("expected deacon to inherit SessionStart from DefaultBase")
	}

	// Refinery should get DefaultBase + built-in patrol-loop guard (same as witness)
	refinery, err := ComputeExpected("refinery")
	if err != nil {
		t.Fatalf("ComputeExpected(refinery) failed: %v", err)
	}
	requireUngatedGuardCommand(t, "refinery", refinery, "tap guard patrol-loop")
	requireIfConditions(t, "refinery", refinery, prWorkflowIfConditions)
	if len(refinery.SessionStart) != len(defaultBase.SessionStart) {
		t.Error("expected refinery to inherit SessionStart from DefaultBase")
	}
}

var prWorkflowIfConditions = []string{
	"Bash(gh pr create*)",
	"Bash(git checkout -b*)",
	"Bash(git switch -c*)",
}

// requireIfConditions asserts that cfg's PreToolUse has a single bare-"Bash"
// matcher entry whose Hooks carry every want condition in their If field
// (gt-5ihs: matcher must be the tool name, never a pattern).
func requireIfConditions(t *testing.T, label string, cfg *HooksConfig, want []string) {
	t.Helper()
	entry, ok := findPreToolUse(cfg, "Bash")
	if !ok {
		t.Fatalf("%s: missing bare \"Bash\" PreToolUse matcher entry", label)
	}
	have := make(map[string]bool, len(entry.Hooks))
	for _, h := range entry.Hooks {
		have[h.If] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s: missing PreToolUse hook with If=%q under matcher \"Bash\"", label, w)
		}
	}
}

// requireUngatedGuardCommand asserts that cfg's PreToolUse has a bare-"Bash"
// matcher entry with a Hook whose Command contains commandSubstring and
// whose If field is empty — the shape a self-filtering guard (one that
// reads tool_input.command off stdin and inspects it directly, like
// dangerous-command and patrol-loop) needs, since it must run
// unconditionally rather than depend on an "if" permission-glob to decide
// when it's even invoked (gt-qqfy).
func requireUngatedGuardCommand(t *testing.T, label string, cfg *HooksConfig, commandSubstring string) {
	t.Helper()
	entry, ok := findPreToolUse(cfg, "Bash")
	if !ok {
		t.Fatalf("%s: missing bare \"Bash\" PreToolUse matcher entry", label)
	}
	for _, h := range entry.Hooks {
		if strings.Contains(h.Command, commandSubstring) && h.If == "" {
			return
		}
	}
	t.Errorf("%s: missing ungated (If=\"\") PreToolUse hook with Command containing %q under matcher \"Bash\", got: %+v", label, commandSubstring, entry.Hooks)
}

// TestComputeExpectedWitnessRigSpecific verifies patrol-formula-guard propagates
// to rig-specific witness targets (e.g., sky/witness) via the witness role default.
func TestComputeExpectedWitnessRigSpecific(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	// No on-disk overrides — all witnesses should still get patrol-formula-guard
	// from the built-in DefaultOverrides for "witness".
	skyWitness, err := ComputeExpected("sky/witness")
	if err != nil {
		t.Fatalf("ComputeExpected(sky/witness) failed: %v", err)
	}

	// Should have the ungated patrol-loop guard (self-filters, no If —
	// gt-qqfy) from DefaultOverrides["witness"], accumulated under the bare
	// "Bash" matcher entry.
	requireUngatedGuardCommand(t, "sky/witness", skyWitness, "tap guard patrol-loop")

	// Should also inherit base hooks (pr-workflow-guard, etc.)
	if len(skyWitness.SessionStart) == 0 {
		t.Error("sky/witness should inherit SessionStart from DefaultBase")
	}
	if len(skyWitness.UserPromptSubmit) != 0 {
		t.Error("sky/witness should disable UserPromptSubmit mail-check from DefaultBase")
	}
}

func TestComputeExpectedPatrolRolesDisableUserPromptMailCheck(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	for _, target := range []string{"witness", "refinery", "deacon", "boot", "sky/witness", "sky/refinery"} {
		t.Run(target, func(t *testing.T) {
			cfg, err := ComputeExpected(target)
			if err != nil {
				t.Fatalf("ComputeExpected(%s): %v", target, err)
			}
			if len(cfg.UserPromptSubmit) != 0 {
				t.Fatalf("%s should disable UserPromptSubmit mail-check, got %+v", target, cfg.UserPromptSubmit)
			}
		})
	}
}

func TestComputeExpectedDogGetsFormulaAllowlistGuard(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	dog, err := ComputeExpected("dog")
	if err != nil {
		t.Fatalf("ComputeExpected(dog): %v", err)
	}

	// Post gt-5ihs, dog's formula-allowlist guard and the base's pr-workflow
	// / dangerous-command guards all share the bare "Bash" matcher — Claude
	// Code's matcher only ever matches the tool name — and accumulate as
	// separate Hooks rather than one replacing the other (merge.go's
	// unionHooks). formula-allowlist has no If (it self-filters, like
	// dangerous-command), so it must appear among the hooks with an empty If.
	entry, ok := findPreToolUse(dog, "Bash")
	if !ok {
		t.Fatal("dog missing PreToolUse guard on bare Bash matcher")
	}
	hasAllowlist := false
	for _, h := range entry.Hooks {
		if strings.Contains(h.Command, "tap guard formula-allowlist") && h.If == "" {
			hasAllowlist = true
		}
	}
	if !hasAllowlist {
		t.Fatalf("dog Bash guard should include formula-allowlist (no If), got: %+v", entry.Hooks)
	}
	if len(dog.UserPromptSubmit) != 0 {
		t.Fatalf("dog should disable UserPromptSubmit mail-check, got %+v", dog.UserPromptSubmit)
	}
	// Base guards must still apply alongside the allowlist guard.
	requireIfConditions(t, "dog", dog, prWorkflowIfConditions)
	hasDangerousCommand := false
	for _, h := range entry.Hooks {
		if strings.Contains(h.Command, "tap guard dangerous-command") && h.If == "" {
			hasDangerousCommand = true
		}
	}
	if !hasDangerousCommand {
		t.Fatal("dog should inherit dangerous-command guard from DefaultBase")
	}

	// Other roles must not receive the dog guard. Mayor still has its own
	// bare "Bash" entry (base's pr-workflow/dangerous-command guards), but
	// it must not contain formula-allowlist.
	mayorCfg, err := ComputeExpected("mayor")
	if err != nil {
		t.Fatalf("ComputeExpected(mayor): %v", err)
	}
	mayorEntry, ok := findPreToolUse(mayorCfg, "Bash")
	if !ok {
		t.Fatal("mayor should still have the base Bash guard entry")
	}
	for _, h := range mayorEntry.Hooks {
		if strings.Contains(h.Command, "tap guard formula-allowlist") {
			t.Fatal("mayor must not receive the dog formula-allowlist guard")
		}
	}
}

func TestComputeExpectedBootBlocksRawTmuxSendKeys(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	boot, err := ComputeExpected("boot")
	if err != nil {
		t.Fatalf("ComputeExpected(boot): %v", err)
	}

	// Post gt-5ihs, the pattern lives in a Hook's If field under the bare
	// "Bash" matcher — Claude Code's matcher only ever matches the tool name.
	entry, ok := findPreToolUse(boot, "Bash")
	if !ok {
		t.Fatal("boot missing bare Bash PreToolUse entry")
	}
	var command string
	for _, h := range entry.Hooks {
		if h.If == "Bash(*tmux*send-keys*)" {
			command = h.Command
		}
	}
	if command == "" {
		t.Fatal("boot missing raw tmux send-keys guard (If=Bash(*tmux*send-keys*))")
	}
	for _, want := range []string{
		"BLOCKED: Boot must not use raw tmux send-keys",
		"gt nudge --mode=immediate deacon",
		"exit 2",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("boot raw tmux guard command missing %q: %s", want, command)
		}
	}
	if len(boot.UserPromptSubmit) != 0 {
		t.Fatalf("boot should still disable UserPromptSubmit mail-check, got %+v", boot.UserPromptSubmit)
	}

	mayor, err := ComputeExpected("mayor")
	if err != nil {
		t.Fatalf("ComputeExpected(mayor): %v", err)
	}
	mayorEntry, ok := findPreToolUse(mayor, "Bash")
	if !ok {
		t.Fatal("mayor should still have the base Bash guard entry")
	}
	for _, h := range mayorEntry.Hooks {
		if h.If == "Bash(*tmux*send-keys*)" {
			t.Fatal("mayor must not receive Boot's raw tmux send-keys guard")
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

func TestComputeExpectedPolecatsKeepUserPromptMailCheck(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	cfg, err := ComputeExpected("polecats")
	if err != nil {
		t.Fatalf("ComputeExpected(polecats): %v", err)
	}
	if len(cfg.UserPromptSubmit) == 0 {
		t.Fatal("polecats should retain UserPromptSubmit mail-check")
	}
}

// TestComputeExpectedBuiltinPlusOnDisk verifies that on-disk overrides layer
// on top of built-in defaults rather than replacing them.
func TestComputeExpectedBuiltinPlusOnDisk(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	// Save an on-disk mayor override that adds a custom SessionStart hook
	customOverride := &HooksConfig{
		SessionStart: []HookEntry{
			{Matcher: "", Hooks: []Hook{{Type: "command", Command: "custom-mayor-session"}}},
		},
	}
	if err := SaveOverride("mayor", customOverride); err != nil {
		t.Fatalf("SaveOverride failed: %v", err)
	}

	expected, err := ComputeExpected("mayor")
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

	if len(targets) < 4 {
		t.Errorf("expected at least 4 targets, got %d", len(targets))
		for _, tgt := range targets {
			t.Logf("  target: %s (key=%s)", tgt.DisplayKey(), tgt.Key)
		}
	}

	found := make(map[string]bool)
	for _, tgt := range targets {
		found[tgt.DisplayKey()] = true
	}

	for _, expected := range []string{"mayor", "deacon", "testrig/crew", "testrig/witness"} {
		if !found[expected] {
			t.Errorf("expected target %q not found", expected)
		}
	}
}

func TestDiscoverTargets_RoleNames(t *testing.T) {
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
		"mayor":         "mayor",
		"deacon":        "deacon",
		"rig1/crew":     "crew",
		"rig1/polecats": "polecat",
		"rig1/witness":  "witness",
		"rig1/refinery": "refinery",
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

func TestDiscoverTargets_ReturnsOnlyClaude(t *testing.T) {
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon"), 0755)

	// Create a rig with crew members that have both Claude and Gemini settings.
	// DiscoverTargets should only return Claude targets; non-Claude agents are
	// discovered via DiscoverRoleLocations instead.
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "crew", "alice"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "witness"), 0755)

	// Install gemini settings (should NOT appear in DiscoverTargets results)
	geminiDir := filepath.Join(tmpDir, "rig1", "crew", "alice", ".gemini")
	os.MkdirAll(geminiDir, 0755)
	os.WriteFile(filepath.Join(geminiDir, "settings.json"), []byte(`{"hooks":{}}`), 0644)

	targets, err := DiscoverTargets(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverTargets failed: %v", err)
	}

	for _, tgt := range targets {
		if tgt.Provider == "gemini" {
			t.Errorf("DiscoverTargets should not return gemini targets, got: %s", tgt.DisplayKey())
		}
	}
}

func TestDiscoverTargets_BootIncluded(t *testing.T) {
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon", "dogs", "boot"), 0755)

	targets, err := DiscoverTargets(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverTargets failed: %v", err)
	}

	found := false
	for _, tgt := range targets {
		if tgt.Key == "boot" {
			found = true
			wantPath := filepath.Join(tmpDir, "deacon", "dogs", "boot", ".claude", "settings.json")
			if tgt.Path != wantPath {
				t.Errorf("boot target Path = %q, want %q", tgt.Path, wantPath)
			}
			if tgt.Role != "boot" {
				t.Errorf("boot target Role = %q, want %q", tgt.Role, "boot")
			}
		}
	}
	if !found {
		t.Error("expected boot target when deacon/dogs/boot/ exists, not found")
	}
}

func TestDiscoverTargets_DogKennelsIncluded(t *testing.T) {
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon", "dogs", "boot"), 0755)
	// alpha is a real kennel (has .dog.json); scratch is not.
	os.MkdirAll(filepath.Join(tmpDir, "deacon", "dogs", "alpha"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "deacon", "dogs", "alpha", ".dog.json"), []byte(`{"name":"alpha","state":"idle"}`), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "deacon", "dogs", "scratch"), 0755)

	targets, err := DiscoverTargets(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverTargets failed: %v", err)
	}

	var dogTargets []Target
	for _, tgt := range targets {
		if tgt.Key == "dog" {
			dogTargets = append(dogTargets, tgt)
		}
	}
	if len(dogTargets) != 1 {
		t.Fatalf("expected exactly 1 dog target (alpha), got %d: %+v", len(dogTargets), dogTargets)
	}
	wantPath := filepath.Join(tmpDir, "deacon", "dogs", "alpha", ".claude", "settings.json")
	if dogTargets[0].Path != wantPath {
		t.Errorf("dog target Path = %q, want %q", dogTargets[0].Path, wantPath)
	}
	if dogTargets[0].Role != "dog" {
		t.Errorf("dog target Role = %q, want %q", dogTargets[0].Role, "dog")
	}
}

func TestDiscoverTargets_BootAbsent(t *testing.T) {
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

func TestDiscoverRoleLocations(t *testing.T) {
	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "deacon"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "crew", "alice"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "polecats", "toast"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "witness"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "rig1", "refinery"), 0755)

	locations, err := DiscoverRoleLocations(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverRoleLocations failed: %v", err)
	}

	// Build lookup by role+rig
	type key struct{ rig, role string }
	found := make(map[key]RoleLocation)
	for _, loc := range locations {
		found[key{loc.Rig, loc.Role}] = loc
	}

	expected := []struct {
		rig, role string
	}{
		{"", "mayor"},
		{"", "deacon"},
		{"rig1", "crew"},
		{"rig1", "polecat"},
		{"rig1", "witness"},
		{"rig1", "refinery"},
	}

	for _, e := range expected {
		loc, ok := found[key{e.rig, e.role}]
		if !ok {
			t.Errorf("expected location rig=%q role=%q not found", e.rig, e.role)
			continue
		}
		if loc.Dir == "" {
			t.Errorf("location rig=%q role=%q has empty Dir", e.rig, e.role)
		}
	}

	if len(locations) != len(expected) {
		t.Errorf("expected %d locations, got %d", len(expected), len(locations))
	}
}

func TestDiscoverRoleLocations_SkipsNonRigs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a directory that isn't a rig (no crew/witness/polecats/refinery subdirs)
	os.MkdirAll(filepath.Join(tmpDir, "notarig", "something"), 0755)
	// Hidden dirs should be skipped
	os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, ".hidden", "crew"), 0755)

	locations, err := DiscoverRoleLocations(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverRoleLocations failed: %v", err)
	}

	for _, loc := range locations {
		if loc.Rig == "notarig" || loc.Rig == ".beads" || loc.Rig == ".hidden" {
			t.Errorf("unexpected location found: rig=%q role=%q", loc.Rig, loc.Role)
		}
	}
}

func TestDiscoverWorktrees(t *testing.T) {
	tmpDir := t.TempDir()

	// Create worktree subdirectories
	os.MkdirAll(filepath.Join(tmpDir, "alice"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "bob"), 0755)
	// Hidden dirs should be skipped
	os.MkdirAll(filepath.Join(tmpDir, ".claude"), 0755)
	// Files should be skipped
	os.WriteFile(filepath.Join(tmpDir, "state.json"), []byte("{}"), 0644)

	dirs := DiscoverWorktrees(tmpDir)

	if len(dirs) != 2 {
		t.Errorf("expected 2 worktrees, got %d: %v", len(dirs), dirs)
	}

	names := make(map[string]bool)
	for _, d := range dirs {
		names[filepath.Base(d)] = true
	}
	if !names["alice"] || !names["bob"] {
		t.Errorf("expected alice and bob, got %v", names)
	}
	if names[".claude"] {
		t.Error("hidden directory should be skipped")
	}
}

func TestDiscoverWorktrees_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	dirs := DiscoverWorktrees(tmpDir)
	if len(dirs) != 0 {
		t.Errorf("expected 0 worktrees, got %d", len(dirs))
	}
}

func TestDiscoverWorktrees_PrefersNestedGitWorktreeRoots(t *testing.T) {
	tmpDir := t.TempDir()

	worktree := filepath.Join(tmpDir, "fury", "gastown")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "dust"), 0755); err != nil {
		t.Fatal(err)
	}

	dirs := DiscoverWorktrees(tmpDir)

	if len(dirs) != 2 {
		t.Fatalf("expected 2 worktrees, got %d: %v", len(dirs), dirs)
	}

	got := make(map[string]bool)
	for _, dir := range dirs {
		got[dir] = true
	}

	if !got[worktree] {
		t.Fatalf("expected nested worktree root %q, got %v", worktree, dirs)
	}
	if !got[filepath.Join(tmpDir, "dust")] {
		t.Fatalf("expected direct worktree fallback %q, got %v", filepath.Join(tmpDir, "dust"), dirs)
	}
}

func TestDiscoverWorktrees_InvalidDir(t *testing.T) {
	dirs := DiscoverWorktrees("/nonexistent/path/that/does/not/exist")
	if dirs != nil {
		t.Errorf("expected nil for invalid dir, got %v", dirs)
	}
}

func TestDiscoverRoleLocations_ReadError(t *testing.T) {
	_, err := DiscoverRoleLocations("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestTargetDisplayKey(t *testing.T) {
	tests := []struct {
		target   Target
		expected string
	}{
		{Target{Key: "mayor", Role: "mayor"}, "mayor"},
		{Target{Key: "gastown/crew", Rig: "gastown", Role: "crew"}, "gastown/crew"},
		{Target{Key: "beads/witness", Rig: "beads", Role: "witness"}, "beads/witness"},
	}

	for _, tt := range tests {
		if got := tt.target.DisplayKey(); got != tt.expected {
			t.Errorf("DisplayKey() = %q, want %q", got, tt.expected)
		}
	}
}

func TestGetSetEntries(t *testing.T) {
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
	setTestHome(t, tmpDir)
	for _, target := range []string{"mayor", "crew", "witness", "refinery", "deacon", "polecats", "dog", "boot", "gastown/crew", "gastown/witness"} {
		expected, err := ComputeExpected(target)
		if err != nil {
			t.Fatalf("ComputeExpected(%s) failed: %v", target, err)
		}
		assertNoParenMatchers(t, "ComputeExpected("+target+")", expected)
	}

	// The embedded settings-autonomous.json/settings-interactive.json
	// templates are what InstallForRole writes verbatim for a fresh
	// polecat/crew scaffold (installer.go writeTemplate) — bypassing
	// DefaultBase/DefaultOverrides entirely. A stale paren-style matcher
	// here reintroduces gt-5ihs for every newly onboarded agent even after
	// DefaultBase is fixed, so the templates need their own guard.
	for _, tmplFile := range []string{"templates/claude/settings-autonomous.json", "templates/claude/settings-interactive.json"} {
		data, err := templateFS.ReadFile(tmplFile)
		if err != nil {
			t.Fatalf("reading %s: %v", tmplFile, err)
		}
		var settings struct {
			Hooks HooksConfig `json:"hooks"`
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatalf("parsing %s: %v", tmplFile, err)
		}
		assertNoParenMatchers(t, tmplFile, &settings.Hooks)
	}
}
