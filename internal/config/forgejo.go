package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ForgejoDir returns the Forgejo compose stack directory for the town at
// townRoot: settings/config.json's forgejo.dir, else ~/forgejo. A leading "~/"
// is expanded.
func ForgejoDir(townRoot string) (string, error) {
	return ForgejoDirFrom(townRoot, os.UserHomeDir)
}

// ForgejoDirFrom is ForgejoDir with the home directory named by userHomeDir, so
// a caller with a home of its own — a test, above all — does not read the
// process environment.
func ForgejoDirFrom(townRoot string, userHomeDir func() (string, error)) (string, error) {
	settings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		return "", err
	}
	dir := ""
	if settings != nil && settings.ForgejoStack != nil {
		dir = strings.TrimSpace(settings.ForgejoStack.Dir)
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding forgejo.dir: %w", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if dir != "" {
		return dir, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the home directory: %w", err)
	}
	return filepath.Join(home, "forgejo"), nil
}
