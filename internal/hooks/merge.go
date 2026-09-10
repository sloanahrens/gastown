package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MergeHooks merges a base config with applicable overrides for a target.
// It does NOT incorporate built-in defaults from DefaultOverrides(); callers
// that need the full production merge should use ComputeExpected() instead.
//
// Merge rules:
//  1. Start with base hooks
//  2. Apply role override (crew, witness, refinery, polecats, mayor, deacon)
//  3. Apply rig+role override if exists (gastown/crew, beads/witness, etc.)
//
// For each hook type (SessionStart, PreToolUse, etc.):
//   - Hooks with same matcher: override replaces base entirely
//   - Hooks with different matcher: both are included
//   - Override with empty hook list for a matcher: removes that hook (explicit disable)
func MergeHooks(base *HooksConfig, overrides map[string]*HooksConfig, target string) *HooksConfig {
	if base == nil {
		base = &HooksConfig{}
	}

	result := cloneConfig(base)

	// Apply overrides in order of specificity
	for _, key := range GetApplicableOverrides(target) {
		override, ok := overrides[key]
		if !ok || override == nil {
			continue
		}
		result = applyOverride(result, override)
	}

	return result
}

// LoadAllOverrides loads all override files from the overrides directory.
func LoadAllOverrides() (map[string]*HooksConfig, error) {
	overrides := make(map[string]*HooksConfig)

	dir := OverridesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return overrides, nil // No overrides dir is fine
		}
		return nil, fmt.Errorf("reading overrides directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		// Convert filename back to target key (gastown__crew.json -> gastown/crew)
		key := strings.TrimSuffix(name, ".json")
		key = strings.ReplaceAll(key, "__", "/")

		cfg, err := loadConfig(filepath.Join(dir, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping invalid hooks override %s: %v\n", name, err)
			continue
		}
		overrides[key] = cfg
	}

	return overrides, nil
}

// applyOverride merges an override onto a result config.
func applyOverride(result, override *HooksConfig) *HooksConfig {
	result.PreToolUse = mergeEntries(result.PreToolUse, override.PreToolUse)
	result.PostToolUse = mergeEntries(result.PostToolUse, override.PostToolUse)
	result.SessionStart = mergeEntries(result.SessionStart, override.SessionStart)
	result.Stop = mergeEntries(result.Stop, override.Stop)
	result.PreCompact = mergeEntries(result.PreCompact, override.PreCompact)
	result.UserPromptSubmit = mergeEntries(result.UserPromptSubmit, override.UserPromptSubmit)
	result.WorktreeCreate = mergeEntries(result.WorktreeCreate, override.WorktreeCreate)
	result.WorktreeRemove = mergeEntries(result.WorktreeRemove, override.WorktreeRemove)
	return result
}

// mergeEntries merges override entries into base entries.
//
// Different-matcher entries are appended. An override entry with an empty
// Hooks list removes that matcher from the result (explicit disable),
// regardless of matcher kind.
//
// For a permission-pattern matcher (contains "(", e.g. "Bash(git push*)"),
// a same-matcher override entry replaces the base entry entirely — this is
// the original, still-tested behavior for on-disk customization of a single
// named rule.
//
// For a bare tool-name matcher (no parentheses, e.g. "Bash", "Edit|Write"),
// a same-matcher override entry instead UNIONS its Hooks into the base
// entry's Hooks list (gt-5ihs). Bare tool-name matchers are how every
// PreToolUse guard must route post-fix — Claude Code's matcher only ever
// matches the tool name, so pr-workflow, dangerous-command, and each role's
// patrol-formula-guard all legitimately share matcher "Bash", discriminated
// by each Hook's If field (or by self-inspecting the command) rather than by
// matcher. Whole-entry replace would silently drop one layer's guards
// whenever another layer also targets "Bash". Union is keyed by (Command,
// If) so re-merging is idempotent: a hook already present is replaced in
// place, a genuinely new one is appended.
func mergeEntries(base, override []HookEntry) []HookEntry {
	if len(override) == 0 {
		return base
	}

	// Build a map of matchers to override entries for quick lookup
	overrideByMatcher := make(map[string]HookEntry)
	for _, entry := range override {
		overrideByMatcher[entry.Matcher] = entry
	}

	// Process base entries: replace, union, or keep
	var result []HookEntry
	handledMatchers := make(map[string]bool)

	for _, baseEntry := range base {
		ovEntry, found := overrideByMatcher[baseEntry.Matcher]
		if !found {
			result = append(result, baseEntry)
			continue
		}
		handledMatchers[baseEntry.Matcher] = true

		// Empty hooks list means explicit disable (remove), regardless of
		// matcher kind.
		if len(ovEntry.Hooks) == 0 {
			continue
		}

		if isBareToolMatcher(baseEntry.Matcher) {
			result = append(result, HookEntry{
				Matcher: baseEntry.Matcher,
				Hooks:   unionHooks(baseEntry.Hooks, ovEntry.Hooks),
			})
		} else {
			result = append(result, ovEntry)
		}
	}

	// Add override entries with new matchers (not in base)
	for _, ovEntry := range override {
		if !handledMatchers[ovEntry.Matcher] {
			if len(ovEntry.Hooks) > 0 {
				result = append(result, ovEntry)
			}
		}
	}

	return result
}

// isBareToolMatcher reports whether m is a plain Claude Code tool-name
// matcher (e.g. "Bash", "Edit|Write") rather than a permission-rule pattern
// like "Bash(git push*)". Claude Code tool names never contain parentheses;
// an empty matcher ("" — used by non-PreToolUse event types like Stop) is
// deliberately excluded so those event types keep whole-entry-replace
// semantics.
func isBareToolMatcher(m string) bool {
	return m != "" && !strings.Contains(m, "(")
}

// unionHooks merges override hooks into base hooks, keyed by (Command, If)
// so repeated merges stay idempotent: a hook already present (same command
// and condition) is replaced in place rather than duplicated, and a
// genuinely new hook is appended.
func unionHooks(base, override []Hook) []Hook {
	result := make([]Hook, len(base))
	copy(result, base)

	index := make(map[string]int, len(result))
	for i, h := range result {
		index[hookKey(h)] = i
	}

	for _, h := range override {
		key := hookKey(h)
		if i, ok := index[key]; ok {
			result[i] = h
			continue
		}
		index[key] = len(result)
		result = append(result, h)
	}

	return result
}

// hookKey returns the identity key used by unionHooks.
func hookKey(h Hook) string {
	return h.Command + "\x00" + h.If
}

// cloneConfig creates a deep copy of a HooksConfig.
func cloneConfig(cfg *HooksConfig) *HooksConfig {
	return &HooksConfig{
		PreToolUse:       cloneEntries(cfg.PreToolUse),
		PostToolUse:      cloneEntries(cfg.PostToolUse),
		SessionStart:     cloneEntries(cfg.SessionStart),
		Stop:             cloneEntries(cfg.Stop),
		PreCompact:       cloneEntries(cfg.PreCompact),
		UserPromptSubmit: cloneEntries(cfg.UserPromptSubmit),
		WorktreeCreate:   cloneEntries(cfg.WorktreeCreate),
		WorktreeRemove:   cloneEntries(cfg.WorktreeRemove),
	}
}

func cloneEntries(entries []HookEntry) []HookEntry {
	if entries == nil {
		return nil
	}
	result := make([]HookEntry, len(entries))
	for i, e := range entries {
		result[i] = HookEntry{
			Matcher: e.Matcher,
			Hooks:   make([]Hook, len(e.Hooks)),
		}
		copy(result[i].Hooks, e.Hooks)
	}
	return result
}
