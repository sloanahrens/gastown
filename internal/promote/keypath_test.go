package promote

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// keyPathStat answers stat from a fixed set of existing paths; any other path
// is missing. The paths are strings only, so no real file is read.
func keyPathStat(existing ...string) func(string) error {
	have := map[string]bool{}
	for _, p := range existing {
		have[p] = true
	}
	return func(p string) error {
		if have[p] {
			return nil
		}
		return &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
}

// keyPathGetenv answers XDG_CONFIG_HOME with configHome and every other name
// with "".
func keyPathGetenv(configHome string) func(string) string {
	return func(name string) string {
		if name == "XDG_CONFIG_HOME" {
			return configHome
		}
		return ""
	}
}

// TestKeyPathResolvesToConfigHomeFile drives every case ResolveKeyFile must
// answer: the configured path is returned unchanged unless it is missing and
// the config-home copy exists. Each configured path is unique to its case
// because the once-per-process log is keyed on it.
func TestKeyPathResolvesToConfigHomeFile(t *testing.T) {
	t.Parallel()
	const configHome = "/run/gt/xdg"
	for _, tc := range []struct {
		name       string
		path       string
		configHome string
		stat       func(string) error
		wantPath   string
		wantLogs   int
	}{
		{
			name:       "configured path exists",
			path:       filepath.Join("/mac", "exists", "promote-rig.key"),
			configHome: configHome,
			stat:       keyPathStat(filepath.Join("/mac", "exists", "promote-rig.key")),
			wantPath:   filepath.Join("/mac", "exists", "promote-rig.key"),
		},
		{
			name:       "empty path stays empty",
			path:       "",
			configHome: configHome,
			stat:       keyPathStat(filepath.Join(configHome, "gt", "promote-rig.key")),
			wantPath:   "",
		},
		{
			name:       "XDG_CONFIG_HOME unset",
			path:       filepath.Join("/mac", "unset", "promote-rig.key"),
			configHome: "",
			// A relative gt/ file must not be taken for the config home copy.
			stat:     keyPathStat(filepath.Join("gt", "promote-rig.key")),
			wantPath: filepath.Join("/mac", "unset", "promote-rig.key"),
		},
		{
			name:       "neither the path nor the config-home file exists",
			path:       filepath.Join("/mac", "missing", "promote-rig.key"),
			configHome: configHome,
			stat:       keyPathStat(),
			wantPath:   filepath.Join("/mac", "missing", "promote-rig.key"),
		},
		{
			name:       "configured path exists but cannot be stat'ed",
			path:       filepath.Join("/mac", "permission", "promote-rig.key"),
			configHome: configHome,
			stat: func(p string) error {
				if p == filepath.Join("/mac", "permission", "promote-rig.key") {
					return &fs.PathError{Op: "stat", Path: p, Err: fs.ErrPermission}
				}
				return keyPathStat(filepath.Join(configHome, "gt", "promote-rig.key"))(p)
			},
			wantPath: filepath.Join("/mac", "permission", "promote-rig.key"),
		},
		{
			name:       "configured path missing and config-home file exists",
			path:       filepath.Join("/mac", "substituted", "promote-rig.key"),
			configHome: configHome,
			stat:       keyPathStat(filepath.Join(configHome, "gt", "promote-rig.key")),
			wantPath:   filepath.Join(configHome, "gt", "promote-rig.key"),
			wantLogs:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			got := ResolveKeyFile(tc.path, keyPathGetenv(tc.configHome), tc.stat, logf)
			if got != tc.wantPath {
				t.Errorf("ResolveKeyFile(%q) = %q, want %q", tc.path, got, tc.wantPath)
			}
			if len(logs) != tc.wantLogs {
				t.Errorf("logged %d lines %q, want %d", len(logs), logs, tc.wantLogs)
			}
		})
	}
}

// TestKeyPathLogsTheSubstitutionOnceForRepeatedCalls: a rig's promotion
// resolves its key on every verdict and sweep tick, so the substitution logs
// once per configured path, names both paths, and never a key's contents.
func TestKeyPathLogsTheSubstitutionOnceForRepeatedCalls(t *testing.T) {
	t.Parallel()
	const configHome = "/run/gt/xdg"
	configured := filepath.Join("/mac", "once", "promote-rig.key")
	resolved := filepath.Join(configHome, "gt", "promote-rig.key")
	stat := keyPathStat(resolved)
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	for range 2 {
		if got := ResolveKeyFile(configured, keyPathGetenv(configHome), stat, logf); got != resolved {
			t.Fatalf("ResolveKeyFile(%q) = %q, want %q", configured, got, resolved)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d lines %q, want 1 for two calls", len(logs), logs)
	}
	if !strings.Contains(logs[0], configured) || !strings.Contains(logs[0], resolved) {
		t.Errorf("log = %q, want both the configured and the resolved path", logs[0])
	}
}
