package hooks

import (
	"fmt"
	"os"
	"path/filepath"
)

// ManagedTargetKey is the override key gt hooks sync manages the settings file
// in settingsDir under for a session of role (DiscoverTargets): crew and
// polecats share one file per rig, keyed "<rig>/crew" and "<rig>/polecats" so
// ~/.gt/hooks-overrides/<rig>__<role>.json applies, and every other role is
// keyed by its name.
func ManagedTargetKey(role, settingsDir string) string {
	key := role
	switch role {
	case "polecat":
		// DefaultOverrides keys the polecat entry "polecats" (plural); role
		// is singular everywhere else.
		key = "polecats"
	case "crew":
	default:
		return key
	}
	// settingsDir is <rig>/polecats or <rig>/crew (config.RoleSettingsDir).
	if rig := filepath.Base(filepath.Dir(settingsDir)); rig != "." && rig != string(filepath.Separator) && rig != "" {
		key = rig + "/" + key
	}
	return key
}

// CheckManagedClaudeSettings is nil when the settings file at target.Path
// exists, parses and carries exactly the managed hooks gt hooks sync writes
// for target.Key; otherwise it says why a session loading it would run
// without the managed guards (gt-be0z).
func CheckManagedClaudeSettings(target Target) error {
	return envConfigHome().checkManagedClaudeSettings(target)
}

func (h configHome) checkManagedClaudeSettings(target Target) error {
	if _, err := os.Stat(target.Path); err != nil {
		return fmt.Errorf("settings file: %w", err)
	}
	res, err := h.syncManagedClaudeSettings(target, true)
	if err != nil {
		return err
	}
	if res != SyncUnchanged {
		return fmt.Errorf("%s does not carry the managed hooks for %s", target.Path, target.Key)
	}
	return nil
}

// CheckManagedClaudeSettings is the package-level CheckManagedClaudeSettings
// against this Home.
func (h Home) CheckManagedClaudeSettings(target Target) error {
	return h.h.checkManagedClaudeSettings(target)
}

// InstallForRole is the package-level InstallForRole against this Home.
func (h Home) InstallForRole(provider, settingsDir, workDir, role, hooksDir, hooksFile, command string, useSettingsDir bool) error {
	return h.h.installForRole(provider, settingsDir, workDir, role, hooksDir, hooksFile, command, useSettingsDir)
}
