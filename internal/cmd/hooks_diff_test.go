package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

func TestDiffHooksConfigsNoChanges(t *testing.T) {
	cfg := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "test"}}},
		},
	}

	lines := diffHooksConfigs(cfg, cfg)
	if len(lines) != 0 {
		t.Errorf("expected no diff lines for identical configs, got %d", len(lines))
		for _, l := range lines {
			t.Logf("  line: %q", l)
		}
	}
}

func TestDiffHooksConfigsAddedHookType(t *testing.T) {
	current := &hooks.HooksConfig{}
	expected := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "new-cmd"}}},
		},
	}

	lines := diffHooksConfigs(current, expected)
	if len(lines) == 0 {
		t.Error("expected diff lines for added hook type")
	}

	// Should contain an addition indicator
	found := false
	for _, l := range lines {
		if len(l) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected non-empty diff lines")
	}
}

func TestDiffHooksConfigsRemovedHookType(t *testing.T) {
	current := &hooks.HooksConfig{
		Stop: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "old-cmd"}}},
		},
	}
	expected := &hooks.HooksConfig{}

	lines := diffHooksConfigs(current, expected)
	if len(lines) == 0 {
		t.Error("expected diff lines for removed hook type")
	}
}

func TestDiffHooksConfigsModifiedCommand(t *testing.T) {
	current := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "old-cmd"}}},
		},
	}
	expected := &hooks.HooksConfig{
		SessionStart: []hooks.HookEntry{
			{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "new-cmd"}}},
		},
	}

	lines := diffHooksConfigs(current, expected)
	if len(lines) == 0 {
		t.Error("expected diff lines for modified command")
	}

	// Should have at least header + removal + addition
	if len(lines) < 3 {
		t.Errorf("expected at least 3 diff lines, got %d", len(lines))
	}
}

func TestDiffHookEntriesAddedMatcher(t *testing.T) {
	current := []hooks.HookEntry{}
	expected := []hooks.HookEntry{
		{Matcher: "Bash(git*)", Hooks: []hooks.Hook{{Type: "command", Command: "block"}}},
	}

	lines := diffHookEntries("PreToolUse", current, expected)
	if len(lines) == 0 {
		t.Error("expected diff lines for new matcher")
	}
}

func TestDiffHookEntriesRemovedMatcher(t *testing.T) {
	current := []hooks.HookEntry{
		{Matcher: "Bash(git*)", Hooks: []hooks.Hook{{Type: "command", Command: "block"}}},
	}
	expected := []hooks.HookEntry{}

	lines := diffHookEntries("PreToolUse", current, expected)
	if len(lines) == 0 {
		t.Error("expected diff lines for removed matcher")
	}
}

func TestTruncateCommand(t *testing.T) {
	short := "echo hello"
	if got := truncateCommand(short); got != short {
		t.Errorf("short command should not be truncated: got %q", got)
	}

	long := "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gt prime --hook && some-other-really-long-command-that-goes-on"
	if got := truncateCommand(long); len(got) > 80 {
		t.Errorf("truncated command should be ≤80 chars, got %d", len(got))
	}
	if got := truncateCommand(long); len(got) < 77 { // 37 + 3 + 37
		t.Errorf("truncated command too short: %d chars", len(got))
	}
}

func TestMatcherDisplay(t *testing.T) {
	if got := matcherDisplay(""); got != `"" (all)` {
		t.Errorf("empty matcher: got %q", got)
	}

	if got := matcherDisplay("Bash(git*)"); got != `"Bash(git*)"` {
		t.Errorf("specific matcher: got %q", got)
	}
}

// TestDiffCommandsDetectsIfOnlyDrift pins finding 6 (gt-wisp-db27): three
// pr-workflow hooks legitimately share an identical Command on matcher
// "Bash" post-gt-5ihs, differing only by If. Before this fix, diffCommands
// compared only Command, so a typo'd If pattern in an on-disk override was
// invisible to gt hooks diff.
func TestDiffCommandsDetectsIfOnlyDrift(t *testing.T) {
	current := hooks.HookEntry{
		Matcher: "Bash",
		Hooks: []hooks.Hook{
			{Type: "command", Command: "gt tap guard pr-workflow", If: "Bash(gt checkout -b*)"}, // typo: gt vs git
		},
	}
	expected := hooks.HookEntry{
		Matcher: "Bash",
		Hooks: []hooks.Hook{
			{Type: "command", Command: "gt tap guard pr-workflow", If: "Bash(git checkout -b*)"},
		},
	}

	lines := diffCommands("PreToolUse", "Bash", current, expected)
	if len(lines) == 0 {
		t.Fatal("expected diff lines for If-only drift, got none")
	}

	joined := ""
	for _, l := range lines {
		joined += l
	}
	if !strings.Contains(joined, "git checkout -b") || !strings.Contains(joined, "gt checkout -b") {
		t.Errorf("expected diff to show both If values, got: %s", joined)
	}
}

// TestHooksFingerprintDistinguishesByIf pins that two hooks sharing
// Type+Command but differing only in If (e.g. three pr-workflow entries on
// matcher "Bash") produce different fingerprints — otherwise gt hooks init
// would deduplicate them into one (finding 6, gt-wisp-db27).
func TestHooksFingerprintDistinguishesByIf(t *testing.T) {
	a := []hooks.Hook{{Type: "command", Command: "gt tap guard pr-workflow", If: "Bash(gh pr create*)"}}
	b := []hooks.Hook{{Type: "command", Command: "gt tap guard pr-workflow", If: "Bash(git checkout -b*)"}}

	if hooksFingerprint(a) == hooksFingerprint(b) {
		t.Errorf("expected distinct fingerprints for hooks differing only by If, got equal: %q", hooksFingerprint(a))
	}
}

// TestHooksListEqualDistinguishesByIf mirrors TestHooksFingerprintDistinguishesByIf
// for hooksListEqual, used by computeDiff.
func TestHooksListEqualDistinguishesByIf(t *testing.T) {
	a := []hooks.Hook{{Type: "command", Command: "gt tap guard pr-workflow", If: "Bash(gh pr create*)"}}
	b := []hooks.Hook{{Type: "command", Command: "gt tap guard pr-workflow", If: "Bash(git checkout -b*)"}}

	if hooksListEqual(a, b) {
		t.Error("expected hooksListEqual to return false for hooks differing only by If")
	}
	if !hooksListEqual(a, a) {
		t.Error("expected hooksListEqual to return true for identical hook lists")
	}
}

func TestIndexByMatcher(t *testing.T) {
	entries := []hooks.HookEntry{
		{Matcher: "", Hooks: []hooks.Hook{{Type: "command", Command: "all"}}},
		{Matcher: "Bash(git*)", Hooks: []hooks.Hook{{Type: "command", Command: "git"}}},
	}

	m := indexByMatcher(entries)

	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if m[""].Hooks[0].Command != "all" {
		t.Error("empty matcher entry wrong")
	}
	if m["Bash(git*)"].Hooks[0].Command != "git" {
		t.Error("specific matcher entry wrong")
	}
}
