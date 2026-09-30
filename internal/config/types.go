// Package config provides configuration types and serialization for Gas Town.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// TownConfig represents the main town identity (mayor/town.json).
type TownConfig struct {
	Type       string    `json:"type"`                  // "town"
	Version    int       `json:"version"`               // schema version
	Name       string    `json:"name"`                  // town identifier (internal)
	Owner      string    `json:"owner,omitempty"`       // owner email (entity identity)
	PublicName string    `json:"public_name,omitempty"` // public display name
	CreatedAt  time.Time `json:"created_at"`
}

// MayorConfig represents town-level behavioral configuration (mayor/config.json).
// This is separate from TownConfig (identity) to keep configuration concerns distinct.
type MayorConfig struct {
	Type    string           `json:"type"`             // "mayor-config"
	Version int              `json:"version"`          // schema version
	Theme   *TownThemeConfig `json:"theme,omitempty"`  // global theme settings
	Daemon  *DaemonConfig    `json:"daemon,omitempty"` // daemon settings
	// Deacon is retired with the deacon role (gt-4k3fj.6.1): nothing reads
	// it; it is declared so an existing mayor/config.json still decodes, and
	// kept verbatim across rewrites.
	Deacon          json.RawMessage `json:"deacon,omitempty"`
	DefaultCrewName string          `json:"default_crew_name,omitempty"` // default crew name for new rigs
}

// CurrentTownSettingsVersion is the current schema version for TownSettings.
const CurrentTownSettingsVersion = 1

// TownSettings represents town-level behavioral configuration (settings/config.json).
// This contains agent configuration that applies to all rigs unless overridden.
type TownSettings struct {
	Type    string `json:"type"`    // "town-settings"
	Version int    `json:"version"` // schema version

	// CLITheme controls CLI output color scheme.
	// Values: "dark", "light", "auto" (default).
	// "auto" lets the terminal emulator's background color guide the choice.
	// Can be overridden by GT_THEME environment variable.
	CLITheme string `json:"cli_theme,omitempty"`

	// DefaultAgent is the name of the agent preset to use by default.
	// Can be a built-in preset ("claude", "gemini", "codex", "cursor", "auggie", "amp", "opencode", "copilot")
	// or a custom agent name defined in settings/agents.json.
	// Default: "claude"
	DefaultAgent string `json:"default_agent,omitempty"`

	// Agents defines custom agent configurations or overrides.
	// Keys are agent names that can be referenced by DefaultAgent or rig settings.
	// Values override or extend the built-in presets.
	// Example: {"gemini": {"command": "/custom/path/to/gemini"}}
	Agents map[string]*RuntimeConfig `json:"agents,omitempty"`

	// RoleAgents maps role names to agent aliases for per-role model selection.
	// Keys are role names: "mayor", "deacon", "witness", "refinery", "polecat", "crew".
	// Values are agent names (built-in presets or custom agents defined in Agents).
	// This allows cost optimization by using different models for different roles.
	// Example: {"mayor": "claude-opus", "polecat": "claude-sonnet"}
	RoleAgents map[string]string `json:"role_agents,omitempty"`

	// PolecatPool, when set, lets `gt sling` choose a polecat's agent from a
	// bounded local-model pool instead of role_agents.polecat: up to MaxLocal
	// live polecat sessions run LocalAgent, new local spawns are at least
	// MinSpawnGap apart (a fresh session's prefill starves every decoding
	// slot, so spawns are staggered), and everything else runs OverflowAgent
	// (empty = the role default). An explicit --agent always wins.
	PolecatPool *PolecatPool `json:"polecat_pool,omitempty"`

	// CrewAgents maps individual crew worker names to agent aliases at the town level.
	// This allows town-wide per-crew agent assignment without modifying each rig's config.
	// Resolution: --agent flag > rig WorkerAgents > town CrewAgents > role agents > defaults.
	// Example: {"bob": "codex", "alice": "claude"}
	CrewAgents map[string]string `json:"crew_agents,omitempty"`

	// AgentEmailDomain is the domain used for agent git identity emails.
	// Agent addresses like "gastown/crew/jack" become "gastown.crew.jack@{domain}".
	// Default: "gastown.local"
	AgentEmailDomain string `json:"agent_email_domain,omitempty"`

	// FeedCurator configures event deduplication and aggregation windows.
	FeedCurator *FeedCuratorConfig `json:"feed_curator,omitempty"`

	// Convoy configures convoy behavior settings.
	Convoy *ConvoyConfig `json:"convoy,omitempty"`

	// RoleEffort maps role names to effort levels for per-role effort configuration.
	// Keys are role names: "mayor", "polecat", "crew". Keys for retired roles
	// ("deacon", "witness", "refinery", "boot", "dog") are accepted and ignored.
	// Values are effort levels: "low", "medium", "high", "max".
	// Allows cost/speed optimization by using lower effort for simpler roles.
	// Managed by cost-tier presets alongside RoleAgents.
	RoleEffort map[string]string `json:"role_effort,omitempty"`

	// CostTier tracks which cost tier preset was applied (informational).
	// Actual model assignments live in RoleAgents and Agents.
	// Values: "standard", "economy", "budget", or empty for custom configs.
	CostTier string `json:"cost_tier,omitempty"`

	// Scheduler configures the capacity scheduler for polecat dispatch.
	Scheduler *capacity.SchedulerConfig `json:"scheduler,omitempty"`

	// Polecat configures per-polecat behavior (target/ clean hook, etc.).
	// Added for hq-x0v7v.
	Polecat *PolecatConfig `json:"polecat,omitempty"`

	// Operational configures operational thresholds (timeouts, retries, intervals).
	// These were previously hardcoded as Go constants throughout the codebase.
	// All values are optional — omitted values use compiled-in defaults.
	Operational *OperationalConfig `json:"operational,omitempty"`

	// DisabledPatrols lists patrol names to disable at the town level.
	// This provides a simple way to turn off individual daemon patrol dogs
	// without editing mayor/daemon.json. Patrol names match the keys used
	// in daemon.json patrols section (e.g., "deacon", "witness", "refinery",
	// "doctor_dog", "compactor_dog", "checkpoint_dog", "wisp_reaper",
	// "dolt_backup", "jsonl_git_backup", "scheduled_maintenance",
	// "main_branch_test", "landing_worker", "events_prune", "handler").
	// Example: ["doctor_dog", "compactor_dog"]
	DisabledPatrols []string `json:"disabled_patrols,omitempty"`
}

// NewTownSettings creates a new TownSettings with defaults.
func NewTownSettings() *TownSettings {
	return &TownSettings{
		Type:         "town-settings",
		Version:      CurrentTownSettingsVersion,
		DefaultAgent: "claude",
		Agents:       make(map[string]*RuntimeConfig),
		RoleAgents:   make(map[string]string),
	}
}

// FeedCuratorConfig configures event deduplication and aggregation windows.
type FeedCuratorConfig struct {
	// DoneDedupeWindow is the time window for deduplicating repeated done events.
	// Default: "10s".
	DoneDedupeWindow string `json:"done_dedupe_window,omitempty"`
	// SlingAggregateWindow is the time window for aggregating sling events.
	// Default: "30s".
	SlingAggregateWindow string `json:"sling_aggregate_window,omitempty"`
	// MinAggregateCount is the minimum number of events to trigger aggregation.
	// Default: 3.
	MinAggregateCount int `json:"min_aggregate_count,omitempty"`
}

// DefaultFeedCuratorConfig returns a FeedCuratorConfig with sensible defaults.
func DefaultFeedCuratorConfig() *FeedCuratorConfig {
	return &FeedCuratorConfig{
		DoneDedupeWindow:     "10s",
		SlingAggregateWindow: "30s",
		MinAggregateCount:    3,
	}
}

// OperationalConfig groups operational thresholds that were previously hardcoded
// as Go constants. All fields are optional — omitted values use compiled-in defaults.
// This enables per-town tuning without code changes (ZFC: Zero Fixed Constants).
type OperationalConfig struct {
	// Session configures session management thresholds.
	Session *SessionThresholds `json:"session,omitempty"`

	// Nudge configures nudge delivery thresholds.
	Nudge *NudgeThresholds `json:"nudge,omitempty"`

	// Daemon configures daemon lifecycle thresholds.
	Daemon *DaemonThresholds `json:"daemon,omitempty"`

	// Deacon is retired with the deacon role (gt-4k3fj.6.1): nothing reads
	// it; it is declared so a settings file that still carries it decodes,
	// and kept verbatim across rewrites.
	Deacon json.RawMessage `json:"deacon,omitempty"`

	// Polecat, Dolt and Web have no reader (gt-e2kxa): the code uses its own
	// constants. They are declared so a settings file that still carries
	// them decodes, and kept verbatim across rewrites.
	Polecat json.RawMessage `json:"polecat,omitempty"`
	Dolt    json.RawMessage `json:"dolt,omitempty"`
	Web     json.RawMessage `json:"web,omitempty"`

	// Mail configures mail system thresholds.
	Mail *MailThresholds `json:"mail,omitempty"`

	// Recovery configures stalled-polecat recovery thresholds. The key is
	// still "witness": the retired witness role owned these knobs, and the
	// live settings/config.json carries them under that name.
	Recovery *RecoveryThresholds `json:"witness,omitempty"`

	// ContainerGate sizes the town-level container-suite gate pool
	// (internal/slot): how many Docker-backed suites may run at once on the
	// host's Docker VM and how many of those slots are reserved for gate-
	// class callers (refinery, batch gate, main-branch test).
	ContainerGate *ContainerGateThresholds `json:"container_gate,omitempty"`
}

// ContainerGateThresholds configures the container-gate slot pool.
type ContainerGateThresholds struct {
	// Slots is the total number of concurrently held container-gate slots
	// (default 1: one Docker-backed suite at a time townwide).
	Slots *int `json:"slots,omitempty"`
	// ReservedForGate is how many of the lowest slots only the refinery,
	// the batch gate and the daemon's main-branch test may take, so the
	// merge path never waits behind polecat pre-verify suites (default 0).
	// Clamped to Slots-1; ignored when Slots is 1.
	ReservedForGate *int `json:"reserved_for_gate,omitempty"`
	// YieldToGate makes a new non-gate suite (crew, polecat) wait while a
	// gate holds a gate-reserved slot, so the merge gate never shares the
	// machine with a suite that started after it. Running suites are never
	// preempted and gates never yield (default true, gt-22hdp.29).
	YieldToGate *bool `json:"yield_to_gate,omitempty"`
	// MaxGateYield caps how long one non-gate acquisition yields to running
	// gates in total, so a hung gate or back-to-back gates cannot starve it
	// (default "30m").
	MaxGateYield string `json:"max_gate_yield,omitempty"`
}

// SessionThresholds configures session management timeouts.
type SessionThresholds struct {
	// StartupNudgeVerifyDelay is wait after startup nudge before checking (default "5s").
	StartupNudgeVerifyDelay string `json:"startup_nudge_verify_delay,omitempty"`

	// StartupNudgeMaxRetries is max retries for startup nudge (default 3).
	StartupNudgeMaxRetries *int `json:"startup_nudge_max_retries,omitempty"`

	// The keys below have no reader (gt-e2kxa): the code uses the
	// internal/constants values. They are declared so a settings file that
	// still carries them decodes.
	ClaudeStartTimeout      string `json:"claude_start_timeout,omitempty"`
	ShellReadyTimeout       string `json:"shell_ready_timeout,omitempty"`
	GracefulShutdownTimeout string `json:"graceful_shutdown_timeout,omitempty"`
	BdCommandTimeout        string `json:"bd_command_timeout,omitempty"`
	BdSubprocessTimeout     string `json:"bd_subprocess_timeout,omitempty"`
	GUPPViolationTimeout    string `json:"gupp_violation_timeout,omitempty"`
	HungSessionThreshold    string `json:"hung_session_threshold,omitempty"`
}

// NudgeThresholds configures nudge queue and delivery timeouts.
type NudgeThresholds struct {
	// MaxQueueDepth is max pending nudges per session (default 50).
	MaxQueueDepth *int `json:"max_queue_depth,omitempty"`

	// StaleClaimThreshold is how long a .claimed file must be untouched
	// before treated as orphan (default "5m").
	StaleClaimThreshold string `json:"stale_claim_threshold,omitempty"`

	// MaxDeliveryAttempts caps how many times a nudge may be requeued after a
	// failed injection before it is dropped (default 3). Bounds the
	// re-injection loop described in gt-tmlu.
	MaxDeliveryAttempts *int `json:"max_delivery_attempts,omitempty"`

	// RequeueBackoff is the minimum delay before a requeued nudge becomes
	// eligible for delivery again (default "30s"). Spaces out retries so a
	// persistently failing injection cannot re-inject at the poll interval.
	RequeueBackoff string `json:"requeue_backoff,omitempty"`

	// The keys below have no reader (gt-e2kxa): the code uses the
	// internal/constants and internal/nudge values. They are declared so a
	// settings file that still carries them decodes.
	ReadyTimeout  string `json:"ready_timeout,omitempty"`
	RetryInterval string `json:"retry_interval,omitempty"`
	LockTimeout   string `json:"lock_timeout,omitempty"`
	NormalTTL     string `json:"normal_ttl,omitempty"`
	UrgentTTL     string `json:"urgent_ttl,omitempty"`
}

// DaemonThresholds configures daemon lifecycle and patrol thresholds.
type DaemonThresholds struct {
	// DogIdleSessionTimeout, DogIdleRemoveTimeout, StaleWorkingTimeout and
	// MaxDogPoolSize are retired with the dog pack (gt-ckunw): nothing reads
	// them. They are declared so a config that still carries them decodes.
	DogIdleSessionTimeout json.RawMessage `json:"dog_idle_session_timeout,omitempty"`
	DogIdleRemoveTimeout  json.RawMessage `json:"dog_idle_remove_timeout,omitempty"`
	StaleWorkingTimeout   json.RawMessage `json:"stale_working_timeout,omitempty"`
	MaxDogPoolSize        json.RawMessage `json:"max_dog_pool_size,omitempty"`

	// PolecatIdleSessionTimeout is how long a polecat can be idle before its session
	// is killed to prevent API slot burn (default "15m"). Polecats are ephemeral workers
	// and should not persist when idle.
	PolecatIdleSessionTimeout string `json:"polecat_idle_session_timeout,omitempty"`

	// PolecatSelfTerminate controls whether polecats kill their own session after
	// gt done completes (default false). When true, polecats terminate 3 seconds
	// after work submission instead of transitioning to IDLE. This gives fresh
	// context windows per task, reduces token waste, and eliminates stale state
	// issues at scale. Worktree reuse is preserved — ReuseIdlePolecat creates
	// a fresh branch on the existing worktree.
	PolecatSelfTerminate *bool `json:"polecat_self_terminate,omitempty"`

	// RecoveryHeartbeatInterval is the fixed interval for recovery-focused daemon heartbeat (default "3m").
	RecoveryHeartbeatInterval string `json:"recovery_heartbeat_interval,omitempty"`

	// The boot and deacon keys are retired with those roles (gt-4k3fj.6.1):
	// nothing reads them; they are declared so a settings file that still
	// carries them decodes.
	BootSpawnCooldown   string `json:"boot_spawn_cooldown,omitempty"`
	BootTurnBudget      string `json:"boot_turn_budget,omitempty"`
	BootIdleSuppression string `json:"boot_idle_suppression,omitempty"`
	BootMode            string `json:"boot_mode,omitempty"`
	DeaconGracePeriod   string `json:"deacon_grace_period,omitempty"`

	// The keys below have no reader (gt-e2kxa): the daemon uses its own
	// constants. They are declared so a settings file that still carries
	// them decodes.
	MassDeathWindow                string `json:"mass_death_window,omitempty"`
	MassDeathThreshold             *int   `json:"mass_death_threshold,omitempty"`
	MaxLifecycleMessageAge         string `json:"max_lifecycle_message_age,omitempty"`
	SyncFailureEscalationThreshold *int   `json:"sync_failure_escalation_threshold,omitempty"`
	DoctorMolCooldown              string `json:"doctor_mol_cooldown,omitempty"`

	// PressureCPUThreshold is the per-core load average above which new
	// non-infrastructure spawns are deferred. Disabled by default (0).
	// Recommended starting value: 3.0 (only trips under severe load).
	PressureCPUThreshold *float64 `json:"pressure_cpu_threshold,omitempty"`

	// PressureMemThresholdGB is the minimum available memory (in GB) below
	// which new non-infrastructure spawns are deferred. Disabled by default (0).
	// Recommended starting value: 0.5 (only trips when swapping).
	PressureMemThresholdGB *float64 `json:"pressure_mem_threshold_gb,omitempty"`

	// PressureMaxSessions is the maximum number of concurrent agent tmux
	// sessions before new non-infrastructure spawns are deferred. Disabled by default (0 = unlimited).
	PressureMaxSessions *int `json:"pressure_max_sessions,omitempty"`
}

// MailThresholds configures mail system thresholds.
type MailThresholds struct {
	// ReplyReminderDelay is how long after mail delivery to nudge the recipient
	// to reply via gt mail send rather than in chat (default "30s").
	// Set to "0s" to disable reply reminders entirely.
	ReplyReminderDelay string `json:"reply_reminder_delay,omitempty"`

	// The keys below have no reader (gt-e2kxa). They are declared so a
	// settings file that still carries them decodes.
	IdleNotifyTimeout   string `json:"idle_notify_timeout,omitempty"`
	BdReadTimeout       string `json:"bd_read_timeout,omitempty"`
	BdWriteTimeout      string `json:"bd_write_timeout,omitempty"`
	MaxConcurrentAckOps *int   `json:"max_concurrent_ack_ops,omitempty"`
}

// RecoveryThresholds configures stalled-polecat recovery thresholds.
type RecoveryThresholds struct {
	// MaxBeadRespawns is the threshold above which a bead respawn is blocked
	// and escalated to mayor instead of re-dispatched (default 3).
	MaxBeadRespawns *int `json:"max_bead_respawns,omitempty"`

	// HeartbeatStartupGrace is how long after session creation a live polecat
	// with assigned work but no heartbeat file counts as possibly stuck at
	// startup (e.g., auth 401 blocking initialization, default "5m").
	HeartbeatStartupGrace string `json:"heartbeat_startup_grace,omitempty"`

	// The keys below belonged to the retired witness patrol (gt-4k3fj.6.1):
	// nothing reads them; they are declared so a settings file that still
	// carries them decodes.
	StartupStallThreshold  string `json:"startup_stall_threshold,omitempty"`
	StartupActivityGrace   string `json:"startup_activity_grace,omitempty"`
	DoneIntentStuckTimeout string `json:"done_intent_stuck_timeout,omitempty"`
	DoneIntentRecentGrace  string `json:"done_intent_recent_grace,omitempty"`
	DoneIntentMaxAge       string `json:"done_intent_max_age,omitempty"`
	ComposerStallFrozenFor string `json:"composer_stall_frozen_for,omitempty"`
}

// DefaultOperationalConfig returns an OperationalConfig with all defaults.
func DefaultOperationalConfig() *OperationalConfig {
	return &OperationalConfig{}
}

// ConvoyConfig configures convoy behavior settings.
type ConvoyConfig struct {
	// NotifyOnComplete controls whether convoy completion pushes a notification
	// into the active Mayor session (in addition to mail). Opt-in; default false.
	NotifyOnComplete bool `json:"notify_on_complete,omitempty"`
}

// PolecatConfig configures per-polecat behavior. Added for hq-x0v7v
// (target/ clean hook on reuse).
type PolecatConfig struct {
	// TargetCleanPolicy controls when the daemon deletes <polecat>/target/
	// before reusing an idle polecat for a new bead.
	// Values: "per_bead" (default), "every_n_beads:<N>", "never".
	// Parsed by polecat.ParseTargetCleanPolicy.
	TargetCleanPolicy string `json:"target_clean_policy,omitempty"`
}

// ParseDurationOrDefault parses a Go duration string, returning fallback on error or empty input.
func ParseDurationOrDefault(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// DaemonConfig represents daemon process settings.
type DaemonConfig struct {
	HeartbeatInterval string `json:"heartbeat_interval,omitempty"` // e.g., "30s"
	PollInterval      string `json:"poll_interval,omitempty"`      // e.g., "10s"
}

// PatrolConfig represents a single patrol configuration.
type PatrolConfig struct {
	Enabled  bool     `json:"enabled"`            // whether this patrol is enabled
	Interval string   `json:"interval,omitempty"` // e.g., "5m"
	Agent    string   `json:"agent,omitempty"`    // agent that runs this patrol
	Rigs     []string `json:"rigs,omitempty"`     // rigs this patrol manages (empty = all)
}

// CurrentDaemonPatrolConfigVersion is the current schema version for DaemonPatrolConfig.
const CurrentDaemonPatrolConfigVersion = 1

// DaemonPatrolConfigFileName is the filename for daemon patrol configuration.
const DaemonPatrolConfigFileName = "daemon.json"

// NewDaemonPatrolConfig creates a new DaemonPatrolConfig with sensible defaults.
func NewDaemonPatrolConfig() *DaemonPatrolConfig {
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: CurrentDaemonPatrolConfigVersion,
		Heartbeat: &PatrolConfig{
			Enabled:  true,
			Interval: "3m",
		},
		Patrols: &PatrolsConfig{
			// The patrol_scan tick restarts dead polecats; a new town has no
			// other path that does (ADR 0005).
			PatrolScan: &PatrolScanConfig{Enabled: true},
		},
	}
}

// CurrentMayorConfigVersion is the current schema version for MayorConfig.
const CurrentMayorConfigVersion = 1

// DefaultCrewName is the default name for crew workspaces when not overridden.
const DefaultCrewName = "max"

// RigsConfig represents the rigs registry (mayor/rigs.json).
type RigsConfig struct {
	Version int                 `json:"version"`
	Rigs    map[string]RigEntry `json:"rigs"`
}

// RigEntry represents a single rig in the registry.
type RigEntry struct {
	GitURL      string       `json:"git_url"`
	PushURL     string       `json:"push_url,omitempty"`
	UpstreamURL string       `json:"upstream_url,omitempty"` // optional upstream URL (for fork workflows)
	LocalRepo   string       `json:"local_repo,omitempty"`
	AddedAt     time.Time    `json:"added_at"`
	BeadsConfig *BeadsConfig `json:"beads,omitempty"`
}

// BeadsConfig represents beads configuration for a rig.
type BeadsConfig struct {
	Repo   string `json:"repo"`   // "local" | path | git-url
	Prefix string `json:"prefix"` // issue prefix
}

// CurrentTownVersion is the current schema version for TownConfig.
// Version 2: Added Owner and PublicName fields for federation identity.
const CurrentTownVersion = 2

// CurrentRigsVersion is the current schema version for RigsConfig.
const CurrentRigsVersion = 1

// CurrentRigConfigVersion is the current schema version for RigConfig.
const CurrentRigConfigVersion = 1

// CurrentRigSettingsVersion is the current schema version for RigSettings.
const CurrentRigSettingsVersion = 1

// RigConfig represents per-rig identity (rig/config.json).
// This contains only identity - behavioral config is in settings/config.json.
type RigConfig struct {
	Type        string       `json:"type"`                   // "rig"
	Version     int          `json:"version"`                // schema version
	Name        string       `json:"name"`                   // rig name
	GitURL      string       `json:"git_url"`                // git repository URL
	PushURL     string       `json:"push_url,omitempty"`     // optional push URL (fork for read-only upstreams)
	UpstreamURL string       `json:"upstream_url,omitempty"` // optional upstream URL (for fork workflows)
	LocalRepo   string       `json:"local_repo,omitempty"`
	CreatedAt   time.Time    `json:"created_at"` // when the rig was created
	Beads       *BeadsConfig `json:"beads,omitempty"`

	// The fields below are written by internal/rig (rig.RigConfig) into the
	// same file. They are declared here so strict decoding accepts the file
	// the rig manager writes (gt-y3pgh.1); the two types merge in gt-y3pgh.2.
	DefaultBranch string            `json:"default_branch,omitempty"`
	MergeQueue    *MergeQueueConfig `json:"merge_queue,omitempty"`
	// Witness is retired with the witness role (gt-4k3fj.6.1): nothing reads
	// it; it is declared so a rig config.json that still carries it decodes,
	// and kept verbatim across rewrites.
	Witness         json.RawMessage `json:"witness,omitempty"`
	PolecatPoolSize int             `json:"polecat_pool_size,omitempty"`
	PolecatNames    []string        `json:"polecat_names,omitempty"`
}

// WorkflowConfig represents workflow settings for a rig.
type WorkflowConfig struct {
	// DefaultFormula is the formula to use when `gt formula run` is called without arguments.
	// If empty, no default is set and a formula name must be provided.
	DefaultFormula string `json:"default_formula,omitempty"`
}

// RigSettings represents per-rig behavioral configuration (settings/config.json).
type RigSettings struct {
	Type       string            `json:"type"`                  // "rig-settings"
	Version    int               `json:"version"`               // schema version
	MergeQueue *MergeQueueConfig `json:"merge_queue,omitempty"` // merge queue settings
	Theme      *ThemeConfig      `json:"theme,omitempty"`       // tmux theme settings
	Namepool   *NamepoolConfig   `json:"namepool,omitempty"`    // polecat name pool settings
	Crew       *CrewConfig       `json:"crew,omitempty"`        // crew startup settings
	Workflow   *WorkflowConfig   `json:"workflow,omitempty"`    // workflow settings
	Runtime    *RuntimeConfig    `json:"runtime,omitempty"`     // LLM runtime settings (deprecated: use Agent)

	// Agent selects which agent preset to use for this rig.
	// Can be a built-in preset ("claude", "gemini", "codex", "cursor", "auggie", "amp", "opencode", "copilot")
	// or a custom agent defined in settings/agents.json.
	// If empty, uses the town's default_agent setting.
	// Takes precedence over Runtime if both are set.
	Agent string `json:"agent,omitempty"`

	// Agents defines custom agent configurations or overrides for this rig.
	// Similar to TownSettings.Agents but applies to this rig only.
	// Allows per-rig custom agents for polecats and crew members.
	Agents map[string]*RuntimeConfig `json:"agents,omitempty"`

	// RoleAgents maps role names to agent aliases for per-role model selection.
	// Keys are role names: "witness", "refinery", "polecat", "crew".
	// Values are agent names (built-in presets or custom agents).
	// Overrides TownSettings.RoleAgents for this specific rig.
	// Example: {"crew": "claude-haiku", "polecat": "claude-sonnet"}
	RoleAgents map[string]string `json:"role_agents,omitempty"`

	// WorkerAgents maps individual crew worker names to agent aliases.
	// Allows per-worker agent selection, overriding RoleAgents["crew"].
	// Takes precedence over RoleAgents["crew"] but is overridden by explicit --agent flags.
	// Example: {"denali": "codex", "glacier": "gemini"}
	WorkerAgents map[string]string `json:"worker_agents,omitempty"`

	// RoleEffort maps role names to effort levels, overriding TownSettings.RoleEffort for this rig.
	// Keys are role names: "witness", "refinery", "polecat", "crew".
	// Values are effort levels: "low", "medium", "high", "max".
	// Example: {"crew": "max", "witness": "low"}
	RoleEffort map[string]string `json:"role_effort,omitempty"`
}

// CrewConfig represents crew workspace settings for a rig.
type CrewConfig struct {
	// Startup is a natural language instruction for which crew to start on boot.
	// Interpreted by AI during startup. Examples:
	//   "max"                    - start only max
	//   "joe and max"            - start joe and max
	//   "all"                    - start all crew members
	//   "pick one"               - start any one crew member
	//   "none"                   - don't auto-start any crew
	//   "max, but not emma"      - start max, skip emma
	// If empty, defaults to starting no crew automatically.
	Startup string `json:"startup,omitempty"`
}

// RuntimeConfig represents LLM runtime configuration for agent sessions.
// This allows switching between different LLM backends (claude, aider, etc.)
// without modifying startup code.
type RuntimeConfig struct {
	// Provider selects runtime-specific defaults and integration behavior.
	// Known values: "claude", "codex", "generic". Default: "claude".
	Provider string `json:"provider,omitempty"`

	// Command is the CLI command to invoke (e.g., "claude", "aider").
	// Default: "claude"
	Command string `json:"command,omitempty"`

	// Args are additional command-line arguments.
	// Default: ["--dangerously-skip-permissions"] for built-in agents.
	// Empty array [] means no args (not "use defaults").
	Args []string `json:"args"`

	// Env are environment variables to set when starting the agent.
	// These are merged with the standard GT_* variables.
	// Used for agent-specific configuration like OPENCODE_PERMISSION.
	Env map[string]string `json:"env,omitempty"`

	// InitialPrompt is an optional first message to send after startup.
	// For claude, this is passed as the prompt argument.
	// Empty by default (hooks handle context).
	InitialPrompt string `json:"initial_prompt,omitempty"`

	// PromptMode controls how prompts are passed to the runtime.
	// Supported values: "arg" (append prompt arg), "none" (ignore prompt).
	// Default: "arg" for built-in interactive runtimes.
	PromptMode string `json:"prompt_mode,omitempty"`

	// Session config controls environment integration for runtime session IDs.
	Session *RuntimeSessionConfig `json:"session,omitempty"`

	// Hooks config controls runtime hook installation (if supported).
	Hooks *RuntimeHooksConfig `json:"hooks,omitempty"`

	// Tmux config controls process detection and readiness heuristics.
	Tmux *RuntimeTmuxConfig `json:"tmux,omitempty"`

	// Instructions controls the per-workspace instruction file name.
	Instructions *RuntimeInstructionsConfig `json:"instructions,omitempty"`

	// ACP configures ACP (Agent Communication Protocol) support.
	// When set, the agent can run in ACP mode. If nil, ACP support is
	// determined by matching the Command to a known preset with ACP config.
	ACP *ACPConfig `json:"acp,omitempty"`

	// ExecWrapper is a command prefix inserted between environment variables
	// and the agent binary in the startup command. Used for sandboxed execution.
	// Example: ["exitbox", "run", "--profile=gastown-polecat", "--"]
	// Produces: exec env VAR=val ... exitbox run --profile=gastown-polecat -- claude ...
	ExecWrapper []string `json:"exec_wrapper,omitempty"`

	// ResolvedAgent is the agent name that was resolved during config lookup.
	// Set by ResolveRoleAgentConfig / resolveAgentConfigInternal so that
	// BuildStartupCommand can export GT_AGENT for process detection.
	// Not serialized — this is a runtime-only field.
	ResolvedAgent string `json:"-"`
}

// RuntimeSessionConfig configures how Gas Town discovers runtime session IDs.
type RuntimeSessionConfig struct {
	// SessionIDEnv is the environment variable set by the runtime to identify a session.
	// Default: "CLAUDE_SESSION_ID" for claude, empty for codex/generic.
	SessionIDEnv string `json:"session_id_env,omitempty"`

	// ConfigDirEnv is the environment variable that selects a runtime account/config dir.
	// Default: "CLAUDE_CONFIG_DIR" for claude, empty for codex/generic.
	ConfigDirEnv string `json:"config_dir_env,omitempty"`
}

// RuntimeHooksConfig configures runtime hook installation.
type RuntimeHooksConfig struct {
	// Provider controls which hook templates to install: "claude", "opencode", "copilot", or "none".
	Provider string `json:"provider,omitempty"`

	// Dir is the settings directory (e.g., ".claude").
	Dir string `json:"dir,omitempty"`

	// SettingsFile is the settings file name (e.g., "settings.json").
	SettingsFile string `json:"settings_file,omitempty"`

	// Informational indicates the hooks provider installs instructions files only,
	// not executable lifecycle hooks. When true, Gas Town sends startup fallback
	// commands (gt prime) via nudge since hooks won't run automatically.
	// Defaults to false (backwards compatible with claude/opencode which have real hooks).
	Informational bool `json:"informational,omitempty"`

	// UseSettingsDir is the hooks provider preset's HooksUseSettingsDir, taken
	// from the agent registry the config was resolved against (town and rig
	// settings/agents.json included). Nil means not resolved: readers fall
	// back to the built-in preset. Not read from or written to settings.
	UseSettingsDir *bool `json:"-"`
}

// RuntimeTmuxConfig controls tmux heuristics for detecting runtime readiness.
type RuntimeTmuxConfig struct {
	// ProcessNames are tmux pane commands that indicate the runtime is running.
	ProcessNames []string `json:"process_names,omitempty"`

	// ReadyPromptPrefix is the prompt prefix to detect readiness (e.g., "> ").
	ReadyPromptPrefix string `json:"ready_prompt_prefix,omitempty"`

	// ReadyDelayMs is a fixed delay used when prompt detection is unavailable.
	ReadyDelayMs int `json:"ready_delay_ms,omitempty"`
}

// RuntimeInstructionsConfig controls the name of the role instruction file.
type RuntimeInstructionsConfig struct {
	// File is the instruction filename (e.g., "CLAUDE.md", "AGENTS.md").
	File string `json:"file,omitempty"`
}

// DefaultRuntimeConfig returns a RuntimeConfig with sensible defaults.
func DefaultRuntimeConfig() *RuntimeConfig {
	return defaultRuntimeConfigIn(nil)
}

// defaultRuntimeConfigIn is DefaultRuntimeConfig with preset defaults taken
// from reg (nil means the built-in presets).
func defaultRuntimeConfigIn(reg *AgentRegistry) *RuntimeConfig {
	return normalizeRuntimeConfigIn(reg, &RuntimeConfig{Provider: "claude"})
}

// BuildCommand returns the full command line string.
// For use with tmux SendKeys and respawn-pane, where the string is
// interpreted by the user's shell. Args containing shell-special
// characters (e.g., brackets in "sonnet[1m]") are quoted to prevent
// glob expansion.
func (rc *RuntimeConfig) BuildCommand() string {
	resolved := normalizeRuntimeConfig(rc)

	cmd := resolved.Command
	args := resolved.Args

	// Combine command and args, quoting any that contain shell metacharacters
	if len(args) > 0 {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = ShellQuote(a)
		}
		return cmd + " " + strings.Join(quoted, " ")
	}
	return cmd
}

// BuildCommandWithPrompt returns the full command line with an initial prompt.
// If the config has an InitialPrompt, it's appended as a quoted argument.
// If prompt is provided, it overrides the config's InitialPrompt.
// For opencode, uses --prompt flag; for other agents, uses positional argument.
func (rc *RuntimeConfig) BuildCommandWithPrompt(prompt string) string {
	return rc.buildCommandWithPrompt(prompt, os.Stderr)
}

// buildCommandWithPrompt is BuildCommandWithPrompt writing its dropped-prompt
// warning to warn.
func (rc *RuntimeConfig) buildCommandWithPrompt(prompt string, warn io.Writer) string {
	resolved := normalizeRuntimeConfig(rc)
	base := resolved.BuildCommand()

	// Use provided prompt or fall back to config
	p := prompt
	if p == "" {
		p = resolved.InitialPrompt
	}

	if p == "" || resolved.PromptMode == "none" {
		if p != "" {
			// A non-empty prompt was silently dropped because prompt_mode is "none".
			// This commonly happens when a user copies a codex agent entry (which ships
			// with prompt_mode: "none") to create a claude override, inadvertently
			// suppressing the daemon's startup beacon injection and causing a crash-loop
			// that looks like a deacon failure. Warn so misconfiguration is self-diagnosing.
			fmt.Fprintf(warn, "warning: agent %q has prompt_mode: \"none\" — startup prompt dropped (agent may not bootstrap correctly)\n", resolved.Command)
		}
		return base
	}

	// OpenCode requires --prompt flag for initial prompt in interactive mode.
	// Positional argument causes opencode to exit immediately.
	// Match both "opencode" and full paths like "/home/user/.opencode/bin/opencode".
	if resolved.Command == "opencode" || filepath.Base(resolved.Command) == "opencode" {
		return base + " --prompt " + quoteForShell(p)
	}

	// Copilot requires -i flag for initial prompt in interactive mode.
	if resolved.Command == "copilot" || filepath.Base(resolved.Command) == "copilot" {
		return base + " -i " + quoteForShell(p)
	}

	// Gemini requires -i (--prompt-interactive) to auto-execute the prompt
	// while staying in interactive mode. Positional args populate the input
	// field but don't execute, and -p runs headless (exits after completion).
	if resolved.Command == "gemini" || filepath.Base(resolved.Command) == "gemini" {
		return base + " -i " + quoteForShell(p)
	}

	// Quote the prompt for shell safety (positional arg for claude and others)
	return base + " " + quoteForShell(p)
}

// BuildArgsWithPrompt returns the runtime command and args suitable for exec.
func (rc *RuntimeConfig) BuildArgsWithPrompt(prompt string) []string {
	return rc.buildArgsWithPrompt(prompt, os.Stderr)
}

// buildArgsWithPrompt is BuildArgsWithPrompt writing its dropped-prompt
// warning to warn.
func (rc *RuntimeConfig) buildArgsWithPrompt(prompt string, warn io.Writer) []string {
	resolved := normalizeRuntimeConfig(rc)
	args := append([]string{resolved.Command}, resolved.Args...)

	p := prompt
	if p == "" {
		p = resolved.InitialPrompt
	}

	if p != "" && resolved.PromptMode != "none" {
		switch resolved.Command {
		case "opencode":
			args = append(args, "--prompt", p)
		case "copilot", "gemini":
			args = append(args, "-i", p)
		default:
			args = append(args, p)
		}
	} else if p != "" {
		fmt.Fprintf(warn, "warning: agent %q has prompt_mode: \"none\" — startup prompt dropped (agent may not bootstrap correctly)\n", resolved.Command)
	}

	return args
}

// normalizeRuntimeConfig fills unset fields from the built-in presets.
func normalizeRuntimeConfig(rc *RuntimeConfig) *RuntimeConfig {
	return normalizeRuntimeConfigIn(nil, rc)
}

// normalizeRuntimeConfigIn fills unset fields from the presets in reg (nil
// means the built-in presets).
func normalizeRuntimeConfigIn(reg *AgentRegistry, rc *RuntimeConfig) *RuntimeConfig {
	if rc == nil {
		rc = &RuntimeConfig{}
	}

	// Shallow copy to avoid mutating the input
	copy := *rc
	rc = &copy

	// Deep copy nested structs to avoid shared references
	if rc.Session != nil {
		s := *rc.Session
		rc.Session = &s
	}
	if rc.Hooks != nil {
		h := *rc.Hooks
		rc.Hooks = &h
	}
	if rc.Tmux != nil {
		t := *rc.Tmux
		rc.Tmux = &t
	}
	if rc.Instructions != nil {
		i := *rc.Instructions
		rc.Instructions = &i
	}

	if rc.Provider == "" {
		rc.Provider = "claude"
	}

	if rc.Command == "" {
		rc.Command = defaultRuntimeCommand(reg, rc.Provider)
	}

	if rc.Args == nil {
		rc.Args = defaultRuntimeArgs(reg, rc.Provider)
	}
	rc.Args = ensureCodexAutomationArgs(rc.Command, rc.Args)

	if rc.PromptMode == "" {
		rc.PromptMode = defaultPromptMode(reg, rc.Provider)
	}

	if rc.Session == nil {
		rc.Session = &RuntimeSessionConfig{}
	}

	if rc.Session.SessionIDEnv == "" {
		rc.Session.SessionIDEnv = defaultSessionIDEnv(reg, rc.Provider)
	}

	if rc.Session.ConfigDirEnv == "" {
		rc.Session.ConfigDirEnv = defaultConfigDirEnv(reg, rc.Provider)
	}

	if rc.Hooks == nil {
		rc.Hooks = &RuntimeHooksConfig{}
	}

	if rc.Hooks.Provider == "" {
		rc.Hooks.Provider = defaultHooksProvider(reg, rc.Provider)
	}

	if rc.Hooks.Dir == "" {
		rc.Hooks.Dir = defaultHooksDir(reg, rc.Provider)
	}

	if rc.Hooks.SettingsFile == "" {
		rc.Hooks.SettingsFile = defaultHooksFile(reg, rc.Provider)
	}

	if rc.Hooks.UseSettingsDir == nil {
		rc.Hooks.UseSettingsDir = hooksUseSettingsDir(reg, rc.Hooks.Provider)
	}

	// Set informational flag for providers whose "hooks" are instructions files,
	// not executable lifecycle hooks. This tells startup fallback logic to send
	// gt prime via nudge since hooks won't run automatically.
	if !rc.Hooks.Informational {
		rc.Hooks.Informational = defaultHooksInformational(reg, rc.Provider)
	}

	if rc.Tmux == nil {
		rc.Tmux = &RuntimeTmuxConfig{}
	}

	if rc.Tmux.ProcessNames == nil {
		rc.Tmux.ProcessNames = defaultProcessNames(reg, rc.Provider, rc.Command)
	}

	if rc.Tmux.ReadyPromptPrefix == "" {
		rc.Tmux.ReadyPromptPrefix = defaultReadyPromptPrefix(reg, rc.Provider)
	}

	if rc.Tmux.ReadyDelayMs == 0 {
		rc.Tmux.ReadyDelayMs = defaultReadyDelayMs(reg, rc.Provider)
	}

	if rc.Instructions == nil {
		rc.Instructions = &RuntimeInstructionsConfig{}
	}

	if rc.Instructions.File == "" {
		rc.Instructions.File = defaultInstructionsFile(reg, rc.Provider)
	}

	return rc
}

const codexUpdateCheckKey = "check_for_update_on_startup"
const codexUpdateCheckConfig = codexUpdateCheckKey + "=false"

func ensureCodexAutomationArgs(command string, args []string) []string {
	if !isCodexRuntime(command) || hasCodexUpdateCheckConfig(args) {
		return args
	}
	result := make([]string, 0, len(args)+2)
	result = append(result, "-c", codexUpdateCheckConfig)
	result = append(result, args...)
	return result
}

func isCodexRuntime(command string) bool {
	return filepath.Base(command) == string(AgentCodex)
}

func hasCodexUpdateCheckConfig(args []string) bool {
	for _, arg := range args {
		if arg == codexUpdateCheckKey || strings.HasPrefix(arg, codexUpdateCheckKey+"=") {
			return true
		}
	}
	return false
}

func defaultRuntimeCommand(reg *AgentRegistry, provider string) string {
	if provider == "generic" {
		return ""
	}
	if preset := reg.Preset(provider); preset != nil {
		cmd := preset.Command
		// Resolve claude path for Claude preset (handles alias installations)
		if preset.Name == AgentClaude && cmd == "claude" {
			return resolveClaudePath(reg.host())
		}
		return cmd
	}
	return resolveClaudePath(reg.host()) // fallback for unknown providers
}

// resolveClaudePath finds the claude binary, checking PATH first then common installation locations.
// This handles the case where claude is installed as an alias (not in PATH) which doesn't work
// in non-interactive shells spawned by tmux.
func resolveClaudePath(h host) string {
	// First, try to find claude in PATH
	if path, err := h.lookPath("claude"); err == nil {
		return path
	}

	// Check common Claude Code installation locations
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude" // Fall back to bare command
	}

	// Standard Claude Code installation path
	claudePath := filepath.Join(home, ".claude", "local", "claude")
	if _, err := os.Stat(claudePath); err == nil {
		return claudePath
	}

	// Fall back to bare command (might work if PATH is set differently in tmux)
	return "claude"
}

func defaultRuntimeArgs(reg *AgentRegistry, provider string) []string {
	if preset := reg.Preset(provider); preset != nil && preset.Args != nil {
		return append([]string(nil), preset.Args...) // copy to avoid mutation
	}
	return nil
}

func defaultPromptMode(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil && preset.PromptMode != "" {
		return preset.PromptMode
	}
	return "arg"
}

func defaultSessionIDEnv(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil {
		return preset.SessionIDEnv
	}
	return ""
}

func defaultConfigDirEnv(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil {
		return preset.ConfigDirEnv
	}
	return ""
}

func defaultHooksProvider(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil && preset.HooksProvider != "" {
		return preset.HooksProvider
	}
	return "none"
}

func defaultHooksDir(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil {
		return preset.HooksDir
	}
	return ""
}

func defaultHooksFile(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil {
		return preset.HooksSettingsFile
	}
	return ""
}

// hooksUseSettingsDir returns the HooksUseSettingsDir of the hooks provider's
// preset in reg, or nil when the provider has no preset.
func hooksUseSettingsDir(reg *AgentRegistry, hooksProvider string) *bool {
	preset := reg.Preset(hooksProvider)
	if preset == nil {
		return nil
	}
	v := preset.HooksUseSettingsDir
	return &v
}

// defaultHooksInformational returns true for providers whose hooks are instructions
// files only (not executable lifecycle hooks). For these providers, Gas Town sends
// startup fallback commands (gt prime) via nudge since hooks won't auto-run.
func defaultHooksInformational(reg *AgentRegistry, provider string) bool {
	if preset := reg.Preset(provider); preset != nil {
		return preset.HooksInformational
	}
	return false
}

func defaultProcessNames(reg *AgentRegistry, provider, command string) []string {
	if preset := reg.Preset(provider); preset != nil && len(preset.ProcessNames) > 0 {
		return append([]string(nil), preset.ProcessNames...) // copy to avoid mutation
	}
	if command != "" {
		return []string{filepath.Base(command)}
	}
	return nil
}

func defaultReadyPromptPrefix(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil {
		return preset.ReadyPromptPrefix
	}
	return ""
}

func defaultReadyDelayMs(reg *AgentRegistry, provider string) int {
	if preset := reg.Preset(provider); preset != nil {
		return preset.ReadyDelayMs
	}
	return 0
}

func defaultInstructionsFile(reg *AgentRegistry, provider string) string {
	if preset := reg.Preset(provider); preset != nil && preset.InstructionsFile != "" {
		return preset.InstructionsFile
	}
	return "AGENTS.md"
}

// quoteForShell quotes a string for safe shell usage.
func quoteForShell(s string) string {
	if runtime.GOOS == "windows" {
		// PowerShell: use single quotes (no interpolation). Double embedded single quotes.
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	// POSIX shell: wrap in double quotes, escaping special characters.
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "`", "\\`")
	escaped = strings.ReplaceAll(escaped, "$", `\$`)
	return `"` + escaped + `"`
}

// ThemeConfig represents tmux theme settings for a rig.
type ThemeConfig struct {
	// Disabled skips tmux status/window theming for this rig.
	Disabled bool `json:"disabled,omitempty"`

	// Name picks from the default palette (e.g., "ocean", "forest").
	// If empty, a theme is auto-assigned based on rig name.
	Name string `json:"name,omitempty"`

	// Custom overrides the palette with specific colors.
	Custom *CustomTheme `json:"custom,omitempty"`

	// CrewThemes maps crew member names to theme names.
	// Checked before RoleThemes, so individual crew members can have distinct colors
	// while other crew members fall back to the role-level theme.
	// Example: {"krieger": "teal", "mallory": "ember"}
	CrewThemes map[string]string `json:"crew_themes,omitempty"`

	// RoleThemes overrides themes for specific roles in this rig.
	// Keys: "witness", "refinery", "crew", "polecat".
	// A value of "none" disables tmux theming for that role.
	RoleThemes map[string]string `json:"role_themes,omitempty"`

	// WindowTint controls window background (window-style) coloring for this rig.
	// If nil, falls back to town-level window tint config.
	WindowTint *WindowTint `json:"window_tint,omitempty"`
}

// CustomTheme allows specifying exact colors for the status bar.
type CustomTheme struct {
	BG string `json:"bg"` // Background color (hex or tmux color name)
	FG string `json:"fg"` // Foreground color (hex or tmux color name)
}

// TownThemeConfig represents global theme settings (mayor/config.json).
type TownThemeConfig struct {
	// Disabled skips tmux status/window theming for all sessions unless a rig
	// theme overrides it.
	Disabled bool `json:"disabled,omitempty"`

	// Name picks from the default palette when no role-specific override exists.
	Name string `json:"name,omitempty"`

	// Custom overrides the palette with specific colors when no role-specific
	// override exists.
	Custom *CustomTheme `json:"custom,omitempty"`

	// CrewThemes maps crew member names to theme names (town-wide defaults).
	// Checked before RoleDefaults. Per-rig CrewThemes take precedence.
	CrewThemes map[string]string `json:"crew_themes,omitempty"`

	// RoleDefaults sets default themes for roles across all rigs.
	// Keys: "mayor", "deacon", "witness", "refinery", "crew", "polecat".
	// A value of "none" disables tmux theming for that role.
	RoleDefaults map[string]string `json:"role_defaults,omitempty"`

	// WindowTint controls window background (window-style) coloring globally.
	// Per-rig WindowTint in ThemeConfig takes precedence over this.
	WindowTint *WindowTint `json:"window_tint,omitempty"`
}

// WindowTint controls window background (window-style) coloring.
// Mirrors status bar theme customization: palette name, custom colors, per-role overrides.
// When Enabled is nil or true, window backgrounds are tinted.
// When Enabled is false, window backgrounds use terminal defaults.
type WindowTint struct {
	// Enabled controls whether window tinting is active.
	// nil or true = enabled, false = disabled (window uses terminal default).
	Enabled *bool `json:"enabled,omitempty"`

	// Name picks a palette theme for the window background.
	// If empty, falls back to the session's status bar theme colors.
	Name string `json:"name,omitempty"`

	// Custom overrides the palette with specific window background colors.
	Custom *CustomTheme `json:"custom,omitempty"`

	// RoleTints overrides window tint themes for specific roles.
	// Keys: "witness", "refinery", "crew", "polecat"
	RoleTints map[string]string `json:"role_tints,omitempty"`

	// TintFactor controls how much the window background is darkened when
	// inheriting from the status bar theme (0.0–1.0). Lower = darker.
	// Default: 0.4 (40% of status bar brightness).
	// Only applies when window tint inherits from the status bar theme
	// (i.e., no explicit name, custom, or role_tints match).
	TintFactor *float64 `json:"tint_factor,omitempty"`
}

// BuiltinRoleThemes returns the default themes for each role.
// These are used when no explicit configuration is provided.
func BuiltinRoleThemes() map[string]string {
	return map[string]string{
		"refinery": "plum", // Purple - processing, refining
		// crew and polecat use rig theme by default (no override)
	}
}

// MergeQueueConfig represents merge queue settings for a rig.
type MergeQueueConfig struct {
	// IntegrationBranchPolecatEnabled controls whether polecats auto-source
	// their worktrees from integration branches when the parent epic has one.
	// Nil defaults to true.
	IntegrationBranchPolecatEnabled *bool `json:"integration_branch_polecat_enabled,omitempty"`

	// MergeStrategy controls how the refinery lands approved work: "direct" (default)
	// merges directly to the base branch, "pr" uses the VCS provider's merge API
	// which respects branch protection/restriction rules.
	MergeStrategy string `json:"merge_strategy,omitempty"`

	// RequireReview controls whether the refinery requires at least one approving
	// review before merging a PR. Only meaningful when merge_strategy="pr".
	// Nil defaults to false (no review required).
	RequireReview *bool `json:"require_review,omitempty"`

	// TestCommand is the command to run for tests.
	TestCommand string `json:"test_command,omitempty"`

	// PresubmitCommand is the command gt done runs on the rebased branch
	// before pushing (gt-ssyxd). Empty means `make presubmit` when a Go
	// repo's Makefile has that target: lint, build and the tests of the
	// changed packages only. With no such target gt done falls back to
	// `make gate` or the lint/build/test commands. The landing worker's own
	// gate on the merged tree (Gate) is unaffected.
	PresubmitCommand string `json:"presubmit_command,omitempty"`

	// Gate is the one command the landing worker runs on the merged tree
	// (land.LandGate, ADR 0004). Exit 0 lands; anything else rejects. Empty
	// means `make gate` when the repo's Makefile has that target, else
	// `make test`. A Docker-backed gate carries its own slot wrapper, e.g.
	// "gt slot run --role hm/crew/sloan -- make test".
	Gate string `json:"gate,omitempty"`

	// LintCommand is the command to run for linting (used by formulas).
	LintCommand string `json:"lint_command,omitempty"`

	// BuildCommand is the command to run for building (used by formulas).
	BuildCommand string `json:"build_command,omitempty"`

	// SetupCommand is the command to run for project setup (e.g., pnpm install).
	SetupCommand string `json:"setup_command,omitempty"`

	// TypecheckCommand is the command to run for type checking (e.g., tsc --noEmit).
	TypecheckCommand string `json:"typecheck_command,omitempty"`

	// MaxReadyForDispatch is the ready-MR ceiling above which a new dispatch
	// is refused: the merge queue, not the pool, is the real limit on how
	// much work the town can absorb, so `gt sling` stops feeding it while the
	// rig has more than this many ready MRs (plan Task 3 / A3). A bead
	// labeled `rework` or an explicit --force passes anyway. Zero or unset
	// disables the guard (no queue read at all).
	MaxReadyForDispatch int `json:"max_ready_for_dispatch,omitempty"`

	// Editorial configures the om editorial gate: whether a merge requires
	// an om review before it can land, and how that review runs. Nil means
	// no tier has set an editorial block; Required defaults to false so
	// rigs that never configure it keep upstream (pre-gate) behavior.
	Editorial *EditorialConfig `json:"editorial,omitempty"`

	// PostLandCommand runs once per landing, after the push and the record,
	// in a throwaway worktree at the landed commit (the slow test tier, e.g.
	// "make test-slow"). The landing worker runs it asynchronously, one at a
	// time per rig, coalescing landings that finish while one runs; a red
	// run comments on the landed bead and never reverts the landing
	// (gt-v4ssj.2). Empty disables it. Read only from the rig's own
	// settings/config.json, so merged repo content cannot choose it.
	PostLandCommand string `json:"post_land_command,omitempty"`
}

// EditorialConfig is the merge_queue.editorial block. Only Required is
// read: the editorial-required doctor check warns when a rig carries an
// .om.json rubric without it.
type EditorialConfig struct {
	// Required marks the rig as expecting om editorial review. Defaults to
	// false.
	Required bool `json:"required"`
}

// IsPolecatIntegrationEnabled returns whether polecat integration branch
// sourcing is enabled. Nil-safe, defaults to true.
func (c *MergeQueueConfig) IsPolecatIntegrationEnabled() bool {
	if c.IntegrationBranchPolecatEnabled == nil {
		return true
	}
	return *c.IntegrationBranchPolecatEnabled
}

// IsRequireReviewEnabled returns whether PR reviews are required before merging.
// Nil-safe, defaults to false.
func (c *MergeQueueConfig) IsRequireReviewEnabled() bool {
	if c.RequireReview == nil {
		return false
	}
	return *c.RequireReview
}

// GetMaxReadyForDispatch returns the ready-MR ceiling above which a new
// dispatch is refused. Nil-safe, and zero means the guard is off: a rig that
// never sets the knob must not pay for a queue read on every sling.
func (c *MergeQueueConfig) GetMaxReadyForDispatch() int {
	if c == nil {
		return 0
	}
	return c.MaxReadyForDispatch
}

// HasAnyGateCommand reports whether at least one of the five gate commands
// (setup, typecheck, lint, test, build) is configured. Nil-safe.
//
// Used to decide whether a --pre-verified claim is even possible to honor:
// a rig with zero configured gate commands has nothing a polecat could have
// run, so the claim has no basis (gt-k4sy).
func (c *MergeQueueConfig) HasAnyGateCommand() bool {
	if c == nil {
		return false
	}
	return c.SetupCommand != "" || c.TypecheckCommand != "" || c.LintCommand != "" ||
		c.TestCommand != "" || c.BuildCommand != ""
}

// GateSetSHA returns a sha256 hex digest identifying the ordered set of
// non-empty gate commands (setup, typecheck, lint, build, test) in cfg.
// Nil-safe: a nil cfg hashes the same as an empty gate set.
//
// A `gt done --pre-verified` stamp records this alongside its verification
// so the refinery's fast-path can detect when the gate set has changed
// since the polecat verified — a rig that adds, removes, or edits a gate
// command must invalidate any pre-verification recorded against the old
// set (om-gate T8).
func GateSetSHA(cfg *MergeQueueConfig) string {
	if cfg == nil {
		cfg = &MergeQueueConfig{}
	}
	ordered := []string{cfg.SetupCommand, cfg.TypecheckCommand, cfg.LintCommand, cfg.BuildCommand, cfg.TestCommand}
	nonEmpty := make([]string, 0, len(ordered))
	for _, c := range ordered {
		if c != "" {
			nonEmpty = append(nonEmpty, c)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(nonEmpty, "\n")))
	return hex.EncodeToString(sum[:])
}

// CombineGateSetSHA folds a set of named gate commands (name -> shell
// command) into the hash GateSetSHA produces, yielding one hash that
// changes if either binding changes. Nil or empty namedGates hashes
// identically to GateSetSHA(cfg) alone.
//
// This is the single algorithm the pre-verification producer (`gt done`,
// stamping pre_verified_gates) and consumer (the refinery's fast-path
// staleness check) must both call — hashed two ways, the values never agree
// and the fast-path never fires (om-gate T8).
func CombineGateSetSHA(cfg *MergeQueueConfig, namedGates map[string]string) string {
	base := GateSetSHA(cfg)
	if len(namedGates) == 0 {
		return base
	}

	names := make([]string, 0, len(namedGates))
	for name := range namedGates {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(base)
	for _, name := range names {
		b.WriteString("\x00")
		b.WriteString(name)
		b.WriteString("\x00")
		b.WriteString(namedGates[name])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}

// DefaultMergeQueueConfig returns a MergeQueueConfig with sensible defaults.
func DefaultMergeQueueConfig() *MergeQueueConfig {
	return &MergeQueueConfig{
		IntegrationBranchPolecatEnabled: boolPtr(true),
	}
}

// NamepoolConfig represents namepool settings for themed polecat names.
type NamepoolConfig struct {
	// Style picks from a built-in theme (e.g., "mad-max", "minerals", "wasteland").
	// If empty, defaults to "mad-max".
	Style string `json:"style,omitempty"`

	// Names is a custom list of names to use instead of a built-in theme.
	// If provided, overrides the Style setting.
	Names []string `json:"names,omitempty"`

	// MaxBeforeNumbering is when to start appending numbers.
	// Default is 50. After this many polecats, names become name-01, name-02, etc.
	MaxBeforeNumbering int `json:"max_before_numbering,omitempty"`
}

// DefaultNamepoolConfig returns a NamepoolConfig with sensible defaults.
func DefaultNamepoolConfig() *NamepoolConfig {
	return &NamepoolConfig{
		Style:              "mad-max",
		MaxBeforeNumbering: 50,
	}
}

// AccountsConfig represents Claude Code account configuration (mayor/accounts.json).
// This enables Gas Town to manage multiple Claude Code accounts with easy switching.
type AccountsConfig struct {
	Version  int                `json:"version"`  // schema version
	Accounts map[string]Account `json:"accounts"` // handle -> account details
	Default  string             `json:"default"`  // default account handle
}

// Account represents a single Claude Code account.
type Account struct {
	Email       string `json:"email"`                 // account email
	Description string `json:"description,omitempty"` // human description
	ConfigDir   string `json:"config_dir"`            // path to CLAUDE_CONFIG_DIR
}

// CurrentAccountsVersion is the current schema version for AccountsConfig.
const CurrentAccountsVersion = 1

// DefaultAccountsConfigDir returns the default base directory for account configs.
func DefaultAccountsConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return home + "/.claude-accounts", nil
}

// MessagingConfig represents the messaging configuration (config/messaging.json).
// This defines mailing lists, work queues, and announcement channels.
type MessagingConfig struct {
	Type    string `json:"type"`    // "messaging"
	Version int    `json:"version"` // schema version

	// Lists are static mailing lists. Messages are fanned out to all recipients.
	// Each recipient gets their own copy of the message.
	// Example: {"oncall": ["mayor/", "gastown/witness"]}
	Lists map[string][]string `json:"lists,omitempty"`

	// Queues are shared work queues. Only one copy exists; workers claim messages.
	// Messages sit in the queue until explicitly claimed by a worker.
	// Example: {"work/gastown": ["gastown/polecats/*"]}
	Queues map[string]QueueConfig `json:"queues,omitempty"`

	// Announces are bulletin boards. One copy exists; anyone can read, no claiming.
	// Used for broadcast announcements that don't need acknowledgment.
	// Example: {"alerts": {"readers": ["@town"]}}
	Announces map[string]AnnounceConfig `json:"announces,omitempty"`

	// NudgeChannels are named groups for real-time nudge fan-out.
	// Like mailing lists but for tmux send-keys instead of durable mail.
	// Example: {"workers": ["gastown/polecats/*", "gastown/crew/*"], "witnesses": ["*/witness"]}
	NudgeChannels map[string][]string `json:"nudge_channels,omitempty"`
}

// QueueConfig represents a work queue configuration.
type QueueConfig struct {
	// Workers lists addresses eligible to claim from this queue.
	// Supports wildcards: "gastown/polecats/*" matches all polecats in gastown.
	Workers []string `json:"workers"`

	// MaxClaims is the maximum number of concurrent claims (0 = unlimited).
	MaxClaims int `json:"max_claims,omitempty"`
}

// AnnounceConfig represents a bulletin board configuration.
type AnnounceConfig struct {
	// Readers lists addresses eligible to read from this announce channel.
	// Supports @group syntax: "@town", "@rig/gastown", "@witnesses".
	Readers []string `json:"readers"`

	// RetainCount is the number of messages to retain (0 = unlimited).
	RetainCount int `json:"retain_count,omitempty"`
}

// CurrentMessagingVersion is the current schema version for MessagingConfig.
const CurrentMessagingVersion = 1

// NewMessagingConfig creates a new MessagingConfig with defaults.
func NewMessagingConfig() *MessagingConfig {
	return &MessagingConfig{
		Type:          "messaging",
		Version:       CurrentMessagingVersion,
		Lists:         make(map[string][]string),
		Queues:        make(map[string]QueueConfig),
		Announces:     make(map[string]AnnounceConfig),
		NudgeChannels: make(map[string][]string),
	}
}

// EscalationConfig represents escalation routing configuration (settings/escalation.json).
// This defines severity-based routing for escalations to different channels.
type EscalationConfig struct {
	Type    string `json:"type"`    // "escalation"
	Version int    `json:"version"` // schema version

	// Routes maps severity levels to action lists.
	// Actions are executed in order for each escalation.
	// Action formats:
	//   - "bead"        → Create escalation bead (always first, implicit)
	//   - "mail:<target>" → Send gt mail to target (e.g., "mail:mayor")
	//   - "email:human" → Send email to contacts.human_email
	//   - "sms:human"   → Send SMS to contacts.human_sms
	//   - "slack"       → Post to contacts.slack_webhook
	//   - "log"         → Write to escalation log file
	Routes map[string][]string `json:"routes"`

	// Contacts contains contact information for external notification actions.
	Contacts EscalationContacts `json:"contacts"`

	// StaleThreshold is how long before an unacknowledged escalation
	// is considered stale and gets re-escalated.
	// Format: Go duration string (e.g., "4h", "30m", "24h")
	// Default: "4h"
	StaleThreshold string `json:"stale_threshold,omitempty"`

	// MaxReescalations limits how many times an escalation can be
	// re-escalated. Default: 2 (low→medium→high, then stops)
	// Pointer type to distinguish "not configured" (nil) from explicit 0.
	MaxReescalations *int `json:"max_reescalations,omitempty"`

	// RenotifyWindow is the minimum time between two notifications for the
	// same recurring escalation. A repeat firing bumps the bead's occurrence
	// count every time, but only re-sends mail/email/sms/slack/log once this
	// much time has passed since the last notification — otherwise a
	// condition that fires every few minutes would spam every channel on
	// every cycle. Format: Go duration string (e.g., "1h", "30m").
	// Default: "1h". A value of "0" re-notifies on every firing.
	RenotifyWindow string `json:"renotify_window,omitempty"`
}

// EscalationContacts contains contact information for external notification channels.
type EscalationContacts struct {
	HumanEmail   string `json:"human_email,omitempty"`   // email address for email:human action
	HumanSMS     string `json:"human_sms,omitempty"`     // phone number for sms:human action
	SlackWebhook string `json:"slack_webhook,omitempty"` // webhook URL for slack action
	SMTPHost     string `json:"smtp_host,omitempty"`     // SMTP server host (e.g. "smtp.gmail.com")
	SMTPPort     string `json:"smtp_port,omitempty"`     // SMTP server port (default "587")
	SMTPFrom     string `json:"smtp_from,omitempty"`     // sender address for email notifications
	SMTPUser     string `json:"smtp_user,omitempty"`     // SMTP auth username (optional)
	SMTPPass     string `json:"smtp_pass,omitempty"`     // SMTP auth password (optional)
	SMSWebhook   string `json:"sms_webhook,omitempty"`   // webhook URL for SMS delivery (e.g. Twilio)
}

// CurrentEscalationVersion is the current schema version for EscalationConfig.
const CurrentEscalationVersion = 1

// Escalation severity level constants.
const (
	SeverityCritical = "critical" // P0: immediate attention required
	SeverityHigh     = "high"     // P1: urgent, needs attention soon
	SeverityMedium   = "medium"   // P2: standard escalation (default)
	SeverityLow      = "low"      // P3: informational, can wait
)

// ValidSeverities returns the list of valid severity levels in order of priority.
func ValidSeverities() []string {
	return []string{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}
}

// IsValidSeverity checks if a severity level is valid.
func IsValidSeverity(severity string) bool {
	switch severity {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

// NextSeverity returns the next higher severity level for re-escalation.
// Returns the same level if already at critical.
func NextSeverity(severity string) string {
	switch severity {
	case SeverityLow:
		return SeverityMedium
	case SeverityMedium:
		return SeverityHigh
	case SeverityHigh:
		return SeverityCritical
	default:
		return SeverityCritical
	}
}

// intPtr returns a pointer to the given int value.
func intPtr(v int) *int { return &v }

// NewEscalationConfig creates a new EscalationConfig with sensible defaults.
func NewEscalationConfig() *EscalationConfig {
	return &EscalationConfig{
		Type:    "escalation",
		Version: CurrentEscalationVersion,
		Routes: map[string][]string{
			SeverityLow:      {"bead"},
			SeverityMedium:   {"bead", "mail:mayor"},
			SeverityHigh:     {"bead", "mail:mayor", "email:human"},
			SeverityCritical: {"bead", "mail:mayor", "email:human", "sms:human"},
		},
		Contacts:         EscalationContacts{},
		StaleThreshold:   "4h",
		MaxReescalations: intPtr(2),
		RenotifyWindow:   "1h",
	}
}

// PolecatPool bounds how many polecats run at once: max_local on the local
// model, max_overflow on the overflow agent. See TownSettings.PolecatPool.
type PolecatPool struct {
	// LocalAgent is the agent alias to use while the pool has room.
	LocalAgent string `json:"local_agent"`
	// MaxLocal is the number of live polecat sessions allowed on LocalAgent.
	MaxLocal int `json:"max_local"`
	// MinSpawnGap is the minimum time between two local spawns (e.g. "4m").
	MinSpawnGap string `json:"min_spawn_gap,omitempty"`
	// OverflowAgent is used when the pool is full or a spawn is too soon.
	// Empty means the normal role_agents resolution.
	OverflowAgent string `json:"overflow_agent,omitempty"`
	// MaxOverflow is the number of live polecat sessions allowed on
	// OverflowAgent. Zero leaves the overflow seat uncapped.
	MaxOverflow int `json:"max_overflow,omitempty"`
	// IdleFill lets an overflow-shaped bead (bug, feature) take a free local
	// seat rather than the overflow agent. Unset means on, so a town that never
	// sets it keeps the behavior it already had.
	IdleFill *bool `json:"idle_fill,omitempty"`
}

// MinSpawnGapD returns the parsed MinSpawnGap, or zero when unset/invalid.
func (p *PolecatPool) MinSpawnGapD() time.Duration {
	if p == nil || p.MinSpawnGap == "" {
		return 0
	}
	d, err := time.ParseDuration(p.MinSpawnGap)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// OverflowCapped reports whether the pool bounds live polecats on
// OverflowAgent: max_overflow set, and an agent whose sessions to count.
func (p *PolecatPool) OverflowCapped() bool {
	return p != nil && p.MaxOverflow > 0 && p.OverflowAgent != ""
}

// IdleFillEnabled reports whether an overflow-shaped bead may take a free
// local seat; unset or a nil pool means on.
func (p *PolecatPool) IdleFillEnabled() bool {
	return p == nil || p.IdleFill == nil || *p.IdleFill
}
