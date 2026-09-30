package config

import (
	"encoding/json"
	"reflect"
	"time"
)

// The mayor/daemon.json schema (gt-y3pgh.1). It lives here, not in
// internal/daemon, so the config kernel decodes daemon.json strictly with
// the same types the daemon runs on; internal/daemon aliases them. Comments
// that name functions ("see maintenance_gc.go") refer to internal/daemon.

// DaemonPatrolConfig is the structure of mayor/daemon.json.
type DaemonPatrolConfig struct {
	Type      string         `json:"type"`
	Version   int            `json:"version"`
	Heartbeat *PatrolConfig  `json:"heartbeat,omitempty"`
	Patrols   *PatrolsConfig `json:"patrols,omitempty"`
	// Env holds environment variables to set at startup.
	// Propagated to all sessions spawned by the daemon and read by gt up/mayor attach.
	// Example: {"GT_DOLT_PORT": "43211"}
	Env map[string]string `json:"env,omitempty"`
}

// PatrolsConfig holds configuration for all patrols.
type PatrolsConfig struct {
	// Refinery is ignored: the refinery was deleted (gt-v4ssj.6) and the
	// landing worker replaced it. It stays in the schema only so existing
	// daemon.json files, which the kernel decodes strictly, still load.
	// Deprecated: remove "patrols.refinery" from mayor/daemon.json.
	Refinery             *PatrolConfig               `json:"refinery,omitempty"`
	Witness              *PatrolConfig               `json:"witness,omitempty"`
	Deacon               *PatrolConfig               `json:"deacon,omitempty"`
	Handler              *PatrolConfig               `json:"handler,omitempty"`
	DoltServer           *DoltServerConfig           `json:"dolt_server,omitempty"`
	DoltBackup           *DoltBackupConfig           `json:"dolt_backup,omitempty"`
	JsonlGitBackup       *JsonlGitBackupConfig       `json:"jsonl_git_backup,omitempty"`
	WispReaper           *WispReaperConfig           `json:"wisp_reaper,omitempty"`
	DoctorDog            *DoctorDogConfig            `json:"doctor_dog,omitempty"`
	CompactorDog         *CompactorDogConfig         `json:"compactor_dog,omitempty"`
	CheckpointDog        *CheckpointDogConfig        `json:"checkpoint_dog,omitempty"`
	ScheduledMaintenance *ScheduledMaintenanceConfig `json:"scheduled_maintenance,omitempty"`
	MainBranchTest       *MainBranchTestConfig       `json:"main_branch_test,omitempty"`
	QuotaDog             *QuotaDogConfig             `json:"quota_dog,omitempty"`
	QuotaResume          *QuotaDogConfig             `json:"quota_resume,omitempty"`
	MayorDispatch        *MayorDispatchConfig        `json:"mayor_dispatch,omitempty"`
	SpecDispatch         *SpecDispatchConfig         `json:"spec_dispatch,omitempty"`
	RestartTracker       *RestartTrackerConfig       `json:"restart_tracker,omitempty"`

	// ScheduledSlings dispatches a formula onto a rig on an interval, one bead
	// per run; the open bead is the double-dispatch guard (gt-nj23).
	ScheduledSlings *ScheduledSlingsConfig `json:"scheduled_slings,omitempty"`

	// PatrolWatchdog flags a patrol role (witness, deacon, refinery) whose
	// session is alive but whose last COMPLETED patrol cycle is older than
	// N x its cadence — awake but not patrolling (gt-4z3b7).
	PatrolWatchdog *PatrolWatchdogConfig `json:"patrol_watchdog,omitempty"`

	// LandingWorker lands work beads labeled gt:ready-to-land, one worker per
	// rig (ADR 0004, gt-v4ssj.2). Opt-in: absent or enabled=false never lands.
	LandingWorker *LandingWorkerConfig `json:"landing_worker,omitempty"`

	// DoltRemotes is retired: the dolt_remotes push patrol was removed
	// (ADR 0002) and nothing reads this key. It is declared so a daemon.json
	// that still carries it decodes under strict decoding, and it is kept
	// verbatim so a rewrite does not drop operator data. Delete the key from
	// daemon.json by hand.
	DoltRemotes json.RawMessage `json:"dolt_remotes,omitempty"`
}

// DoltServerConfig holds configuration for the Dolt SQL server.
type DoltServerConfig struct {
	// Enabled controls whether the daemon manages a Dolt server.
	Enabled bool `json:"enabled"`

	// External indicates the server is externally managed (daemon monitors only).
	External bool `json:"external,omitempty"`

	// Port is the MySQL protocol port (default 3306).
	Port int `json:"port,omitempty"`

	// Host is the bind/connect address (default 127.0.0.1).
	Host string `json:"host,omitempty"`

	// User is the MySQL user name (default root).
	User string `json:"user,omitempty"`

	// Password is the MySQL password. Empty means no password.
	Password string `json:"password,omitempty"`

	// DataDir is the directory containing Dolt databases.
	// Each subdirectory becomes a database.
	DataDir string `json:"data_dir,omitempty"`

	// LogFile is the path to the Dolt server log file.
	LogFile string `json:"log_file,omitempty"`

	// AutoRestart controls whether to restart on crash.
	AutoRestart bool `json:"auto_restart,omitempty"`

	// RestartDelay is the initial delay before restarting after crash (default 5s).
	RestartDelay time.Duration `json:"restart_delay,omitempty"`

	// MaxRestartDelay is the maximum backoff delay (default 5min).
	MaxRestartDelay time.Duration `json:"max_restart_delay,omitempty"`

	// MaxRestartsInWindow is the maximum number of restarts allowed within
	// RestartWindow before escalating instead of retrying (default 5).
	MaxRestartsInWindow int `json:"max_restarts_in_window,omitempty"`

	// RestartWindow is the time window for counting restarts (default 10min).
	RestartWindow time.Duration `json:"restart_window,omitempty"`

	// HealthyResetInterval is how long the server must stay healthy before
	// the backoff counter resets (default 5min).
	HealthyResetInterval time.Duration `json:"healthy_reset_interval,omitempty"`

	// HealthCheckInterval is how often to run the Dolt health check,
	// independent of the general daemon heartbeat. This enables fast
	// detection of Dolt server crashes without changing the overall
	// heartbeat frequency. Default 30s.
	HealthCheckInterval time.Duration `json:"health_check_interval,omitempty"`
}

// DoltBackupConfig holds configuration for the dolt_backup patrol.
// This patrol periodically syncs Dolt databases to local filesystem backups.
type DoltBackupConfig struct {
	// Enabled controls whether backup sync runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to sync, as a string (e.g., "15m").
	IntervalStr string `json:"interval,omitempty"`

	// Databases lists specific database names to back up.
	// If empty, auto-discovers databases with configured backup remotes.
	Databases []string `json:"databases,omitempty"`
}

// JsonlGitBackupConfig holds configuration for the jsonl_git_backup patrol.
// This patrol exports issues to JSONL files, scrubs ephemeral data, and pushes to a git repo.
type JsonlGitBackupConfig struct {
	// Enabled controls whether JSONL git backup runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "15m").
	IntervalStr string `json:"interval,omitempty"`

	// Databases lists specific database names to export.
	// If empty, auto-discovers from dolt server.
	Databases []string `json:"databases,omitempty"`

	// GitRepo is the path to the git repository for backup.
	// Default: ~/.dolt-archive/git
	GitRepo string `json:"git_repo,omitempty"`

	// Scrub controls whether ephemeral data is filtered out.
	// Default: true
	Scrub *bool `json:"scrub,omitempty"`

	// SpikeThreshold is the maximum allowed percentage change in record counts
	// between consecutive exports. If the delta exceeds this threshold (in either
	// direction), the export is halted and escalated. Default: 0.20 (20%).
	SpikeThreshold *float64 `json:"spike_threshold,omitempty"`
}

// WispReaperConfig holds configuration for the wisp_reaper patrol.
type WispReaperConfig struct {
	Enabled      bool     `json:"enabled"`
	DryRun       bool     `json:"dry_run,omitempty"`
	IntervalStr  string   `json:"interval,omitempty"`
	MaxAgeStr    string   `json:"max_age,omitempty"`
	DeleteAgeStr string   `json:"delete_age,omitempty"`
	Databases    []string `json:"databases,omitempty"`

	// StaleIssueAgeStr overrides how long an issue may sit untouched before
	// auto-close (e.g. "720h" or "30d"). Empty means defaultStaleIssueAge.
	StaleIssueAgeStr string `json:"stale_issue_age,omitempty"`

	// AutoClose disarms ONLY the stale-issue auto-close step, leaving reap and
	// purge running — the point of a dedicated knob (gt-2qzr), since dry_run
	// pauses the whole patrol. nil means unset: the dog path keeps its default
	// behavior while the inline fallback stays disarmed.
	AutoClose *bool `json:"auto_close,omitempty"`
}

// DoctorDogConfig holds configuration for the doctor_dog patrol.
type DoctorDogConfig struct {
	// Enabled controls whether the doctor dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "5m").
	IntervalStr string `json:"interval,omitempty"`

	// Databases lists the expected production databases.
	// If empty, uses the default set.
	Databases []string `json:"databases,omitempty"`

	// Advisory thresholds — when exceeded, recommendations are added to the report.
	// Agents (Mayor/Deacon) read the report and decide what actions to take.
	// Zero values mean "use default".

	// LatencyAlertMs: latency threshold in ms. Default: 5000 (5s).
	LatencyAlertMs float64 `json:"latency_alert_ms,omitempty"`

	// OrphanAlertCount: database count threshold. Default: 20.
	OrphanAlertCount int `json:"orphan_alert_count,omitempty"`

	// BackupStaleSeconds: backup age threshold in seconds. Default: 3600 (1hr).
	BackupStaleSeconds float64 `json:"backup_stale_seconds,omitempty"`
}

// CompactorDogConfig holds configuration for the compactor_dog patrol.
type CompactorDogConfig struct {
	Enabled     bool   `json:"enabled"`
	IntervalStr string `json:"interval,omitempty"`
	// Threshold is the minimum commit count before this patrol escalates.
	// The daemon monitors and escalates — it does not compact. Defaults to
	// 2000; see defaultCompactorCommitThreshold for why that is well above the
	// plugin's 500/1000 escalation lines.
	Threshold int `json:"threshold,omitempty"`
	// Databases lists specific database names to check.
	// If empty, falls back to wisp_reaper config, then auto-discovery.
	Databases []string `json:"databases,omitempty"`

	// Deprecated: has no effect. The daemon patrol no longer compacts, so there
	// is no mode to select. Compaction is operator-only:
	// plugins/compactor-dog/run.sh --compact. The field is still parsed so that
	// a stale daemon.json value is reported rather than silently dropped.
	Mode string `json:"mode,omitempty"`
	// Deprecated: has no effect. The daemon patrol no longer compacts, so there
	// is no recent history to preserve. See Mode.
	KeepRecent int `json:"keep_recent,omitempty"`
}

// CheckpointDogConfig holds configuration for the checkpoint_dog patrol.
type CheckpointDogConfig struct {
	// Enabled controls whether the checkpoint dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "10m").
	IntervalStr string `json:"interval,omitempty"`
}

// ScheduledMaintenanceConfig holds configuration for the scheduled_maintenance patrol.
// User opts in via:
//
//	gt config set maintenance.window 03:00
//	gt config set maintenance.interval daily
//
// The daemon checks commit counts per DB during the window and then acts on
// the Mode: "monitor" (the default) escalates with the counts, "flatten" runs
// `gt maintain --force` when any DB exceeds the threshold. "gc" ignores commit
// counts and gc's each database whose on-disk size crossed the size trigger
// (GCMinBytes, GCGrowthRatio), history kept.
type ScheduledMaintenanceConfig struct {
	// Enabled controls whether scheduled maintenance runs.
	Enabled bool `json:"enabled"`

	// Window is the time of day to start maintenance (e.g., "03:00").
	// Uses 24-hour format HH:MM in local time.
	Window string `json:"window,omitempty"`

	// Interval controls how often maintenance runs.
	// Supported values: "daily", "weekly", "monthly", or a Go duration (e.g., "48h").
	// Default: "daily".
	Interval string `json:"interval,omitempty"`

	// Threshold is the minimum commit count before maintenance triggers.
	// Default: 1000.
	Threshold *int `json:"threshold,omitempty"`

	// Mode selects what happens to a database at or above the threshold.
	// MaintenanceModeMonitor (the default) escalates with the counts and
	// rewrites nothing; MaintenanceModeFlatten runs `gt maintain --force`.
	// Only the trimmed string "flatten" arms the destructive path — see
	// maintenanceMode. MaintenanceModeGC ("gc") runs a history-preserving
	// CALL dolt_gc('--full') per database on a size trigger instead of the
	// commit threshold; see maintenance_gc.go.
	//
	// Compatibility: a binary built before gc mode existed reads "gc" as
	// monitor (its resolver matches only "flatten"), so rolling back after
	// setting mode=gc degrades to escalate-only, never to a flatten.
	Mode string `json:"mode,omitempty"`

	// GCMinBytes is gc mode's size floor: a database smaller than this on
	// disk is never gc'd by the patrol. Default 256MiB (DefaultGCMinBytes);
	// a non-positive value is replaced by the default with a warning.
	GCMinBytes *int64 `json:"gc_min_bytes,omitempty"`

	// GCGrowthRatio is gc mode's growth trigger: a database at or above
	// GCMinBytes is gc'd when its size is at least this multiple of the size
	// recorded right after its last patrol gc (daemon/maintenance_state.json),
	// or when no such record exists. Default 2.0; a value below 1, NaN or
	// Inf is replaced by the default with a warning.
	GCGrowthRatio *float64 `json:"gc_growth_ratio,omitempty"`
}

// MainBranchTestConfig holds configuration for the main_branch_test patrol.
// This patrol periodically runs quality gates on each rig's main branch to
// catch regressions from direct-to-main pushes, bad merges, or sequential
// merge conflicts that individually pass but break together.
type MainBranchTestConfig struct {
	// Enabled controls whether the main-branch test runner runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "30m").
	IntervalStr string `json:"interval,omitempty"`

	// TimeoutStr is the maximum time each rig's test run can take.
	// Default: "10m".
	TimeoutStr string `json:"timeout,omitempty"`

	// Rigs limits testing to specific rigs. If empty, all rigs are tested.
	Rigs []string `json:"rigs,omitempty"`

	// MinCPUIdlePercent is the minimum estimated CPU idle percentage required
	// before a cycle starts. Below it the host is too busy for a red verdict
	// to be trustworthy — the suite's own contention produces failures that
	// pass in isolation — so the cycle is skipped and logged as
	// "skipped: host busy" instead of escalating a false FAILED (gt-f57o).
	//
	// Disabled by default (0), like the sibling spawn-pressure gate
	// (operational.daemon.pressure_cpu_threshold, also opt-in): a gate that
	// skips by default can silently stop the patrol that catches regressions
	// in main. Set 25 to skip a cycle when less than a quarter of the host's
	// CPU is idle. Values outside (0, 100] are treated as disabled.
	MinCPUIdlePercent *float64 `json:"min_cpu_idle_percent,omitempty"`

	// SkipWhenGateBusy makes a cycle yield the container-gate pool to a
	// refinery holding it instead of competing for a slot (gt-lf2r).
	//
	// This patrol is itself gate-class (role "<rig>/main-branch-test", see
	// slot.IsGateRole), so it takes the reserved slot the merge gates take: a
	// baseline run that starts while a merge gate is queued delays the merge,
	// at any interval. Yielding is what makes that harmless.
	//
	// Enabled by default, unlike MinCPUIdlePercent: the caller it protects has
	// the harder deadline, and the rig is picked up again on the next tick.
	// That default is only safe because the yield is bounded — see
	// GateBusyStarveAfterStr — and skip_when_gate_busy=false competes instead.
	SkipWhenGateBusy *bool `json:"skip_when_gate_busy,omitempty"`

	// GateBusyStarveAfterStr bounds the yield above: a rig whose skips have run
	// unbroken for this long is escalated as untested rather than quietly
	// skipped again (e.g., "6h"). Default: 6h.
	//
	// The yield trades "tested on schedule" for "the merge gate does not
	// wait", and that trade is only safe bounded: a pool held indefinitely
	// would otherwise stop the patrol that catches regressions in main,
	// silently. The rig still yields past the bound rather than running, since
	// starving a merge gate is what the yield exists to prevent; the
	// escalation is what gets the held pool looked at. A value <= 0 disables
	// the bound.
	GateBusyStarveAfterStr string `json:"gate_busy_starve_after,omitempty"`

	// IntegrationIntervalStr is how often each rig's integration tier runs
	// (see main_branch_integration.go), e.g. "24h". Default: 24h. A value
	// <= 0 ("0s") turns the integration run off.
	IntegrationIntervalStr string `json:"integration_interval,omitempty"`

	// IntegrationTimeoutStr bounds one integration run, separately from
	// TimeoutStr. Default: 30m.
	IntegrationTimeoutStr string `json:"integration_timeout,omitempty"`
}

// QuotaDogConfig holds configuration for the quota_dog patrol.
type QuotaDogConfig struct {
	// Enabled controls whether the quota dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "5m").
	IntervalStr string `json:"interval,omitempty"`
}

// MayorDispatchConfig holds configuration for the mayor_dispatch patrol.
type MayorDispatchConfig struct {
	// Enabled controls whether the patrol runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to check, as a string (e.g., "30m").
	IntervalStr string `json:"interval,omitempty"`
}

// SpecDispatchConfig configures the spec dispatcher ticker (gt-4k3fj.5): a
// ready, unassigned, spec-labeled feature bead that passes the spec lint is
// slung onto a polecat seat within the seat-class budget. Off unless enabled.
type SpecDispatchConfig struct {
	// Enabled turns the ticker on. Default off.
	Enabled bool `json:"enabled"`

	// IntervalStr is the tick cadence (default "60s").
	IntervalStr string `json:"interval,omitempty"`

	// Seats are per agent, each with its own cap, and classed by the agent's
	// provider: provider=claude is hooked (managed settings and guards), any
	// other provider is hookless. A spec uses hooked seats only, unless it
	// carries label host-safe and names no host-touching command or path.

	// HookedAgent is a provider=claude agent seat the dispatcher may use
	// beside the pool's overflow_agent. Default "claude-sonnet"; ignored when
	// its provider is not claude.
	HookedAgent string `json:"hooked_agent,omitempty"`

	// MaxHooked caps live polecats on HookedAgent (default 2). Zero means
	// default; a negative value closes the seat.
	MaxHooked int `json:"max_hooked,omitempty"`

	// HooklessAgent is an optional extra seat, typically a non-claude
	// provider. Only host-safe specs use a hookless seat.
	HooklessAgent string `json:"hookless_agent,omitempty"`

	// MaxHookless caps live polecats on HooklessAgent (default 2). The pool's
	// overflow_agent seat is capped by polecat_pool.max_overflow instead
	// (default 2 when unset).
	MaxHookless int `json:"max_hookless,omitempty"`

	// PreferHooked puts the HookedAgent seat first. Default: the pool's
	// overflow_agent first, then HooklessAgent, then HookedAgent.
	PreferHooked bool `json:"prefer_hooked,omitempty"`

	// MaxPerTick bounds slings per tick (default 1).
	MaxPerTick int `json:"max_per_tick,omitempty"`

	// Template overrides the spec template path.
	Template string `json:"template,omitempty"`
}

// RestartTrackerConfig holds configurable parameters for restart tracking.
// All fields have sensible defaults if zero-valued.
type RestartTrackerConfig struct {
	// InitialBackoff is the delay before the first retry (default 30s).
	InitialBackoff time.Duration `json:"initial_backoff,omitempty"`

	// MaxBackoff is the maximum backoff delay (default 10m).
	MaxBackoff time.Duration `json:"max_backoff,omitempty"`

	// BackoffMultiplier scales the backoff on each retry (default 2.0).
	BackoffMultiplier float64 `json:"backoff_multiplier,omitempty"`

	// CrashLoopWindow is the time window for counting crash-loop restarts (default 15m).
	CrashLoopWindow time.Duration `json:"crash_loop_window,omitempty"`

	// CrashLoopCount is how many restarts within the window trigger crash-loop state (default 5).
	CrashLoopCount int `json:"crash_loop_count,omitempty"`

	// StabilityPeriod is how long an agent must run without restarting
	// before its backoff resets (default 30m).
	StabilityPeriod time.Duration `json:"stability_period,omitempty"`

	// PauseBackoff is the fixed delay applied when an agent is paused due
	// to a transient external limit (e.g., Claude usage-limit reached)
	// rather than a true crash. Does not escalate and does not count toward
	// the crash-loop fault budget. Default 60s — long enough for the
	// quota_dog patrol to rotate accounts (5m cadence), short enough to
	// recover quickly when the limit resets.
	PauseBackoff time.Duration `json:"pause_backoff,omitempty"`

	// CrashLoopRecoveryWindow is how long an agent's heartbeat must be
	// continuously fresh with an advancing cycle count before a crash-loop
	// flag is auto-cleared (default 10m). Prevents stale flags from an
	// earlier outage from pinning a now-healthy agent indefinitely (gt-ayx).
	CrashLoopRecoveryWindow time.Duration `json:"crash_loop_recovery_window,omitempty"`
}

// ScheduledSlingsConfig is the opt-in scheduled_slings patrol: each entry is a
// formula slung onto a rig on an interval, one bead per run (gt-nj23).
type ScheduledSlingsConfig struct {
	Enabled bool                  `json:"enabled"`
	Entries []ScheduledSlingEntry `json:"entries,omitempty"`
}

// ScheduledSlingEntry is one scheduled formula. Name is the schedule's
// identity: runs are found by the label "scheduled:<name>".
type ScheduledSlingEntry struct {
	Name        string            `json:"name"`
	Rig         string            `json:"rig"`
	Formula     string            `json:"formula"`
	Agent       string            `json:"agent,omitempty"`
	IntervalStr string            `json:"interval"`
	Priority    int               `json:"priority,omitempty"`
	Vars        map[string]string `json:"vars,omitempty"`
}

// PatrolWatchdogConfig holds configuration for the patrol_watchdog patrol.
type PatrolWatchdogConfig struct {
	// Enabled controls whether the patrol runs. Defaults to true (see
	// IsPatrolEnabled) — an explicit false is required to turn it off.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often the watchdog checks, as a string (e.g. "10m").
	IntervalStr string `json:"interval,omitempty"`

	// CadenceStr is the expected time between a role's completed patrol
	// cycles, as a string (e.g. "10m"). Applied uniformly to every role;
	// per-role overrides can be added later if cadences diverge.
	CadenceStr string `json:"cadence,omitempty"`

	// Multiplier is how many cadences of silence are tolerated before a role
	// is considered stale. Zero means "use the default" (3).
	Multiplier int `json:"multiplier,omitempty"`

	// Nudge controls whether a stale-but-alive role is also sent a "resume
	// patrol" nudge in addition to being escalated. Defaults to true.
	Nudge *bool `json:"nudge,omitempty"`
}

// LandingWorkerConfig holds configuration for the landing_worker patrol.
type LandingWorkerConfig struct {
	// Enabled turns the workers on. Defaults to false: the overseer enables
	// it, because it is the one automated path that pushes main.
	Enabled bool `json:"enabled"`

	// IntervalStr is the wait between passes of one rig's worker, as a
	// string (e.g. "60s"). Default 60s.
	IntervalStr string `json:"interval,omitempty"`

	// LandTimeoutStr bounds one landing, gate and review included (e.g.
	// "90m"). Default 90m.
	LandTimeoutStr string `json:"land_timeout,omitempty"`

	// Rigs limits the workers to these rigs. Empty means every known rig.
	Rigs []string `json:"rigs,omitempty"`

	// Review runs the om editorial review on every landing. Nil or true
	// means on; only an explicit false turns it off (the landing then records
	// om_verdict "skipped").
	Review *bool `json:"review,omitempty"`

	// OMPath is the om binary. Empty means "om" on the daemon's PATH, else
	// $HOME/go/bin/om.
	OMPath string `json:"om_path,omitempty"`

	// OMTimeoutStr bounds one om review (e.g. "20m"). Default 20m. An om
	// that times out does not block the landing: it lands with
	// om_verdict "error:<reason>".
	OMTimeoutStr string `json:"om_timeout,omitempty"`

	// WorkRoot is where throwaway landing and post-landing worktrees are
	// created (a <rig> directory under it, 0700). It must not be under the
	// town root: the git guard refuses worktrees there. Empty means
	// $TMPDIR/gt-landing-<uid>.
	WorkRoot string `json:"work_root,omitempty"`

	// PostLandTimeoutStr bounds one run of the rig's
	// merge_queue.post_land_command (e.g. "60m"). Default 60m.
	PostLandTimeoutStr string `json:"post_land_timeout,omitempty"`
}

// RolePatrol returns the patrol entry for a role-shaped patrol ("witness",
// "refinery", "deacon", "handler"), or nil when the entry or the name is absent.
func (p *PatrolsConfig) RolePatrol(name string) *PatrolConfig {
	if p == nil {
		return nil
	}
	switch name {
	case "witness":
		return p.Witness
	case "deacon":
		return p.Deacon
	case "handler":
		return p.Handler
	}
	return nil
}

// Count returns how many patrol entries daemon.json declares, not counting
// the retired dolt_remotes key.
func (p *PatrolsConfig) Count() int {
	if p == nil {
		return 0
	}
	n := 0
	v := reflect.ValueOf(*p)
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Pointer && !f.IsNil() {
			n++
		}
	}
	return n
}
