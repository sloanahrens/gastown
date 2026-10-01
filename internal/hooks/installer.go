// Package hooks installs and syncs the Claude Code settings.json that carries
// Gas Town's hooks. Claude Code is the only agent runtime (D4), so every role
// gets the same file shape: the embedded templates for interactive roles, and
// the JSON merge path (base + overrides) for boot and polecats.
package hooks

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/hookutil"
)

//go:embed templates/*
var templateFS embed.FS

// settingsFile is the file Claude Code reads its hooks from, under .claude/
// in the settings directory (passed to Claude via --settings for crew and
// polecats).
const settingsFile = "settings.json"

// InstallForRole provisions settingsDir/.claude/settings.json for a role.
// It creates the file if it does not exist, or overwrites if the existing file
// contains known stale patterns (e.g., legacy "export PATH=" format). Otherwise
// it does not overwrite — this is the safe path for session startup, where
// settings.json may have been customized by syncTarget (base + role overrides
// merge) and must not be clobbered.
//
// Parameters:
//   - settingsDir: the gastown-managed parent (passed to Claude via --settings
//     for crew and polecats; the working directory for town-level roles).
//   - role: the Gas Town role (e.g., "polecat", "crew", "mayor").
//
// Template resolution: templates/claude/settings-autonomous.json for
// autonomous roles, settings-interactive.json for the rest.
//
// For boot/polecat, install goes through the JSON merge path
// (SyncManagedClaudeSettings) and fails closed: an unparseable hooks-base.json,
// hooks-override file, or existing settings.json aborts the install with an
// error naming the file, rather than silently falling back to a template that
// may be missing hooks a rig-scoped override added. See gt-8stz.
func InstallForRole(settingsDir, role string) error {
	return envConfigHome().installForRole(settingsDir, role)
}

func (h configHome) installForRole(settingsDir, role string) error {
	targetPath := filepath.Join(settingsDir, ".claude", settingsFile)
	// Boot and polecat settings are managed through the JSON merge path
	// so their role overrides are kept in sync rather than frozen at first
	// install; the needsUpgrade heuristic below has no way to detect a hook
	// type added in code (gt-8stz REOPENED).
	if role == "boot" || role == "polecat" {
		// DefaultOverrides keys the polecat entry "polecats" (plural); role is
		// singular everywhere else. ComputeExpected resolves Key literally, so
		// this must be normalized or the merge silently drops the override.
		key := role
		if role == "polecat" {
			key = "polecats"
			// Polecat settings are shared per rig (config.RoleSettingsDir joins
			// rigPath + "polecats"), and DiscoverTargets/GetApplicableOverrides
			// manage the file under the rig-scoped key "<rig>/polecats" so that
			// ~/.gt/hooks-overrides/<rig>__polecats.json is applied. Using the
			// bare "polecats" key here skips that override, so every polecat
			// spawn (this call site) would silently drop rig-scoped hooks the
			// next sync had put in, then gt hooks sync would put them back —
			// the file would flip on every spawn/sync cycle.
			if rig := filepath.Base(filepath.Dir(settingsDir)); rig != "." && rig != string(filepath.Separator) && rig != "" {
				key = rig + "/polecats"
			}
		}
		if _, err := h.syncManagedClaudeSettings(Target{
			Path: targetPath,
			Key:  key,
			Role: role,
		}, false); err != nil {
			return fmt.Errorf("installing managed claude settings for role %q at %s: %w", role, targetPath, err)
		}
		return nil
	}

	if existing, err := os.ReadFile(targetPath); err == nil {
		if !needsUpgrade(existing) {
			return nil // File exists and is current — don't overwrite
		}
		// Stale file detected — fall through to overwrite with current template
	}

	return writeTemplate(role, targetPath)
}

// needsUpgrade returns true if an existing hooks file contains stale patterns
// that should be replaced by the current template. This allows the installer
// to auto-upgrade hooks from earlier versions without requiring manual intervention.
func needsUpgrade(content []byte) bool {
	// Stale pattern: export PATH=... && gt — replaced by {{GT_BIN}} in current templates.
	if bytes.Contains(content, []byte(`export PATH=`)) {
		return true
	}
	// Stale pattern: a PreToolUse matcher written as a permission-rule
	// pattern (e.g. "Bash(gh pr create*)") instead of a bare tool name —
	// Claude Code's hooks[].matcher only ever matches the tool name, so a
	// pattern-style matcher never fires (gt-5ihs). Checked structurally
	// (not a raw substring scan) so it only trips on an actual PreToolUse
	// matcher field, not on unrelated JSON containing "Bash(" elsewhere.
	if hasParenPreToolUseMatcher(content) {
		return true
	}
	// Stale pattern: a PreToolUse matcher naming Bash but not Monitor, the
	// shape the shipped settings-autonomous.json/settings-interactive.json
	// templates wrote. The paren check above cannot see it ("Bash" has no
	// "("), so such a file was judged current and kept every self-filtering
	// shell guard invisible to Monitor (gt-ly9c4).
	if hasBareBashPreToolUseMatcher(content) {
		return true
	}
	return false
}

// preToolUseMatchers returns the matcher strings of a settings.json's
// PreToolUse entries. Non-JSON or non-settings content yields nil, so every
// caller treats unreadable content as not-stale.
func preToolUseMatchers(content []byte) []string {
	var settings struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(content, &settings); err != nil {
		return nil
	}
	matchers := make([]string, 0, len(settings.Hooks.PreToolUse))
	for _, entry := range settings.Hooks.PreToolUse {
		matchers = append(matchers, entry.Matcher)
	}
	return matchers
}

// hasParenPreToolUseMatcher reports whether content is a settings.json whose
// PreToolUse section has a matcher containing "(" — a dead permission-rule
// pattern rather than a tool name (gt-5ihs).
func hasParenPreToolUseMatcher(content []byte) bool {
	for _, matcher := range preToolUseMatchers(content) {
		if strings.Contains(matcher, "(") {
			return true
		}
	}
	return false
}

// hasBareBashPreToolUseMatcher reports whether content is a settings.json
// whose PreToolUse section has a matcher naming the Bash tool without naming
// Monitor, which leaves a Monitor-run command (same tool_input.command as
// Bash) outside every guard mounted on that entry (gt-vx2mm, gt-ly9c4).
//
// Matched per "|"-separated token rather than by string equality so a
// compound matcher that omits Monitor ("Bash|Edit") is caught too, while a
// different tool name such as "BashOutput" is not mistaken for Bash.
func hasBareBashPreToolUseMatcher(content []byte) bool {
	for _, matcher := range preToolUseMatchers(content) {
		hasBash, hasMonitor := false, false
		for _, name := range strings.Split(matcher, "|") {
			switch strings.TrimSpace(name) {
			case "Bash":
				hasBash = true
			case "Monitor":
				hasMonitor = true
			}
		}
		if hasBash && !hasMonitor {
			return true
		}
	}
	return false
}

// SyncResult describes what a settings sync did.
type SyncResult int

const (
	SyncUnchanged SyncResult = iota // File already matches template
	SyncCreated                     // File did not exist, created
	SyncUpdated                     // File existed but content differed, updated
)

// renderTemplate returns the role's settings template with {{GT_BIN}}
// resolved to the gt binary.
func renderTemplate(role string) ([]byte, error) {
	name := "settings-interactive.json"
	if hookutil.IsAutonomousRole(role) {
		name = "settings-autonomous.json"
	}
	content, err := templateFS.ReadFile("templates/claude/" + name)
	if err != nil {
		return nil, fmt.Errorf("reading settings template %s: %w", name, err)
	}

	if bytes.Contains(content, []byte("{{GT_BIN}}")) {
		// JSON-encode the path so Windows backslashes are properly escaped.
		// json.Marshal produces `"C:\\path\\gt.exe"` (with quotes); strip the quotes.
		gtBin := resolveGTBinary()
		gtBinBytes := []byte(gtBin)
		if encoded, err := json.Marshal(gtBin); err == nil {
			gtBinBytes = encoded[1 : len(encoded)-1]
		}
		content = bytes.ReplaceAll(content, []byte("{{GT_BIN}}"), gtBinBytes)
	}

	return content, nil
}

// writeTemplate renders the role's template and writes it to targetPath.
func writeTemplate(role, targetPath string) error {
	content, err := renderTemplate(role)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("creating hooks directory: %w", err)
	}

	// Atomic write (temp + rename) prevents concurrent polecat spawns from
	// interleaving truncates+writes into a partial JSON file that Claude
	// rejects at startup. See gh#3500.
	if err := atomicfile.WriteFile(targetPath, content, 0600); err != nil {
		return fmt.Errorf("writing hooks file: %w", err)
	}

	return nil
}

// resolveGTBinary returns the absolute path to the gt binary.
// Tries os.Executable() first (most reliable when running as gt), then
// falls back to exec.LookPath for PATH-based discovery. If both fail,
// returns "gt" and hopes the runtime PATH has it.
func resolveGTBinary() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	if path, err := exec.LookPath("gt"); err == nil {
		return path
	}
	return "gt"
}
