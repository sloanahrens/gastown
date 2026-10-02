package config

import (
	"errors"
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
			return nil, &ParseError{Path: path, Line: i + 1, Err: fmt.Errorf("missing '=' in %q", rawLine)}
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, &ParseError{Path: path, Line: i + 1, Err: errors.New("empty key")}
		}
		env[key] = strings.TrimSpace(value)
	}
	return env, nil
}

// ResolveDoltPassword resolves d's password setting for the town at townRoot
// to the value the Dolt server and its clients authenticate with. Unset or
// empty yields "" — the town takes no password, which is the normal state.
//
// A ${VAR} reference is read from the town's settings/daemon.env, and a name
// daemon.env does not define falls back to this process's environment, the
// rule resolveSpawnEnv applies to agent env: the token can live in daemon.env
// or be handed to the process at spawn, and never in settings/config.json. A
// literal value is returned as it stands; FindLiteralSecrets reports it so
// 'gt config secrets migrate' can move it to daemon.env.
//
// A settings/daemon.env that exists but does not parse contributes no entries,
// leaving the process-environment fallback in place — Load refuses such a town
// long before a server starts (townconfig.loadDaemonEnv).
func ResolveDoltPassword(townRoot string, d *DoltThresholds) string {
	return resolveDoltPassword(townRoot, d, processHost.getenv)
}

// resolveDoltPassword is ResolveDoltPassword reading undefined names through
// getenv.
func resolveDoltPassword(townRoot string, d *DoltThresholds, getenv func(string) string) string {
	value, ok := d.PasswordSetting()
	if !ok {
		return ""
	}
	fileEnv := map[string]string{}
	if townRoot != "" {
		if loaded, err := LoadDaemonEnv(townRoot); err == nil {
			fileEnv = loaded
		}
	}
	return expandEnvRefs(value, func(name string) string {
		if v, ok := fileEnv[name]; ok {
			return v
		}
		return getenv(name)
	})
}
