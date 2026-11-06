package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	// Attention is retired (gt-3ey5j): attention.jev_triage was the planned
	// switch for the three-way Jev triage of the attention queue, and nothing
	// read it. The triage and its replay were replaced by the attention
	// battery and its scorecard adapter (gt jev attention, gt jev scorecard
	// --battery attention). It is declared so a daemon.json that still
	// carries the block decodes under strict decoding, and kept verbatim so a
	// rewrite does not drop operator data. Delete the key by hand.
	Attention json.RawMessage `json:"attention,omitempty"`
	// Env holds environment variables to set at startup.
	// Propagated to all sessions spawned by the daemon and read by gt up.
	// Example: {"GT_SEAT_REFILL_MAX_PRIORITY": "50"}
	Env map[string]string `json:"env,omitempty"`

	// Supervisor selects the service manager a provisioned daemon runs under;
	// empty keeps the host's own detection. SupervisorKind validates it.
	Supervisor string `json:"supervisor,omitempty"`

	// ContainerNameSetting is the container the container supervisor starts
	// and inspects, read through ContainerName so the default lives in one
	// place. The name is per town: a scratch town names its own, so it can
	// never reach the live daemon's container. The field cannot take the
	// method's name in Go, hence the suffix.
	ContainerNameSetting string `json:"container_name,omitempty"`

	// ComposeProjectSetting is the compose project the container supervisor
	// runs under, read through ComposeProject for the same reason.
	ComposeProjectSetting string `json:"compose_project,omitempty"`

	// OMTokenMetaSetting is the path of the small JSON sidecar whose expires_at
	// field dates the subscription token om authenticates with in the container
	// (gt-qpeei.37). Setting it is what tells gt doctor that this town runs om
	// on a token: unset, the expiry check is not applicable. It is a setting,
	// not a GT_ variable, because the environment-read guard forbids a new one
	// and the same town settings are read on both sides of the container
	// boundary. The sidecar never holds the token.
	OMTokenMetaSetting string `json:"om_token_meta,omitempty"`

	// ContainerOffSetting names the plugins and patrols this town's daemon does
	// not run while it is the container: the ones that need the host's
	// toolchain or engine (gt-1oz0t). The launchd daemon ignores it and runs
	// everything. The host agent runs the same names on the host.
	ContainerOffSetting []string `json:"container_off,omitempty"`
}

// The supervisor kinds daemon.supervisor accepts, and the names the container
// kind runs under when the town sets neither.
const (
	SupervisorLaunchd   = "launchd"
	SupervisorSystemd   = "systemd"
	SupervisorContainer = "container"

	// ContainerModeEnv is the environment variable the rendered compose file
	// sets to "container" (SupervisorContainer) beside the daemon, which puts
	// `gt daemon run` in container mode: it writes no pid file, because a pid
	// from another namespace means nothing on the host.
	ContainerModeEnv = "GT_DAEMON_SUPERVISOR"

	// ContainerNameEnv carries the container name the rendered compose file
	// set beside the daemon into container mode, where it is the owner id the
	// daemon writes into state.json (gt-qpeei.12). An empty value is a refused
	// start, never a guessed id.
	ContainerNameEnv = "GT_DAEMON_CONTAINER_NAME"

	// TakeoverEnv is the variable the compose file passes through from the
	// operator's `--takeover` start. A value of "1" lets the startup guard pass
	// a recorded owner that the probe shows not running (gt-qpeei.27).
	TakeoverEnv = "GT_DAEMON_TAKEOVER"

	// LaunchdJobEnv is the variable launchd sets to the job label of the
	// process it started; the label is the launchd owner's id (gt-qpeei.12).
	LaunchdJobEnv = "XPC_SERVICE_NAME"

	// SystemdInvocationEnv is the variable systemd sets to the unit's
	// invocation id, which is the systemd owner's id (gt-qpeei.12).
	SystemdInvocationEnv = "INVOCATION_ID"

	// DefaultContainerName and DefaultComposeProject name the container and
	// its compose project for a town that sets container_name and
	// compose_project neither.
	DefaultContainerName  = "gt-town-daemon-1"
	DefaultComposeProject = "gt-town"

	// ScratchTownMarker is the file the runbook creates in the scratch town
	// root. Its presence proves the directory is the scratch town, and it is
	// visible from the host (gt-qpeei.26).
	ScratchTownMarker = ".gt-scratch-town"

	// ScratchInstallIncompleteMarker is the file the entrypoint creates in the
	// scratch town root, exclusively, before it runs the install, and removes
	// only after the install succeeded. While it exists the entrypoint starts
	// no daemon, whatever mayor/town.json looks like, so an install that failed
	// and could not be cleaned up never becomes a town the next start runs
	// (gt-qpeei.26).
	ScratchInstallIncompleteMarker = ".gt-install-incomplete"

	// ScratchTownFlag is the entrypoint flag, without its dashes, that asks for
	// the scratch town's one install; the entrypoint registers it and the
	// scratch render passes it, so both read one string (gt-qpeei.26).
	ScratchTownFlag = "scratch-town"

	// OMTokenFileFlag is the entrypoint flag, without its dashes, that names the
	// file the om subscription token is read from; the entrypoint registers it
	// and the scratch render can pass it, so both read one string (gt-qpeei.36).
	OMTokenFileFlag = "om-token-file" //nolint:gosec // a flag name, not a credential

	// DoltHostFlag is the entrypoint flag, without its dashes, that names the
	// Dolt server the scratch town's install uses; the entrypoint registers it
	// and hands its value to `gt install` as the same flag, and the scratch
	// render passes it, so all three read one string (gt-4f3j2). Only
	// --scratch-town takes it: the live project's command line stays empty.
	DoltHostFlag = "dolt-host"

	// DoltPortFlag is the entrypoint flag, without its dashes, that names the
	// port of the Dolt server DoltHostFlag names; the entrypoint hands it to
	// `gt install` as the same flag, and the scratch render passes it, so all
	// three read one string. Without it the install dials Dolt's default 3307.
	DoltPortFlag = "dolt-port"

	// ForgeHostFlag and ForgeTokenFileFlag are the entrypoint flags, without
	// their dashes, that make the container's git ask the forge credential
	// helper (GitCredentialForgejoCommand) for the landing bot's login and
	// token: the Forgejo's host:port, and the absolute path of the bot's token
	// file. The entrypoint registers them, the scratch render passes them when
	// the throwaway Forgejo is on, and the live render passes them when
	// `gt daemon compose write` is given them, so every one reads one string
	// (gt-2d6f2). They are taken together or not at all, in either container.
	ForgeHostFlag      = "forge-host"
	ForgeTokenFileFlag = "forge-token-file" //nolint:gosec // a flag name, not a credential

	// GitCredentialForgejoCommand is the gt subcommand that answers git's
	// credential protocol for a Forgejo from a bot's token file (gt-dhjz6).
	GitCredentialForgejoCommand = "git-credential-forgejo" //nolint:gosec // a command name, not a credential
)

// ForgeHostPattern is the shape of a forge host:port: DNS-name characters
// starting with a letter or digit (so a leading dash cannot read as a flag), a
// colon and the port's digits, which the caller range-checks. The container
// entrypoint and the live compose render both read it, so the host a render
// writes into the command line is one the entrypoint accepts (gt-rpx80).
var ForgeHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*:([0-9]{1,5})$`)

// SupervisorKind returns the configured supervisor kind, or "" when the town
// sets none and the host's own detection answers, and an error for any value
// that is not one of the three kinds.
func (c *DaemonPatrolConfig) SupervisorKind() (string, error) {
	if c == nil || c.Supervisor == "" {
		return "", nil
	}
	switch c.Supervisor {
	case SupervisorLaunchd, SupervisorSystemd, SupervisorContainer:
		return c.Supervisor, nil
	}
	return "", fmt.Errorf("daemon.supervisor %q: accepted values are %q, %q and %q; leave it unset to detect the host's service manager",
		c.Supervisor, SupervisorLaunchd, SupervisorSystemd, SupervisorContainer)
}

// OMTokenMeta returns the sidecar path daemon.om_token_meta sets, "" when the
// town sets none.
func (c *DaemonPatrolConfig) OMTokenMeta() string {
	if c == nil {
		return ""
	}
	return c.OMTokenMetaSetting
}

// ContainerOff returns the plugin and patrol names daemon.container_off lists.
func (c *DaemonPatrolConfig) ContainerOff() []string {
	if c == nil {
		return nil
	}
	return c.ContainerOffSetting
}

// ContainerName returns the container the container supervisor manages.
func (c *DaemonPatrolConfig) ContainerName() string {
	if c == nil || c.ContainerNameSetting == "" {
		return DefaultContainerName
	}
	return c.ContainerNameSetting
}

// ComposeProject returns the compose project the container supervisor runs
// under.
func (c *DaemonPatrolConfig) ComposeProject() string {
	if c == nil || c.ComposeProjectSetting == "" {
		return DefaultComposeProject
	}
	return c.ComposeProjectSetting
}

// The shapes docker and compose accept for a container name and a compose
// project. The container supervisor builds argv from both, so a value outside
// them (a leading dash, a space, a slash) must be refused before it gets there.
var (
	containerNameShape  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)
	composeProjectShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// ValidateContainerNames refuses a container_name or compose_project that is set
// without the other or is outside the shape docker and compose accept, because a
// town that sets one inherits the live town's default for the other.
func (c *DaemonPatrolConfig) ValidateContainerNames() error {
	if c == nil {
		return nil
	}
	if (c.ContainerNameSetting == "") != (c.ComposeProjectSetting == "") {
		return fmt.Errorf("daemon.container_name and daemon.compose_project: set both or neither; a town that sets one inherits the live town's default for the other")
	}
	if c.ContainerNameSetting != "" && !containerNameShape.MatchString(c.ContainerNameSetting) {
		return fmt.Errorf("daemon.container_name %q: want at least two letters, digits, dots, dashes and underscores, starting with a letter or digit", c.ContainerNameSetting)
	}
	if c.ComposeProjectSetting != "" && !composeProjectShape.MatchString(c.ComposeProjectSetting) {
		return fmt.Errorf("daemon.compose_project %q: want lowercase letters, digits, dashes and underscores, starting with a letter or digit", c.ComposeProjectSetting)
	}
	return nil
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
	DiskMaintenance      *DiskMaintenanceConfig      `json:"disk_maintenance,omitempty"`
	HMSync               *HMSyncConfig               `json:"hm_sync,omitempty"`
	SpecDispatch         *SpecDispatchConfig         `json:"spec_dispatch,omitempty"`
	OverseerReport       *OverseerReportConfig       `json:"overseer_report,omitempty"`
	BirdReports          *BirdReportsConfig          `json:"bird_reports,omitempty"`
	FindingsFiler        *FindingsFilerConfig        `json:"findings_filer,omitempty"`
	PatrolScan           *PatrolScanConfig           `json:"patrol_scan,omitempty"`
	EventsPrune          *EventsPruneConfig          `json:"events_prune,omitempty"`

	// ResourceSampler appends the host's CPU and memory rows every interval,
	// which the metrics snapshot's resources group reads
	// (internal/daemon/resource_sampler.go, gt-qqwey). On when absent: the
	// snapshot reports a silent sampler as an error, so a sampler that must be
	// switched on would leave every new town with that error.
	ResourceSampler *PatrolConfig `json:"resource_sampler,omitempty"`

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

	// JevSubmission asks Jev about each branch submitted for landing and logs
	// the answers (internal/jev/submission). Opt-in for the same reason as
	// JevDedupe.
	JevSubmission *JevSubmissionConfig `json:"jev_submission,omitempty"`

	// JevDrift asks Jev whether each landing left the doc sections closest to
	// its diff stale, and logs the answers (internal/jev/drift). Opt-in for the
	// same reason as JevDedupe.
	JevDrift *JevDriftConfig `json:"jev_drift,omitempty"`
	// JevTestsel asks Jev which packages a landing's diff cannot affect and
	// logs that beside the landing gate's per-package results
	// (internal/jev/testsel). Shadow only: the gate still runs every package.
	// Opt-in for the same reason as JevDedupe.
	JevTestsel *JevTestselConfig `json:"jev_testsel,omitempty"`

	// JevFailcause asks Jev, for each Go test a candidate gate fails, whether
	// it will fail again within fourteen days and what cause fits its failure
	// excerpt, and logs both answers (internal/jev/failcause). Shadow only: the
	// verdict is untouched. Opt-in for the same reason as JevDedupe.
	JevFailcause *JevFailcauseConfig `json:"jev_failcause,omitempty"`

	// JevTargets asks Jev to score each file in the deep-review rotation slice
	// and logs the answers beside the slice's risk rank (internal/jev/targets).
	// The deep-review script reads it, not the daemon. Opt-in for the same
	// reason as JevDedupe.
	JevTargets *JevTargetsConfig `json:"jev_targets,omitempty"`

	// JevDispatch asks Jev to predict, for each bead just placed on a seat,
	// whether it lands first try, its size and the model it needs, and logs
	// every answer (internal/jev/dispatch). Opt-in, like JevDedupe.
	JevDispatch *JevDispatchConfig `json:"jev_dispatch,omitempty"`

	// JevConflict asks Jev, for each bead just placed while others were in
	// flight, whether the two will touch the same files and the same function,
	// and logs every answer (internal/jev/conflict). It changes no dispatch.
	// Opt-in, like JevDispatch, and under the same town-wide daily cap.
	JevConflict *JevConflictConfig `json:"jev_conflict,omitempty"`

	// JevProgress asks Jev whether a polecat the overrun guard nudged is making
	// progress, from the last fifteen minutes of its transcript, and logs every
	// answer (internal/jev/progress). It acts on nothing. Opt-in, like
	// JevDedupe.
	JevProgress *JevProgressConfig `json:"jev_progress,omitempty"`

	// JevStale asks Jev, once a week, whether each open bead untouched for
	// fourteen days is obsolete, superseded, worth doing, in need of
	// reshaping, and how its priority fits, and logs every answer
	// (internal/jev/stale). It acts on nothing. Opt-in, like JevDedupe.
	JevStale *JevStaleConfig `json:"jev_stale,omitempty"`

	// JevNotes asks Jev, once a week, whether each memory note and the operator
	// handbook still describes the town, and logs every answer beside a
	// mechanical label (internal/jev/notes). It edits no note. Opt-in, like
	// JevDedupe.
	JevNotes *JevNotesConfig `json:"jev_notes,omitempty"`

	// JevScorecard refreshes the cached scorecard once a day and at start when
	// the cache is older than its interval (internal/daemon/jev_scorecard_patrol.go).
	// It runs `gt jev scorecard --refresh`, so it is opt-in like JevDedupe.
	JevScorecard *JevScorecardConfig `json:"jev_scorecard,omitempty"`

	// JevLogtriage asks Jev about each group of the daemon log's warning and
	// error lines, every fifteen minutes, and logs every answer
	// (internal/jev/logtriage). It acts on nothing. Opt-in, like JevDedupe.
	JevLogtriage *JevLogtriageConfig `json:"jev_logtriage,omitempty"`

	// JevRework asks Jev, for each om review that refused a landing, why it
	// failed and whether the landing will need a rework commit, and logs both
	// answers (internal/jev/rework). It changes no landing. Opt-in for the
	// same reason as JevDedupe.
	JevRework *JevReworkConfig `json:"jev_rework,omitempty"`

	// JevThrash asks Jev, for each bead just placed on a seat, whether the
	// polecat will be respawned on it and whether the overrun guard will stop
	// it for running long, and logs both answers (internal/jev/thrash). It
	// changes no bead. Opt-in and under the same town-wide daily cap as
	// JevDispatch.
	JevThrash *JevThrashConfig `json:"jev_thrash,omitempty"`

	// JevRules asks Jev, for each submission gt done made, whether the polecat
	// session broke a standing rule and which rule, and logs both answers
	// (internal/jev/rules). It changes nothing. Opt-in and under the same
	// town-wide daily cap as JevDispatch.
	JevRules *JevRulesConfig `json:"jev_rules,omitempty"`

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

	// Update lets the daemon decide when `gt update` (install, image and
	// maintenance as one job) is due (internal/daemon/update_due.go). Off when
	// absent: the job swaps the running binary and holds dispatch, so a town
	// that has not asked for it must not get it from a default. Read through
	// UpdateConfig.Settings, which also parses the window and the durations.
	Update *UpdateConfig `json:"update,omitempty"`

	// RebuildOM keeps the installed om at the om origin's main
	// (internal/daemon/rebuild_om.go). Opt-in: it builds and swaps a binary
	// the landing worker execs, so the operator turns it on.
	RebuildOM *PatrolConfig `json:"rebuild_om,omitempty"`

	// RebuildHM installs the hm origin's main when the hm-skew monitor reads the
	// installed hm as behind (internal/daemon/rebuild_hm.go). Opt-in: it swaps
	// the hm binary on the machine, so the operator turns it on.
	RebuildHM *PatrolConfig `json:"rebuild_hm,omitempty"`

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

	// TierSweep runs the shell tier and the integration and race tiers (the
	// Go command, cmd/tier-sweep, and the shell runner it calls,
	// internal/shelltier) against origin/main on an interval, the tiers
	// `make gate` does not run (internal/daemon/tier_sweep.go, gt-vsct7.5).
	// Opt-in: it runs the town's expensive suites.
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

// Daemon config keys for the update block, so every refusal names the key the
// operator has to edit.
const (
	UpdateWindowKey      = "patrols.update.window"
	UpdateMinIntervalKey = "patrols.update.min_interval"
	UpdateMaxRuntimeKey  = "patrols.update.max_runtime"
)

// The update block's defaults and bounds.
const (
	DefaultUpdateWindow      = "03:00"
	DefaultUpdateMinInterval = 3 * time.Hour
	DefaultUpdateMaxRuntime  = 2 * time.Hour

	MinUpdateMinInterval = 30 * time.Minute
	MaxUpdateMinInterval = 24 * time.Hour
	MinUpdateMaxRuntime  = 10 * time.Minute
	MaxUpdateMaxRuntime  = 12 * time.Hour
)

// UpdateConfig is the daemon.patrols.update block: whether the daemon decides
// that `gt update` is due, and the window and limits it decides with.
type UpdateConfig struct {
	// Enabled turns the trigger on. Absent means off.
	Enabled bool `json:"enabled"`

	// Window is the daily start of the update window, HH:MM local time.
	// Empty means DefaultUpdateWindow.
	Window string `json:"window,omitempty"`

	// MinInterval is how long after a run ends before main moving with
	// landings waiting makes the next one due, as a Go duration. Empty means
	// DefaultUpdateMinInterval.
	MinInterval string `json:"min_interval,omitempty"`

	// MaxRuntime is how long a run may last before the daemon alerts on it,
	// as a Go duration. Empty means DefaultUpdateMaxRuntime.
	MaxRuntime string `json:"max_runtime,omitempty"`
}

// UpdateSettings is UpdateConfig parsed into the values the due check uses.
type UpdateSettings struct {
	Enabled                  bool
	WindowHour, WindowMinute int
	MinInterval, MaxRuntime  time.Duration
}

// Settings returns the update settings with the defaults applied. A nil
// receiver gives the defaults with Enabled false. A window that is not HH:MM,
// or a duration that does not parse or falls outside its bounds, is refused
// rather than clamped, and the error names the daemon.json key.
func (c *UpdateConfig) Settings() (UpdateSettings, error) {
	cfg := UpdateConfig{}
	if c != nil {
		cfg = *c
	}
	s := UpdateSettings{Enabled: cfg.Enabled}

	window := DefaultUpdateWindow
	if set := strings.TrimSpace(cfg.Window); set != "" {
		window = set
	}
	hour, minute, err := parseUpdateWindow(window)
	if err != nil {
		return UpdateSettings{}, fmt.Errorf("%s = %q: %w", UpdateWindowKey, window, err)
	}
	s.WindowHour, s.WindowMinute = hour, minute

	if s.MinInterval, err = parseUpdateDuration(UpdateMinIntervalKey, cfg.MinInterval, DefaultUpdateMinInterval, MinUpdateMinInterval, MaxUpdateMinInterval); err != nil {
		return UpdateSettings{}, err
	}
	if s.MaxRuntime, err = parseUpdateDuration(UpdateMaxRuntimeKey, cfg.MaxRuntime, DefaultUpdateMaxRuntime, MinUpdateMaxRuntime, MaxUpdateMaxRuntime); err != nil {
		return UpdateSettings{}, err
	}
	return s, nil
}

// parseUpdateWindow parses HH:MM, with both parts exactly two digits so "3pm",
// "3:00" and "+3:00" are refused rather than guessed at.
func parseUpdateWindow(window string) (hour, minute int, err error) {
	parts := strings.SplitN(window, ":", 2)
	if len(parts) != 2 || !twoDigits(parts[0]) || !twoDigits(parts[1]) {
		return 0, 0, errors.New("want HH:MM")
	}
	hour, _ = strconv.Atoi(parts[0])
	if hour > 23 {
		return 0, 0, errors.New("want an hour from 00 to 23")
	}
	minute, _ = strconv.Atoi(parts[1])
	if minute > 59 {
		return 0, 0, errors.New("want a minute from 00 to 59")
	}
	return hour, minute, nil
}

// twoDigits reports whether s is exactly two ASCII digits.
func twoDigits(s string) bool {
	return len(s) == 2 && s[0] >= '0' && s[0] <= '9' && s[1] >= '0' && s[1] <= '9'
}

// parseUpdateDuration reads one duration setting, keeping def when it is
// empty, and refusing one outside lo to hi rather than clamping it.
func parseUpdateDuration(key, setting string, def, lo, hi time.Duration) (time.Duration, error) {
	setting = strings.TrimSpace(setting)
	if setting == "" {
		return def, nil
	}
	d, err := time.ParseDuration(setting)
	if err != nil {
		return 0, fmt.Errorf("%s = %q: want a Go duration such as 3h: %w", key, setting, err)
	}
	if d < lo || d > hi {
		return 0, fmt.Errorf("%s = %q: want a duration from %s to %s", key, setting, lo, hi)
	}
	return d, nil
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

	// EventsRetentionStr turns on events retention (e.g. "30d", "720h"): rows
	// of the events table older than it are deleted in counted batches, and only
	// those the events archive has already written out (gt-2i850). A database
	// with no archive watermark deletes nothing. Empty leaves the step off
	// (gt-hk21d, opt-in because deleting audit rows is the operator's call).
	EventsRetentionStr string `json:"events_retention,omitempty"`

	// ExtraBeadsRoots are directories searched for a database's beads
	// directory after the town root, so the reaper can reach a workspace
	// outside the town (the claude database lives in the ~/.claude
	// workspace). Empty searches the town alone. Read it through
	// ParseExtraBeadsRoots, which refuses a relative path (gt-33etw).
	ExtraBeadsRoots []string `json:"extra_beads_roots,omitempty"`
}

// maxDayCount is the most whole days a time.Duration holds. A larger day count
// wraps the multiplication into a different duration, so ParseAgeDuration
// refuses it rather than return an age the operator did not ask for.
const maxDayCount = int64(math.MaxInt64) / int64(24*time.Hour)

// ParseAgeDuration parses an age string, accepting the day suffix the retired
// mol-dog-reaper formula emitted for its 30d default (gt-2qzr). time.ParseDuration
// rejects "30d" as "unknown unit d".
func ParseAgeDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(days), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day suffix in %q: %w", s, err)
		}
		if n > maxDayCount || n < -maxDayCount {
			return 0, fmt.Errorf("day count in %q is out of range", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// EventsRetention parses events_retention: zero (with no error) means the step
// is off, and a value that is set but unusable is refused rather than read as
// off or as a delete-everything pass.
func (c *WispReaperConfig) EventsRetention() (time.Duration, error) {
	if c == nil || strings.TrimSpace(c.EventsRetentionStr) == "" {
		return 0, nil
	}
	d, err := ParseAgeDuration(c.EventsRetentionStr)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("patrols.wisp_reaper.events_retention = %q: want a positive age such as %q",
			c.EventsRetentionStr, "30d")
	}
	return d, nil
}

// ParseExtraBeadsRoots parses extra_beads_roots: nil (with no error) means the
// town root is the only place searched. A set entry that is empty or not an
// absolute path is refused rather than skipped: a relative root is resolved
// against the daemon's working directory, not the operator's, so it would
// silently search a tree nobody meant while the databases it was added for
// still had no writer (gt-33etw).
func (c *WispReaperConfig) ParseExtraBeadsRoots() ([]string, error) {
	if c == nil || len(c.ExtraBeadsRoots) == 0 {
		return nil, nil
	}
	roots := make([]string, 0, len(c.ExtraBeadsRoots))
	for _, root := range c.ExtraBeadsRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			return nil, fmt.Errorf("patrols.wisp_reaper.extra_beads_roots holds an empty entry: want an absolute directory path")
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("patrols.wisp_reaper.extra_beads_roots = %q: want an absolute path", root)
		}
		roots = append(roots, filepath.Clean(root))
	}
	return roots, nil
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

	// ArchiveDir is where the history archive writes each database's new
	// dolt_log commits and events rows as gzip CSV, one file per database per
	// UTC day (gt-7nxj4). Empty turns archiving off. Read it through
	// ParseArchiveDir, which refuses a relative path.
	ArchiveDir string `json:"archive_dir,omitempty"`

	// ArchiveCommitLimit bounds one maintenance run's dolt_log read per
	// database, so a first export against a whole history writes a bounded
	// batch and the next run reads the rest (gt-s6srv). Nil or zero means
	// DefaultArchiveCommitLimit; read it through ParseArchiveCommitLimit,
	// which refuses a negative value and one above MaxArchiveLimit.
	ArchiveCommitLimit *int `json:"archive_commit_limit,omitempty"`

	// ArchiveEventsLimit bounds one maintenance run's events read per
	// database, the same way ArchiveCommitLimit bounds dolt_log; the read
	// already skips the ids the watermark holds, so the bound counts fresh
	// rows (gt-s6srv). Nil or zero means DefaultArchiveEventsLimit; read it
	// through ParseArchiveEventsLimit, which refuses a negative value and one
	// above MaxArchiveLimit.
	ArchiveEventsLimit *int `json:"archive_events_limit,omitempty"`

	// ArchiveBudget bounds how long one maintenance cycle keeps taking archive
	// batches: each database's commits and events keep going in limit-sized
	// batches until nothing more is behind or this budget ends, so a database
	// that is many batches behind catches up within one cycle (gt-o4sk8.1). A
	// Go duration string, e.g. "10m". Empty means DefaultArchiveBudget; read it
	// through ParseArchiveBudget, which refuses a value below MinArchiveBudget,
	// one above MaxArchiveBudget, and one it cannot parse.
	ArchiveBudget string `json:"archive_budget,omitempty"`

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

// MaintenanceArchiveDirKey is the history archive's setting as it appears in
// daemon.json, the path the daemon's logs and escalations tell an operator to
// fix (gt-7nxj4). It is not yet a `gt config set maintenance.*` key.
const MaintenanceArchiveDirKey = "patrols.scheduled_maintenance.archive_dir"

// The history archive's per-run batch bounds as they appear in daemon.json, the
// paths the daemon's logs and escalations tell an operator to fix (gt-s6srv).
const (
	MaintenanceArchiveCommitLimitKey = "patrols.scheduled_maintenance.archive_commit_limit"
	MaintenanceArchiveEventsLimitKey = "patrols.scheduled_maintenance.archive_events_limit"
)

// MaintenanceArchiveBudgetKey is the history archive's time budget as it
// appears in daemon.json, the path the daemon's logs and escalations tell an
// operator to fix (gt-o4sk8.1).
const MaintenanceArchiveBudgetKey = "patrols.scheduled_maintenance.archive_budget"

// DefaultArchiveBudget is how long one archive step keeps taking batches when
// archive_budget is unset: long enough for a database a few batches behind to
// catch up in one nightly cycle, short enough to leave the backup and gc room
// in the step's 30-minute timeout (gt-o4sk8.1).
const DefaultArchiveBudget = 10 * time.Minute

// MinArchiveBudget and MaxArchiveBudget bound archive_budget: a budget shorter
// than one limit-sized batch can take would rarely allow a second one, and the
// archive step's own timeout is 30 minutes, so a longer one could never be
// spent (gt-o4sk8.1).
const (
	MinArchiveBudget = time.Minute
	MaxArchiveBudget = 30 * time.Minute
)

// The unset and zero reading of each batch bound: one run reads at most this
// many rows per database, which is the behavior before the settings existed
// (gt-s6srv).
const (
	DefaultArchiveCommitLimit = 5000
	DefaultArchiveEventsLimit = 5000
)

// MaxArchiveLimit caps either batch bound so a mis-typed value cannot make one
// maintenance run hold a whole table in memory (gt-s6srv).
const MaxArchiveLimit = 1_000_000

// ParseArchiveDir returns the history archive's directory: empty (with no
// error) means archiving is off. A set entry that is not an absolute path is
// refused rather than resolved against the daemon's working directory, which is
// not the operator's, so the history would land in a tree nobody meant
// (gt-7nxj4).
func (c *ScheduledMaintenanceConfig) ParseArchiveDir() (string, error) {
	if c == nil {
		return "", nil
	}
	dir := strings.TrimSpace(c.ArchiveDir)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s = %q: want an absolute path", MaintenanceArchiveDirKey, dir)
	}
	return filepath.Clean(dir), nil
}

// ParseArchiveCommitLimit returns the configured per-run dolt_log bound. An
// absent section, nil receiver, or zero setting reads DefaultArchiveCommitLimit.
func (c *ScheduledMaintenanceConfig) ParseArchiveCommitLimit() (int, error) {
	if c == nil {
		return DefaultArchiveCommitLimit, nil
	}
	return parseArchiveLimit(MaintenanceArchiveCommitLimitKey, c.ArchiveCommitLimit, DefaultArchiveCommitLimit)
}

// ParseArchiveEventsLimit returns the configured per-run events bound. An
// absent section, nil receiver, or zero setting reads DefaultArchiveEventsLimit.
func (c *ScheduledMaintenanceConfig) ParseArchiveEventsLimit() (int, error) {
	if c == nil {
		return DefaultArchiveEventsLimit, nil
	}
	return parseArchiveLimit(MaintenanceArchiveEventsLimitKey, c.ArchiveEventsLimit, DefaultArchiveEventsLimit)
}

// ParseArchiveBudget returns how long one archive step may keep taking batches
// after a limit-sized one. An absent section, nil receiver, or empty setting
// reads DefaultArchiveBudget. A duration that does not parse, or one outside
// MinArchiveBudget to MaxArchiveBudget, is refused rather than clamped, and the
// error names the daemon.json key so the operator can fix the entry it came
// from (gt-o4sk8.1).
func (c *ScheduledMaintenanceConfig) ParseArchiveBudget() (time.Duration, error) {
	if c == nil {
		return DefaultArchiveBudget, nil
	}
	setting := strings.TrimSpace(c.ArchiveBudget)
	if setting == "" {
		return DefaultArchiveBudget, nil
	}
	budget, err := time.ParseDuration(setting)
	if err != nil {
		return 0, fmt.Errorf("%s = %q: want a Go duration such as 10m: %w", MaintenanceArchiveBudgetKey, setting, err)
	}
	if budget < MinArchiveBudget || budget > MaxArchiveBudget {
		return 0, fmt.Errorf("%s = %q: want a duration from %s to %s",
			MaintenanceArchiveBudgetKey, setting, MinArchiveBudget, MaxArchiveBudget)
	}
	return budget, nil
}

// parseArchiveLimit reads one batch bound. Nil and zero keep def; a negative
// value or one above MaxArchiveLimit is refused rather than clamped, and the
// error names the daemon.json key so the operator can fix the entry it came
// from (gt-s6srv).
func parseArchiveLimit(key string, v *int, def int) (int, error) {
	if v == nil || *v == 0 {
		return def, nil
	}
	if *v < 0 || *v > MaxArchiveLimit {
		return 0, fmt.Errorf("%s = %d: want 0 (the default) or a value from 1 to %d", key, *v, MaxArchiveLimit)
	}
	return *v, nil
}

// DiskMaintenanceConfig configures the disk_maintenance patrol: the daily
// quiet-window run that plans, and in apply mode prunes, the town's registry
// (internal/daemon/disk_maintenance.go).
//
// The patrol is opt-in. Nothing runs unless Enabled is true, and it changes
// nothing unless Mode is "apply": disk maintenance deletes images, so a town
// that has not asked for it must not get it from a default.
type DiskMaintenanceConfig struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"` // "daily"
	Window   string `json:"window"`   // "04:30", local time
	Mode     string `json:"mode"`     // "dry_run" (default) | "apply"
	// CacheTrimInterval is how often the shared Go cache is trimmed, in any
	// window. Empty means DiskMaintenanceDefaultCacheTrimInterval.
	CacheTrimInterval string `json:"cache_trim_interval,omitempty"` // Go duration such as "2h"
	// CICacheTrimInterval is how often the CI runner's Go build cache is
	// trimmed, in any window. Empty means DiskMaintenanceDefaultCICacheTrimInterval.
	CICacheTrimInterval string `json:"ci_cache_trim_interval,omitempty"` // Go duration such as "1h"
	// CICacheMaxAge is how long an entry of that cache may go unwritten before
	// the trim removes it. Empty means DiskMaintenanceDefaultCICacheMaxAge.
	CICacheMaxAge string `json:"ci_cache_max_age,omitempty"` // Go duration such as "12h"
	// CICacheWarnBytes is the size above which a trim that removed nothing is an
	// attention item. Empty means DiskMaintenanceDefaultCICacheWarnBytes.
	CICacheWarnBytes string `json:"ci_cache_warn_bytes,omitempty"` // size such as "30GB"
	// Targets holds the targets with a cadence of their own. A target absent
	// here keeps the daily window.
	Targets map[string]DiskMaintenanceTarget `json:"targets,omitempty"`
}

// CacheTrimEvery returns the cadence the cache trim runs on: the default when
// the setting is empty, and an error naming the setting for a value the patrol
// cannot schedule. The error never falls back to the default (gt-gg9hv.24.2).
func (c DiskMaintenanceConfig) CacheTrimEvery() (time.Duration, error) {
	raw := c.CacheTrimInterval
	if raw == "" {
		raw = DiskMaintenanceDefaultCacheTrimInterval
	}
	every, err := parseDiskMaintenanceCadence(raw)
	if err != nil {
		return 0, fmt.Errorf("patrols.disk_maintenance.cache_trim_interval: %w", err)
	}
	return every, nil
}

// CICacheTrimEvery returns the cadence the CI build-cache trim runs on: the
// default when the setting is empty, and an error naming the setting for a value
// the patrol cannot schedule. It never falls back to the default on an error
// (gt-gg9hv.57).
func (c DiskMaintenanceConfig) CICacheTrimEvery() (time.Duration, error) {
	raw := c.CICacheTrimInterval
	if raw == "" {
		raw = DiskMaintenanceDefaultCICacheTrimInterval
	}
	every, err := parseDiskMaintenanceCadence(raw)
	if err != nil {
		return 0, fmt.Errorf("patrols.disk_maintenance.ci_cache_trim_interval: %w", err)
	}
	return every, nil
}

// CICacheMaxAgeDuration returns how old an entry must be before the CI cache
// trim removes it: the default when the setting is empty, and an error naming
// the setting otherwise, zero and negative values included.
func (c DiskMaintenanceConfig) CICacheMaxAgeDuration() (time.Duration, error) {
	raw := c.CICacheMaxAge
	if raw == "" {
		raw = DiskMaintenanceDefaultCICacheMaxAge
	}
	age, err := parseDiskMaintenanceCadence(raw)
	if err != nil {
		return 0, fmt.Errorf("patrols.disk_maintenance.ci_cache_max_age: %w", err)
	}
	return age, nil
}

// CICacheWarnLimit returns the size above which a CI cache trim that removed
// nothing raises its attention item: the default when the setting is empty, and
// an error naming the setting for a value that is not a positive size.
func (c DiskMaintenanceConfig) CICacheWarnLimit() (int64, error) {
	raw := c.CICacheWarnBytes
	if raw == "" {
		raw = DiskMaintenanceDefaultCICacheWarnBytes
	}
	limit, err := parseDiskMaintenanceSize(raw)
	if err != nil {
		return 0, fmt.Errorf("patrols.disk_maintenance.ci_cache_warn_bytes: %w", err)
	}
	return limit, nil
}

// diskMaintenanceSizeRE is a byte count: digits and an optional binary unit.
var diskMaintenanceSizeRE = regexp.MustCompile(`^([0-9]+)(B|KB|MB|GB|TB)?$`)

// diskMaintenanceSizeUnits maps each unit diskMaintenanceSizeRE accepts to its
// multiple of a byte.
var diskMaintenanceSizeUnits = map[string]int64{
	"": 1, "B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40,
}

// parseDiskMaintenanceSize reads a size such as "30GB" as a byte count. Zero is
// refused, as a negative one is by the form, so a setting never reads as a
// limit the patrol cannot act on. A count too large for an int64 is refused
// rather than wrapped.
func parseDiskMaintenanceSize(raw string) (int64, error) {
	m := diskMaintenanceSizeRE.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(raw)))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size such as \"30GB\"", raw)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size such as \"30GB\"", raw)
	}
	if n == 0 {
		return 0, fmt.Errorf("%q is zero; want a size above zero", raw)
	}
	mult := diskMaintenanceSizeUnits[m[2]]
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%q is too large to read as bytes", raw)
	}
	return n * mult, nil
}

// DiskMaintenanceTarget is one target's own cadence: a set Interval runs that
// target on it in any window, and an empty one keeps the daily window.
type DiskMaintenanceTarget struct {
	Interval string `json:"interval,omitempty"` // Go duration such as "2h"
}

// Every returns the cadence Interval names, or zero for an empty setting,
// which keeps the daily window. A setting the patrol cannot schedule is an
// error rather than the daily window: reading a typo as daily would run the
// target on a cadence the town never asked for, and nothing would say so.
func (t DiskMaintenanceTarget) Every() (time.Duration, error) {
	if t.Interval == "" {
		return 0, nil
	}
	return parseDiskMaintenanceCadence(t.Interval)
}

// parseDiskMaintenanceCadence reads a cadence: a Go duration of at least a
// minute. A zero or negative duration is under that minimum, so it is refused
// here too.
func parseDiskMaintenanceCadence(raw string) (time.Duration, error) {
	every, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a Go duration such as \"2h\"", raw)
	}
	if every < time.Minute {
		return 0, fmt.Errorf("%q is under the one-minute minimum", raw)
	}
	return every, nil
}

// The maintenance targets the patrol knows, so a per-target interval applies
// to one that exists. The registry prune is built today; the CI cache volume
// trim (internal/diskmaint/civol) joins it, which is why the shipped default
// gives it an interval before the patrol builds it (gt-z6qre.17).
const (
	DiskMaintenanceTargetCivol    = "civol"
	DiskMaintenanceTargetRegistry = "registry"
)

// diskMaintenanceTargets is the set Target names are checked against.
var diskMaintenanceTargets = []string{
	DiskMaintenanceTargetCivol,
	DiskMaintenanceTargetRegistry,
}

// isDiskMaintenanceTarget reports whether name is a maintenance target the
// patrol knows.
func isDiskMaintenanceTarget(name string) bool {
	return slices.Contains(diskMaintenanceTargets, name)
}

// Cadence returns the interval the named target runs on: zero for a target
// keeping the daily window, and an error for a name the patrol does not know
// or a cadence it cannot schedule.
//
// Decoding refuses a config file's targets with it, and the daemon calls it for
// a config built in code, so a per-target interval applies only to a target
// that exists however the config was made (gt-kdbzb).
func (c *DiskMaintenanceConfig) Cadence(name string) (time.Duration, error) {
	if !isDiskMaintenanceTarget(name) {
		return 0, fmt.Errorf("patrols.disk_maintenance.targets.%s: unknown target; the patrol knows %s",
			name, strings.Join(diskMaintenanceTargets, " and "))
	}
	every, err := c.Targets[name].Every()
	if err != nil {
		return 0, fmt.Errorf("patrols.disk_maintenance.targets.%s.interval: %w", name, err)
	}
	return every, nil
}

// The values DiskMaintenanceConfig accepts. An empty field means the default,
// so a town that sets only enabled still gets a window and a mode.
const (
	DiskMaintenanceIntervalDaily = "daily"
	DiskMaintenanceModeDryRun    = "dry_run"
	DiskMaintenanceModeApply     = "apply"
	DiskMaintenanceDefaultWindow = "04:30"
	// DiskMaintenanceDefaultCivolInterval is the CI cache volumes' cadence:
	// they grow about 3 GB an hour, so the daily window cannot hold them. It
	// matches the shell job the patrol replaces (gt-z6qre.17).
	DiskMaintenanceDefaultCivolInterval = "2h"
	// DiskMaintenanceDefaultCacheTrimInterval is the shared Go cache's cadence:
	// it grows about 3 GB an hour, so one trim a day cannot hold the 10 GB cap
	// (gt-gg9hv.24.2).
	DiskMaintenanceDefaultCacheTrimInterval = "2h"
	// DiskMaintenanceDefaultCICacheTrimInterval is the CI build-cache trim's
	// cadence: the gate jobs fill that cache faster than a daily pass can hold it
	// (gt-gg9hv.57).
	DiskMaintenanceDefaultCICacheTrimInterval = "1h"
	// DiskMaintenanceDefaultCICacheMaxAge is how long a CI build-cache entry may
	// go unwritten before the trim removes it. An entry the build still uses is
	// rewritten, so only cold entries age past it (gt-gg9hv.57).
	DiskMaintenanceDefaultCICacheMaxAge = "12h"
	// DiskMaintenanceDefaultCICacheWarnBytes is the size above which a CI cache
	// trim that removed nothing raises its attention item (gt-gg9hv.57).
	DiskMaintenanceDefaultCICacheWarnBytes = "30GB"
)

// UnmarshalJSON decodes the patrol's block strictly, refusing a value the
// patrol cannot act on: an unknown mode, an interval other than daily, or a
// window that is not HH:MM. A mistyped mode must not read as either a planned
// run or a dry one, so it is an error at decode rather than a default later.
//
// The unknown-field check is repeated here because a type that decodes itself
// is decoded by the config kernel's strict decoder opaquely.
func (c *DiskMaintenanceConfig) UnmarshalJSON(data []byte) error {
	type plain DiskMaintenanceConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("patrols.disk_maintenance: %w", err)
	}
	switch p.Interval {
	case "", DiskMaintenanceIntervalDaily:
	default:
		return fmt.Errorf("patrols.disk_maintenance.interval = %q: only %q is supported", p.Interval, DiskMaintenanceIntervalDaily)
	}
	switch p.Mode {
	case "", DiskMaintenanceModeDryRun, DiskMaintenanceModeApply:
	default:
		return fmt.Errorf("patrols.disk_maintenance.mode = %q: use %q or %q", p.Mode, DiskMaintenanceModeDryRun, DiskMaintenanceModeApply)
	}
	if p.Window != "" {
		if _, err := parseClock(p.Window); err != nil {
			return fmt.Errorf("patrols.disk_maintenance.window %q: want HH:MM", p.Window)
		}
	}
	cfg := DiskMaintenanceConfig(p)
	if _, err := cfg.CacheTrimEvery(); err != nil {
		return err
	}
	if _, err := cfg.CICacheTrimEvery(); err != nil {
		return err
	}
	if _, err := cfg.CICacheMaxAgeDuration(); err != nil {
		return err
	}
	if _, err := cfg.CICacheWarnLimit(); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Targets)) {
		if _, err := cfg.Cadence(name); err != nil {
			return err
		}
	}
	*c = cfg
	return nil
}

// HMSyncConfig configures the hm_sync patrol: the interval run of `hm sync`
// that keeps history-man's index fresh (internal/daemon/hm_sync.go).
//
// The patrol is opt-in. It runs a program on a timer, so a town that has not
// asked for it must not get it from a default.
type HMSyncConfig struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"` // Go duration, default "30m"
	Command  string `json:"command"`  // default "hm"
}

// The values HMSyncConfig accepts. An empty field means the default, so a town
// that sets only enabled still gets an interval and a command.
const (
	HMSyncDefaultInterval = "30m"
	HMSyncDefaultCommand  = "hm"
)

// UnmarshalJSON decodes the patrol's block strictly, refusing an interval the
// patrol cannot schedule: a mistyped one must not read as the default, or the
// town would run hm on a cadence it never asked for.
//
// The unknown-field check is repeated here because a type that decodes itself
// is decoded by the config kernel's strict decoder opaquely.
func (c *HMSyncConfig) UnmarshalJSON(data []byte) error {
	type plain HMSyncConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("patrols.hm_sync: %w", err)
	}
	if p.Interval != "" {
		every, err := time.ParseDuration(p.Interval)
		if err != nil || every <= 0 {
			return fmt.Errorf("patrols.hm_sync.interval = %q: want a positive Go duration such as %q", p.Interval, HMSyncDefaultInterval)
		}
	}
	*c = HMSyncConfig(p)
	return nil
}

// JevScrub turns on the state scrubber for one Jev battery: secret-shaped text
// and the operator's identity are replaced in the state before it leaves the
// machine (internal/jev/scrub), and the request log counts the rules that
// fired. It is embedded in every battery's config so the key has one
// definition, and an operator flips it on one battery at a time once that
// battery's signal cost is measured (gt-aqczg).
type JevScrub struct {
	// Scrub replaces the state's secrets and account name before it is sent.
	Scrub bool `json:"scrub,omitempty"`

	// ScrubCheck samples a share of the battery's requests and logs both the
	// unscrubbed and the scrubbed answers to scrubcheck.jsonl, so the signal
	// cost of Scrub is measured before the battery switches to it (gt-jb20i).
	ScrubCheck bool `json:"scrub_check,omitempty"`
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

	JevScrub
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

	JevScrub
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

	JevScrub
}

// JevSubmissionConfig switches on the submission battery and prices its Jev
// spend. The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevSubmissionConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevDriftConfig switches on the drift battery and prices its Jev spend. The
// pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevDriftConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevLogtriageConfig switches on the daemon-log triage battery and prices its
// Jev spend. The battery is off unless Enabled is true, and an enabled one needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevLogtriageConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the battery for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevTestselConfig switches on the test-selection shadow battery and prices its
// Jev spend. The battery is off unless Enabled is true, and an enabled one needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevTestselConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the battery for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevFailcauseConfig switches on the failing-test cause battery and prices its
// Jev spend. The battery is off unless Enabled is true, and an enabled one needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevFailcauseConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the battery for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevProgressConfig switches on the polecat progress battery and prices its Jev
// spend. The battery is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevProgressConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevStaleConfig switches on the stale-bead read and prices its Jev spend. The
// read is off unless Enabled is true, and an enabled read needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevStaleConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the read for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevNotesConfig switches on the notes read and prices its Jev spend. The read
// is off unless Enabled is true, and an enabled read needs USDPerBillionTokens:
// the usage ledger stores tokens, not dollars.
type JevNotesConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the read for the day once the town's Jev spend, every
	// caller counted, reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevScorecardConfig configures the jev_scorecard patrol (gt-jk0162): once a
// day at Hour the daemon runs `gt jev scorecard --refresh` for the town, and
// at start it runs once when the cached scorecard is older than Interval. The
// command scores every registered battery, so Batteries names the batteries
// the town expects it to score; the daemon refuses a name the binary does not
// register, and an empty list makes the patrol refresh nothing.
//
// Off unless enabled: a refresh reads every battery's logs and label sources,
// so a town that has not asked for it must not get it from a default.
type JevScorecardConfig struct {
	Enabled bool `json:"enabled"`

	// Hour is the local time of day the refresh is due, "HH:MM". Empty means
	// JevScorecardDefaultHour; midnight is "00:00", not an unset value.
	Hour string `json:"hour,omitempty"`

	// IntervalStr is how old the cache may be at start before the patrol
	// refreshes at once. Empty means JevScorecardDefaultInterval.
	IntervalStr string `json:"interval,omitempty"`

	// Batteries names the batteries the refresh is expected to score. Empty
	// refreshes nothing.
	Batteries []string `json:"batteries,omitempty"`
}

// The jev_scorecard defaults. JevScorecardDefaultHour is the hour the daemon
// already owns for its nightly upkeep, so the review spends nothing while the
// town is busy.
const (
	JevScorecardDefaultHour     = "04:30"
	JevScorecardDefaultInterval = 24 * time.Hour
)

// ClockMinute returns the local time of day a refresh is due as minutes after
// midnight. An empty setting is the default; a value the patrol cannot read as
// HH:MM is an error naming the key rather than a fallback to the default, so a
// typo cannot move the run to an hour the town never asked for.
func (c *JevScorecardConfig) ClockMinute() (int, error) {
	raw := JevScorecardDefaultHour
	if c != nil && c.Hour != "" {
		raw = c.Hour
	}
	minute, err := parseClock(raw)
	if err != nil {
		return 0, fmt.Errorf("patrols.jev_scorecard.hour: %w", err)
	}
	return minute, nil
}

// IntervalDuration returns how old the cache may be at start before the patrol
// refreshes at once. An empty setting is the default; a value that is not a
// positive Go duration is an error naming the key, never the default.
func (c *JevScorecardConfig) IntervalDuration() (time.Duration, error) {
	if c == nil || c.IntervalStr == "" {
		return JevScorecardDefaultInterval, nil
	}
	d, err := time.ParseDuration(c.IntervalStr)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("patrols.jev_scorecard.interval = %q: want a positive Go duration such as %q",
			c.IntervalStr, "24h")
	}
	return d, nil
}

// BatteriesValue returns the batteries the town declares, copied so a caller
// cannot change the config through the slice. An empty list means the patrol
// refreshes nothing.
func (c *JevScorecardConfig) BatteriesValue() []string {
	if c == nil {
		return nil
	}
	return slices.Clone(c.Batteries)
}

// UnmarshalJSON decodes the patrol's block strictly, refusing a value the
// patrol cannot act on: an hour that is not HH:MM, an interval it cannot
// schedule, or an empty or repeated battery name. Whether a name is one the
// binary registers is checked when the daemon reads the patrol's settings: the
// scorecard package imports this one, so the registry cannot be read here
// without a cycle.
//
// The unknown-field check is repeated here because a type that decodes itself
// is decoded by the config kernel's strict decoder opaquely.
func (c *JevScorecardConfig) UnmarshalJSON(data []byte) error {
	type plain JevScorecardConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("patrols.jev_scorecard: %w", err)
	}
	cfg := JevScorecardConfig(p)
	if _, err := cfg.ClockMinute(); err != nil {
		return err
	}
	if _, err := cfg.IntervalDuration(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(cfg.Batteries))
	for _, name := range cfg.Batteries {
		if strings.TrimSpace(name) == "" {
			return errors.New("patrols.jev_scorecard.batteries: a battery name is empty")
		}
		if seen[name] {
			return fmt.Errorf("patrols.jev_scorecard.batteries: %q is listed twice", name)
		}
		seen[name] = true
	}
	*c = cfg
	return nil
}

// JevReworkConfig switches on the refused-review cause battery and prices its
// Jev spend. The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevReworkConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevTargetsConfig switches on the review-targeting battery and prices its Jev
// spend. The battery is off unless Enabled is true, and an enabled run needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevTargetsConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the run for the day once the town's Jev spend
	// reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
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

	JevScrub
}

// JevThrashConfig switches on the thrash battery and prices its Jev spend. The
// pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevThrashConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend, every
	// caller counted, reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevRulesConfig switches on the rules battery and prices its Jev spend. The
// pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevRulesConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend, every
	// caller counted, reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
}

// JevConflictConfig switches on the conflict battery and prices its Jev spend.
// The pass is off unless Enabled is true, and an enabled pass needs
// USDPerBillionTokens: the usage ledger stores tokens, not dollars.
type JevConflictConfig struct {
	Enabled bool `json:"enabled"`

	// MaxUSDPerDay stops the pass for the day once the town's Jev spend, every
	// caller counted, reaches it. Zero takes the default of one dollar.
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`

	// USDPerBillionTokens prices input and output tokens alike.
	USDPerBillionTokens float64 `json:"usd_per_billion_tokens,omitempty"`

	JevScrub
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

	// StaleCrew adds the stale crew clones section, which lists the crew
	// clones not fetched in a day (gt-t3mky). Default off.
	StaleCrew bool `json:"stale_crew,omitempty"`
}

// BirdReportsConfig configures the bird_reports patrol (gt-cbsat): every hour
// the daemon checks each reporter's <town>/.runtime/<name>/reports/daily.json
// and, from StartHour on, raises one attention item, bird-report-stale:<name>,
// while that report is missing, unreadable or older than MaxAge.
//
// Off unless enabled: a town that keeps no birds must not get ten items for
// ten files that were never going to exist. Enabling it is a config change,
// and so is adding a reporter to the list.
type BirdReportsConfig struct {
	// Enabled turns the patrol on. Default off.
	Enabled bool `json:"enabled"`

	// StartHour is the local hour the day's reports are expected from, 0 to
	// 23. Absent means BirdReportsDefaultStartHour; 0 is a real setting
	// (midnight), which is why the field is a pointer.
	StartHour *int `json:"start_hour,omitempty"`

	// MaxAgeStr is how old a report may be before it is late (default "26h").
	MaxAgeStr string `json:"max_age,omitempty"`

	// Reporters names the reporters whose daily report is expected. Absent
	// means DefaultBirdReporters; each name is one directory under .runtime.
	Reporters []string `json:"reporters,omitempty"`
}

// The bird_reports defaults.
const (
	// BirdReportsDefaultStartHour is the local hour the day's reports are
	// expected from.
	BirdReportsDefaultStartHour = 6
	// BirdReportsDefaultMaxAge is how old a report may be before it is late:
	// a day plus two hours, so a report written a little later each morning is
	// not late the moment its hour passes.
	BirdReportsDefaultMaxAge = 26 * time.Hour
)

// DefaultBirdReporters returns the reporters the patrol expects when the
// config lists none: the nine birds who report every day, and the overseer's
// own report as the tenth.
func DefaultBirdReporters() []string {
	return []string{"dunlin", "dipper", "eagle", "goose", "peacock", "egret", "seagull", "owl", "merlin", "overseer"}
}

// StartHourValue returns the hour the day's reports are expected from.
func (c *BirdReportsConfig) StartHourValue() int {
	if c == nil || c.StartHour == nil {
		return BirdReportsDefaultStartHour
	}
	return *c.StartHour
}

// MaxAgeValue returns how old a report may be before it is late, or an error
// for a value the patrol cannot judge against. An empty setting is the
// default; an unparseable one is refused rather than read as the default, so
// a typo cannot widen the window to no alarm.
func (c *BirdReportsConfig) MaxAgeValue() (time.Duration, error) {
	if c == nil || c.MaxAgeStr == "" {
		return BirdReportsDefaultMaxAge, nil
	}
	d, err := time.ParseDuration(c.MaxAgeStr)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("patrols.bird_reports.max_age = %q: want a positive Go duration such as %q",
			c.MaxAgeStr, BirdReportsDefaultMaxAge)
	}
	return d, nil
}

// ReportersValue returns the reporters whose daily report is expected: the
// configured list, or DefaultBirdReporters when it is empty.
func (c *BirdReportsConfig) ReportersValue() []string {
	if c == nil || len(c.Reporters) == 0 {
		return DefaultBirdReporters()
	}
	return slices.Clone(c.Reporters)
}

// validateBirdReporterName refuses a name that is not one directory name under
// the town's .runtime: the patrol builds a path from each name, so a separator
// or a dot entry would read a file outside the reporter's own directory.
func validateBirdReporterName(name string) error {
	switch {
	case name == "":
		return errors.New("patrols.bird_reports.reporters: a reporter name is empty")
	case name == "." || name == "..":
		return fmt.Errorf("patrols.bird_reports.reporters: %q is not a reporter directory name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("patrols.bird_reports.reporters: %q is not a reporter directory name: it carries a path separator", name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("patrols.bird_reports.reporters: %q is not a reporter directory name", name)
	}
	return nil
}

// UnmarshalJSON decodes the patrol's block strictly, refusing a value it
// cannot act on: an hour outside the clock, an age it cannot measure, or a
// reporter name that is not one directory name. A typo must not quietly read
// as the default, or the town would run on a setting it never wrote.
//
// The unknown-field check is repeated here because a type that decodes itself
// is decoded by the config kernel's strict decoder opaquely.
func (c *BirdReportsConfig) UnmarshalJSON(data []byte) error {
	type plain BirdReportsConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("patrols.bird_reports: %w", err)
	}
	if p.StartHour != nil && (*p.StartHour < 0 || *p.StartHour > 23) {
		return fmt.Errorf("patrols.bird_reports.start_hour = %d: want an hour from 0 to 23", *p.StartHour)
	}
	cfg := BirdReportsConfig(p)
	if _, err := cfg.MaxAgeValue(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(cfg.Reporters))
	for _, name := range cfg.Reporters {
		if err := validateBirdReporterName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("patrols.bird_reports.reporters: %q is listed twice", name)
		}
		seen[name] = true
	}
	*c = cfg
	return nil
}

// FindingsFilerConfig configures the findings_filer patrol (gt-rws9m): every
// interval the daemon reads the FINDING: comments gt done recorded on beads
// closed, deferred or hooked in the last day and files one Found bead per
// comment in the rig the finding names.
//
// Off unless enabled: a pass files beads into rig databases, so a town opts in
// once rather than discovering the filings.
type FindingsFilerConfig struct {
	// Enabled turns the patrol on. Default off.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often one pass runs (default "10m").
	IntervalStr string `json:"interval,omitempty"`

	// MaxPerRun caps the Found beads one pass files (default 5). It is a
	// pointer so an explicit 0 is refused rather than read as the default: a
	// patrol that runs and files nothing is a typo, not a setting.
	MaxPerRun *int `json:"max_per_run,omitempty"`
}

// The findings_filer defaults.
const (
	// FindingsFilerDefaultInterval is how often one pass runs.
	FindingsFilerDefaultInterval = 10 * time.Minute
	// FindingsFilerDefaultMaxPerRun is how many Found beads one pass files. A
	// finding waits at most a few intervals, and a pass cannot flood a rig's
	// board with a day of them at once.
	FindingsFilerDefaultMaxPerRun = 5
)

// IntervalValue returns how often one pass runs, or an error for a value the
// patrol cannot schedule. An empty setting is the default; an unparseable one
// is refused rather than read as the default, so a typo cannot leave the
// patrol silently never running.
func (c *FindingsFilerConfig) IntervalValue() (time.Duration, error) {
	if c == nil || c.IntervalStr == "" {
		return FindingsFilerDefaultInterval, nil
	}
	d, err := time.ParseDuration(c.IntervalStr)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("patrols.findings_filer.interval = %q: want a positive Go duration such as %q",
			c.IntervalStr, FindingsFilerDefaultInterval)
	}
	return d, nil
}

// MaxPerRunValue returns how many Found beads one pass may file: the
// configured count, or the default when the key is absent.
func (c *FindingsFilerConfig) MaxPerRunValue() int {
	if c == nil || c.MaxPerRun == nil {
		return FindingsFilerDefaultMaxPerRun
	}
	return *c.MaxPerRun
}

// UnmarshalJSON decodes the patrol's block strictly, refusing a value it
// cannot act on. The unknown-field check is repeated here because a type that
// decodes itself is decoded by the config kernel's strict decoder opaquely.
func (c *FindingsFilerConfig) UnmarshalJSON(data []byte) error {
	type plain FindingsFilerConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("patrols.findings_filer: %w", err)
	}
	cfg := FindingsFilerConfig(p)
	if _, err := cfg.IntervalValue(); err != nil {
		return err
	}
	if p.MaxPerRun != nil && *p.MaxPerRun < 1 {
		return fmt.Errorf("patrols.findings_filer.max_per_run = %d: want at least 1", *p.MaxPerRun)
	}
	*c = cfg
	return nil
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
	// before a restart (default 2). The seat roster and the health line read
	// the same count, so all three call a seat dead at once (gt-4rxdi).
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

	// MaxPerTick is a ceiling on slings per tick. Zero or absent means as
	// many as there are free seats; a set value still binds (the interim 5 of
	// 2026-10-21 came off with the parallel slings). How many of those slings
	// run at once is MaxInFlight, not this: a tick's candidates past the cap
	// wait for a slot (gt-16pqa).
	MaxPerTick int `json:"max_per_tick,omitempty"`

	// MaxInFlight is how many slings run at once: a sling past it waits for a
	// slot to free, in the tick or the daemon's background slings. Zero or
	// negative means DefaultSpecMaxInFlight, never unbounded (gt-16pqa).
	MaxInFlight int `json:"max_in_flight,omitempty"`

	// Template overrides the spec template path.
	Template string `json:"template,omitempty"`
}

// DefaultSpecMaxInFlight is the sling concurrency of a spec_dispatch block that
// sets none. Above one it is what keeps concurrent slings off each other's
// polecat pool lock and the Dolt writes behind it (gt-16pqa).
const DefaultSpecMaxInFlight = 2

// InFlightCap returns the sling concurrency, defaulting to
// DefaultSpecMaxInFlight for zero or negative.
func (c *SpecDispatchConfig) InFlightCap() int {
	if c == nil || c.MaxInFlight <= 0 {
		return DefaultSpecMaxInFlight
	}
	return c.MaxInFlight
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
	// on any agent with a free seat (or RunAgent's), and cannot carry an Agent:
	// the file does not load with one (retired_keys.go, gt-mukgk).
	// "shadow" means the scheduler runs the deep-review plan from the host's
	// landings ledger itself, appends the plan to the run bead's notes and
	// closes the bead; nothing is slung, so it carries no Agent either.
	Dispatch string `json:"dispatch,omitempty"`

	// RunAgent is the agent a "spec" entry's run beads must run on: the
	// scheduler labels each bead agent:<name>, and the dispatcher then uses only
	// that agent's seats. It names an agent in polecat_pool.seats (gt-tplg6).
	RunAgent string `json:"run_agent,omitempty"`

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

	// RestartStaleAfterStr is how old the last sweep may be before a pending
	// upgrade restart stops deferring the next one, as a string (e.g. "90m").
	// Default 90m. Past the bound the sweep runs and the restart waits for it
	// (gt-zvqn8).
	RestartStaleAfterStr string `json:"restart_stale_after,omitempty"`

	// Runner, when set, runs the integration and race tiers off the host: the
	// daemon renders the run through internal/sweeprunner, starts it detached,
	// and reads the result.json it writes (gt-z4ppb). Nil runs them on the
	// host, as before. The shell stage leaves the host only when the runner
	// block's Shell is set.
	Runner *TierSweepRunnerConfig `json:"runner,omitempty"`
}

// TierSweepRunnerConfig is where the sweep's tiers run when they run off the
// host (gt-z4ppb). Every field but Image and Shell is required: a runner block
// that names no engine or output directory cannot render a run, and a default
// would start a run the operator never asked for. Image may be empty or
// TierSweepRunnerInstalledImage, which follows the installed build; Shell sends
// the shell stage off the host too, and defaults to false.
type TierSweepRunnerConfig struct {
	// Backend names the engine the run starts on: "docker" for a container
	// through the Docker CLI's context, or "kubernetes" for a batch/v1 Job
	// (the two internal/sweeprunner renders).
	Backend string `json:"backend"`

	// Image is the runner container: the polecat image tag, the same reference
	// pinned by digest, or empty/installed to follow the installed gt's build.
	// An installed image is the polecat image make polecat-image publishes at
	// that build's revision, which the daemon loads into the sweep engine when
	// it is missing (gt-c3sue).
	Image string `json:"image"`

	// CPUs is the run's CPU share, 1 to 16. On the Kubernetes backend it is the
	// whole Job pod's total: the dind sidecar's fixed share comes out of it, so
	// with dind the minimum is 3 (the sidecar's 2 CPUs and the runner's 1).
	CPUs int `json:"cpus"`

	// Memory is the run's memory limit in GB, 2 to 48, and on the Kubernetes
	// backend the whole Job pod's total, as CPUs is. With dind the minimum is 6
	// (the sidecar's 4 GB and the runner's 2).
	Memory int `json:"memory"`

	// Jobs is how many packages the run tests at once, 1 to 16.
	Jobs int `json:"jobs"`

	// OutputRoot is the host directory the run's output goes under: the
	// daemon gives each run a directory of its own beneath it, and polls that
	// directory for result.json. It must be absolute.
	OutputRoot string `json:"output_root"`

	// Context is the engine context the run's steps target: the Docker CLI
	// context (desktop-linux, colima-gt-sbx) or the kubectl context. It is
	// named rather than left to the environment, so a run reaches the engine
	// the config means.
	Context string `json:"context"`

	// ImageRegistry is a bare host:port (localhost:5001) the daemon reaches
	// through its sidecar, for the installed build's runner image (gt-gg9hv.26).
	// Empty keeps the engine load as before.
	ImageRegistry string `json:"image_registry,omitempty"`

	// Dind starts a Docker-in-Docker sidecar for the packages whose tests need
	// a Docker API (internal/testutil starts Dolt through testcontainers).
	Dind bool `json:"dind"`

	// Shell sends the shell stage to the runner too, so the stage's tests run
	// in the container rather than on the host. Defaults to false: the shell
	// stage runs on the host as before, and the operator turns it on after
	// reading one container sweep (gt-b0mwr).
	Shell bool `json:"shell,omitempty"`

	// ProtectedNamespaces names the namespaces the Kubernetes preflight checks
	// before it launches: a pod there at or below the sweep's priority is one
	// the run could displace. Unset means DefaultSweepProtectedNamespaces; an
	// explicit empty list is refused, because a preflight that checks no
	// namespace is not a preflight (gt-gg9hv.39).
	ProtectedNamespaces []string `json:"protected_namespaces,omitempty"`
}

// DefaultSweepProtectedNamespaces is what the sweep's priority preflight
// protects when the setting names none: Forgejo's namespace, whose pods ran
// below the sweep on 2026-11-05 (gt-gg9hv.39).
var DefaultSweepProtectedNamespaces = []string{"forgejo"}

// ProtectedNamespaceList is the namespaces the sweep's preflight checks: the
// setting, or the default when it names none. It returns a copy, so a caller
// cannot change the config through it.
func (c *TierSweepRunnerConfig) ProtectedNamespaceList() []string {
	if len(c.ProtectedNamespaces) > 0 {
		return append([]string(nil), c.ProtectedNamespaces...)
	}
	return append([]string(nil), DefaultSweepProtectedNamespaces...)
}

// The ranges TierSweepRunnerConfig accepts, matching sweeprunner.Spec.Validate.
// The spec's own Validate is the authority: it runs on every rendered run, so a
// config built in code rather than decoded cannot reach a backend with a value
// the render would refuse. These checks only refuse a file at load, where the
// error can name the key the operator typed (gt-z4ppb).
const (
	runnerCPUsMin   = 1
	runnerCPUsMax   = 16
	runnerMemoryMin = 2
	runnerMemoryMax = 48
	runnerJobsMin   = 1
	runnerJobsMax   = 16
)

// SweepDindCPUs and SweepDindMemoryGB are the dind sidecar's fixed share on the
// Kubernetes backend. internal/sweeprunner renders the sidecar from these two
// constants and this package refuses a pod total that cannot hold them, so the
// figure has one source (gt-gg9hv.38).
const (
	SweepDindCPUs     = 2
	SweepDindMemoryGB = 4
)

// The backends TierSweepRunnerConfig names, as sweeprunner's Backends spell
// them.
const (
	TierSweepRunnerDocker     = "docker"
	TierSweepRunnerKubernetes = "kubernetes"
)

// TierSweepRunnerInstalledImage is the runner.image value that follows the
// installed gt's build: the daemon names the polecat image at that build's
// revision instead of reading a pin an install has to move by hand (gt-c3sue).
const TierSweepRunnerInstalledImage = "installed"

// UnmarshalJSON decodes the runner block strictly, refusing a key it does not
// declare and a value no run can use, naming each. A mistyped key here would
// otherwise read as a default: the runner starts a container, so a block the
// daemon half-understands must not run.
//
// The unknown-field check is repeated here because a type that decodes itself
// is decoded by the config kernel's strict decoder opaquely.
func (c *TierSweepRunnerConfig) UnmarshalJSON(data []byte) error {
	type plain TierSweepRunnerConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("patrols.tier_sweep.runner: %w", err)
	}
	cfg := TierSweepRunnerConfig(p)
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("patrols.tier_sweep.runner.%w", err)
	}
	*c = cfg
	return nil
}

// validate refuses a block no run can use, naming the field that is wrong. The
// error starts with the field so the caller's prefix completes a dotted key.
func (c *TierSweepRunnerConfig) validate() error {
	switch c.Backend {
	case TierSweepRunnerDocker, TierSweepRunnerKubernetes:
	case "":
		return errors.New("backend: want " + TierSweepRunnerDocker + " or " + TierSweepRunnerKubernetes)
	default:
		return fmt.Errorf("backend = %q: want %s or %s", c.Backend, TierSweepRunnerDocker, TierSweepRunnerKubernetes)
	}
	if c.CPUs < runnerCPUsMin || c.CPUs > runnerCPUsMax {
		return fmt.Errorf("cpus = %d: want %d-%d", c.CPUs, runnerCPUsMin, runnerCPUsMax)
	}
	if c.Memory < runnerMemoryMin || c.Memory > runnerMemoryMax {
		return fmt.Errorf("memory = %d: want %d-%d GB", c.Memory, runnerMemoryMin, runnerMemoryMax)
	}
	// With dind the Kubernetes setting is the whole pod's total, so it must hold
	// the sidecar's fixed share and still leave the runner its own minimum. A
	// total at or below the sidecar's share renders a runner with nothing
	// (gt-gg9hv.38).
	if c.Backend == TierSweepRunnerKubernetes && c.Dind {
		if min := runnerCPUsMin + SweepDindCPUs; c.CPUs < min {
			return fmt.Errorf("cpus = %d: want at least %d with dind (the sidecar's %d CPUs plus the runner's %d)", c.CPUs, min, SweepDindCPUs, runnerCPUsMin)
		}
		if min := runnerMemoryMin + SweepDindMemoryGB; c.Memory < min {
			return fmt.Errorf("memory = %d: want at least %d GB with dind (the sidecar's %d GB plus the runner's %d)", c.Memory, min, SweepDindMemoryGB, runnerMemoryMin)
		}
	}
	if c.Jobs < runnerJobsMin || c.Jobs > runnerJobsMax {
		return fmt.Errorf("jobs = %d: want %d-%d", c.Jobs, runnerJobsMin, runnerJobsMax)
	}
	if !filepath.IsAbs(c.OutputRoot) {
		return fmt.Errorf("output_root = %q: want an absolute path", c.OutputRoot)
	}
	if c.Context == "" {
		return errors.New("context: want the engine context the run starts in")
	}
	if c.ImageRegistry != "" {
		if err := validImageRegistry(c.ImageRegistry); err != nil {
			return fmt.Errorf("image_registry = %q: %w", c.ImageRegistry, err)
		}
	}
	// Nil is "unset" and takes the default; an explicit empty list is refused,
	// because it would make the preflight check nothing and pass.
	if c.ProtectedNamespaces != nil && len(c.ProtectedNamespaces) == 0 {
		return errors.New("protected_namespaces: want at least one namespace the sweep protects, such as forgejo")
	}
	for _, ns := range c.ProtectedNamespaces {
		if !validDNS1123Label(ns) {
			return fmt.Errorf("protected_namespaces = %q: want a namespace name", ns)
		}
	}
	return nil
}

// validImageRegistry reports why registry is not a bare host:port with a port
// in range, or nil. A scheme, a path and whitespace each get their own message,
// since a URL pasted in where a host:port belongs is the likely mistake.
func validImageRegistry(registry string) error {
	switch {
	case strings.Contains(registry, "://"):
		return errors.New("want a bare host:port, not a URL with a scheme")
	case strings.Contains(registry, "/"):
		return errors.New("want a bare host:port, not a path")
	case strings.ContainsFunc(registry, unicode.IsSpace):
		return errors.New("want a bare host:port with no whitespace")
	}
	host, port, err := net.SplitHostPort(registry)
	if err != nil || host == "" {
		return errors.New("want host:port, such as localhost:5001")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("want host:port with a port from 1 to 65535")
	}
	if _, err := url.Parse("http://" + registry); err != nil {
		return errors.New("want a host:port the registry address parses as")
	}
	return nil
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
	// gt:needs-overseer and escalates (gt-b5ugw), except a red-main revert.
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

	// PostLandTimeoutStr bounds one post-land verdict (e.g. "60m"): the run
	// of merge_queue.post_land_command, or, in status mode, the age at which a
	// pending verdict that is still UNKNOWN raises its attention item. Nothing
	// waits on CI in status mode; the pending entry is read on each worker pass
	// and the timeout is a condition of the entry. Default 60m.
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

// ContainerComposeFile is where a container town's compose file lives: rendered
// when the town is set up, and the file its container supervisor drives.
func ContainerComposeFile(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "container", "compose.yml")
}

// ContainerKubeconfigFile is where the compose render writes the sandbox
// cluster's kubeconfig the container reads, derived because the host's file
// names 127.0.0.1, which inside the container is its own loopback.
func ContainerKubeconfigFile(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "container", "kubeconfig.yaml")
}

// MayBeContainerTown reports whether the town carries the compose file only a
// container town has. It reads no settings, so it answers when they cannot be
// read: a caller that cannot tell how the daemon is supervised asks it whether
// the daemon.lock pid could be a container's, which names nothing on this host.
// A compose file that cannot be statted for a reason other than being absent
// counts as present, so the answer fails toward the container.
func MayBeContainerTown(townRoot string) bool {
	_, err := os.Stat(ContainerComposeFile(townRoot))
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
