package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// The daemon patrol schema (gt-y3pgh.1): settings/config.json's "daemon"
// section on a two-file town, mayor/daemon.json on a five-file one. It lives
// here, not in internal/daemon, so the config kernel decodes that document
// strictly with the same types the daemon runs on; internal/daemon aliases
// them. Comments that name functions ("see maintenance_gc.go") refer to
// internal/daemon.

// DaemonPatrolConfig is the structure of the daemon patrol config:
// settings/config.json's "daemon" section on a two-file town,
// mayor/daemon.json on a five-file one.
type DaemonPatrolConfig struct {
	Type      string         `json:"type"`
	Version   int            `json:"version"`
	Heartbeat *PatrolConfig  `json:"heartbeat,omitempty"`
	Patrols   *PatrolsConfig `json:"patrols,omitempty"`
	// Env holds environment variables to set at startup.
	// Propagated to all sessions spawned by the daemon and read by gt up.
	// Example: {"GT_SEAT_REFILL_MAX_PRIORITY": "50"}
	Env map[string]string `json:"env,omitempty"`
}

// PatrolsConfig holds configuration for all patrols.
type PatrolsConfig struct {
	// Refinery is ignored: the refinery was deleted (gt-v4ssj.6) and the
	// landing worker replaced it. It stays in the schema only so existing
	// daemon.json files, which the kernel decodes strictly, still load.
	// Deprecated: remove "patrols.refinery" from the daemon patrol config
	// (settings/config.json's "daemon" section on a two-file town,
	// mayor/daemon.json on a five-file one).
	Refinery *PatrolConfig `json:"refinery,omitempty"`
	// Mayor and MayorDispatch are retired (gt-rwp7z.3): the role's supervision
	// and the idle-seat patrol are gone and nothing reads these keys. They are
	// declared, like the other retired keys below, so a daemon.json or
	// settings/config.json that still carries them decodes under strict
	// decoding and is kept verbatim through a rewrite. A retired patrol is
	// simply absent from the schema's active fields, so it has no effect.
	Mayor         json.RawMessage `json:"mayor,omitempty"`
	MayorDispatch json.RawMessage `json:"mayor_dispatch,omitempty"`

	Handler              *PatrolConfig               `json:"handler,omitempty"`
	DoltServer           *DoltServerConfig           `json:"dolt_server,omitempty"`
	JsonlGitBackup       *JsonlGitBackupConfig       `json:"jsonl_git_backup,omitempty"`
	WispReaper           *WispReaperConfig           `json:"wisp_reaper,omitempty"`
	DoctorDog            *DoctorDogConfig            `json:"doctor_dog,omitempty"`
	CompactorDog         *CompactorDogConfig         `json:"compactor_dog,omitempty"`
	CheckpointDog        *CheckpointDogConfig        `json:"checkpoint_dog,omitempty"`
	OverrunGuard         *OverrunGuardConfig         `json:"overrun_guard,omitempty"`
	ScheduledMaintenance *ScheduledMaintenanceConfig `json:"scheduled_maintenance,omitempty"`
	SpecDispatch         *SpecDispatchConfig         `json:"spec_dispatch,omitempty"`
	OverseerReport       *OverseerReportConfig       `json:"overseer_report,omitempty"`
	PatrolScan           *PatrolScanConfig           `json:"patrol_scan,omitempty"`
	EventsPrune          *EventsPruneConfig          `json:"events_prune,omitempty"`

	// JevDedupe asks Jev which existing bead each new finding duplicates and
	// logs every answer (internal/jev/dedupe). Opt-in: each pass pays for Jev
	// calls, so only an explicit enabled:true turns it on.
	JevDedupe *JevDedupeConfig `json:"jev_dedupe,omitempty"`

	// JevAttention asks Jev the question set about each new attention item and
	// logs every answer (internal/jev/attention). Opt-in, for the same reason
	// as JevDedupe.
	JevAttention *JevAttentionConfig `json:"jev_attention,omitempty"`

	// JevLanding asks Jev a second opinion on each risk-path landing and logs
	// the answers (internal/jev/landing). Opt-in for the same reason as
	// JevDedupe.
	JevLanding *JevLandingConfig `json:"jev_landing,omitempty"`

	// JevDispatch asks Jev to predict, for each bead just placed on a seat,
	// whether it lands first try, its size and the model it needs, and logs
	// every answer (internal/jev/dispatch). Opt-in, like JevDedupe.
	JevDispatch *JevDispatchConfig `json:"jev_dispatch,omitempty"`

	// RestartTracker is retired (gt-gqftr): no code read the block. It is
	// declared, like the other retired keys, so a daemon.json that still
	// carries it keeps loading under strict decoding and is kept verbatim.
	// Delete the key by hand.
	RestartTracker json.RawMessage `json:"restart_tracker,omitempty"`

	// GitHygiene cleans merged and orphaned branches and runs git gc in each
	// rig's repository (internal/daemon/git_hygiene.go). On when absent.
	GitHygiene *PatrolConfig `json:"git_hygiene,omitempty"`

	// RebuildGT brings the installed gt binary in force from main when it
	// falls behind (internal/daemon/rebuild_gt.go, was the rebuild-gt plugin).
	// On when absent: a patrol that has to be switched on cannot prevent the
	// staleness it exists for (gt-oqbw).
	RebuildGT *PatrolConfig `json:"rebuild_gt,omitempty"`

	// RebuildOM keeps the installed om at the om origin's main
	// (internal/daemon/rebuild_om.go). Opt-in: it builds and swaps a binary
	// the landing worker execs, so the operator turns it on.
	RebuildOM *PatrolConfig `json:"rebuild_om,omitempty"`

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

	// Steward and StewardPlan are the retired job-runner patrols' keys: the
	// job runner was removed (gt-4rizh) and nothing reads them. Declared as
	// raw values, like the other retired keys above, so a daemon.json or
	// settings/config.json that still carries a patrols.steward block decodes
	// under strict decoding and is kept verbatim through a rewrite.
	Steward     json.RawMessage `json:"steward,omitempty"`
	StewardPlan json.RawMessage `json:"steward_plan,omitempty"`

	// LandingInfra probes the Forgejo instance and CI runners a cut-over rig
	// lands through, so an outage shows on the town health line instead of
	// only as a landing that never finishes (internal/daemon/landing_infra.go,
	// gt-fn9e6.11). On when absent: it only observes, and it makes no request
	// where no rig carries merge_queue.forgejo.
	LandingInfra *LandingInfraConfig `json:"landing_infra,omitempty"`

	// TierSweep runs scripts/tier-sweep.sh against origin/main on an interval,
	// the tiers `make gate` does not run (internal/daemon/tier_sweep.go,
	// gt-vsct7.5). Opt-in: it runs the town's expensive suites.
	TierSweep *TierSweepConfig `json:"tier_sweep,omitempty"`

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

	// AutoRestart decides whether the manager itself brings a crashed or
	// unhealthy server back up. Nil means on; false leaves it as the check
	// found it and raises the alert alone. A server that is simply not
	// running is still started: that is a start, not a restart (gt-gqftr).
	AutoRestart *bool `json:"auto_restart,omitempty"`

	// RestartDelayStr is the wait before the first restart after a crash
	// (default "5s"); it doubles per restart, up to MaxRestartDelayStr. The
	// five duration knobs take a string; a bare number is a type error
	// rather than a nanosecond count (gt-gqftr). An unparseable or
	// non-positive string reads as the default.
	RestartDelayStr string `json:"restart_delay,omitempty"`

	// MaxRestartDelayStr caps the backoff delay (default "5m").
	MaxRestartDelayStr string `json:"max_restart_delay,omitempty"`

	// MaxRestartsInWindow is the maximum number of restarts allowed within
	// RestartWindowStr before escalating instead of retrying (default 5).
	MaxRestartsInWindow int `json:"max_restarts_in_window,omitempty"`

	// RestartWindowStr is the time window for counting restarts (default "10m").
	RestartWindowStr string `json:"restart_window,omitempty"`

	// HealthyResetIntervalStr is how long the server must stay healthy before
	// the backoff counter resets (default "5m").
	HealthyResetIntervalStr string `json:"healthy_reset_interval,omitempty"`

	// HealthCheckIntervalStr is how often to run the Dolt health check,
	// independent of the general daemon heartbeat, so a crash is seen within
	// seconds of it happening (default "30s").
	HealthCheckIntervalStr string `json:"health_check_interval,omitempty"`
}

// The dolt_server knobs' defaults, read through the accessors below. They live
// here rather than at the call sites so a block that omits a key cannot fall
// back to something else per caller (gt-gqftr).
const (
	DefaultDoltRestartDelay         = 5 * time.Second
	DefaultDoltMaxRestartDelay      = 5 * time.Minute
	DefaultDoltRestartWindow        = 10 * time.Minute
	DefaultDoltHealthyResetInterval = 5 * time.Minute
	DefaultDoltHealthCheckInterval  = 30 * time.Second
)

// AutoRestartEnabled reports whether the manager restarts the server itself; an
// absent key means it does.
func (c *DoltServerConfig) AutoRestartEnabled() bool {
	return c == nil || c.AutoRestart == nil || *c.AutoRestart
}

// RestartDelay returns the wait before the first restart after a crash.
func (c *DoltServerConfig) RestartDelay() time.Duration {
	if c == nil {
		return DefaultDoltRestartDelay
	}
	return positiveDurationOrDefault(c.RestartDelayStr, DefaultDoltRestartDelay)
}

// MaxRestartDelay returns the cap on the backoff delay.
func (c *DoltServerConfig) MaxRestartDelay() time.Duration {
	if c == nil {
		return DefaultDoltMaxRestartDelay
	}
	return positiveDurationOrDefault(c.MaxRestartDelayStr, DefaultDoltMaxRestartDelay)
}

// RestartWindow returns the window restarts are counted in.
func (c *DoltServerConfig) RestartWindow() time.Duration {
	if c == nil {
		return DefaultDoltRestartWindow
	}
	return positiveDurationOrDefault(c.RestartWindowStr, DefaultDoltRestartWindow)
}

// HealthyResetInterval returns how long the server must stay healthy before the
// backoff counter resets.
func (c *DoltServerConfig) HealthyResetInterval() time.Duration {
	if c == nil {
		return DefaultDoltHealthyResetInterval
	}
	return positiveDurationOrDefault(c.HealthyResetIntervalStr, DefaultDoltHealthyResetInterval)
}

// HealthCheckInterval returns how often the Dolt health check runs.
func (c *DoltServerConfig) HealthCheckInterval() time.Duration {
	if c == nil {
		return DefaultDoltHealthCheckInterval
	}
	return positiveDurationOrDefault(c.HealthCheckIntervalStr, DefaultDoltHealthCheckInterval)
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
	// Operators read the report and decide what actions to take.
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
	// 2000; see daemon.DefaultCompactorCommitThreshold.
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

// OverrunGuardConfig holds configuration for the overrun_guard patrol, which
// nudges a polecat once when its bead runs long and stops the session when it
// runs over. Off unless Enabled is true.
type OverrunGuardConfig struct {
	// Enabled controls whether the overrun guard runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "1m"). Default 1m.
	IntervalStr string `json:"interval,omitempty"`

	// NudgeAfter is how long after a bead was slung to its seat the one split
	// nudge goes out (e.g., "12m"). Default 12m.
	NudgeAfter string `json:"nudge_after,omitempty"`

	// StopAfter is how long after the sling the seat is stopped (e.g., "20m").
	// Default 20m.
	StopAfter string `json:"stop_after,omitempty"`

	// ScheduledStopAfter is how long after the sling a seat on a scheduled
	// run (a bead labeled scheduled:<name>) is stopped (e.g., "60m"). Those
	// runs are judged by this limit alone, not by NudgeAfter or StopAfter.
	// Default 60m; "0" never stops them. A negative or unparseable value loads
	// with a warning and is kept as written; the guard uses the default, and
	// saving it is refused.
	ScheduledStopAfter string `json:"scheduled_stop_after,omitempty"`

	// DryRun logs what a stop would do and sends nothing.
	DryRun bool `json:"dry_run,omitempty"`
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

// JevDedupeConfig switches on the duplicate-finding pass and prices its Jev
// spend. The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevDedupeConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the job's spend reaches
	// it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`
}

// JevAttentionConfig switches on the attention-item pass and prices its Jev
// spend. The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevAttentionConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the job's spend reaches
	// it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`
}

// JevLandingConfig switches on the landing battery and prices its Jev spend.
// The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevLandingConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`
}

// JevDispatchConfig switches on the dispatch battery and prices its Jev spend.
// The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevDispatchConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend, every
	// caller counted, reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`
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

// OverseerReportConfig configures the overseer_report patrol (gt-a6fg7): every
// interval the daemon writes the digest the overseer used to write by hand to
// <town>/.runtime/overseer/reports/, where the dashboard's Report pane reads
// it. Off unless enabled.
type OverseerReportConfig struct {
	// Enabled turns the patrol on. Default off.
	Enabled bool `json:"enabled"`

	// IntervalStr is the slot length and cadence (default "15m").
	IntervalStr string `json:"interval,omitempty"`
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

	// WorktreeCleanup reaps polecat worktrees that can never be reused.
	// Absent means off: the tick behaves as if the block were not there.
	WorktreeCleanup *WorktreeCleanupConfig `json:"worktree_cleanup,omitempty"`
}

// WorktreeCleanupConfig configures the patrol_scan reap pass (gt-rwfua).
// Every default fails safe: an unset block is off, and an enabled block
// without a dry_run key is dry-run. Read it through the accessors, which
// treat a nil receiver and an empty or malformed field as its default.
type WorktreeCleanupConfig struct {
	// Enabled turns the reap pass on. Default off.
	Enabled bool `json:"enabled"`

	// DryRun reports candidates without removing them. It is a pointer so an
	// unset key (nil) defaults to true; only an explicit false reaps.
	DryRun *bool `json:"dry_run,omitempty"`

	// Rigs limits the pass to these rigs; empty means ["gastown"].
	Rigs []string `json:"rigs,omitempty"`

	// GraceStr is how long a seat must sit idle before it is eligible
	// (default "30m").
	GraceStr string `json:"grace,omitempty"`

	// ParkedGraceStr is the idle time a parked seat needs (default "24h").
	ParkedGraceStr string `json:"parked_grace,omitempty"`

	// MaxPerTick caps removals per tick; zero or negative means 2, never
	// unlimited.
	MaxPerTick int `json:"max_per_tick,omitempty"`

	// BlockedAlertThreshold is the blocked count that raises one alert
	// (default 5).
	BlockedAlertThreshold int `json:"blocked_alert_threshold,omitempty"`
}

// DefaultWorktreeCleanupRig is the rig the reap pass covers when Rigs is empty.
const DefaultWorktreeCleanupRig = "gastown"

// IsEnabled reports whether the reap pass runs; a nil block is off.
func (c *WorktreeCleanupConfig) IsEnabled() bool { return c != nil && c.Enabled }

// IsDryRun reports whether the pass only reports; a nil block, or one without
// an explicit dry_run key, is dry-run.
func (c *WorktreeCleanupConfig) IsDryRun() bool {
	return c == nil || c.DryRun == nil || *c.DryRun
}

// CoversRig reports whether the reap pass may touch rig; an empty Rigs list
// covers only DefaultWorktreeCleanupRig.
func (c *WorktreeCleanupConfig) CoversRig(rig string) bool {
	if c == nil || len(c.Rigs) == 0 {
		return rig == DefaultWorktreeCleanupRig
	}
	for _, r := range c.Rigs {
		if r == rig {
			return true
		}
	}
	return false
}

// Grace returns the idle time before a seat is eligible, defaulting to 30m on
// an unset or malformed value.
func (c *WorktreeCleanupConfig) Grace() time.Duration {
	return positiveDurationOrDefault(c.graceStr(), 30*time.Minute)
}

// ParkedGrace returns the idle time a parked seat needs, defaulting to 24h on
// an unset or malformed value.
func (c *WorktreeCleanupConfig) ParkedGrace() time.Duration {
	return positiveDurationOrDefault(c.parkedGraceStr(), 24*time.Hour)
}

// Cap returns the per-tick removal cap, defaulting to 2 for zero or negative.
func (c *WorktreeCleanupConfig) Cap() int {
	if c == nil || c.MaxPerTick <= 0 {
		return 2
	}
	return c.MaxPerTick
}

// AlertThreshold returns the blocked count that raises one alert, defaulting
// to 5 for zero or negative.
func (c *WorktreeCleanupConfig) AlertThreshold() int {
	if c == nil || c.BlockedAlertThreshold <= 0 {
		return 5
	}
	return c.BlockedAlertThreshold
}

func (c *WorktreeCleanupConfig) graceStr() string {
	if c == nil {
		return ""
	}
	return c.GraceStr
}

func (c *WorktreeCleanupConfig) parkedGraceStr() string {
	if c == nil {
		return ""
	}
	return c.ParkedGraceStr
}

// positiveDurationOrDefault parses a duration knob written as a string, falling
// back to def when it is unset, unparseable or non-positive. A non-positive
// value is not usable for a knob that bounds a wait: it would make a grace
// expire instantly, or spin a restart with no backoff at all.
func positiveDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// SpecDispatchConfig configures the spec dispatcher ticker (gt-4k3fj.5): a
// ready, unassigned work bead that passes the spec lint is slung onto a
// polecat seat within the seat budget. The retired label spec and type feature
// are accepted and ignored (gt-mmsr2). On unless disabled (gt-1gnq9).
type SpecDispatchConfig struct {
	// Enabled runs the ticker. Nil means on (gt-1gnq9): the seat-refill plugin
	// is deleted, so nothing else fills a free seat, and a block that tunes
	// only "interval" or "max_per_tick" must not switch the dispatcher off by
	// omitting the key. Set false to turn it off.
	Enabled *bool `json:"enabled,omitempty"`

	// IntervalStr is the tick cadence (default "60s").
	IntervalStr string `json:"interval,omitempty"`

	// The dispatcher's seats are polecat_pool.seats in the town settings: one
	// entry per agent, each with its own cap. hooked_agent, max_hooked and
	// prefer_hooked are retired with the second seat they named (gt-mukgk);
	// gt pool migrate converts a file that still holds them.

	// HooklessAgent and MaxHookless are retired with the hookless seat class
	// (gt-4k3fj.8.7): nothing reads them. They are declared so a daemon.json
	// that still carries them decodes under strict decoding, and kept
	// verbatim. Delete the keys by hand.
	HooklessAgent json.RawMessage `json:"hookless_agent,omitempty"`
	MaxHookless   json.RawMessage `json:"max_hookless,omitempty"`

	// MaxPerTick bounds slings per tick (default 1).
	MaxPerTick int `json:"max_per_tick,omitempty"`

	// Template overrides the spec template path.
	Template string `json:"template,omitempty"`
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

	// Dispatch says who runs a due run. Empty means the scheduler slings the
	// run bead itself, to Agent. "spec" means the scheduler only creates the
	// shaped bead, labeled scheduled:<name> and formula:<formula> with Vars in
	// a "## Formula vars" section, and the spec dispatcher picks the seat, the
	// queue position and the moment: a full pool is a wait, not a failed sling.
	// The dispatcher chooses the seat and the moment, so a "spec" entry runs
	// on any agent with a free seat and cannot carry an Agent: the file does
	// not load with one (retired_keys.go, gt-mukgk).
	Dispatch string `json:"dispatch,omitempty"`

	// GateCommand is a shell command that decides whether a due run happens.
	// The patrol runs it through `sh -c` in <town>/<rig>/mayor/rig, with
	// GT_TOWN_ROOT and GT_SCHEDULE_NAME set, after the interval and open-run
	// checks say a run is due and before the run bead is created. Exit 0 runs;
	// exit 1 means nothing to do (no bead; the next tick asks again); any other
	// exit or a timeout skips the tick, and the third failure in a row raises a
	// low attention item scheduled-gate:<name>. Empty means no gate: every due
	// interval runs.
	GateCommand string `json:"gate_command,omitempty"`

	// GateTimeoutStr bounds GateCommand, as a Go duration (default 60s).
	GateTimeoutStr string `json:"gate_timeout,omitempty"`
}

// TierSweepConfig holds configuration for the tier_sweep patrol
// (internal/daemon/tier_sweep.go, gt-vsct7.5).
type TierSweepConfig struct {
	// Enabled turns the sweep on. Defaults to false: it runs the shell,
	// integration and race tiers, so the operator opts in.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often a rig is swept, as a string (e.g. "1h").
	// Default 1h.
	IntervalStr string `json:"interval,omitempty"`

	// Rigs limits the sweep to these rigs. Empty means ["gastown"], the one
	// rig the overseer's cron swept.
	Rigs []string `json:"rigs,omitempty"`

	// RunnerIdleCommand is a shell command that reports whether the CI runner
	// is idle: exit 0 means idle, anything else (a command that cannot start
	// included) means busy. The sweep runs it, through `sh -c` in the
	// checked-out tree, before a stage that starts containers takes the
	// container-gate slot, so a busy runner delays that stage rather than
	// stalling its containers beside the load (gt-5ejux). Empty means no wait,
	// the behavior without this key.
	RunnerIdleCommand string `json:"runner_idle_command,omitempty"`

	// RunnerIdleWaitStr caps the wait RunnerIdleCommand describes, as a string
	// (e.g. "10m"). Default 10m. The stage starts anyway when the cap runs
	// out, and the cycle's log says it ran under load.
	RunnerIdleWaitStr string `json:"runner_idle_wait,omitempty"`

	// RestartStaleAfterStr is how old the last sweep may be before a pending
	// upgrade restart stops deferring the next one, as a string (e.g. "90m").
	// Default 90m. Past the bound the sweep runs and the restart waits for it
	// (gt-zvqn8).
	RestartStaleAfterStr string `json:"restart_stale_after,omitempty"`
}

// LandingWorkerConfig holds configuration for the landing_worker patrol.
type LandingWorkerConfig struct {
	// Enabled turns the workers on. Defaults to false: the overseer enables
	// it, because it is the one automated path that pushes main.
	Enabled bool `json:"enabled"`

	// IntervalStr is the wait between passes of one rig's worker, as a
	// string (e.g. "60s"). Default 60s.
	IntervalStr string `json:"interval,omitempty"`

	// LandTimeoutStr is no longer read. It bounded a landing that ran the
	// local merged-tree gate; every rig now lands through Forgejo CI, and the
	// deadline is derived from the CI wait, om_timeout and the merge slack
	// (gt-fn9e6.26, gt-fn9e6.32). The key still decodes so an existing
	// daemon.json keeps loading.
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

	// LintTimeoutStr is no longer read: it bounded the lint stage of the local
	// merged-tree gate, which is gone (gt-fn9e6.32). A CI gate's timeout is
	// the CI wait's own. The key still decodes so an existing daemon.json
	// keeps loading.
	LintTimeoutStr string `json:"lint_timeout,omitempty"`

	// TestTimeoutStr is no longer read, for the same reason as
	// LintTimeoutStr: the local gate whose test stage it bounded is gone
	// (gt-fn9e6.32). The key still decodes so an existing daemon.json keeps
	// loading.
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

	// Forgejo holds the town-level Forgejo facts this patrol needs, the same
	// for every rig (gt-fn9e6.3). Nil means the defaults.
	Forgejo *ForgejoWorkerConfig `json:"forgejo,omitempty"`
}

// ForgejoWorkerConfig is the landing worker's town-level Forgejo block. The
// half that changes per rig — the remote URL, the gate workflow, the bot
// logins and the mirror target — is merge_queue.forgejo, resolved by
// rig.ResolveForgejoConfig. What is left is the same for every rig, which is
// why it is daemon.json's and not a rig's (gt-fn9e6.3).
type ForgejoWorkerConfig struct {
	// TokenDir is the directory holding the per-role bot tokens, read by the
	// Forgejo client as <token_dir>/forgejo-<role>.env, mode 600. Empty means
	// DefaultForgejoTokenDir: $XDG_CONFIG_HOME/gt, else ~/.config/gt, the
	// path the epic names. The key names a directory, never a token: a token
	// value here would be a secret in config, which the design forbids.
	TokenDir string `json:"token_dir,omitempty"`
}

// ForgejoTokenDir returns the directory the per-role Forgejo token files live
// in: the configured directory, else DefaultForgejoTokenDir. Nil-safe.
func (c *LandingWorkerConfig) ForgejoTokenDir() (string, error) {
	if c != nil && c.Forgejo != nil && c.Forgejo.TokenDir != "" {
		return c.Forgejo.TokenDir, nil
	}
	return DefaultForgejoTokenDir()
}

// DefaultForgejoTokenDir returns the host's default token directory:
// $XDG_CONFIG_HOME/gt when set, else ~/.config/gt (the path the epic names
// for forgejo-<role>.env).
func DefaultForgejoTokenDir() (string, error) {
	return forgejoTokenDir(os.Getenv, os.UserHomeDir)
}

// forgejoTokenDir is DefaultForgejoTokenDir with its environment injected,
// the way config.ClaudeConfigDir is, so a test can pin the directory without
// mutating the process environment.
func forgejoTokenDir(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gt"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gt"), nil
}

// LandingInfraConfig configures the landing_infra probe (gt-fn9e6.11). The
// probe exists to make a Forgejo or CI-runner outage visible; it changes
// nothing about landing, which keeps waiting out CI silence on its own
// infrastructure-retry path.
type LandingInfraConfig struct {
	// Enabled turns the probe on. Nil means on: a probe that must be switched
	// on cannot report the outage it exists for, and it does nothing where no
	// rig lands through Forgejo.
	Enabled *bool `json:"enabled,omitempty"`

	// IntervalStr is the wait between probe passes, as a string (e.g.
	// "60s"). Default 60s.
	IntervalStr string `json:"interval,omitempty"`
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
