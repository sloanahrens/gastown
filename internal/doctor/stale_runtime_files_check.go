package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// buildPrefixSet builds a set of all known rig prefixes from rigs.json.
// This maps prefix→true for efficient lookup. (gt-85w7)
func buildPrefixSet(registeredRigs map[string]bool, townRoot string) map[string]bool {
	prefixes := make(map[string]bool)
	for rigName := range registeredRigs {
		prefixes[rigName] = true // rig name itself is always valid
		prefix := config.GetRigPrefix(townRoot, rigName)
		if prefix != "" {
			prefixes[prefix] = true
		}
	}
	return prefixes
}

// townLevelPrefixes are the PID-file prefixes of town-level agents, which
// belong to no rig.
var townLevelPrefixes = []string{"hq", "gt"}

// StaleRuntimeFilesCheck detects stale PID files and wisp configs for rigs
// that are no longer registered. These can cause the daemon to incorrectly
// think agents are running or try to start agents for removed rigs.
type StaleRuntimeFilesCheck struct {
	FixableCheck
	stalePIDFiles    []string
	staleWispConfigs []string
}

// NewStaleRuntimeFilesCheck creates a new stale runtime files check.
func NewStaleRuntimeFilesCheck() *StaleRuntimeFilesCheck {
	return &StaleRuntimeFilesCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stale-runtime-files",
				CheckDescription: "Detect stale PID files and wisp configs for removed rigs",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run checks for stale runtime files.
func (c *StaleRuntimeFilesCheck) Run(ctx *CheckContext) *CheckResult {
	c.stalePIDFiles = nil
	c.staleWispConfigs = nil

	// Load registered rigs
	rigsConfigPath := filepath.Join(ctx.TownRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load rigs registry",
			Details: []string{err.Error()},
		}
	}

	// Build set of registered rig names
	registeredRigs := make(map[string]bool)
	for rigName := range rigsConfig.Rigs {
		registeredRigs[rigName] = true
	}

	// Build prefix set that includes both rig names and their beads prefixes.
	// Some rigs use prefix as DB/PID name (e.g., "lc" for laneassist). (gt-85w7)
	knownPrefixes := buildPrefixSet(registeredRigs, ctx.TownRoot)
	// Town-level agents (hq, gt) are always valid. They join the set so they
	// match by the same rule as rig prefixes. (gt-0x9p2)
	for _, prefix := range townLevelPrefixes {
		knownPrefixes[prefix] = true
	}

	var details []string

	// Check PID files in .runtime/pids/
	pidsDir := filepath.Join(ctx.TownRoot, ".runtime", "pids")
	if files, err := os.ReadDir(pidsDir); err == nil {
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			name := file.Name()
			// PID files are named like: sw-crew-max.pid, pir-nux.pid, hq-overseer.pid
			// Match the file name against the known prefixes: rig prefixes may
			// themselves contain hyphens, so cutting at the first separator would
			// read my-app-nux.pid as prefix "my" and delete a live file. (gt-a4po7)
			rigPrefix, known := matchKnownPrefix(name, knownPrefixes)
			if known || rigPrefix == "" {
				// Registered rigs and town-level agents are always valid
				continue
			}
			c.stalePIDFiles = append(c.stalePIDFiles, filepath.Join(pidsDir, name))
			details = append(details, fmt.Sprintf("Stale PID file for unregistered rig: %s", name))
		}
	}

	// Check wisp configs in .beads-wisp/config/
	wispConfigDir := filepath.Join(ctx.TownRoot, ".beads-wisp", "config")
	if files, err := os.ReadDir(wispConfigDir); err == nil {
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			name := file.Name()
			// Wisp configs are named like: sallaWork.json, pir.json
			rigName := strings.TrimSuffix(name, ".json")
			if rigName == "" {
				continue
			}
			// Check if this rig is registered
			if !registeredRigs[rigName] {
				c.staleWispConfigs = append(c.staleWispConfigs, filepath.Join(wispConfigDir, name))
				details = append(details, fmt.Sprintf("Stale wisp config for unregistered rig: %s", name))
			}
		}
	}

	if len(c.stalePIDFiles) == 0 && len(c.staleWispConfigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No stale runtime files found",
		}
	}

	msg := ""
	if len(c.stalePIDFiles) > 0 {
		msg = fmt.Sprintf("%d stale PID file(s)", len(c.stalePIDFiles))
	}
	if len(c.staleWispConfigs) > 0 {
		if msg != "" {
			msg += ", "
		}
		msg += fmt.Sprintf("%d stale wisp config(s)", len(c.staleWispConfigs))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: msg,
		Details: details,
		FixHint: "Run 'gt doctor fix stale-runtime-files' to remove stale runtime files",
	}
}

// Fix removes stale runtime files.
func (c *StaleRuntimeFilesCheck) Fix(ctx *CheckContext) error {
	for _, path := range c.stalePIDFiles {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", path, err)
		}
	}
	for _, path := range c.staleWispConfigs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", path, err)
		}
	}
	return nil
}

// matchKnownPrefix reports the known rig prefix that owns a PID filename.
// A PID file is named <session-id>.pid, and the session id starts with the rig
// prefix followed by a hyphen. Prefixes may contain hyphens themselves, so the
// longest known prefix that matches is the owner — never the text before the
// first separator. (gt-a4po7)
//
// When no known prefix matches, known is false and prefix falls back to
// extractRigPrefix, which is what the caller reports as stale.
func matchKnownPrefix(filename string, knownPrefixes map[string]bool) (prefix string, known bool) {
	name := strings.TrimSuffix(filename, ".pid")
	for candidate := range knownPrefixes {
		if candidate == "" {
			continue
		}
		if name != candidate && !strings.HasPrefix(name, candidate+"-") {
			continue
		}
		if len(candidate) > len(prefix) {
			prefix = candidate
		}
	}
	if prefix != "" {
		return prefix, true
	}
	return extractRigPrefix(filename), false
}

// extractRigPrefix extracts the rig prefix from a PID filename.
// Examples: sw-nux.pid -> sw, pir-crew-dickle.pid -> pir, hq-overseer.pid -> hq
func extractRigPrefix(filename string) string {
	// Remove .pid extension
	name := strings.TrimSuffix(filename, ".pid")
	// Split on first hyphen or underscore
	for i, c := range name {
		if c == '-' || c == '_' {
			return name[:i]
		}
	}
	return name
}
