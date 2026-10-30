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

	// Owner is what started this daemon (see DetectSelf). A state written by a
	// binary older than gt-qpeei.12 has none, and its readers treat that as
	// the legacy case.
	Owner *Owner `json:"owner,omitempty"`
}

// Owner names what started the daemon: the container it runs in, the service
// manager job, or the operator's hand-run process. ID is the container name,
// the job label, the systemd invocation id, or "<hostname>:<pid>".
type Owner struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
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

// The daemon patrol config schema (the "daemon" section of settings/config.json,
// the legacy mayor/daemon.json on an unmigrated town) lives in internal/config
// so the config kernel decodes the file strictly with the types the daemon runs
// on (gt-y3pgh.1). These aliases keep the daemon's names.
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
	OverrunGuardConfig         = agentconfig.OverrunGuardConfig
	ScheduledMaintenanceConfig = agentconfig.ScheduledMaintenanceConfig
	DiskMaintenanceConfig      = agentconfig.DiskMaintenanceConfig
	DiskMaintenanceTarget      = agentconfig.DiskMaintenanceTarget
	HMSyncConfig               = agentconfig.HMSyncConfig
	SpecDispatchConfig         = agentconfig.SpecDispatchConfig
	OverseerReportConfig       = agentconfig.OverseerReportConfig
	PatrolScanConfig           = agentconfig.PatrolScanConfig
	EventsPruneConfig          = agentconfig.EventsPruneConfig
	JevDedupeConfig            = agentconfig.JevDedupeConfig
	JevAttentionConfig         = agentconfig.JevAttentionConfig
	JevLandingConfig           = agentconfig.JevLandingConfig
	JevSubmissionConfig        = agentconfig.JevSubmissionConfig
	JevDriftConfig             = agentconfig.JevDriftConfig
	JevLogtriageConfig         = agentconfig.JevLogtriageConfig
	JevReworkConfig            = agentconfig.JevReworkConfig
	JevTestselConfig           = agentconfig.JevTestselConfig
	JevFailcauseConfig         = agentconfig.JevFailcauseConfig
	JevTargetsConfig           = agentconfig.JevTargetsConfig
	JevDispatchConfig          = agentconfig.JevDispatchConfig
	JevThrashConfig            = agentconfig.JevThrashConfig
	JevRulesConfig             = agentconfig.JevRulesConfig
	JevConflictConfig          = agentconfig.JevConflictConfig
	JevProgressConfig          = agentconfig.JevProgressConfig
	JevStaleConfig             = agentconfig.JevStaleConfig
	JevNotesConfig             = agentconfig.JevNotesConfig
	ScheduledSlingsConfig      = agentconfig.ScheduledSlingsConfig
	ScheduledSlingEntry        = agentconfig.ScheduledSlingEntry
	LandingWorkerConfig        = agentconfig.LandingWorkerConfig
	LandingInfraConfig         = agentconfig.LandingInfraConfig
	TierSweepConfig            = agentconfig.TierSweepConfig
)

// PatrolConfigFile returns the path to the patrol config file: the legacy
// mayor/daemon.json, which the config loaders follow to the "daemon" section of
// settings/config.json once the town has one.
func PatrolConfigFile(townRoot string) string {
	return filepath.Join(townRoot, constants.DirMayor, "daemon.json")
}

// PatrolConfigSource names the file the patrol config was read from: on the
// two-file layout the patrol config is settings/config.json's "daemon"
// section, so a startup line that names the retired mayor/daemon.json reports
// to the operator a file whose edit the daemon did not read (gt-y3pgh.12).
func PatrolConfigSource(townRoot string) string {
	return agentconfig.SourcePath(PatrolConfigFile(townRoot))
}

// LoadPatrolConfig loads patrol configuration from the "daemon" section of
// settings/config.json (the legacy mayor/daemon.json on an unmigrated town).
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

// ReadPatrolConfig loads the patrol config (see PatrolConfigFile) through the config package's one
// parser, telling absent (nil, nil) from unparseable (nil, *config.ParseError
// naming the file and offset).
func ReadPatrolConfig(townRoot string) (*DaemonPatrolConfig, error) {
	cfg, err := agentconfig.LoadDaemonPatrolConfig(PatrolConfigFile(townRoot))
	if errors.Is(err, agentconfig.ErrNotFound) {
		return nil, nil
	}
	return cfg, err
}

// SavePatrolConfig writes the patrol config (see PatrolConfigFile) through the config package's
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
	// overrun_guard is opt-in: it nudges and stops polecat sessions, so only
	// an explicit enabled:true turns it on.
	if patrol == "overrun_guard" {
		if config == nil || config.Patrols == nil || config.Patrols.OverrunGuard == nil {
			return false
		}
		return config.Patrols.OverrunGuard.Enabled
	}
	if patrol == "scheduled_maintenance" {
		if config == nil || config.Patrols == nil || config.Patrols.ScheduledMaintenance == nil {
			return false
		}
		return config.Patrols.ScheduledMaintenance.Enabled
	}
	// disk_maintenance is opt-in: it deletes registry images, so only an
	// explicit enabled:true turns it on (gt-dl4nv.15).
	if patrol == "disk_maintenance" {
		if config == nil || config.Patrols == nil || config.Patrols.DiskMaintenance == nil {
			return false
		}
		return config.Patrols.DiskMaintenance.Enabled
	}
	// hm_sync is opt-in: it runs a program on a timer, so only an explicit
	// enabled:true turns it on (gt-z6qre.15).
	if patrol == "hm_sync" {
		if config == nil || config.Patrols == nil || config.Patrols.HMSync == nil {
			return false
		}
		return config.Patrols.HMSync.Enabled
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
		if config == nil || config.Patrols == nil || config.Patrols.SpecDispatch == nil || config.Patrols.SpecDispatch.Enabled == nil {
			return true
		}
		return *config.Patrols.SpecDispatch.Enabled
	}
	// landing_infra defaults ON: it only observes (one request per instance
	// and one per rig, per pass) and it makes a Forgejo or runner outage
	// visible on the health line, so a probe that must be switched on cannot
	// prevent the landing silence it exists to name. It does nothing where no
	// rig lands through Forgejo (gt-fn9e6.11).
	if patrol == "landing_infra" {
		if config == nil || config.Patrols == nil || config.Patrols.LandingInfra == nil || config.Patrols.LandingInfra.Enabled == nil {
			return true
		}
		return *config.Patrols.LandingInfra.Enabled
	}
	// events_prune defaults ON: .events.jsonl has no other bound, and a
	// pruner that must be switched on leaves the file growing (gt-ori5j).
	if patrol == "events_prune" {
		if config == nil || config.Patrols == nil || config.Patrols.EventsPrune == nil {
			return true
		}
		return config.Patrols.EventsPrune.Enabled
	}
	// jev_dedupe is opt-in: each pass pays for Jev calls, so only an explicit
	// enabled:true turns it on.
	if patrol == "jev_dedupe" {
		if config == nil || config.Patrols == nil || config.Patrols.JevDedupe == nil {
			return false
		}
		return config.Patrols.JevDedupe.Enabled
	}
	// jev_attention is opt-in for the same reason: each pass pays for Jev calls.
	if patrol == "jev_attention" {
		if config == nil || config.Patrols == nil || config.Patrols.JevAttention == nil {
			return false
		}
		return config.Patrols.JevAttention.Enabled
	}
	// jev_landing is opt-in for the same reason.
	if patrol == "jev_landing" {
		if config == nil || config.Patrols == nil || config.Patrols.JevLanding == nil {
			return false
		}
		return config.Patrols.JevLanding.Enabled
	}
	// jev_submission is opt-in for the same reason.
	if patrol == "jev_submission" {
		if config == nil || config.Patrols == nil || config.Patrols.JevSubmission == nil {
			return false
		}
		return config.Patrols.JevSubmission.Enabled
	}
	// jev_drift is opt-in for the same reason.
	if patrol == "jev_drift" {
		if config == nil || config.Patrols == nil || config.Patrols.JevDrift == nil {
			return false
		}
		return config.Patrols.JevDrift.Enabled
	}
	// jev_rework is opt-in for the same reason.
	if patrol == "jev_rework" {
		if config == nil || config.Patrols == nil || config.Patrols.JevRework == nil {
			return false
		}
		return config.Patrols.JevRework.Enabled
	}
	// jev_logtriage is opt-in for the same reason.
	if patrol == "jev_logtriage" {
		if config == nil || config.Patrols == nil || config.Patrols.JevLogtriage == nil {
			return false
		}
		return config.Patrols.JevLogtriage.Enabled
	}
	// jev_testsel is opt-in for the same reason. It is read at each landing's
	// gate, not on a ticker.
	if patrol == "jev_testsel" {
		if config == nil || config.Patrols == nil || config.Patrols.JevTestsel == nil {
			return false
		}
		return config.Patrols.JevTestsel.Enabled
	}
	// jev_failcause is opt-in for the same reason. It is read at each red
	// candidate verdict, not on a ticker.
	if patrol == "jev_failcause" {
		if config == nil || config.Patrols == nil || config.Patrols.JevFailcause == nil {
			return false
		}
		return config.Patrols.JevFailcause.Enabled
	}
	// jev_dispatch is opt-in for the same reason.
	if patrol == "jev_dispatch" {
		if config == nil || config.Patrols == nil || config.Patrols.JevDispatch == nil {
			return false
		}
		return config.Patrols.JevDispatch.Enabled
	}
	// jev_thrash is opt-in for the same reason.
	if patrol == "jev_thrash" {
		if config == nil || config.Patrols == nil || config.Patrols.JevThrash == nil {
			return false
		}
		return config.Patrols.JevThrash.Enabled
	}
	// jev_rules is opt-in for the same reason.
	if patrol == "jev_rules" {
		if config == nil || config.Patrols == nil || config.Patrols.JevRules == nil {
			return false
		}
		return config.Patrols.JevRules.Enabled
	}
	// jev_conflict is opt-in for the same reason.
	if patrol == "jev_conflict" {
		if config == nil || config.Patrols == nil || config.Patrols.JevConflict == nil {
			return false
		}
		return config.Patrols.JevConflict.Enabled
	}
	// jev_progress is opt-in for the same reason.
	if patrol == "jev_progress" {
		if config == nil || config.Patrols == nil || config.Patrols.JevProgress == nil {
			return false
		}
		return config.Patrols.JevProgress.Enabled
	}
	// jev_stale is opt-in for the same reason.
	if patrol == "jev_stale" {
		if config == nil || config.Patrols == nil || config.Patrols.JevStale == nil {
			return false
		}
		return config.Patrols.JevStale.Enabled
	}
	// jev_notes is opt-in for the same reason.
	if patrol == "jev_notes" {
		if config == nil || config.Patrols == nil || config.Patrols.JevNotes == nil {
			return false
		}
		return config.Patrols.JevNotes.Enabled
	}
	// jev_targets is opt-in for the same reason: each deep-review run it
	// serves pays for Jev calls. It is called by the deep-review schedule, not
	// on a ticker, so this key alone does not call it.
	if patrol == "jev_targets" {
		if config == nil || config.Patrols == nil || config.Patrols.JevTargets == nil {
			return false
		}
		return config.Patrols.JevTargets.Enabled
	}
	// rebuild_om is opt-in: it swaps the om binary the landing worker execs,
	// so only an explicit enabled:true turns it on (gt-0f228).
	if patrol == "rebuild_om" {
		if config == nil || config.Patrols == nil || config.Patrols.RebuildOM == nil {
			return false
		}
		return config.Patrols.RebuildOM.Enabled
	}
	// overseer_report defaults OFF: the operator retires the overseer's cron
	// prompt before the daemon takes the digest over (gt-a6fg7).
	if patrol == "overseer_report" {
		if config == nil || config.Patrols == nil || config.Patrols.OverseerReport == nil {
			return false
		}
		return config.Patrols.OverseerReport.Enabled
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
	// resource_sampler defaults ON: the snapshot reads a silent sampler as an
	// error, and a sampler that must be switched on cannot fill the metrics a
	// new town needs (gt-qqwey).
	case "resource_sampler":
		if config.Patrols.ResourceSampler != nil {
			return config.Patrols.ResourceSampler.Enabled
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
// daemon patrol config (settings/config.json's "daemon" section) with the town-level
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
