//go:build !windows

package util

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// beadsDirComponent is the directory name every beads workspace keeps its bd
// state in; a Dolt server whose --config lies under one belongs to that
// workspace, not to the town.
const beadsDirComponent = ".beads"

// DoltServerProcess is a running `dolt sql-server` as the process table and its
// --config describe it.
type DoltServerProcess struct {
	PID int
	// Port is the --port argv value, else the config file's listener port; 0
	// when neither names one, which callers read as "unknown", not as "none".
	Port       int
	ConfigPath string // --config argv, or "" when the argv names none
	DataDir    string // --data-dir argv, else the config file's directory
}

// FindDoltServers lists every running `dolt sql-server` — the town's own server
// included — with the port and data directory its argv and --config reveal.
//
// This is the read-only counterpart of FindOrphanDoltServers, which subtracts
// the town's own server and asks instead which servers should not be running
// (gt-4js4f).
func FindDoltServers() ([]DoltServerProcess, error) {
	entries, err := doltPSSnapshot()
	if err != nil {
		return nil, err
	}

	var servers []DoltServerProcess
	for _, e := range entries {
		if !isDoltSQLServerArgs(e.Args) {
			continue
		}
		cfg := doltSQLServerConfigPath(e.Args)
		port := doltSQLServerPort(e.Args)
		if port == 0 {
			port = doltConfigListenerPort(cfg)
		}
		servers = append(servers, DoltServerProcess{
			PID:        e.PID,
			Port:       port,
			ConfigPath: cfg,
			DataDir:    doltProcessDataDir(e.Args, cfg),
		})
	}
	return servers, nil
}

// BeadsDirFromConfigPath names the .beads directory a Dolt server's --config
// path lies under and the workspace root that owns it, taking the innermost
// .beads component; ok is false for a path with no .beads component.
//
// The component is what separates a workspace's own bd-started server from the
// town's, which runs from .dolt-data (gt-4js4f).
func BeadsDirFromConfigPath(configPath string) (beadsDir, workspaceRoot string, ok bool) {
	if configPath == "" {
		return "", "", false
	}
	for dir := filepath.Dir(filepath.Clean(configPath)); ; {
		if filepath.Base(dir) == beadsDirComponent {
			return dir, filepath.Dir(dir), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false // reached the root without a .beads
		}
		dir = parent
	}
}

// doltConfigListenerPort reads the port a `dolt sql-server --config` file
// serves on: the `port` of its `listener` block, or 0 when the file is
// unreadable or sets none.
//
// A workspace server bd starts puts its random port here and nowhere else, with
// no --port on the argv (gt-4js4f). The parser mirrors internal/config's
// parsePortFromConfigYAML, which is unexported and outside this scan's scope.
func doltConfigListenerPort(path string) int {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: --config of a process already in the table
	if err != nil {
		return 0
	}

	inListener := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		atRoot := line[0] != ' ' && line[0] != '\t'
		if atRoot {
			inListener = trimmed == "listener:"
			continue
		}
		value, found := strings.CutPrefix(trimmed, "port:")
		if !inListener || !found {
			continue
		}
		if port, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && port > 0 {
			return port
		}
	}
	return 0
}
