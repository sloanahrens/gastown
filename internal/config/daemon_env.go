package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DaemonEnvPath returns the path to the town-level daemon environment file.
// KEY=VALUE lines in this file are carried into the supervised daemon's
// environment (launchd plist / systemd unit) by 'gt daemon enable-supervisor'.
// This exists because a manually-started daemon inherits host-specific env
// vars (e.g. SDKROOT, CMUX_CLAUDE_HOOKS_DISABLED) from the operator's shell,
// but a launchd/systemd-spawned daemon starts with a bare environment and
// would otherwise lose them.
func DaemonEnvPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "daemon.env")
}

// LoadDaemonEnv reads the town-level daemon environment file: one KEY=VALUE
// pair per line, blank lines and lines starting with '#' ignored. Returns an
// empty (non-nil) map, not an error, when the file does not exist — the file
// is optional.
func LoadDaemonEnv(townRoot string) (map[string]string, error) {
	path := DaemonEnvPath(townRoot)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path constructed internally from townRoot
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading daemon env file %s: %w", path, err)
	}

	env := map[string]string{}
	for i, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("daemon env file %s line %d: missing '=' in %q", path, i+1, rawLine)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("daemon env file %s line %d: empty key", path, i+1)
		}
		env[key] = strings.TrimSpace(value)
	}
	return env, nil
}
