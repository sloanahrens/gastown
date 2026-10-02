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
	// Example: {"GT_DOLT_LOGLEVEL": "info"}
	Env map[string]string `json:"env,omitempty"`
}

// PatrolsConfig holds configuration for all patrols.
type PatrolsConfig struct {
	// Refinery is ignored: the refinery was deleted (gt-v4ssj.6) and the
	// landing worker replaced it. It stays in the schema only so existing
	// daemon.json files, which the kernel decodes strictly, still load.
	// Deprecated: remove "patrols.refinery" from mayor/daemon.json.
	Refinery *PatrolConfig `json:"refinery,omitempty"`
	// Mayor gates the daemon's ensure-mayor supervision: {"enabled": false} stops
	// the daemon from restarting a missing Mayor session (default on).
	Mayor                *PatrolConfig               `json:"mayor,omitempty"`
	Handler              *PatrolConfig               `json:"handler,omitempty"`
	DoltServer           *DoltServerConfig           `json:"dolt_server,omitempty"`
	JsonlGitBackup       *JsonlGitBackupConfig       `json:"jsonl_git_backup,omitempty"`
	WispReaper           *WispReaperConfig           `json:"wisp_reaper,omitempty"`
	DoctorDog            *DoctorDogConfig            `json:"doctor_dog,omitempty"`
	CompactorDog         *CompactorDogConfig         `json:"compactor_dog,omitempty"`
	CheckpointDog        *CheckpointDogConfig        `json:"checkpoint_dog,omitempty"`
	ScheduledMaintenance *ScheduledMaintenanceConfig `json:"scheduled_maintenance,omitempty"`
	MayorDispatch        *MayorDispatchConfig        `json:"mayor_dispatch,omitempty"`
	SpecDispatch         *SpecDispatchConfig         `json:"spec_dispatch,omitempty"`
	PatrolScan           *PatrolScanConfig           `json:"patrol_scan,omitempty"`
	RestartTracker       *RestartTrackerConfig       `json:"restart_tracker,omitempty"`
	EventsPrune          *EventsPruneConfig          `json:"events_prune,omitempty"`

	// GitHygiene cleans merged and orphaned branches and runs git gc in each
	// rig's repository (internal/daemon/git_hygiene.go). On when absent.
	GitHygiene *PatrolConfig `json:"git_hygiene,omitempty"`

	// RebuildGT brings the installed gt binary in force from main when it
	// falls behind (internal/daemon/rebuild_gt.go, was the rebuild-gt plugin).
	// On when absent: a patrol that has to be switched on cannot prevent the
	// staleness it exists for (gt-oqbw).
	RebuildGT *PatrolConfig `json:"rebuild_gt,omitempty"`

	// MainBranchTest is the deleted main_branch_test patrol's key
	// (gt-v4ssj.4: the landing worker's post-landing run owns red main). It is
	// accepted and ignored so a daemon.json that still carries it parses.
	MainBranchTest json.RawMessage `json:"main_branch_test,omitempty"`

	// ScheduledSlings dispatches a formula onto a rig on an interval, one bead
	// per run; the open bead is the double-dispatch guard (gt-nj23).
	ScheduledSlings *ScheduledSlingsConfig `json:"scheduled_slings,omitempty"`

	// LandingWorker lands work beads labeled gt:ready-to-land, one worker per
	// rig (ADR 0004, gt-v4ssj.2). Opt-in: absent or enabled=false never lands.
	LandingWorker *LandingWorkerConfig `json:"landing_worker,omitempty"`

	// Steward spawns one headless job per landing-queue event, the daemon side
	// of gt-9bioi. Opt-in: the jobs act on submitted work.
	Steward *StewardConfig `json:"steward,omitempty"`

	// DoltBackup is retired: the 15-minute dolt_backup patrol (dolt backup
	// sync into <town>/.dolt-backup plus an iCloud rsync) was replaced by the
	// nightly backup in scheduled_maintenance (gt-8z769.5) and nothing reads
	// this key. It is declared so a daemon.json that still carries it decodes
	// under strict decoding, and kept verbatim. Delete the key by hand.
	DoltBackup json.RawMessage `json:"dolt_backup,omitempty"`

	// DoltRemotes is retired: the dolt_remotes push patrol was removed
	// (ADR 0002) and nothing reads this key. It is declared so a daemon.json
	// that still carries it decodes under strict decoding, and it is kept
	// verbatim so a rewrite does not drop operator data. Delete the key from
	// daemon.json by hand.
	DoltRemotes json.RawMessage `json:"dolt_remotes,omitempty"`

	// QuotaDog and QuotaResume are retired: the quota patrols were deleted
	// with gt quota (gt-638go.2) and nothing reads these keys. They are
	// declared so a daemon.json that still carries them decodes under strict
	// decoding, and kept verbatim so a rewrite does not drop operator data.
	// Delete the keys from daemon.json by hand.
	QuotaDog    json.RawMessage `json:"quota_dog,omitempty"`
	QuotaResume json.RawMessage `json:"quota_resume,omitempty"`

	// Witness, Deacon and PatrolWatchdog are retired: the witness and deacon
	// roles and the watchdog over their patrols were deleted when the
	// patrol_scan tick replaced them (gt-4k3fj.6.1, ADR 0005), and nothing
	// reads these keys. They are declared so a daemon.json that still
	// carries them decodes under strict decoding, and kept verbatim so a
	// rewrite does not drop operator data. Delete the keys from daemon.json
	// by hand.
	Witness        json.RawMessage `json:"witness,omitempty"`
	Deacon         json.RawMessage `json:"deacon,omitempty"`
	PatrolWatchdog json.RawMessage `json:"patrol_watchdog,omitempty"`
}

// DoltServerConfig holds configuration for the Dolt SQL server.
type DoltServerConfig struct {
	// Enabled controls whether the daemon manages a Dolt server.
	Enabled bool `json:"enabled"`

	// External indicates the server is externally managed (daemon monitors only).
	External bool `json:"external,omitempty"`

	// Port and Host are the town's Dolt endpoint, filled from
	// config.ResolveDoltEndpoint when the daemon builds its server manager.
	// daemon.json may still carry them, but the values there are ignored
	// (gt-y3pgh.9): mayor/town.json "dolt" is the one source.
	Port int    `json:"port,omitempty"`
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

	// BackupStaleSeconds: nightly backup age threshold in seconds. Default: 129600 (36h).
	BackupStaleSeconds float64 `json:"backup_stale_seconds,omitempty"`
}

// CompactorDogConfig holds configuration for the compactor_dog patrol.
type CompactorDogConfig struct {
	Enabled     bool   `json:"enabled"`
	IntervalStr string `json:"interval,omitempty"`
	// Threshold is the minimum commit count before this patrol escalates.
	// The daemon monitors and escalates — it does not compact. Defaults to
	// 2000; see defaultCompactorCommitThreshold.
	Threshold int `json:"threshold,omitempty"`
	// Databases lists specific database names to check.
	// If empty, falls back to wisp_reaper config, then auto-discovery.
	Databases []string `json:"databases,omitempty"`

	// Deprecated: has no effect. The daemon patrol no longer compacts, so there
	// is no mode to select. Rewriting history is an offline operator
	// procedure (docs/dolt-history-offline.md). The field is still parsed so that
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

// ScheduledMaintenanceConfig holds configuration for the scheduled_maintenance
// patrol, the town's one Dolt GC actor. User opts in via:
//
//	gt config set maintenance.window 03:00
//
// In the window the daemon runs CALL dolt_gc('--full') on each database that
// is due: weekly, or sooner when its old generation grew more than 20% since
// its last gc (internal/daemon/maintenance_gc.go). The schedule is fixed by
// policy (gt-8z769.3); only the window is configurable.
type ScheduledMaintenanceConfig struct {
	// Enabled controls whether scheduled maintenance runs.
	Enabled bool `json:"enabled"`

	// Window is the time of day to start maintenance (e.g., "03:00").
	// Uses 24-hour format HH:MM in local time.
	Window string `json:"window,omitempty"`

	// Deprecated: have no effect. They configured the retired monitor,
	// flatten and size-triggered gc modes. They are still parsed so a live
	// daemon.json that carries them decodes strictly, and the daemon logs
	// that they are ignored rather than dropping them silently.
	Interval      string   `json:"interval,omitempty"`
	Threshold     *int     `json:"threshold,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	GCMinBytes    *int64   `json:"gc_min_bytes,omitempty"`
	GCGrowthRatio *float64 `json:"gc_growth_ratio,omitempty"`
}

// MayorDispatchConfig holds configuration for the mayor_dispatch patrol.
type MayorDispatchConfig struct {
	// Enabled controls whether the patrol runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to check, as a string (e.g., "30m").
	IntervalStr string `json:"interval,omitempty"`
}

// EventsPruneConfig bounds the town's raw event log, .events.jsonl, which
// otherwise only grows (gt-ori5j). On when the key is absent; an explicit
// entry must set "enabled". Empty fields take the daemon's defaults
// (see events_prune.go).
type EventsPruneConfig struct {
	// Enabled controls whether the patrol runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to prune, as a string (e.g., "1h").
	IntervalStr string `json:"interval,omitempty"`

	// MaxAgeStr drops events older than this, as a string (e.g., "168h").
	MaxAgeStr string `json:"max_age,omitempty"`

	// MaxBytes caps the file size; over it, the newest half of the cap is kept.
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

// PatrolScanConfig configures the patrol_scan tick (ADR 0005, gt-4k3fj.6):
// the deterministic Go replacement for the witness LLM's patrol. Per rig it
// restarts confirmed-dead polecats holding unfinished, unheld work through the
// supervisor, closes orphaned work molecules of polecats that no longer exist,
// and reports stranded work once per window. Off unless enabled.
type PatrolScanConfig struct {
	// Enabled turns the tick on. Default off.
	Enabled bool `json:"enabled"`

	// IntervalStr is the tick cadence (default "2m").
	IntervalStr string `json:"interval,omitempty"`

	// Rigs limits the tick to these rigs; empty means every operational rig.
	Rigs []string `json:"rigs,omitempty"`

	// DeadSamples is how many consecutive dead liveness samples a seat needs
	// before a restart (default 2).
	DeadSamples int `json:"dead_samples,omitempty"`

	// ReportWindow is the minimum gap between two stranded-work comments on
	// one bead (default "24h").
	ReportWindow string `json:"report_window,omitempty"`
}

// SpecDispatchConfig configures the spec dispatcher ticker (gt-4k3fj.5): a
// ready, unassigned work bead that passes the spec lint is slung onto a
// polecat seat within the seat budget. The retired label spec and type feature
// are accepted and ignored (gt-mmsr2). Off unless enabled.
type SpecDispatchConfig struct {
	// Enabled turns the ticker on. Default off.
	Enabled bool `json:"enabled"`

	// IntervalStr is the tick cadence (default "60s").
	IntervalStr string `json:"interval,omitempty"`

	// Seats are per agent, each with its own cap. Every agent runs the claude
	// CLI with the town's managed settings, so no seat is less guarded than
	// another. The "hooked" key names predate that and are kept for config
	// compatibility.

	// HookedAgent is the agent seat the dispatcher may use beside the pool's
	// overflow_agent. Default "claude-sonnet".
	HookedAgent string `json:"hooked_agent,omitempty"`

	// MaxHooked caps live polecats on HookedAgent (default 2). Zero means
	// default; a negative value closes the seat.
	MaxHooked int `json:"max_hooked,omitempty"`

	// HooklessAgent and MaxHookless are retired with the hookless seat class
	// (gt-4k3fj.8.7): nothing reads them. They are declared so a daemon.json
	// that still carries them decodes under strict decoding, and kept
	// verbatim. Delete the keys by hand.
	HooklessAgent json.RawMessage `json:"hookless_agent,omitempty"`
	MaxHookless   json.RawMessage `json:"max_hookless,omitempty"`

	// PreferHooked puts the HookedAgent seat first. Default: the pool's
	// overflow_agent first (capped by polecat_pool.max_overflow, default 2
	// when unset), then HookedAgent.
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
	// the crash-loop fault budget. Default 60s: long enough to damp the
	// retry loop, short enough to recover quickly when the limit resets.
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

	// OMTimeoutStr bounds the one om run (e.g. "5m"). Default 5m. om runs
	// only after lint and tests pass and is never retried; work never lands
	// unreviewed: a review with no verdict rejects the landing to
	// gt:needs-human and escalates (gt-b5ugw), except a red-main revert.
	OMTimeoutStr string `json:"om_timeout,omitempty"`

	// LintTimeoutStr bounds the gate's lint stage (make gate-lint), its
	// lint-lock wait included (e.g. "2m"). Default 2m. A lint that outlives
	// it is an infrastructure failure (lock contention, not a verdict): the
	// next pass retries it (gt-b5ugw).
	LintTimeoutStr string `json:"lint_timeout,omitempty"`

	// TestTimeoutStr bounds the gate's test stage (make gate-test: build and
	// the unit tier), or the whole gate when the rig's gate is one command
	// (e.g. "6m"). Default 6m. A test stage that outlives it rejects the
	// landing as a timeout to gt:needs-human and escalates (gt-b5ugw).
	TestTimeoutStr string `json:"test_timeout,omitempty"`

	// AlarmAfterStr is how long a landing's gate or om stage runs before the
	// worker logs "SLOW <stage>" and files a low-severity escalation with the
	// stage's process tree (e.g. "8m"). Default 8m. It reports only; the stage
	// timeouts kill (gt-lcu5p).
	AlarmAfterStr string `json:"alarm_after,omitempty"`

	// WorkRoot is where throwaway landing and post-landing worktrees are
	// created (a <rig> directory under it, 0700). It must not be under the
	// town root: the git guard refuses worktrees there. Empty means
	// $TMPDIR/gt-landing-<uid>.
	WorkRoot string `json:"work_root,omitempty"`

	// PostLandTimeoutStr bounds one run of the rig's
	// merge_queue.post_land_command (e.g. "60m"). Default 60m.
	PostLandTimeoutStr string `json:"post_land_timeout,omitempty"`
}

// StewardConfig holds configuration for the steward patrol.
type StewardConfig struct {
	// Enabled turns the jobs on. Defaults to false: a job acts on submitted
	// work, so the operator opts in.
	Enabled bool `json:"enabled"`

	// IntervalStr is the wait between landing-queue scans (e.g. "60s").
	// Default 60s.
	IntervalStr string `json:"interval,omitempty"`

	// MaxJobs caps jobs running at once across every rig (default 2).
	MaxJobs int `json:"max_jobs,omitempty"`

	// JobTimeoutStr bounds one job, agent session included (e.g. "45m").
	// Default 45m.
	JobTimeoutStr string `json:"job_timeout,omitempty"`

	// RoutineAgent is the preset routine jobs run on (default
	// "deepseek-flash": Sloan's Q3 call to try flash first).
	RoutineAgent string `json:"routine_agent,omitempty"`

	// HardAgent is the preset a job retries on after a routine failure, and
	// the one a conflict job starts on (default "deepseek-pro": the town runs fully on DeepSeek, Claude is only the human-run overseer).
	HardAgent string `json:"hard_agent,omitempty"`

	// Mode is "shadow" or "live" (default shadow). In shadow a job reviews
	// and decides but posts only "STEWARD (shadow)" comments: it pushes,
	// requeues, re-slings and escalates nothing, so the overseer can compare
	// its verdicts with its own before live lets it act (gt-9bioi.4).
	Mode string `json:"mode,omitempty"`

	// Rigs limits the scans to these rigs. Empty means every known rig.
	Rigs []string `json:"rigs,omitempty"`

	// WorkRoot is where a job's throwaway worktree is created (a <rig>
	// directory under it, 0700). It must not be under the town root: the git
	// guard refuses worktrees there. Empty means $TMPDIR/gt-steward-<uid>.
	WorkRoot string `json:"work_root,omitempty"`
}

// RolePatrol returns the patrol entry for a role-shaped patrol ("handler"),
// or nil when the entry or the name is absent.
func (p *PatrolsConfig) RolePatrol(name string) *PatrolConfig {
	if p == nil {
		return nil
	}
	switch name {
	case "handler":
		return p.Handler
	}
	return nil
}

// Count returns how many patrol entries daemon.json declares, not counting
// the retired keys (dolt_backup, dolt_remotes, quota_dog, quota_resume, witness, deacon,
// patrol_watchdog).
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
