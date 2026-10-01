// Package config provides configuration loading and environment variable management.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// EnvAgent names the coding agent a session runs. It is written to the tmux
// session table at spawn so liveness checks read the right process names.
const EnvAgent = "GT_AGENT"

// EnvAgentOverride marks a session whose EnvAgent came from an explicit
// --agent override rather than from role_agents resolution. A handoff
// re-resolves the role's agent from live role_agents config, but an explicit
// override is not a snapshot of that config and must survive (gt-di8p).
const EnvAgentOverride = "GT_AGENT_OVERRIDE"

// IdentityEnvVars are agent identity env vars that must not leak across
// process or session boundaries. Used by daemon sanitization (clearing
// inherited vars), tmux global cleanup, and prime session env repair.
// See GH#3006.
var IdentityEnvVars = []string{
	"GT_ROLE", "GT_RIG", "GT_CREW", "GT_POLECAT", "GT_DOG_NAME",
	"GT_SESSION", EnvAgent, EnvAgentOverride, "BD_ACTOR", "GIT_AUTHOR_NAME", "BEADS_AGENT_NAME",
}

var bdTargetSelectorEnvVars = []string{
	"BEADS_DIR",
	"BEADS_DB",
	"BD_DB",
	"BEADS_SHARED_SERVER_DIR",
	"BEADS_DOLT_DATA_DIR",
	"BEADS_DOLT_DATABASE",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_HOST",
	"BEADS_DOLT_SHARED_SERVER",
	"BEADS_DOLT_SERVER_MODE",
	"BEADS_DOLT_SERVER_SOCKET",
	"GT_DOLT_DATA",
}

// AgentEnvConfig specifies the configuration for generating agent environment variables.
// This is the single source of truth for all agent environment configuration.
type AgentEnvConfig struct {
	// Role is the agent role: mayor, deacon, witness, refinery, crew, polecat, dog, boot
	Role string

	// Rig is the rig name (empty for town-level agents like mayor/deacon)
	Rig string

	// AgentName is the specific agent name (empty for singletons like witness/refinery)
	// For polecats, this is the polecat name. For crew, this is the crew member name.
	AgentName string

	// TownRoot is the root of the Gas Town workspace.
	// Sets GT_ROOT environment variable.
	TownRoot string

	// RuntimeConfigDir is the optional CLAUDE_CONFIG_DIR path
	RuntimeConfigDir string

	// SessionIDEnv is the environment variable name that holds the session ID.
	// Sets GT_SESSION_ID_ENV so the runtime knows where to find the session ID.
	SessionIDEnv string

	// Agent is the agent override (e.g., "claude-haiku").
	// If set, GT_AGENT is written to the tmux session table via SetEnvironment
	// so that IsAgentAliveChecked and waitForPolecatReady can read it via GetEnvironment.
	// Without this, GetEnvironment returns empty (tmux show-environment reads the
	// session table, not the process env set via exec env in the startup command).
	Agent string

	// SessionName is the tmux session name for this agent (e.g., "hq-mayor", "gt-crew-max").
	// Set as the GT_SESSION env var.
	SessionName string

	// Getenv reads the environment the agent's variables are resolved from
	// (Dolt endpoint, cost tier, passthrough credentials). Nil
	// reads the process environment.
	Getenv func(string) string
}

// getenv returns cfg.Getenv, or os.Getenv when it is nil.
func (cfg AgentEnvConfig) getenv() func(string) string {
	if cfg.Getenv != nil {
		return cfg.Getenv
	}
	return os.Getenv
}

// AgentEnv returns all environment variables for an agent based on the config.
// This is the single source of truth for agent environment variables.
func AgentEnv(cfg AgentEnvConfig) map[string]string {
	getenv := cfg.getenv()
	env := make(map[string]string)

	// Set role-specific variables
	// GT_ROLE is set in compound format (e.g., "beads/crew/jane") so that
	// beads can parse it without knowing about Gas Town role types.
	switch cfg.Role {
	case constants.RoleMayor:
		env["GT_ROLE"] = constants.RoleMayor
		env["BD_ACTOR"] = constants.RoleMayor
		env["GIT_AUTHOR_NAME"] = constants.RoleMayor

	case constants.RolePolecat:
		env["GT_ROLE"] = fmt.Sprintf("%s/polecats/%s", cfg.Rig, cfg.AgentName)
		env["GT_RIG"] = cfg.Rig
		env["GT_POLECAT"] = cfg.AgentName
		env["BD_ACTOR"] = fmt.Sprintf("%s/polecats/%s", cfg.Rig, cfg.AgentName)
		env["GIT_AUTHOR_NAME"] = cfg.AgentName
		// Disable Dolt auto-commit for polecats. With branch-per-polecat,
		// individual commits are pointless — all changes merge at gt done time
		// via DOLT_MERGE. Without this, concurrent polecats cause manifest
		// contention leading to Dolt read-only mode (gt-5cc2p).
		env["BD_DOLT_AUTO_COMMIT"] = "off"

	case constants.RoleCrew:
		env["GT_ROLE"] = fmt.Sprintf("%s/crew/%s", cfg.Rig, cfg.AgentName)
		env["GT_RIG"] = cfg.Rig
		env["GT_CREW"] = cfg.AgentName
		env["BD_ACTOR"] = fmt.Sprintf("%s/crew/%s", cfg.Rig, cfg.AgentName)
		env["GIT_AUTHOR_NAME"] = cfg.AgentName

	}

	// Only set GT_ROOT if provided
	// Empty values would override tmux session environment
	if cfg.TownRoot != "" {
		env["GT_ROOT"] = cfg.TownRoot
		// Prevent git from walking up to umbrella repo when running in rig worktrees.
		// This stops accidental commits to the umbrella when running git commands from
		// intermediate directories (e.g., polecats/) that don't have their own .git.
		env["GIT_CEILING_DIRECTORIES"] = cfg.TownRoot
	}

	// Set BEADS_AGENT_NAME for polecat/crew (uses same format as BD_ACTOR)
	if cfg.Role == constants.RolePolecat || cfg.Role == constants.RoleCrew {
		env["BEADS_AGENT_NAME"] = fmt.Sprintf("%s/%s", cfg.Rig, cfg.AgentName)
	}

	// Add optional runtime config directory
	if cfg.RuntimeConfigDir != "" {
		env["CLAUDE_CONFIG_DIR"] = cfg.RuntimeConfigDir
	}

	// Add session ID env var name if provided
	if cfg.SessionIDEnv != "" {
		env["GT_SESSION_ID_ENV"] = cfg.SessionIDEnv
	}

	// Set GT_SESSION when a session name is provided, so gt commands and
	// cost reports can correlate activity to a specific tmux session.
	if cfg.SessionName != "" {
		env["GT_SESSION"] = cfg.SessionName
	}

	// Set GT_AGENT when an agent override is in use.
	// This makes the override visible via tmux show-environment so that
	// IsAgentAliveChecked and waitForPolecatReady use the correct process names.
	// EnvAgentOverride records that the pin came from --agent rather than from
	// role_agents resolution, so a handoff can tell a deliberate override from
	// a stale snapshot of config it is free to re-resolve (gt-di8p).
	if cfg.Agent != "" {
		env[EnvAgent] = cfg.Agent
		env[EnvAgentOverride] = "1"
	}

	// Disable bd's per-repo JSONL auto-backup for all Gas Town agents.
	// bd auto-enables backup when a git remote exists, then force-adds
	// .beads/backup/ files (bypassing .gitignore) and commits/pushes them
	// to the project repo. In Gas Town, Dolt is the persistent data store
	// and the daemon provides centralized backups (the nightly Dolt backup
	// in scheduled_maintenance, jsonl_git_backup), making per-repo backup redundant and harmful —
	// it pollutes rig git history on both main and feature branches.
	// See: https://github.com/steveyegge/beads/issues/2241
	env["BD_BACKUP_ENABLED"] = "false"

	// Clear NODE_OPTIONS to prevent debugger flags (e.g., --inspect from VSCode)
	// from being inherited through tmux into Claude's Node.js runtime.
	// This is the PRIMARY guard: setting it here (the single source of truth
	// for agent env) protects all AgentEnv-based paths automatically — tmux
	// SetEnvironment, EnvForExecCommand, PrependEnv. SanitizeAgentEnv provides
	// a SUPPLEMENTAL guard for non-AgentEnv paths (lifecycle default, handoff).
	// In BuildStartupCommand, rc.Env is merged after AgentEnv and can override
	// this empty value with intentional settings like --max-old-space-size.
	env["NODE_OPTIONS"] = ""

	// Resolve effort level from per-role config (role_effort in town/rig settings,
	// or cost-tier presets). Falls back to "high" when no config exists.
	// The CLAUDE_CODE_EFFORT_LEVEL env var is deprecated — effort is now configured
	// per-role through config, matching the pattern used for model selection.
	rigPath := ""
	if cfg.Rig != "" && cfg.TownRoot != "" {
		rigPath = filepath.Join(cfg.TownRoot, cfg.Rig)
	}
	effort := resolveRoleEffort(getenv, cfg.Role, cfg.TownRoot, rigPath)
	if effort == "" {
		effort = "high"
	}
	env["CLAUDE_CODE_EFFORT_LEVEL"] = effort

	// Clear CLAUDECODE to prevent nested session detection in Claude Code v2.x.
	// When gt sling is invoked from within a Claude Code session, CLAUDECODE=1
	// leaks through tmux's global environment into new polecat sessions, causing
	// Claude Code to refuse to start with a "nested sessions" error.
	// See: https://github.com/steveyegge/gastown/issues/1666
	env["CLAUDECODE"] = ""

	clearBDTargetSelectorEnv(env)

	// Inject Dolt server endpoint so agents' direct bd invocations connect to
	// gt's central server instead of auto-starting rogue per-rig servers.
	// BEADS_DOLT_* values are output aliases only; they are never authoritative.
	// The endpoint comes from the town's config only (gt-y3pgh.3).
	for k, v := range ConfiguredDoltEnv(cfg.TownRoot) {
		env[k] = v
	}
	// Suppress bd's Dolt auto-start for all Gas Town agents (GH#2930).
	// Gas Town manages its own Dolt server (gt dolt start/stop). When the
	// server is momentarily unreachable (restart, journal hiccup), bd's
	// auto-start tries to launch a shadow server in the agent's .beads/dolt/
	// directory — which conflicts with the real server on the same port and
	// triggers an escalation flood loop. Dogs are especially affected because
	// their kennel's .beads/ has no explicit dolt_server_port in metadata.json.
	if cfg.TownRoot != "" {
		env["BEADS_DOLT_AUTO_START"] = "0"
	}

	// Pass through cloud API credentials and provider configuration from the parent shell.
	// Only variables explicitly listed here are forwarded; all others are blocked for isolation.
	for _, key := range []string{
		// Anthropic API (direct)
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		// ANTHROPIC_BASE_URL intentionally excluded — agents that need a custom
		// base URL (MiniMax, Groq, etc.) get it from their agent config's Env
		// block, not from the parent process. Passthrough caused cross-provider
		// contamination: a MiniMax deacon's base URL leaked into Claude polecats.
		"ANTHROPIC_CUSTOM_HEADERS",

		// Model selection
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",

		// AWS Bedrock
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_REGION",
		"AWS_PROFILE",
		"AWS_BEARER_TOKEN_BEDROCK",
		"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",

		// Microsoft Foundry
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_RESOURCE",

		// Google Vertex AI
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_SKIP_VERTEX_AUTH",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"VERTEX_PROJECT",
		"VERTEX_LOCATION",
		"VERTEX_REGION_CLAUDE_3_5_HAIKU",
		"VERTEX_REGION_CLAUDE_3_7_SONNET",
		"VERTEX_REGION_CLAUDE_4_0_OPUS",
		"VERTEX_REGION_CLAUDE_4_0_SONNET",
		"VERTEX_REGION_CLAUDE_4_1_OPUS",

		// Proxy / network
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",

		// mTLS
		"CLAUDE_CODE_CLIENT_CERT",
		"CLAUDE_CODE_CLIENT_KEY",
		"CLAUDE_CODE_CLIENT_KEY_PASSPHRASE",
	} {
		if val := getenv(key); val != "" {
			env[key] = val
		}
	}

	return env
}

func clearBDTargetSelectorEnv(env map[string]string) {
	for _, key := range bdTargetSelectorEnvVars {
		env[key] = ""
	}
}

// ResolveDoltEndpoint is the one resolver for the town's Dolt server endpoint
// (gt-y3pgh.3). It reads durable config only, never the environment:
//
//  1. mayor/town.json "dolt" (written by gt install and gt config set dolt.port)
//  2. .dolt-data/config.yaml listener, for a town whose town.json predates
//     the field (gt dolt start writes that file from this resolver)
//
// ok is false when neither names a port: the town has no endpoint, and the
// caller must not guess one (no fallback to DefaultPort).
func ResolveDoltEndpoint(townRoot string) (DoltEndpoint, bool) {
	if townRoot == "" {
		return DoltEndpoint{}, false
	}
	if town, err := LoadTownConfig(filepath.Join(townRoot, "mayor", "town.json")); err == nil && town.Dolt != nil {
		return DoltEndpoint{Host: strings.TrimSpace(town.Dolt.Host), Port: town.Dolt.Port}, true
	}
	data, err := os.ReadFile(filepath.Join(townRoot, ".dolt-data", "config.yaml")) //nolint:gosec // G304: managed file under the town root
	if err != nil {
		return DoltEndpoint{}, false
	}
	port := parsePortFromConfigYAML(data)
	if port <= 0 {
		return DoltEndpoint{}, false
	}
	return DoltEndpoint{Host: parseHostFromConfigYAML(data), Port: port}, true
}

// ResolveDoltPort is ResolveDoltEndpoint's port, or 0 when the town has no
// endpoint.
func ResolveDoltPort(townRoot string) int {
	ep, _ := ResolveDoltEndpoint(townRoot)
	return ep.Port
}

// ResolveDoltHost is ResolveDoltEndpoint's host, or "" (the local machine).
func ResolveDoltHost(townRoot string) string {
	ep, _ := ResolveDoltEndpoint(townRoot)
	return ep.Host
}

// NormalizeConfiguredDoltEnv replaces the Dolt endpoint variables in base
// with the town's endpoint. When the town has no endpoint base is returned
// unchanged.
func NormalizeConfiguredDoltEnv(base []string, townRoot string) []string {
	ep, ok := ResolveDoltEndpoint(townRoot)
	if !ok {
		return base
	}
	base = stripDoltEndpointEnv(base)
	for _, key := range DoltEndpointEnvKeys {
		if v := doltEndpointEnvValue(ep, key); v != "" {
			base = append(base, key+"="+v)
		}
	}
	return base
}

// DoltEndpointEnvKeys are the environment variables gt exports to name the
// Dolt endpoint for the bd it starts. gt itself never reads them. A startup
// boundary unsets all of them before exporting ConfiguredDoltEnv.
// GT_DOLT_HOST and GT_DOLT_PORT are not exported: nothing reads them
// (gt-y3pgh.9).
var DoltEndpointEnvKeys = []string{"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// ConfiguredDoltEnv returns the Dolt endpoint variables a startup boundary
// (gt up, the daemon) exports to the children it spawns, from the town's
// endpoint. It is empty when the town has no endpoint.
func ConfiguredDoltEnv(townRoot string) map[string]string {
	env := make(map[string]string)
	ep, ok := ResolveDoltEndpoint(townRoot)
	if !ok {
		return env
	}
	for _, key := range DoltEndpointEnvKeys {
		if v := doltEndpointEnvValue(ep, key); v != "" {
			env[key] = v
		}
	}
	return env
}

func doltEndpointEnvValue(ep DoltEndpoint, key string) string {
	switch key {
	case "BEADS_DOLT_SERVER_HOST":
		return ep.Host
	default:
		return strconv.Itoa(ep.Port)
	}
}

func stripDoltEndpointEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && isDoltEndpointEnvKey(key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func isDoltEndpointEnvKey(key string) bool {
	for _, want := range DoltEndpointEnvKeys {
		if runtime.GOOS == "windows" {
			if strings.EqualFold(key, want) {
				return true
			}
			continue
		}
		if key == want {
			return true
		}
	}
	return false
}

// parsePortFromConfigYAML extracts the listener port from a Dolt config.yaml
// without a yaml dependency. The file is machine-generated by gt dolt start
// with the format:
//
//	listener:
//	  port: 3307
func parsePortFromConfigYAML(data []byte) int {
	lines := strings.Split(string(data), "\n")
	inListener := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "listener:" {
			inListener = true
			continue
		}
		if inListener {
			if strings.HasPrefix(trimmed, "port:") {
				portStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "port:"))
				if port, err := strconv.Atoi(portStr); err == nil {
					return port
				}
			}
			// Any non-indented line ends the listener block
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
				inListener = false
			}
		}
	}
	return 0
}

func parseHostFromConfigYAML(data []byte) string {
	lines := strings.Split(string(data), "\n")
	inListener := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "listener:" {
			inListener = true
			continue
		}
		if inListener {
			if strings.HasPrefix(trimmed, "host:") {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "host:")), `"'`)
			}
			// Any non-indented line ends the listener block
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
				inListener = false
			}
		}
	}
	return ""
}

// AgentEnvSimple is a convenience function for simple role-based env var lookup.
// Use this when you only need role, rig, and agentName without advanced options.
func AgentEnvSimple(role, rig, agentName string) map[string]string {
	return AgentEnv(AgentEnvConfig{
		Role:      role,
		Rig:       rig,
		AgentName: agentName,
	})
}

// ExpandEnvRefs returns a copy of env with ${VAR} references in each value
// replaced by VAR's value in the process environment.
//
// Only the braced form expands, so a value that legitimately contains a bare
// dollar sign — a password, a shell snippet, a prompt template — is passed
// through verbatim, and ShellQuote still quotes it. Shell parameter-expansion
// syntax such as ${VAR:-default} is left alone for the same reason: it is not
// a variable reference this function can resolve, so deciding what it means
// belongs to whoever wrote it.
//
// A reference to an unset variable expands to the empty string, so a missing
// credential reaches the provider as an empty value instead of authenticating
// with the literal sentinel text. ValidateAgentConfig rejects such an agent;
// an agent resolved out of settings is rejected by both startup-command
// builders, which check the reference before expanding it (gt-wisp-jsm).
func ExpandEnvRefs(env map[string]string) map[string]string {
	return expandEnvRefsIn(env, os.Getenv)
}

// expandEnvRefsIn is ExpandEnvRefs reading variables through getenv.
func expandEnvRefsIn(env map[string]string, getenv func(string) string) map[string]string {
	if len(env) == 0 {
		return env
	}
	expanded := make(map[string]string, len(env))
	for k, v := range env {
		expanded[k] = expandEnvRefs(v, getenv)
	}
	return expanded
}

// expandEnvRefs replaces ${VAR} references in s with VAR's value from getenv,
// leaving everything else in place.
func expandEnvRefs(s string, getenv func(string) string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		name, end, ok := envRefAt(s, i)
		if ok {
			b.WriteString(getenv(name))
			i = end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// envRefNames returns the variable names s references as ${VAR}, in order of
// appearance and without duplicates.
func envRefNames(s string) []string {
	var names []string
	seen := make(map[string]bool)
	for i := 0; i < len(s); {
		name, end, ok := envRefAt(s, i)
		if !ok {
			i++
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		i = end
	}
	return names
}

// envRefAt reports whether a ${VAR} reference starts at s[i], returning the
// variable name and the index just past the closing brace.
func envRefAt(s string, i int) (name string, end int, ok bool) {
	if i+1 >= len(s) || s[i] != '$' || s[i+1] != '{' {
		return "", 0, false
	}
	rest := s[i+2:]
	close := strings.IndexByte(rest, '}')
	if close < 0 {
		return "", 0, false
	}
	name = rest[:close]
	if !isEnvVarName(name) {
		return "", 0, false
	}
	return name, i + 2 + close + 1, true
}

// isEnvVarName reports whether name can be looked up with os.Getenv: a
// non-empty run of letters, digits, and underscores that does not start with a
// digit. Anything else is shell syntax envRefAt has no value for.
func isEnvVarName(name string) bool {
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return name != ""
}

// ShellQuote returns a shell-safe quoted string.
// Values containing special characters are wrapped in single quotes.
// Single quotes within the value are escaped using the '\” idiom.
func ShellQuote(s string) string {
	// Check if quoting is needed (contains shell special chars)
	needsQuoting := false
	for _, c := range s {
		switch c {
		case ' ', '\t', '\n', '"', '\'', '`', '$', '\\', '!', '*', '?',
			'[', ']', '{', '}', '(', ')', '<', '>', '|', '&', ';', '#':
			needsQuoting = true
		}
		if needsQuoting {
			break
		}
	}

	if !needsQuoting {
		return s
	}

	// Use single quotes, escaping any embedded single quotes
	// 'foo'\''bar' means: 'foo' + escaped-single-quote + 'bar'
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// psQuote quotes a value for use in PowerShell $env: assignments.
// Uses single quotes and doubles embedded single quotes.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ExportPrefix builds an export statement prefix for shell commands.
// Returns a string like "export GT_ROLE=mayor BD_ACTOR=mayor && "
// The keys are sorted for deterministic output.
// Values containing special characters are properly shell-quoted.
func ExportPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}

	// Sort keys for deterministic output
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if runtime.GOOS == "windows" {
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("$env:%s=%s", k, psQuote(env[k])))
		}
		return strings.Join(parts, "; ") + "; "
	}

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, ShellQuote(env[k])))
	}
	return "export " + strings.Join(parts, " ") + " && "
}

// BuildStartupCommandWithEnv builds a startup command with the given environment variables.
// This combines the export prefix with the agent command and optional prompt.
func BuildStartupCommandWithEnv(env map[string]string, agentCmd, prompt string) string {
	prefix := ExportPrefix(env)

	if prompt != "" {
		// Include prompt as argument to agent command
		return fmt.Sprintf("%s%s %q", prefix, agentCmd, prompt)
	}
	return prefix + agentCmd
}

// MergeEnv merges multiple environment maps, with later maps taking precedence.
func MergeEnv(maps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}

// FilterEnv returns a new map with only the specified keys.
func FilterEnv(env map[string]string, keys ...string) map[string]string {
	result := make(map[string]string)
	for _, k := range keys {
		if v, ok := env[k]; ok {
			result[k] = v
		}
	}
	return result
}

// WithoutEnv returns a new map without the specified keys.
func WithoutEnv(env map[string]string, keys ...string) map[string]string {
	result := make(map[string]string)
	exclude := make(map[string]bool)
	for _, k := range keys {
		exclude[k] = true
	}
	for k, v := range env {
		if !exclude[k] {
			result[k] = v
		}
	}
	return result
}

// EnvForExecCommand returns os.Environ() with the given env vars appended.
// This is useful for setting cmd.Env on exec.Command.
func EnvForExecCommand(env map[string]string) []string {
	result := os.Environ()
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}

// EnvToSlice converts an env map to a slice of "K=V" strings.
// Useful for appending to os.Environ() manually.
func EnvToSlice(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}

// ClaudeConfigDir resolves the Claude Code configuration directory.
// Resolution order:
//  1. CLAUDE_CONFIG_DIR env var (if set and non-empty)
//  2. $HOME/.claude (fallback)
func ClaudeConfigDir() (string, error) {
	return claudeConfigDir(os.Getenv, os.UserHomeDir)
}

func claudeConfigDir(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}
