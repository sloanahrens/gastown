// Package daemon provides the town-level background service for Gas Town.
//
// The daemon is a simple Go process (not a Claude agent) that:
// 1. Pokes agents periodically (heartbeat)
// 2. Processes lifecycle requests (cycle, restart, shutdown)
// 3. Restarts sessions when agents request cycling
//
// The daemon is a "dumb scheduler" - all intelligence is in agents.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// Config holds daemon configuration.
type Config struct {
	// HeartbeatInterval is how often to poke agents.
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`

	// TownRoot is the Gas Town workspace root.
	TownRoot string `json:"town_root"`

	// LogFile is the path to the daemon log file.
	LogFile string `json:"log_file"`

	// PidFile is the path to the PID file.
	PidFile string `json:"pid_file"`
}

// DefaultConfig returns the default daemon configuration.
func DefaultConfig(townRoot string) *Config {
	daemonDir := filepath.Join(townRoot, "daemon")
	return &Config{
		HeartbeatInterval: 5 * time.Minute, // Deacon wakes on mail too, no need to poke often
		TownRoot:          townRoot,
		LogFile:           filepath.Join(daemonDir, "daemon.log"),
		PidFile:           filepath.Join(daemonDir, "daemon.pid"),
	}
}

// State represents the daemon's runtime state.
type State struct {
	// Running indicates if the daemon is running.
	Running bool `json:"running"`

	// PID is the process ID of the daemon.
	PID int `json:"pid"`

	// StartedAt is when the daemon started.
	StartedAt time.Time `json:"started_at"`

	// LastHeartbeat is when the last heartbeat completed.
	LastHeartbeat time.Time `json:"last_heartbeat"`

	// HeartbeatCount is how many heartbeats have completed.
	HeartbeatCount int64 `json:"heartbeat_count"`

	// Commit is the build commit of the running daemon, fully resolved when
	// the gastown source repo is available (see resolveOwnCommit). rebuild-gt
	// reads it to tell whether the daemon is running the installed binary.
	Commit string `json:"commit,omitempty"`
}

// StateFile returns the path to the state file.
func StateFile(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "state.json")
}

// LoadState loads daemon state from disk.
func LoadState(townRoot string) (*State, error) {
	stateFile := StateFile(townRoot)
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SaveState saves daemon state to disk using atomic write.
func SaveState(townRoot string, state *State) error {
	stateFile := StateFile(townRoot)

	// Ensure daemon directory exists
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return err
	}

	return atomicfile.WriteJSON(stateFile, state)
}

// The mayor/daemon.json schema lives in internal/config so the config
// kernel decodes the file strictly with the types the daemon runs on
// (gt-y3pgh.1). These aliases keep the daemon's names.
type (
	DaemonPatrolConfig         = agentconfig.DaemonPatrolConfig
	PatrolConfig               = agentconfig.PatrolConfig
	PatrolsConfig              = agentconfig.PatrolsConfig
	DoltServerConfig           = agentconfig.DoltServerConfig
	JsonlGitBackupConfig       = agentconfig.JsonlGitBackupConfig
	WispReaperConfig           = agentconfig.WispReaperConfig
	DoctorDogConfig            = agentconfig.DoctorDogConfig
	CompactorDogConfig         = agentconfig.CompactorDogConfig
	CheckpointDogConfig        = agentconfig.CheckpointDogConfig
	ScheduledMaintenanceConfig = agentconfig.ScheduledMaintenanceConfig
	SpecDispatchConfig         = agentconfig.SpecDispatchConfig
	PatrolScanConfig           = agentconfig.PatrolScanConfig
	RestartTrackerConfig       = agentconfig.RestartTrackerConfig
	EventsPruneConfig          = agentconfig.EventsPruneConfig
	ScheduledSlingsConfig      = agentconfig.ScheduledSlingsConfig
	ScheduledSlingEntry        = agentconfig.ScheduledSlingEntry
	LandingWorkerConfig        = agentconfig.LandingWorkerConfig
	StewardConfig              = agentconfig.StewardConfig
	StewardPlanConfig          = agentconfig.StewardPlanConfig
	TierSweepConfig            = agentconfig.TierSweepConfig
)

// PatrolConfigFile returns the path to the patrol config file.
func PatrolConfigFile(townRoot string) string {
	return filepath.Join(townRoot, constants.DirMayor, "daemon.json")
}

// PatrolConfigSource names the file the patrol config was read from: on the
// two-file layout mayor/daemon.json is settings/config.json's "daemon"
// section, so a startup line that names mayor/daemon.json reports to the
// operator a file whose edit the daemon did not read (gt-y3pgh.12).
func PatrolConfigSource(townRoot string) string {
	return agentconfig.SourcePath(PatrolConfigFile(townRoot))
}

// LoadPatrolConfig loads patrol configuration from mayor/daemon.json.
// Returns nil if the file doesn't exist or can't be parsed.
//
// It returns nil both when the file is absent and when it does not parse, so
// it is for read-only callers only. Anything that writes the file back, or
// starts the town from it, uses ReadPatrolConfig (gt-fcxe9.10).
func LoadPatrolConfig(townRoot string) *DaemonPatrolConfig {
	cfg, err := ReadPatrolConfig(townRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return nil
	}
	return cfg
}

// ReadPatrolConfig loads mayor/daemon.json through the config package's one
// parser, telling absent (nil, nil) from unparseable (nil, *config.ParseError
// naming the file and offset).
func ReadPatrolConfig(townRoot string) (*DaemonPatrolConfig, error) {
	cfg, err := agentconfig.LoadDaemonPatrolConfig(PatrolConfigFile(townRoot))
	if errors.Is(err, agentconfig.ErrNotFound) {
		return nil, nil
	}
	return cfg, err
}

// SavePatrolConfig writes mayor/daemon.json through the config package's
// locked writer, which refuses to replace a file that does not parse.
func SavePatrolConfig(townRoot string, config *DaemonPatrolConfig) error {
	return agentconfig.WriteConfigJSON(PatrolConfigFile(townRoot), config, 0o644)
}

// IsPatrolEnabled checks if a patrol is enabled in the config.
// Returns true if the config doesn't exist (default enabled for backwards compatibility).
// Exception: the opt-in patrols checked first below default to disabled.
func IsPatrolEnabled(config *DaemonPatrolConfig, patrol string) bool {
	// Opt-in patrols: disabled unless explicitly enabled in config.
	// Must check before the nil-config fallback, otherwise nil config
	// returns true for patrols that should default to disabled.
	if patrol == "scheduled_slings" {
		if config == nil || config.Patrols == nil || config.Patrols.ScheduledSlings == nil {
			return false
		}
		return config.Patrols.ScheduledSlings.Enabled
	}
	if patrol == "jsonl_git_backup" {
		if config == nil || config.Patrols == nil || config.Patrols.JsonlGitBackup == nil {
			return false
		}
		return config.Patrols.JsonlGitBackup.Enabled
	}
	if patrol == "wisp_reaper" {
		if config == nil || config.Patrols == nil || config.Patrols.WispReaper == nil {
			return false
		}
		return config.Patrols.WispReaper.Enabled
	}
	if patrol == "doctor_dog" {
		if config == nil || config.Patrols == nil || config.Patrols.DoctorDog == nil {
			return false
		}
		return config.Patrols.DoctorDog.Enabled
	}
	if patrol == "compactor_dog" {
		if config == nil || config.Patrols == nil || config.Patrols.CompactorDog == nil {
			return false
		}
		return config.Patrols.CompactorDog.Enabled
	}
	if patrol == "checkpoint_dog" {
		if config == nil || config.Patrols == nil || config.Patrols.CheckpointDog == nil {
			return false
		}
		return config.Patrols.CheckpointDog.Enabled
	}
	if patrol == "scheduled_maintenance" {
		if config == nil || config.Patrols == nil || config.Patrols.ScheduledMaintenance == nil {
			return false
		}
		return config.Patrols.ScheduledMaintenance.Enabled
	}
	// steward is opt-in: its jobs act on submitted work, so only an explicit
	// enabled:true turns it on (gt-9bioi.1).
	if patrol == "steward" {
		if config == nil || config.Patrols == nil || config.Patrols.Steward == nil {
			return false
		}
		return config.Patrols.Steward.Enabled
	}
	// steward_plan is opt-in on its own key: a plan job spends an LLM session
	// to propose a spec's breakdown, and a town scanning the landing queue has
	// not asked for planning work (gt-4k3fj.14).
	if patrol == "steward_plan" {
		if config == nil || config.Patrols == nil || config.Patrols.StewardPlan == nil {
			return false
		}
		return config.Patrols.StewardPlan.Enabled
	}
	// landing_worker is opt-in: it pushes main, so only an explicit
	// enabled:true turns it on (gt-v4ssj.2).
	if patrol == "landing_worker" {
		if config == nil || config.Patrols == nil || config.Patrols.LandingWorker == nil {
			return false
		}
		return config.Patrols.LandingWorker.Enabled
	}
	// tier_sweep is opt-in: it runs the town's expensive suites, so only an
	// explicit enabled:true turns it on (gt-vsct7.5).
	if patrol == "tier_sweep" {
		if config == nil || config.Patrols == nil || config.Patrols.TierSweep == nil {
			return false
		}
		return config.Patrols.TierSweep.Enabled
	}
	// spec_dispatch defaults ON because the seat-refill plugin it replaced is
	// deleted (gt-4k3fj.8.8), leaving it the only filler of a free seat: a
	// dispatcher that must be switched on cannot prevent the idle town it was
	// written for (gt-1gnq9). The operator hold file (<town>/seat-refill.hold)
	// and ESTOP still park it, and patrols.spec_dispatch.enabled:false turns
	// it off.
	if patrol == "spec_dispatch" {
		if config == nil || config.Patrols == nil || config.Patrols.SpecDispatch == nil {
			return true
		}
		return config.Patrols.SpecDispatch.Enabled
	}
	// events_prune defaults ON: .events.jsonl has no other bound, and a
	// pruner that must be switched on leaves the file growing (gt-ori5j).
	if patrol == "events_prune" {
		if config == nil || config.Patrols == nil || config.Patrols.EventsPrune == nil {
			return true
		}
		return config.Patrols.EventsPrune.Enabled
	}
	// patrol_scan defaults OFF: it restarts polecats on its own, so the
	// operator opts in (gt-4k3fj.6, ADR 0005).
	if patrol == "patrol_scan" {
		if config == nil || config.Patrols == nil || config.Patrols.PatrolScan == nil {
			return false
		}
		return config.Patrols.PatrolScan.Enabled
	}
	if config == nil || config.Patrols == nil {
		return true // Default: enabled
	}

	switch patrol {
	case "handler":
		if config.Patrols.Handler != nil {
			return config.Patrols.Handler.Enabled
		}
	case "git_hygiene":
		if config.Patrols.GitHygiene != nil {
			return config.Patrols.GitHygiene.Enabled
		}
	case "rebuild_gt":
		if config.Patrols.RebuildGT != nil {
			return config.Patrols.RebuildGT.Enabled
		}
	}
	return true // Default: enabled
}

// GetPatrolRigs returns the list of rigs for a patrol, or nil if all rigs should be patrolled.
func GetPatrolRigs(config *DaemonPatrolConfig, patrol string) []string {
	if config == nil || config.Patrols == nil {
		return nil // All rigs
	}

	switch patrol {
	case "patrol_scan":
		if config.Patrols.PatrolScan != nil {
			return config.Patrols.PatrolScan.Rigs
		}
	}
	return nil // All rigs
}

// loadDisabledPatrolsFromTownSettings loads the disabled_patrols list from
// town settings (settings/config.json) as a set for O(1) lookup.
func loadDisabledPatrolsFromTownSettings(townRoot string) map[string]bool {
	return loadDisabledPatrolsFromTownSettingsTo(townRoot, os.Stderr)
}

// loadDisabledPatrolsFromTownSettingsTo is loadDisabledPatrolsFromTownSettings
// with the warning writer passed in, so a unit test reads the line without
// swapping the process's stderr.
//
// It reads through the config loader so a file the kernel rejects (an unknown
// key, say) is named by *config.ParseError instead of being read as a town
// with nothing disabled (gt-y3pgh.2.7). The patrols stay enabled either way:
// this reader gates a subset of the daemon's work, not the town's startup.
func loadDisabledPatrolsFromTownSettingsTo(townRoot string, warn io.Writer) map[string]bool {
	settings, err := agentconfig.LoadOrCreateTownSettings(agentconfig.TownSettingsPath(townRoot))
	if err != nil {
		fmt.Fprintf(warn, "daemon: %v\n", err)
		return nil
	}
	if len(settings.DisabledPatrols) == 0 {
		return nil
	}
	disabled := make(map[string]bool, len(settings.DisabledPatrols))
	for _, p := range settings.DisabledPatrols {
		disabled[p] = true
	}
	return disabled
}

// isPatrolActive checks whether a patrol should run, combining the
// daemon patrol config (mayor/daemon.json) with the town-level
// disabled_patrols list (settings/config.json). A patrol is active
// only if it is enabled in daemon config AND not in the disabled list.
func (d *Daemon) isPatrolActive(patrol string) bool {
	if d.disabledPatrols[patrol] {
		return false
	}
	return IsPatrolEnabled(d.patrolConfig, patrol)
}

// LifecycleAction represents a lifecycle request action.
type LifecycleAction string

const (
	// ActionCycle restarts the session with handoff.
	ActionCycle LifecycleAction = "cycle"

	// ActionRestart does a fresh restart without handoff.
	ActionRestart LifecycleAction = "restart"

	// ActionShutdown terminates without restart.
	ActionShutdown LifecycleAction = "shutdown"
)

// LifecycleRequest represents a request from an agent to the daemon.
type LifecycleRequest struct {
	// From is the agent requesting the action (e.g., "gastown/witness").
	From string `json:"from"`

	// Action is what lifecycle action to perform.
	Action LifecycleAction `json:"action"`

	// Timestamp is when the request was made.
	Timestamp time.Time `json:"timestamp"`
}
