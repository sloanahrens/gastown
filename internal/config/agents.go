// Package config provides configuration types and serialization for Gas Town.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type rawAgentRegistry struct {
	Version int                        `json:"version"`
	Agents  map[string]json.RawMessage `json:"agents"`
}

// AgentPreset identifies a supported LLM agent runtime.
// These presets provide sensible defaults that can be overridden in config.
type AgentPreset string

// Built-in agent presets. Every preset runs the Claude CLI: Claude Code is
// the only agent runtime (D4). A preset may point the CLI at another backend
// through its env (groq-compound here; town settings add DeepSeek-backed ones),
// but hooks, settings, resume and liveness are always Claude Code's.
const (
	// AgentClaude is Claude Code (default).
	AgentClaude AgentPreset = "claude"
	// AgentGroqCompound routes the Claude CLI to Groq's compound-beta model via
	// Groq's OpenAI-compatible API endpoint. The claude binary acts as the SDK
	// proxy; ANTHROPIC_BASE_URL and ANTHROPIC_API_KEY are overridden at runtime
	// to redirect traffic to api.groq.com. GROQ_API_KEY must be set in the shell
	// environment — see the preset's Env for how the key reaches the agent.
	AgentGroqCompound AgentPreset = "groq-compound"
)

// AgentPresetInfo contains the configuration details for an agent preset: a
// named way to start the Claude CLI (command, args, env) plus the readiness
// and liveness details Gas Town needs to drive it in tmux.
type AgentPresetInfo struct {
	// Name is the preset identifier (e.g., "claude", "groq-compound").
	Name AgentPreset `json:"name"`

	// Command is the CLI binary to invoke.
	Command string `json:"command"`

	// Args are the default command-line arguments for autonomous mode.
	Args []string `json:"args"`

	// Env are environment variables to set when starting the agent.
	// These are merged with the standard GT_* variables.
	// Used to point the Claude CLI at another backend (ANTHROPIC_BASE_URL etc.).
	Env map[string]string `json:"env,omitempty"`

	// ProcessNames are the process names to look for when detecting if the agent is running.
	// Used by tmux.IsAgentRunning to check pane_current_command.
	// E.g., ["node", "claude"] for Claude.
	ProcessNames []string `json:"process_names,omitempty"`

	// SessionIDEnv is the environment variable for session ID.
	// Used for resuming sessions across restarts.
	SessionIDEnv string `json:"session_id_env,omitempty"`

	// ResumeFlag is the flag for resuming a specific session ("--resume").
	ResumeFlag string `json:"resume_flag,omitempty"`

	// ContinueFlag is the flag for auto-resuming the most recent session.
	// For claude: "--continue" (--resume without args opens interactive picker)
	// If empty, --resume without a session ID is rejected with a clear error.
	ContinueFlag string `json:"continue_flag,omitempty"`

	// --- Runtime default fields (replaces scattered default*() switch statements) ---

	// ConfigDirEnv is the env var for the agent's config directory (e.g., "CLAUDE_CONFIG_DIR").
	ConfigDirEnv string `json:"config_dir_env,omitempty"`

	// ReadyPromptPrefix is the prompt prefix for tmux readiness detection (e.g., "❯ ").
	// Empty means delay-based detection only.
	ReadyPromptPrefix string `json:"ready_prompt_prefix,omitempty"`

	// ReadyDelayMs is the delay-based readiness fallback in milliseconds.
	ReadyDelayMs int `json:"ready_delay_ms,omitempty"`

	// InstructionsFile is the instructions file for this agent ("CLAUDE.md").
	// Defaults to "AGENTS.md" if empty.
	InstructionsFile string `json:"instructions_file,omitempty"`

	// EmitsPermissionWarning indicates the agent shows a bypass-permissions warning on startup
	// that needs to be acknowledged via tmux.
	EmitsPermissionWarning bool `json:"emits_permission_warning,omitempty"`

	// EscapeCancelsRequest indicates that sending an Escape keystroke to this
	// agent cancels its in-flight generation. NudgeSession normally sends
	// Escape (step 5) to exit vim INSERT mode — harmless for bash, but
	// destructive for agents where Escape aborts the active request. When true,
	// NudgeSessionWithOpts skips the Escape
	// keystroke and the 600ms readline timeout that follows it.
	//
	// Claude Code sets this too (gt-cyyg): a busy-indicator scrape
	// (shouldSendEscape) is the only other gate on the Escape keystroke, and it
	// couples to upstream TUI status text that can silently change or miss a
	// narrow busy window (e.g. mid-tool-call during a long-running command). A
	// missed busy window sends Escape into a working agent, which Claude Code
	// reports back as "[Request interrupted by user for tool use]" —
	// indistinguishable from a real operator stop. Never sending Escape to
	// Claude Code at all removes that failure mode instead of chasing it.
	EscapeCancelsRequest bool `json:"escape_cancels_request,omitempty"`
}

// AgentRegistry contains all known agent presets.
// Can be loaded from JSON config or use built-in defaults.
type AgentRegistry struct {
	// Version is the schema version for the registry.
	Version int `json:"version"`

	// Agents maps agent names to their configurations.
	Agents map[string]*AgentPresetInfo `json:"agents"`

	// env is the host this registry resolves agents against; nil is the
	// running process (see host).
	env *host
}

// CurrentAgentRegistryVersion is the current schema version.
const CurrentAgentRegistryVersion = 1

// builtinPresets contains the default presets for supported agents.
// Each preset is the single source of truth for its agent's behavior.
var builtinPresets = map[AgentPreset]*AgentPresetInfo{
	AgentClaude: {
		Name:         AgentClaude,
		Command:      "claude",
		Args:         []string{"--dangerously-skip-permissions"},
		ProcessNames: []string{"node", "claude"}, // Claude runs as Node.js
		SessionIDEnv: "CLAUDE_SESSION_ID",
		ResumeFlag:   "--resume",
		ContinueFlag: "--continue",
		// Runtime defaults
		ConfigDirEnv:           "CLAUDE_CONFIG_DIR",
		ReadyPromptPrefix:      "❯ ",
		ReadyDelayMs:           10000,
		InstructionsFile:       "CLAUDE.md",
		EmitsPermissionWarning: true,
		EscapeCancelsRequest:   true, // Escape mid-tool-call reads as an interrupt, not vim-mode exit (gt-cyyg)
	},
	// AgentGroqCompound uses the Claude CLI as an SDK proxy but routes all
	// requests to Groq's OpenAI-compatible endpoint by overriding the two
	// Anthropic SDK environment variables that control the backend:
	//
	//   ANTHROPIC_BASE_URL  → https://api.groq.com/openai/v1
	//   ANTHROPIC_API_KEY   → ${GROQ_API_KEY}  (the process environment's key)
	//
	// The model flag --model groq/compound-beta selects Groq's compound
	// reasoning model. Because the transport is the Claude binary, all Gas
	// Town hooks, session tracking, tmux readiness detection, and Claude-SDK
	// lifecycle events work identically to the standard claude preset.
	//
	// Prerequisites:
	//   export GROQ_API_KEY=gsk_...
	//
	// ${GROQ_API_KEY} is a reference, not a literal: ExpandEnvRefs resolves it
	// when the agent env is built for spawn, so the ${} form is what cost tiers
	// persist and the key itself never reaches config.json. A role that lands
	// here without GROQ_API_KEY set is caught by ValidateAgentConfig.
	AgentGroqCompound: {
		Name:    AgentGroqCompound,
		Command: "claude",
		Args: []string{
			"--dangerously-skip-permissions",
		},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.groq.com/openai/v1",
			"ANTHROPIC_MODEL":    "compound-beta",
			"ANTHROPIC_API_KEY":  "${GROQ_API_KEY}",
		},
		ProcessNames:      []string{"node", "claude"},
		SessionIDEnv:      "CLAUDE_SESSION_ID",
		ResumeFlag:        "--resume",
		ContinueFlag:      "--continue",
		ConfigDirEnv:      "CLAUDE_CONFIG_DIR",
		ReadyPromptPrefix: "❯ ",
		ReadyDelayMs:      10000,
		InstructionsFile:  "CLAUDE.md",
	},
}

// builtinAgentRegistry holds the built-in presets. It is never mutated: every
// registry that includes user files is a fresh copy built by
// LoadAgentRegistryFor, so one rig's settings/agents.json cannot leak into
// another rig's resolution (gt-rg4f1).
var builtinAgentRegistry = newBuiltinAgentRegistry()

func newBuiltinAgentRegistry() *AgentRegistry {
	reg := &AgentRegistry{
		Version: CurrentAgentRegistryVersion,
		Agents:  make(map[string]*AgentPresetInfo, len(builtinPresets)),
	}
	for name, preset := range builtinPresets {
		reg.Agents[string(name)] = preset
	}
	return reg
}

// LoadAgentRegistryFor returns the agent registry in effect for a rig: the
// built-in presets, overlaid by <townRoot>/settings/agents.json, overlaid by
// <rigPath>/settings/agents.json. An entry in a file is merged onto the entry
// of the same name below it, so an override only needs the fields it changes.
// An empty townRoot or rigPath skips that layer, as does a missing file.
//
// The files are read on every call and the result is a new registry owned by
// the caller: nothing is cached between scopes. A file that cannot be read or
// parsed is skipped as a whole and reported in the returned error; the
// registry is still usable.
func LoadAgentRegistryFor(townRoot, rigPath string) (*AgentRegistry, error) {
	return loadAgentRegistryFor(processHost, townRoot, rigPath)
}

// loadAgentRegistryFor is LoadAgentRegistryFor resolving agents against h.
func loadAgentRegistryFor(h host, townRoot, rigPath string) (*AgentRegistry, error) {
	reg := &AgentRegistry{
		env:     &h,
		Version: CurrentAgentRegistryVersion,
		Agents:  make(map[string]*AgentPresetInfo, len(builtinAgentRegistry.Agents)),
	}
	for name, preset := range builtinAgentRegistry.Agents {
		reg.Agents[name] = preset
	}
	var errs []error
	if townRoot != "" {
		if err := reg.overlayFile(DefaultAgentRegistryPath(townRoot)); err != nil {
			errs = append(errs, err)
		}
	}
	if rigPath != "" {
		if err := reg.overlayFile(RigAgentRegistryPath(rigPath)); err != nil {
			errs = append(errs, err)
		}
	}
	return reg, errors.Join(errs...)
}

// AgentRegistryFor is LoadAgentRegistryFor for callers that resolve agents
// best-effort: a layer that fails to load is skipped silently.
func AgentRegistryFor(townRoot, rigPath string) *AgentRegistry {
	return agentRegistryFor(processHost, townRoot, rigPath)
}

// agentRegistryFor is AgentRegistryFor resolving agents against h.
func agentRegistryFor(h host, townRoot, rigPath string) *AgentRegistry {
	reg, _ := loadAgentRegistryFor(h, townRoot, rigPath)
	return reg
}

// overlayFile merges the agents defined in a JSON registry file onto r. The
// file applies entirely or not at all.
func (r *AgentRegistry) overlayFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from config
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: %w", path, err)
	}

	// Validate the whole file strictly first so a ParseError names the full
	// key path (agents.<name>.<key>); the merge below then decodes each entry
	// onto the preset it overrides.
	if err := DecodeJSONFile(path, data, &AgentRegistry{}); err != nil {
		return err
	}
	var userRegistry rawAgentRegistry
	if err := json.Unmarshal(data, &userRegistry); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	merged := make(map[string]*AgentPresetInfo, len(userRegistry.Agents))
	for name, rawPreset := range userRegistry.Agents {
		info := cloneAgentPresetInfo(r.Agents[name])
		if info == nil {
			info = &AgentPresetInfo{}
		}
		if err := json.Unmarshal(rawPreset, info); err != nil {
			return fmt.Errorf("%s: agent %q: %w", path, name, err)
		}
		info.Name = AgentPreset(name)
		merged[name] = info
	}
	for name, info := range merged {
		r.Agents[name] = info
	}
	return nil
}

// agents returns the preset map, treating a nil registry as the built-ins.
func (r *AgentRegistry) agents() map[string]*AgentPresetInfo {
	if r == nil || r.Agents == nil {
		return builtinAgentRegistry.Agents
	}
	return r.Agents
}

// Preset returns the preset registered under name, or nil. A nil registry
// answers from the built-in presets.
func (r *AgentRegistry) Preset(name string) *AgentPresetInfo {
	return r.agents()[name]
}

// Names returns every registered preset name, sorted.
func (r *AgentRegistry) Names() []string {
	return sortedPresetNames(r.agents())
}

// IsKnown reports whether name is a registered preset.
func (r *AgentRegistry) IsKnown(name string) bool {
	_, ok := r.agents()[name]
	return ok
}

func cloneAgentPresetInfo(src *AgentPresetInfo) *AgentPresetInfo {
	if src == nil {
		return nil
	}
	clone := *src
	if src.Args != nil {
		clone.Args = append([]string(nil), src.Args...)
	}
	if src.Env != nil {
		clone.Env = make(map[string]string, len(src.Env))
		for k, v := range src.Env {
			clone.Env[k] = v
		}
	}
	if src.ProcessNames != nil {
		clone.ProcessNames = append([]string(nil), src.ProcessNames...)
	}
	return &clone
}

// DefaultAgentRegistryPath returns the default path for agent registry.
// Located alongside other town settings.
func DefaultAgentRegistryPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "agents.json")
}

// DefaultRigAgentRegistryPath returns the default path for rig-level agent registry.
// Located in <rig>/settings/agents.json.
func DefaultRigAgentRegistryPath(rigPath string) string {
	return filepath.Join(rigPath, "settings", "agents.json")
}

// RigAgentRegistryPath returns the path for rig-level agent registry.
// Alias for DefaultRigAgentRegistryPath for consistency with other path functions.
func RigAgentRegistryPath(rigPath string) string {
	return DefaultRigAgentRegistryPath(rigPath)
}

// GetAgentPreset returns the built-in preset info for a given agent name.
// Returns nil if the preset is not found. Use LoadAgentRegistryFor to see
// town and rig settings/agents.json overrides.
func GetAgentPreset(name AgentPreset) *AgentPresetInfo {
	return builtinAgentRegistry.Preset(string(name))
}

// GetAgentPresetByName returns the built-in preset info by string name.
// Returns nil if not found, allowing caller to fall back to defaults.
func GetAgentPresetByName(name string) *AgentPresetInfo {
	return builtinAgentRegistry.Preset(name)
}

// ListAgentPresets returns all built-in agent preset names.
func ListAgentPresets() []string {
	return builtinAgentRegistry.Names()
}

// BuiltInAgentPresetSummary returns a sorted, comma-separated list of built-in preset names
// for CLI help text (gt config agent list, default-agent, --provider, etc.).
func BuiltInAgentPresetSummary() string {
	return strings.Join(ListAgentPresets(), ", ")
}

// DefaultAgentPreset returns the default agent preset (Claude).
func DefaultAgentPreset() AgentPreset {
	return AgentClaude
}

// RuntimeConfigFromPreset creates a RuntimeConfig from a built-in agent preset.
// This provides the basic Command/Args/Env; additional fields from AgentPresetInfo
// can be accessed separately for extended functionality.
func RuntimeConfigFromPreset(preset AgentPreset) *RuntimeConfig {
	return builtinAgentRegistry.RuntimeConfigFromPreset(preset)
}

// RuntimeConfigFromPreset creates a RuntimeConfig from the preset registered
// in r under the given name, falling back to Claude defaults.
func (r *AgentRegistry) RuntimeConfigFromPreset(preset AgentPreset) *RuntimeConfig {
	return runtimeConfigFromAgentInfo(r, preset, r.Preset(string(preset)))
}

func runtimeConfigFromAgentInfo(reg *AgentRegistry, preset AgentPreset, info *AgentPresetInfo) *RuntimeConfig {
	if info == nil {
		// Fall back to Claude defaults
		return defaultRuntimeConfigIn(reg)
	}

	// Copy Env map to avoid mutation
	var envCopy map[string]string
	if len(info.Env) > 0 {
		envCopy = make(map[string]string, len(info.Env))
		for k, v := range info.Env {
			envCopy[k] = v
		}
	}

	rc := &RuntimeConfig{
		Provider: string(info.Name),
		Command:  info.Command,
		Args:     append([]string(nil), info.Args...),
		Env:      envCopy,
	}

	if preset == AgentClaude && rc.Command == "claude" {
		rc.Command = resolveClaudePath(reg.host())
	}

	return normalizeRuntimeConfigIn(reg, rc)
}

// BuildResumeCommand builds a command to resume a built-in agent's session.
// See (*AgentRegistry).BuildResumeCommand.
func BuildResumeCommand(agentName, sessionID string) string {
	return builtinAgentRegistry.BuildResumeCommand(agentName, sessionID)
}

// BuildResumeCommand builds a command to resume an agent session.
// Returns the full command string including any YOLO/autonomous flags.
// If sessionID is empty or the agent doesn't support resume, returns empty string.
func (r *AgentRegistry) BuildResumeCommand(agentName, sessionID string) string {
	if sessionID == "" {
		return ""
	}

	info := r.Preset(agentName)
	if info == nil || info.ResumeFlag == "" {
		return ""
	}

	// Build base command with args
	args := append([]string(nil), info.Args...)

	// e.g., "claude --dangerously-skip-permissions --resume <session_id>"
	args = append(args, info.ResumeFlag, sessionID)
	return info.Command + " " + strings.Join(args, " ")
}

// SupportsSessionResume checks if a built-in agent supports session resumption.
func SupportsSessionResume(agentName string) bool {
	return builtinAgentRegistry.SupportsSessionResume(agentName)
}

// SupportsSessionResume checks if an agent supports session resumption.
func (r *AgentRegistry) SupportsSessionResume(agentName string) bool {
	info := r.Preset(agentName)
	return info != nil && info.ResumeFlag != ""
}

// GetSessionIDEnvVar returns the environment variable name a built-in agent
// stores its session ID in. See (*AgentRegistry).SessionIDEnvVar.
func GetSessionIDEnvVar(agentName string) string {
	return builtinAgentRegistry.SessionIDEnvVar(agentName)
}

// SessionIDEnvVar returns the environment variable name for storing session IDs
// for a given agent. Returns empty string if the agent doesn't use env vars for this.
func (r *AgentRegistry) SessionIDEnvVar(agentName string) string {
	info := r.Preset(agentName)
	if info == nil {
		return ""
	}
	return info.SessionIDEnv
}

// GetProcessNames returns a built-in agent's process names.
// See (*AgentRegistry).ProcessNames.
func GetProcessNames(agentName string) []string {
	return builtinAgentRegistry.ProcessNames(agentName)
}

// ProcessNames returns the process names used to detect if an agent is running.
// Used by tmux.IsAgentRunning to check pane_current_command.
// Returns ["node", "claude"] (Claude, the default) if the agent is not found or
// has no ProcessNames.
func (r *AgentRegistry) ProcessNames(agentName string) []string {
	info := r.Preset(agentName)
	if info == nil || len(info.ProcessNames) == 0 {
		// Default to Claude's process names for backwards compatibility
		return []string{"node", "claude"}
	}
	return info.ProcessNames
}

// wrapperCommands lists process wrappers that exec into another binary listed
// in their arguments (e.g., `env -u VAR claude ...`). When the agent's Command
// is one of these, ResolveProcessNames must look past the wrapper to find the
// real agent binary in Args, otherwise liveness detection greps for a process
// named after the wrapper that no longer exists post-exec.
var wrapperCommands = map[string]bool{
	"env":    true,
	"nohup":  true,
	"setsid": true,
	"exec":   true,
	"sudo":   true,
	"doas":   true,
	"stdbuf": true,
	"time":   true,
}

// wrapperFlagsTakeValue returns the set of short flags that consume the
// next argument as a value, for the given wrapper. Used by extractWrappedBinary
// to skip "-flag value" pairs when scanning args for the real binary.
func wrapperFlagsTakeValue(wrapper string) map[string]bool {
	switch wrapper {
	case "env":
		// env: -u VAR (unset), -C DIR (chdir), -S STRING (split-string)
		return map[string]bool{"-u": true, "-C": true, "-S": true}
	case "sudo":
		// sudo: most options take a value; list the common ones
		return map[string]bool{
			"-u": true, "-g": true, "-h": true, "-p": true,
			"-U": true, "-A": true, "-c": true, "-r": true,
			"-t": true, "-T": true, "-D": true, "-R": true,
		}
	case "stdbuf":
		// stdbuf: -i, -o, -e all take a value (mode)
		return map[string]bool{"-i": true, "-o": true, "-e": true}
	}
	return nil
}

// lookupProcessNamesByBinary returns the ProcessNames of the preset whose
// Command (or its basename) matches realBin. Searches the registry first,
// then the canonical builtinPresets so shadowed builtins still resolve.
func (r *AgentRegistry) lookupProcessNamesByBinary(realBin string) []string {
	agents := r.agents()
	for _, name := range sortedPresetNames(agents) {
		preset := agents[name]
		if len(preset.ProcessNames) == 0 {
			continue
		}
		if preset.Command == realBin || filepath.Base(preset.Command) == realBin {
			return preset.ProcessNames
		}
	}
	builtins := builtinAgentRegistry.Agents
	for _, name := range sortedPresetNames(builtins) {
		preset := builtins[name]
		if len(preset.ProcessNames) == 0 {
			continue
		}
		if preset.Command == realBin || filepath.Base(preset.Command) == realBin {
			return preset.ProcessNames
		}
	}
	return nil
}

// extractWrappedBinary scans wrapper args for the first non-flag token that
// looks like a binary, and returns its basename. Returns "" if no real binary
// is found (e.g., args malformed or wrapper unsupported).
//
// Walks args left-to-right: skips short flags (and their values for known
// value-taking flags), long flags (--foo=bar or --foo), env-style VAR=value
// assignments (env only), and treats `--` as an end-of-options separator.
func extractWrappedBinary(wrapper string, args []string) string {
	flagsTakeValue := wrapperFlagsTakeValue(wrapper)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return filepath.Base(args[i+1])
			}
			return ""
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			// Long option `--foo=bar` carries its own value
			if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
				continue
			}
			// Long option `--foo` and unknown short options: assume no value
			if flagsTakeValue[a] && i+1 < len(args) {
				i++ // skip the flag's value
			}
			continue
		}
		// env permits VAR=value assignments interspersed with flags
		if wrapper == "env" && strings.Contains(a, "=") {
			continue
		}
		return filepath.Base(a)
	}
	return ""
}

// ResolveProcessNames resolves process names against the built-in presets.
// See (*AgentRegistry).ResolveProcessNames.
func ResolveProcessNames(agentName, command string, args ...string) []string {
	return builtinAgentRegistry.ResolveProcessNames(agentName, command, args...)
}

// ResolveProcessNames determines the correct process names for liveness detection
// given an agent name and the actual command binary. This handles custom agents
// that shadow built-in preset names (e.g., a custom "claude" agent that runs a
// wrapper script instead of the claude binary), and custom agents wrapped
// in process launchers like `env -u VAR <real-binary>`.
//
// args (variadic, optional) is the actual command-line argument slice for the
// agent invocation. When command is a wrapper (env, sudo, nohup, ...), the
// real binary is found in args, not on the registered preset's Args (which
// may belong to the canonical built-in preset, not the user's wrapper).
//
// Resolution order:
//  1. If agentName matches a built-in preset AND the preset's Command matches
//     the actual command → use the preset's ProcessNames (no mismatch).
//  2. If command is a known wrapper, scan args (or the registered preset's Args
//     as a fallback) for the real binary and resolve to its preset's ProcessNames.
//  3. Otherwise, find a built-in preset whose Command matches the actual command
//     and use its ProcessNames (custom agent using a known launcher).
//  4. If the command is unknown but agentName is a registered preset, return
//     the command basename unioned with the preset's ProcessNames (custom
//     wrapper script around a known agent, e.g. a script that execs claude).
//  5. Fallback: [command] (fully custom binary).
func (r *AgentRegistry) ResolveProcessNames(agentName, command string, args ...string) []string {
	agents := r.agents()

	// Normalize command to basename for comparison. Commands may be
	// path-resolved (e.g., "/home/user/.claude/local/claude" from
	// resolveClaudePath), but built-in presets store bare names ("claude").
	// Process matching (processMatchesNames, pgrep) also uses basenames.
	cmdBase := command
	if command != "" {
		cmdBase = filepath.Base(command)
	}
	unwrappedCmdBase := strings.TrimPrefix(cmdBase, "gt-")

	// Check if agentName matches a built-in/registered preset with matching command.
	// Compare against both the raw command and basename to handle registry entries
	// that store absolute-path commands (e.g., "/opt/bin/my-tool").
	info, infoOK := agents[agentName]
	if infoOK {
		if len(info.ProcessNames) > 0 &&
			(info.Command == command ||
				info.Command == cmdBase ||
				filepath.Base(info.Command) == cmdBase ||
				(info.Command == unwrappedCmdBase && strings.HasPrefix(cmdBase, "gt-")) ||
				cmdBase == "") {
			return info.ProcessNames
		}
	}

	// Wrapper case: command is `env`/`sudo`/etc. — find the real binary in
	// args (caller-supplied) or the registered preset's Args, and resolve to
	// THAT binary's preset ProcessNames. Caller args take precedence because
	// the registered preset may be the canonical built-in (not the wrapper).
	if wrapperCommands[cmdBase] {
		argSources := [][]string{args}
		if infoOK && len(info.Args) > 0 {
			argSources = append(argSources, info.Args)
		}
		for _, src := range argSources {
			realBin := extractWrappedBinary(cmdBase, src)
			if realBin == "" {
				continue
			}
			if names := r.lookupProcessNamesByBinary(realBin); len(names) > 0 {
				return names
			}
			return []string{realBin}
		}
	}

	// Agent name doesn't match or command differs — look up by command
	if cmdBase != "" {
		// Canonical-first, sorted: builtin groq-compound also runs "claude",
		// and map order must not decide which preset a binary belongs to.
		for _, name := range canonicalFirst(sortedPresetNames(agents), cmdBase, unwrappedCmdBase) {
			info := agents[name]
			if len(info.ProcessNames) == 0 {
				continue
			}
			if info.Command == command ||
				filepath.Base(info.Command) == cmdBase ||
				(strings.HasPrefix(cmdBase, "gt-") && filepath.Base(info.Command) == unwrappedCmdBase) {
				return info.ProcessNames
			}
		}
		// Unknown command but known agent name — the operator pointed a
		// registered agent at an unrecognized binary (e.g.,
		// agents.claude.command = ~/gt/bin/claude-trusted, a wrapper script
		// that execs the real claude). Post-exec no process carries the
		// wrapper's name, so liveness detection must also accept the named
		// preset's process names. Union covers both exec-style wrappers
		// (matches preset names) and wrappers that stay resident (matches
		// the basename).
		if infoOK && len(info.ProcessNames) > 0 {
			names := []string{cmdBase}
			for _, n := range info.ProcessNames {
				if n != cmdBase {
					names = append(names, n)
				}
			}
			return names
		}
		// Unknown command — use the binary basename itself
		return []string{cmdBase}
	}

	// No command provided, agent not in registry — Claude defaults
	return []string{"node", "claude"}
}

// IsKnownPreset checks if a string is a built-in agent preset name.
func IsKnownPreset(name string) bool {
	return builtinAgentRegistry.IsKnown(name)
}

// SaveAgentRegistry writes the agent registry to a file.
func SaveAgentRegistry(path string, registry *AgentRegistry) error {
	return WriteConfigJSON(path, registry, 0644)
}

// NewExampleAgentRegistry creates an example registry with comments.
func NewExampleAgentRegistry() *AgentRegistry {
	return &AgentRegistry{
		Version: CurrentAgentRegistryVersion,
		Agents: map[string]*AgentPresetInfo{
			// One example custom agent: the Claude CLI pointed at another backend.
			"claude-proxy": {
				Name:    "claude-proxy",
				Command: "claude",
				Args:    []string{"--dangerously-skip-permissions"},
				Env:     map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"},
			},
		},
	}
}
