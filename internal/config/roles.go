// Package config provides role configuration for Gas Town agents.
package config

import (
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed roles/*.toml
var defaultRolesFS embed.FS

// RoleDefinition contains all configuration for a role type.
// This replaces the role bead system with config files.
type RoleDefinition struct {
	// Role is the role identifier (polecat, crew).
	Role string `toml:"role"`

	// Scope is "town" or "rig" - determines where the agent runs.
	Scope string `toml:"scope"`

	// Session contains tmux session configuration.
	Session RoleSessionConfig `toml:"session"`

	// Env contains environment variables to set in the session.
	Env map[string]string `toml:"env,omitempty"`

	// Health contains health check configuration.
	Health RoleHealthConfig `toml:"health"`

	// Nudge is the initial prompt sent when starting the agent.
	Nudge string `toml:"nudge,omitempty"`

	// PromptTemplate is the name of the role's prompt template file.
	PromptTemplate string `toml:"prompt_template,omitempty"`
}

// RoleSessionConfig contains session-related configuration.
type RoleSessionConfig struct {
	// Pattern is the tmux session name pattern.
	// Supports placeholders: {rig}, {name}, {role}
	// Examples: "gt-{rig}-crew-{name}", "gt-{rig}-witness", "gt-{rig}-{name}"
	Pattern string `toml:"pattern"`

	// WorkDir is the working directory pattern.
	// Supports placeholders: {town}, {rig}, {name}, {role}
	// Examples: "{town}", "{town}/{rig}/witness"
	WorkDir string `toml:"work_dir"`

	// NeedsPreSync indicates if workspace needs git sync before starting.
	NeedsPreSync bool `toml:"needs_pre_sync"`

	// StartCommand is the command to run after creating the session.
	// Default: "exec claude --dangerously-skip-permissions"
	StartCommand string `toml:"start_command,omitempty"`
}

// RoleHealthConfig contains health check thresholds.
type RoleHealthConfig struct {
	// PingTimeout is how long to wait for a health check response.
	PingTimeout Duration `toml:"ping_timeout"`

	// ConsecutiveFailures is how many failed health checks before force-kill.
	ConsecutiveFailures int `toml:"consecutive_failures"`

	// KillCooldown is the minimum time between force-kills.
	KillCooldown Duration `toml:"kill_cooldown"`

	// StuckThreshold is how long a wisp can be in_progress before considered stuck.
	StuckThreshold Duration `toml:"stuck_threshold"`

	// HungSessionThreshold is how long a tmux session can be inactive before
	// considered hung. Overrides constants.HungSessionThreshold per role.
	// Zero means use the default from constants.
	HungSessionThreshold Duration `toml:"hung_session_threshold"`

	// SelfProbeBudget is how long the deacon patrol has to acknowledge a
	// DEACON_SELF_PROBE mail before it's judged late. It bounds one patrol
	// cycle, deliberately separate from PingTimeout, which is a network
	// health-check value (seconds) — see daemon.deaconSelfProbeBudget.
	SelfProbeBudget Duration `toml:"self_probe_budget"`
}

// Duration is a wrapper for time.Duration that supports TOML marshaling.
type Duration struct {
	time.Duration
}

// UnmarshalText implements encoding.TextUnmarshaler for Duration.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	d.Duration = parsed
	return nil
}

// MarshalText implements encoding.TextMarshaler for Duration.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// String returns the duration as a string.
func (d Duration) String() string {
	return d.Duration.String()
}

// AllRoles returns the list of all known role names.
func AllRoles() []string {
	return []string{"polecat", "crew"}
}

// TownRoles returns roles that operate at town scope. Empty: the town-scoped
// resident role is retired.
func TownRoles() []string {
	return nil
}

// RigRoles returns roles that operate at rig scope.
func RigRoles() []string {
	return []string{"polecat", "crew"}
}

// loadBuiltinRoleDefinition loads a role definition from embedded defaults.
func loadBuiltinRoleDefinition(roleName string) (*RoleDefinition, error) {
	data, err := defaultRolesFS.ReadFile("roles/" + roleName + ".toml")
	if err != nil {
		return nil, fmt.Errorf("role %s not found in defaults: %w", roleName, err)
	}

	var def RoleDefinition
	if err := toml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parsing role %s: %w", roleName, err)
	}

	return &def, nil
}

// ExpandPattern expands placeholders in a pattern string.
// Supported placeholders: {town}, {rig}, {name}, {role}, {prefix}
func ExpandPattern(pattern, townRoot, rig, name, role, prefix string) string {
	result := pattern
	result = strings.ReplaceAll(result, "{town}", townRoot)
	result = strings.ReplaceAll(result, "{rig}", rig)
	result = strings.ReplaceAll(result, "{name}", name)
	result = strings.ReplaceAll(result, "{role}", role)
	result = strings.ReplaceAll(result, "{prefix}", prefix)
	return result
}
