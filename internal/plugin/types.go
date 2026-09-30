// Package plugin provides plugin discovery and management for Gas Town.
//
// Plugins are periodic automation tasks that the daemon heartbeat dispatches;
// each plugin is defined by a plugin.md file with TOML frontmatter.
//
// Plugin locations:
//   - Town-level: ~/gt/plugins/ (universal, apply everywhere)
//   - Rig-level: <rig>/plugins/ (project-specific)
package plugin

// Plugin represents a discovered plugin definition.
type Plugin struct {
	// Name is the unique plugin identifier (from frontmatter).
	Name string `json:"name"`

	// Description is a human-readable description.
	Description string `json:"description"`

	// Version is the schema version (for future evolution).
	Version int `json:"version"`

	// Location indicates where the plugin was discovered.
	Location Location `json:"location"`

	// Path is the absolute path to the plugin directory.
	Path string `json:"path"`

	// RigName is set for rig-level plugins (empty for town-level).
	RigName string `json:"rig_name,omitempty"`

	// Gate defines when the plugin should run.
	Gate *Gate `json:"gate,omitempty"`

	// Tracking defines labels and digest settings.
	Tracking *Tracking `json:"tracking,omitempty"`

	// Execution defines timeout and notification settings.
	Execution *Execution `json:"execution,omitempty"`

	// Instructions is the markdown body (after frontmatter).
	Instructions string `json:"instructions,omitempty"`

	// HasRunScript is true when a run.sh exists alongside plugin.md; the
	// daemon runs it for a plugin whose [execution] type is "script".
	HasRunScript bool `json:"has_run_script,omitempty"`
}

// Location indicates where a plugin was discovered.
type Location string

const (
	// LocationTown indicates a town-level plugin (~/gt/plugins/).
	LocationTown Location = "town"

	// LocationRig indicates a rig-level plugin (<rig>/plugins/).
	LocationRig Location = "rig"
)

// Gate defines when a plugin should run.
type Gate struct {
	// Type is the gate type: cooldown, cron, condition, event, or manual.
	Type GateType `json:"type" toml:"type"`

	// Duration is for cooldown gates (e.g., "1h", "24h").
	Duration string `json:"duration,omitempty" toml:"duration,omitempty"`

	// Schedule is for cron gates (e.g., "0 9 * * *").
	Schedule string `json:"schedule,omitempty" toml:"schedule,omitempty"`

	// Check is for condition gates (command that returns exit 0 to run).
	Check string `json:"check,omitempty" toml:"check,omitempty"`

	// On is for event gates (e.g., "startup").
	On string `json:"on,omitempty" toml:"on,omitempty"`
}

// GateType is the type of gate that controls plugin execution.
type GateType string

const (
	// GateCooldown runs if enough time has passed since last run.
	GateCooldown GateType = "cooldown"

	// GateCron runs on a cron schedule.
	GateCron GateType = "cron"

	// GateCondition runs if a check command returns exit 0.
	GateCondition GateType = "condition"

	// GateEvent runs on specific events (startup, etc).
	GateEvent GateType = "event"

	// GateManual never auto-runs, must be triggered explicitly.
	GateManual GateType = "manual"
)

// Tracking defines how plugin runs are tracked.
type Tracking struct {
	// Labels are applied to execution wisps.
	Labels []string `json:"labels,omitempty" toml:"labels,omitempty"`

	// Digest indicates whether to include in daily digest.
	Digest bool `json:"digest" toml:"digest"`
}

// ExecutionType controls how a plugin is executed.
type ExecutionType string

const (
	// ExecTypeAgent is the default: markdown instructions for an agent. Nothing
	// runs them automatically since the dog pack was retired (gt-ckunw).
	ExecTypeAgent ExecutionType = "agent"

	// ExecTypeScript means a run.sh script is executed directly.
	ExecTypeScript ExecutionType = "script"

	// ExecTypeExecWrapper wraps session startup commands.
	// Instead of being run, the wrapper tokens are inserted
	// between `exec env VAR=val ...` and the agent binary in the startup command.
	// Example: ["exitbox", "run", "--profile=gastown-polecat", "--"]
	ExecTypeExecWrapper ExecutionType = "exec-wrapper"
)

// Execution defines plugin execution settings.
type Execution struct {
	// Type is the execution type: "agent" (default), "script", or "exec-wrapper".
	Type ExecutionType `json:"type,omitempty" toml:"type,omitempty"`

	// Timeout is the maximum execution time (e.g., "5m").
	Timeout string `json:"timeout,omitempty" toml:"timeout,omitempty"`

	// NotifyOnFailure escalates on failure.
	NotifyOnFailure bool `json:"notify_on_failure" toml:"notify_on_failure"`

	// Severity is the escalation severity on failure.
	Severity string `json:"severity,omitempty" toml:"severity,omitempty"`

	// Wrapper is the command tokens for exec-wrapper plugins.
	// These are inserted between the env vars and the agent command at session startup.
	// Example: ["exitbox", "run", "--profile=gastown-polecat", "--"]
	// Only used when Type is "exec-wrapper".
	Wrapper []string `json:"wrapper,omitempty" toml:"wrapper,omitempty"`

	// AllowDeferredExit opts a script plugin into exit code 3 meaning
	// "deferred: nothing accomplished, retry on the next heartbeat, write no
	// run record" instead of an ordinary failure. It defaults to false: exit 3
	// is otherwise just another nonzero exit, recorded as a failure and
	// escalated like any other. Without a per-plugin opt-in, every
	// script plugin would share one exit code's meaning, so a plugin that
	// happens to exit 3 for an unrelated reason (a shell builtin, a tool it
	// shells out to) would have a real failure silently swallowed as a
	// deferral (gt-oqbw).
	AllowDeferredExit bool `json:"allow_deferred_exit,omitempty" toml:"allow_deferred_exit,omitempty"`
}

// PluginFrontmatter represents the TOML frontmatter in plugin.md files.
type PluginFrontmatter struct {
	Name        string     `toml:"name"`
	Description string     `toml:"description"`
	Version     int        `toml:"version"`
	Gate        *Gate      `toml:"gate,omitempty"`
	Tracking    *Tracking  `toml:"tracking,omitempty"`
	Execution   *Execution `toml:"execution,omitempty"`
}

// IsExecWrapper returns true if this plugin is an exec-wrapper type.
func (p *Plugin) IsExecWrapper() bool {
	return p.Execution != nil && p.Execution.Type == ExecTypeExecWrapper
}

// ExecWrapperArgs returns the wrapper command tokens for an exec-wrapper plugin.
// Returns nil if the plugin is not an exec-wrapper or has no wrapper configured.
func (p *Plugin) ExecWrapperArgs() []string {
	if !p.IsExecWrapper() || len(p.Execution.Wrapper) == 0 {
		return nil
	}
	return p.Execution.Wrapper
}

// PluginSummary provides a concise overview of a plugin.
type PluginSummary struct {
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Location      Location      `json:"location"`
	RigName       string        `json:"rig_name,omitempty"`
	GateType      GateType      `json:"gate_type,omitempty"`
	ExecutionType ExecutionType `json:"execution_type,omitempty"`
	Path          string        `json:"path"`
}

// Summary returns a PluginSummary for this plugin.
func (p *Plugin) Summary() PluginSummary {
	var gateType GateType
	if p.Gate != nil {
		gateType = p.Gate.Type
	} else {
		gateType = GateManual
	}

	var execType ExecutionType
	if p.Execution != nil && p.Execution.Type != "" {
		execType = p.Execution.Type
	}

	return PluginSummary{
		Name:          p.Name,
		Description:   p.Description,
		Location:      p.Location,
		RigName:       p.RigName,
		GateType:      gateType,
		ExecutionType: execType,
		Path:          p.Path,
	}
}
