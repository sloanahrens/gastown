package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lock"
)

// EnsureWorkspaceTrust pre-seeds Claude Code's folder-trust entry for workDir
// so unattended sessions never stall on the "Do you trust this folder?" dialog.
//
// Claude Code only suppresses the dialog for an exact-path
// projects[dir].hasTrustDialogAccepted entry in its .claude.json —
// --dangerously-skip-permissions does not cover it, and trust granted to an
// ancestor directory does not cascade to fresh worktrees (gt-22r). Seeding the
// entry before the session starts is the only reliable suppression; the tmux
// dialog auto-acceptance in AcceptStartupDialogs remains as a backstop.
//
// configDir is the caller's config dir override (CLAUDE_CONFIG_DIR), e.g. from
// accounts.json. The session may read a different one — see
// resolveTrustConfigDir. Non-claude runtimes are a no-op.
func EnsureWorkspaceTrust(workDir, configDir string, rc *config.RuntimeConfig) error {
	if workDir == "" || !isClaudeRuntime(rc) {
		return nil
	}

	dir, err := resolveTrustConfigDir(configDir, rc)
	if err != nil {
		return err
	}

	path, err := claudeConfigJSONPath(dir)
	if err != nil {
		return err
	}

	// Serialize gt's own concurrent spawns (e.g. a convoy dispatching several
	// polecats at once) so parallel read-modify-writes don't drop entries.
	unlock, err := lock.FlockAcquire(path + ".gt.lock")
	if err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer unlock()

	cfg := map[string]any{}
	perm := os.FileMode(0600)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the runtime's own config file
	switch {
	case err == nil:
		if info, statErr := os.Stat(path); statErr == nil {
			perm = info.Mode().Perm()
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// First run on this machine: create a minimal config holding only the
		// trust entry. Claude Code fills in the rest on startup.
	default:
		return fmt.Errorf("reading %s: %w", path, err)
	}

	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		cfg["projects"] = projects
	}

	// Trust entries are exact-path keyed. Seed both the literal path and its
	// symlink-resolved form (macOS: /tmp vs /private/tmp) to match whichever
	// form Claude Code records as the project cwd.
	dirty := seedTrust(projects, workDir)
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil && resolved != workDir {
		dirty = seedTrust(projects, resolved) || dirty
	}
	if !dirty {
		return nil
	}

	return atomicfile.WriteJSONWithPerm(path, cfg, perm)
}

// SeedWorkspaceTrust calls EnsureWorkspaceTrust and downgrades any failure to
// a stderr warning. Trust seeding is best-effort and must never block a spawn:
// the tmux dialog auto-acceptance in AcceptStartupDialogs remains the in-pane
// backstop. Every spawn path that creates an agent session directly (rather
// than through session.StartSession) must call this before creating the tmux
// session (gt-yy9).
func SeedWorkspaceTrust(workDir, configDir string, rc *config.RuntimeConfig) {
	if err := EnsureWorkspaceTrust(workDir, configDir, rc); err != nil {
		fmt.Fprintf(os.Stderr, "warning: seeding workspace trust for %s: %v\n", workDir, err)
	}
}

// seedTrust marks dir as trusted in the projects map, preserving any existing
// per-project settings. Returns true if the map was modified.
func seedTrust(projects map[string]any, dir string) bool {
	entry, ok := projects[dir].(map[string]any)
	if !ok {
		entry = map[string]any{}
		projects[dir] = entry
	}
	if accepted, ok := entry["hasTrustDialogAccepted"].(bool); ok && accepted {
		return false
	}
	entry["hasTrustDialogAccepted"] = true
	return true
}

// isClaudeRuntime reports whether the runtime invokes the claude binary.
//
// A resolved runtime defers to config.IsResolvedAgentClaude, the spawn path's
// own notion of a Claude harness, so a claude behind a wrapper command is
// seeded too rather than only a command whose basename is literally "claude"
// (gt-zbty).
//
// An unresolved runtime (nil, or no command) stays a no-op:
// IsResolvedAgentClaude's nil-means-Claude default decides how to start a
// session, not which .claude.json to write.
func isClaudeRuntime(rc *config.RuntimeConfig) bool {
	if rc == nil || rc.Command == "" {
		return false
	}
	return config.IsResolvedAgentClaude(rc)
}

// resolveTrustConfigDir returns the CLAUDE_CONFIG_DIR path the spawned session
// will read .claude.json from. The spawn path merges a preset's env into the
// agent command after the caller's env, so a preset that sets
// CLAUDE_CONFIG_DIR wins over the caller's config dir (accounts.json) and over
// the gt process's own environment (gt-3vfs).
//
// A preset value that cannot be resolved is an error, not a fall-through: the
// spawn refuses that reference anyway, and the fallback file is one the session
// never reads.
func resolveTrustConfigDir(configDir string, rc *config.RuntimeConfig) (string, error) {
	if rc != nil {
		if raw := rc.Env["CLAUDE_CONFIG_DIR"]; strings.TrimSpace(raw) != "" {
			// Expanded, not trimmed: the path seeded must be the one the
			// session is handed.
			dir := config.ExpandEnvRefs(rc.Env)["CLAUDE_CONFIG_DIR"]
			if strings.TrimSpace(dir) == "" {
				return "", fmt.Errorf("preset env CLAUDE_CONFIG_DIR=%q resolves to an empty path; "+
					"set the referenced variable or drop the override", raw)
			}
			return dir, nil
		}
	}
	if configDir != "" {
		return configDir, nil
	}
	// No explicit override anywhere: the spawned session inherits the
	// environment, so a globally-set CLAUDE_CONFIG_DIR redirects where claude
	// reads its config.
	return os.Getenv("CLAUDE_CONFIG_DIR"), nil
}

// claudeConfigJSONPath returns the .claude.json location for the given config
// dir override, defaulting to the user's home directory.
func claudeConfigJSONPath(configDir string) (string, error) {
	dir := configDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		dir = home
	} else if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	return filepath.Join(dir, ".claude.json"), nil
}
