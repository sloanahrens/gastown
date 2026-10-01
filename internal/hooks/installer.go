// Package hooks installs and syncs the Claude Code settings.json that carries
// Gas Town's hooks. Claude Code is the only agent runtime (D4), so every role's
// settings go through one path: the JSON merge of the base config and its
// overrides that gt hooks sync writes.
package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// InstallForRole syncs settingsDir/.claude/settings.json for a role to the
// managed hooks: what gt hooks sync writes, keyed as gt hooks sync keys it
// (ManagedTargetKey), keeping non-hook settings fields. Each session start so
// picks up guards added since, without an operator running gt hooks sync
// (gt-8stz, gt-4k3fj.8.3).
//
// Parameters:
//   - settingsDir: the gastown-managed parent (passed to Claude via --settings
//     for crew and polecats; the working directory for town-level roles).
//   - role: the Gas Town role (e.g., "polecat", "crew", "mayor").
//
// It fails closed: an unparseable hooks-base.json, hooks-override file, or
// existing settings.json aborts the install with an error naming the file,
// rather than silently leaving a file that may be missing hooks a rig-scoped
// override added. See gt-8stz.
func InstallForRole(settingsDir, role string) error {
	return envConfigHome().installForRole(settingsDir, role)
}

func (h configHome) installForRole(settingsDir, role string) error {
	targetPath := filepath.Join(settingsDir, ".claude", "settings.json")
	if _, err := h.syncManagedClaudeSettings(Target{
		Path: targetPath,
		Key:  ManagedTargetKey(role, settingsDir),
		Role: role,
	}, false); err != nil {
		return fmt.Errorf("installing managed claude settings for role %q at %s: %w", role, targetPath, err)
	}
	return nil
}

// SyncResult describes what a settings sync did.
type SyncResult int

const (
	SyncUnchanged SyncResult = iota // File already matches template
	SyncCreated                     // File did not exist, created
	SyncUpdated                     // File existed but content differed, updated
)

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
