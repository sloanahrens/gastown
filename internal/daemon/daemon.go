package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonboulle/clockwork"
	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/boot"
	"github.com/steveyegge/gastown/internal/channelevents"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/feed"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/wisp"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Daemon is the town-level background service.
// It ensures patrol agents (Deacon, Witnesses) are running and detects failures.
// This is recovery-focused: normal wake is handled by feed subscription (bd activity --follow).
// The daemon is the safety net for dead sessions, GUPP violations, and orphaned work.
type Daemon struct {
	config        *Config
	patrolConfig  *DaemonPatrolConfig
	tmux          sessionTmux
	logger        *log.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	curator       *feed.Curator
	convoyManager *ConvoyManager
	beadsStores   map[string]beadsdk.Storage
	doltServer    *DoltServerManager
	krcPruner     *KRCPruner

	// disabledPatrols is loaded from town settings (disabled_patrols field).
	// Provides a simple way to disable individual patrol dogs without editing
	// mayor/daemon.json. Checked by isPatrolActive alongside patrolConfig.
	disabledPatrols map[string]bool

	// Mass death detection: track recent session deaths
	deathsMu     sync.Mutex
	recentDeaths []sessionDeath

	// PATCH-006: Resolved binary paths to avoid PATH issues in subprocesses.
	gtPath string
	bdPath string

	// notifier sends mail, nudges and escalations; nil means gt run as the
	// daemon (see notify()).
	notifier notify.Notifier

	// clock times the daemon's own waits (see clk()); nil is the real clock.
	clock clockwork.Clock

	// startDeaconFn replaces deacon.Manager's Start in tests, which drive the
	// restart paths against a fake tmux that deacon.Manager cannot see. Nil
	// uses the manager.
	startDeaconFn func() error

	// spawnBootFn replaces boot.Boot's Spawn in tests, for the same reason.
	// Nil spawns through boot.Boot.
	spawnBootFn func(b *boot.Boot) error

	// hostLoadFn pins the host-load reading main_branch_test decides on
	// (see hostLoad); nil measures the real host.
	hostLoadFn func() hostLoad

	// rigOperational memoizes each rig's docked/parked determination for a short
	// window, so the many per-rig-per-heartbeat call sites share one lookup
	// instead of each paying a bd subprocess - which, on a CPU-starved host,
	// costs its entire 60s budget (gt-4nu3).
	//
	// Concurrent by design: isRigOperational runs on the heartbeat goroutine
	// (patrol rig filters), on rigPool workers (witness/refinery auto-start),
	// and on the convoy manager's goroutine (its isRigParked callback).
	rigOperational rigOperationalCache

	// rigStatusAlert / rigStatusClear raise and close the escalation for a rig
	// whose docked/parked status cannot be read - the condition that silently
	// suppresses auto-start for that rig. New wires them to the daemon's alert
	// helpers; they stay nil on a zero-value Daemon (unit tests), which logs
	// the failure but has no town to escalate into (gt-4nu3).
	rigStatusAlert func(key, source, message string)
	rigStatusClear func(reason string, keys ...string)

	// rigBeadShowFn replaces the bd show behind the rig docked/parked read in
	// tests; nil runs bd (see showRigBead).
	rigBeadShowFn func(rigPath, rigBeadID string) (*beads.Issue, error)

	// checkpointRevertAlert raises the escalation for a checkpoint_dog WIP
	// commit that would revert content already merged to main (gt-2bp8). New
	// wires it to the daemon's alert helper; it stays nil on a zero-value
	// Daemon (unit tests), which logs the refusal but has no town to escalate
	// into.
	checkpointRevertAlert func(key, source, message string)

	// Boot spawn cooldown: prevents Boot from spawning on every heartbeat tick.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	bootLastSpawned time.Time

	// scripts tracks script-type plugins whose run.sh is executing
	// in-process (gt-fo2k). Lazily created; safe for concurrent use.
	scripts     *scriptRunner
	scriptsOnce sync.Once

	// consumption rate-limits the daemon's "alive but consuming no input"
	// escalation to one per session per interval (gt-eigw). Lazily created;
	// safe for concurrent use.
	consumption     *consumptionEscalator
	consumptionOnce sync.Once
	// pendingClock records when each probed session's composer was first seen
	// holding unsubmitted input, so the probe can tell how long input has been
	// waiting rather than how long the pane has been silent (gt-afa7).
	// Lazily created.
	pendingClock     *tmux.PendingInputClock
	pendingClockOnce sync.Once
	// findDogFn overrides dog selection in tests; nil uses the real pack.
	findDogFn func() *dog.Dog

	// reaperSlingFn replaces the `gt sling` subprocess behind the wisp_reaper's
	// dog dispatch, and reaperSlingWaitFn replaces its retry backoff, so tests
	// drive the dispatch retry without a gt binary or real sleeps. Nil runs the
	// real command and really sleeps (see runReaperSling, waitReaperDispatch).
	reaperSlingFn     func(args []string) ([]byte, error)
	reaperSlingWaitFn func(time.Duration)

	// bootTriageInFlight is set while a mechanical `gt boot triage` runs.
	bootTriageInFlight atomic.Bool

	// dispatchMu serializes dog dispatch: the heartbeat's dispatchPlugins and
	// a script plugin's failure hand-off (a goroutine) both pick an idle dog
	// and assign it, so they must not interleave.
	dispatchMu sync.Mutex

	// supervisor holds the only Kill and Restart the daemon uses (see
	// sup()); tests may set it, otherwise it is built on first use.
	supOnce    sync.Once
	supervisor *supervisor.Supervisor
	// restartSeatFn replaces restartSeat, the supervisor's restart executor,
	// in tests that cannot run the role managers against a fake tmux.
	restartSeatFn func(supervisor.Seat) error

	// telemetry exports metrics and logs to VictoriaMetrics / VictoriaLogs.
	// Nil when telemetry is disabled (GT_OTEL_METRICS_URL / GT_OTEL_LOGS_URL not set).
	otelProvider *telemetry.Provider
	metrics      *daemonMetrics

	// jsonlPushFailures tracks consecutive git push failures for JSONL backup.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	jsonlPushFailures int

	// dogPourMu guards dogPour.
	dogPourMu sync.Mutex

	// dogPour is the daemon's memory of each dog's molecule-pour health across
	// patrol cycles (gt-i3rpw). Guarded rather than heartbeat-only because
	// compactor_dog's startup catch-up goroutine and pourDoctorMolecule's
	// anomaly goroutine both pour alongside the heartbeat.
	dogPour map[string]dogPourHealth

	// dogPourBdFn backs every bd call made through a poured molecule's handle —
	// the pour, its step discovery, and its closes. dogPourWaitFn replaces the
	// pour retry's backoff only; the close retries keep their own. Both are set
	// by tests, which need an always-failing bd and no real wall-clock to drive
	// the retry and escalation paths without a Dolt server. Nil uses the real bd
	// and time.Sleep.
	dogPourBdFn   func(args ...string) (string, error)
	dogPourWaitFn func(time.Duration)

	// lastDoctorMolTime tracks when the last mol-dog-doctor molecule was poured.
	// Option B throttling: only pour when anomaly detected AND cooldown elapsed.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	lastDoctorMolTime time.Time

	// lastMaintenanceRun tracks when scheduled maintenance last ran.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	lastMaintenanceRun time.Time

	// maintenanceGCRunning is set while a scheduled_maintenance gc cycle runs
	// on its own goroutine; it blocks a second cycle and an upgrade-restart.
	maintenanceGCRunning atomic.Bool
	// maintenanceGCFinishedAt is the UnixNano time a gc cycle completed or
	// failed (0 = none pending). The loop goroutine folds it into
	// lastMaintenanceRun; a deferred cycle never sets it.
	maintenanceGCFinishedAt atomic.Int64
	// maintenanceGCCallStartedAt is the UnixNano start of the dolt_gc call in
	// flight (0 = none); the Dolt health check defers restarts while it is
	// set and under timeout (doltRestartHeldForGC).
	maintenanceGCCallStartedAt atomic.Int64
	// maintenanceGCOverdueEscalated latches the one overdue-gc escalation per
	// call.
	maintenanceGCOverdueEscalated atomic.Bool
	// doltMaintMu serializes a gc call (write side, TryLock) against the
	// daemon's own Dolt tasks (read side, TryRLock via tryDoltTask). Nobody
	// blocks on it; see maintenance_gc_guard.go.
	doltMaintMu sync.RWMutex

	// compactorDogMu guards the three fields below, and serializes compactor
	// cycles. Due-ness is evaluated on the loop's tick and on the startup
	// catch-up goroutine, and a cycle is expensive enough that two in flight at
	// once must not happen.
	compactorDogMu sync.Mutex

	// compactorDogRunning is true while a cycle is in flight.
	compactorDogRunning bool

	// lastCompactorDogRun is the completion time of the last cycle this process
	// ran, which can be newer than the persisted record when the write failed.
	// Zero until the first cycle completes.
	lastCompactorDogRun time.Time

	// compactorDogChecked is true once this process has evaluated due-ness, so
	// that "not due" is logged after a restart and not every 15 minutes.
	compactorDogChecked bool

	// rigPool runs per-rig heartbeat operations (witness checks, refinery checks,
	// polecat health, idle reaping, branch pruning) with bounded concurrency and
	// per-rig context timeouts so one slow rig cannot block all others.
	rigPool *RigWorkerPool

	// knownRigsCache memoizes the result of reading mayor/rigs.json for the
	// duration of a single heartbeat tick. ~10 call sites per tick otherwise
	// re-read and re-parse the same file. Invalidated at the start of each
	// heartbeat so rigs.json changes between ticks are picked up.
	//
	// knownRigsMu guards both fields. The memo is per-tick, but a tick's
	// main_branch_test cycle runs on its own goroutine (gt-uvxy) and reads the
	// cache, so the heartbeat's invalidation and that read would otherwise
	// touch the fields concurrently (gt-f18v). Later background patrols that
	// call getKnownRigs inherit the guard.
	knownRigsMu         sync.Mutex
	knownRigsCache      []string
	knownRigsCacheValid bool

	// legacySocketCleanupOnce ensures upgrade cleanup only runs once per daemon
	// lifetime, before any patrol agent can be started on the current socket.
	legacySocketCleanupOnce sync.Once

	// mainBranchTestRunning guards against overlapping main_branch_test
	// cycles. A single cycle can block for up to (60m slot wait + 10m test)
	// per rig, so it runs on its own goroutine rather than inline in the
	// main select loop (which would otherwise freeze heartbeat, reaper, and
	// dog ticks for hours — gt-uvxy). A tick that arrives while the previous
	// cycle is still running is skipped rather than piling up concurrently.
	mainBranchTestRunning atomic.Bool

	// mainBranchTestCycles counts the cycle goroutines triggerMainBranchTests
	// has started and not yet finished, including the last-run write after
	// the cycle. Waiting on it is how a caller knows a triggered cycle is
	// wholly done, rather than polling mainBranchTestRunning against a clock.
	mainBranchTestCycles sync.WaitGroup

	// mainBranchTestWaitingSlot is true only while a main_branch_test run is
	// blocked in acquireMainBranchTestSlot. Killing a run in that state costs
	// nothing (an interrupted run is a non-verdict, gt-59yz), so
	// isIdleForUpgrade treats it as idle.
	mainBranchTestWaitingSlot atomic.Bool

	// upgradeRestartRequested is set by checkUpgradeRestart; the run loop
	// then shuts down (leaving Dolt running) and Run returns
	// ErrRestartForUpgrade.
	upgradeRestartRequested atomic.Bool
	// upgradeWait* track the pending restart marker for the stuck and
	// no-effect alarms. Run-loop goroutine only (startup and heartbeat).
	upgradeWaitCommit    string
	upgradeWaitSince     time.Time
	upgradeWaitEscalated bool

	// gateBusySince records, per rig, when its current unbroken run of
	// main_branch_test gate-busy skips began — the clock
	// patrols.main_branch_test.gate_busy_starve_after is measured on. Guarded
	// by gateBusyMu rather than relying on mainBranchTestRunning's single
	// flight above: a caller that stops going through triggerMainBranchTests
	// must not have to know that (gt-lf2r).
	gateBusyMu    sync.Mutex
	gateBusySince map[string]time.Time

	// scheduledSlingsRunning is the single-flight guard for the scheduled_slings
	// patrol, on its own goroutine like mainBranchTest so a slow sling never
	// blocks the select loop (gt-nj23).
	scheduledSlingsRunning atomic.Bool

	// scheduledSlingRunner is nil in production (an exec runner is built on
	// first use) and a fake in tests.
	scheduledSlingRunner scheduledSlingRunner

	// scheduledSlingFailures counts consecutive failures per entry name; the
	// third escalates once, success resets. Touched only by the patrol goroutine.
	scheduledSlingFailures map[string]int

	// scheduledSlingEscalate defaults to d.escalate; tests capture it.
	scheduledSlingEscalate func(source, message string)

	// scheduledSlingsHold and queuedWorkHold remember the operator dispatch
	// hold each dispatcher last saw, so a hold is logged once when it appears,
	// changes, or lifts rather than on every tick (gt-ifijm).
	scheduledSlingsHold dispatch.HoldLatch
	queuedWorkHold      dispatch.HoldLatch

	// mayorDispatchRunning is the single-flight guard for the mayor_dispatch
	// patrol, on its own goroutine: the cycle shells out to `gt daemon
	// dispatch-check` and then nudges, either of which can take tens of
	// seconds, and running them inline would hold the tick loop (gt-59o9).
	mayorDispatchRunning atomic.Bool

	// mayorDispatchCycles counts the cycle goroutines triggerMayorDispatch has
	// started and not yet finished, so a caller can wait for a triggered
	// cycle to end instead of polling mayorDispatchRunning against a clock.
	mayorDispatchCycles sync.WaitGroup

	// specDispatchRunning / specDispatchCycles are the spec_dispatch ticker's
	// single-flight guard and cycle count (gt-4k3fj.5, spec_dispatch.go).
	specDispatchRunning atomic.Bool
	specDispatchCycles  sync.WaitGroup

	// patrolWatchdogRunning is the single-flight guard for the patrol_watchdog
	// patrol, on its own goroutine: it checks every known rig's witness and
	// refinery plus the deacon, each read involving a bd subprocess and a
	// tmux liveness check, and any Fail also shells out to `gt escalate` and
	// `gt nudge` — running inline would hold the tick loop (gt-4z3b7).
	patrolWatchdogRunning atomic.Bool

	// patrolWatchdogPaused throttles the watchdog's "skipping <rig>" line to
	// one per rig per patrolWatchdogPausedLogInterval, and remembers the
	// transition into a paused state so the rig's stale patrol alert is
	// cleared exactly once (gt-7g14a).
	patrolWatchdogPaused pausedRigLog

	// jsonlGitBackupRunning, wispReaperRunning, and checkpointDogRunning are
	// the single-flight guards for their patrols, on their own goroutines —
	// the same gt-ima2 shape as compactor_dog and mainBranchTestRunning
	// above, applied by gt-gxpwc so a restart cannot starve these patrols by
	// resetting an in-process ticker's countdown. Due-ness for all three is
	// decided against the persisted last-run time (patrol_last_run.go), not
	// against the ticker.
	jsonlGitBackupRunning atomic.Bool
	wispReaperRunning     atomic.Bool
	checkpointDogRunning  atomic.Bool

	// doltBackupRunning is the single-flight guard for the dolt_backup patrol,
	// on its own goroutine — same gt-ima2/gt-gxpwc shape as the three above,
	// converted in the gt-gxpwc rework after crew review found this patrol was
	// still starved (no persisted last-run or startup catch-up at all).
	doltBackupRunning atomic.Bool
}

// sessionDeath records a detected session death for mass death analysis.
type sessionDeath struct {
	sessionName string
	timestamp   time.Time
}

// Mass death detection parameters — these are fallback defaults.
// Prefer config.OperationalConfig.GetDaemonConfig() accessors when
// a TownSettings is available (loaded via d.loadOperationalConfig()).
const (
	massDeathWindow    = 30 * time.Second // Time window to detect mass death
	massDeathThreshold = 3                // Number of deaths to trigger alert

	// doctorMolCooldown is the minimum interval between mol-dog-doctor molecules.
	// Configurable via operational.daemon.doctor_mol_cooldown.
	doctorMolCooldown = 5 * time.Minute
)

func daemonPathCandidates(home, exePath string) []string {
	candidates := make([]string, 0, 5)
	if exePath != "" {
		candidates = append(candidates, filepath.Dir(exePath))
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local/bin"),
			filepath.Join(home, "bin"),
		)
	}
	return append(candidates,
		"/opt/homebrew/bin",
		"/usr/local/bin",
	)
}

func augmentDaemonPath(logger *log.Logger) {
	exePath := ""
	if exe, err := os.Executable(); err == nil {
		exePath = exe
	}
	extras := daemonPathCandidates(os.Getenv("HOME"), exePath)
	if len(extras) == 0 {
		return
	}

	current := os.Getenv("PATH")
	parts := strings.Split(current, string(os.PathListSeparator))
	seen := make(map[string]struct{}, len(parts)+len(extras))
	for _, p := range parts {
		seen[p] = struct{}{}
	}
	additions := make([]string, 0, len(extras))
	for _, extra := range extras {
		if _, ok := seen[extra]; ok {
			continue
		}
		if info, statErr := os.Stat(extra); statErr == nil && info.IsDir() {
			additions = append(additions, extra)
			seen[extra] = struct{}{}
		}
	}
	augmented := append(additions, parts...)
	newPath := strings.Join(augmented, string(os.PathListSeparator))
	if newPath != current {
		_ = os.Setenv("PATH", newPath)
		logger.Printf("PATCH-007: augmented daemon PATH with user/local bin dirs (was=%q, now=%q)", current, newPath)
	}
}

var cleanupLegacySocketsForDaemon = func(townRoot string) (int, int) {
	defaultCleaned := session.CleanupLegacyDefaultSocket()
	baseCleaned := session.CleanupLegacyBaseSocket(townRoot)
	return defaultCleaned, baseCleaned
}

// New creates a new daemon instance.
func New(config *Config) (*Daemon, error) {
	// Ensure daemon directory exists
	daemonDir := filepath.Dir(config.LogFile)
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		return nil, fmt.Errorf("creating daemon directory: %w", err)
	}

	// Open log file with rotation (100MB max, 3 backups, 7 days, compressed)
	logWriter := &lumberjack.Logger{
		Filename:   config.LogFile,
		MaxSize:    100, // megabytes
		MaxBackups: 3,
		MaxAge:     7, // days
		Compress:   true,
	}

	logger := log.New(logWriter, "", log.LstdFlags)

	// Fail closed on a town config file that does not parse (gt-fcxe9.10):
	// starting from compiled defaults would re-enable patrols the operator
	// turned off and move every role to the default agent. Checked before any
	// env, tmux or file side effect.
	if err := CheckTownConfig(config.TownRoot); err != nil {
		logger.Printf("Refusing to start: %v", err)
		return nil, fmt.Errorf("refusing to start the daemon: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// PATCH-007 (hq-olcb): Augment PATH with common user/local bin
	// directories before any subprocess lookup. The daemon is often launched
	// from systemd / login shells / launchd without user-installed tool dirs
	// such as ~/.local/bin or /opt/homebrew/bin.
	augmentDaemonPath(logger)

	// Initialize session prefix and agent registries from town root.
	if err := session.InitRegistry(config.TownRoot); err != nil {
		logger.Printf("Warning: failed to initialize town registry: %v", err)
	}

	// Set GT_TOWN_ROOT in the daemon process env so Go code (e.g.,
	// sessionPrefixPattern) can read it without relying on GT_ROOT.
	os.Setenv("GT_TOWN_ROOT", config.TownRoot)

	// Also set GT_TOWN_ROOT in tmux global environment so run-shell subprocesses
	// (e.g., gt cycle next/prev) can find the workspace even when CWD is $HOME.
	// Non-fatal: tmux server may not be running yet — daemon creates sessions shortly.
	t := tmux.NewTmux()
	if err := t.SetGlobalEnvironment("GT_TOWN_ROOT", config.TownRoot); err != nil {
		logger.Printf("Warning: failed to set GT_TOWN_ROOT in tmux global env: %v", err)
	}

	// Clear any agent identity vars that leaked into tmux global env.
	// Only GT_TOWN_ROOT should be global. Leaked identity vars cause sessions
	// without their own session-level overrides to inherit a stale identity,
	// misattributing beads and mail. GH#3006.
	identityVars := agentconfig.IdentityEnvVars
	for _, k := range identityVars {
		_ = t.UnsetGlobalEnvironment(k)
	}

	// Load patrol config from mayor/daemon.json, ensuring lifecycle defaults
	// are populated for any missing data maintenance tickers. Without this,
	// opt-in patrols (compactor, reaper, doctor, JSONL backup, dolt backup)
	// remain disabled if the file was created before they were implemented.
	if err := EnsureLifecycleConfigFile(config.TownRoot); err != nil {
		if errors.Is(err, agentconfig.ErrUnparseable) {
			cancel()
			logger.Printf("Refusing to start: %v", err)
			return nil, fmt.Errorf("refusing to start the daemon: %w", err)
		}
		logger.Printf("Warning: failed to ensure lifecycle config: %v", err)
	}
	patrolConfig := LoadPatrolConfig(config.TownRoot)
	if patrolConfig != nil {
		logger.Printf("Loaded patrol config from %s", PatrolConfigFile(config.TownRoot))
		// Propagate env vars from daemon.json to this process and all spawned sessions.
		for k, v := range patrolConfig.Env {
			os.Setenv(k, v)
			logger.Printf("Set env %s=%s from daemon.json", k, v)
		}
	}
	agentconfig.ApplyConfiguredDoltEnv(config.TownRoot)

	// Load disabled_patrols from town settings (settings/config.json).
	// This provides a simpler way to disable patrols than editing daemon.json.
	disabledPatrols := loadDisabledPatrolsFromTownSettings(config.TownRoot)
	if len(disabledPatrols) > 0 {
		names := make([]string, 0, len(disabledPatrols))
		for k := range disabledPatrols {
			names = append(names, k)
		}
		logger.Printf("Patrols disabled via town settings: %v", names)
	}

	// Initialize Dolt server manager if configured
	var doltServer *DoltServerManager
	if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.DoltServer != nil {
		doltServer = NewDoltServerManager(config.TownRoot, patrolConfig.Patrols.DoltServer, logger.Printf)
		if doltServer.IsEnabled() {
			logger.Printf("Dolt server management enabled (port %d)", doltServer.config.Port)
			// Propagate Dolt connection info to process env so AgentEnv() passes it to
			// all spawned agent sessions. Without this, bd in agent sessions
			// auto-starts rogue Dolt instances or connects to localhost. (GH#2412)
			applyDoltServerConfigEnv(doltServer.config)
		}
	}

	// Fallback: if GT_DOLT_PORT still isn't set (no DoltServerManager, daemon
	// started independently of gt up), detect the port from dolt config.
	// This ensures AgentEnv() always has the port for spawned sessions. (GH#2412)
	if os.Getenv("GT_DOLT_PORT") == "" {
		if port := agentconfig.ResolveConfiguredDoltPort(config.TownRoot); port > 0 {
			portStr := strconv.Itoa(port)
			os.Setenv("GT_DOLT_PORT", portStr)
			os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
			os.Setenv("BEADS_DOLT_PORT", portStr)
			logger.Printf("Set GT_DOLT_PORT=%s from resolved Dolt config (fallback)", portStr)
		}
	} else {
		portStr := os.Getenv("GT_DOLT_PORT")
		os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
		os.Setenv("BEADS_DOLT_PORT", portStr)
	}

	// Propagate Dolt host to process env so bd doesn't fall back to 127.0.0.1
	// when the server runs on a remote machine. BEADS_DOLT_SERVER_HOST is a
	// derived alias, not an authority, so stale inherited values are replaced or
	// removed here.
	applyConfiguredDoltHostEnv(config.TownRoot, logger.Printf)

	// PATCH-006: Resolve binary paths at startup.
	gtPath, err := exec.LookPath("gt")
	if err != nil {
		gtPath = "gt"
		logger.Printf("Warning: gt not found in PATH, subprocess calls may fail")
	}
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		bdPath = "bd"
		logger.Printf("Warning: bd not found in PATH, subprocess calls may fail")
	}

	// Initialize OpenTelemetry (best-effort — telemetry failure never blocks startup).
	// Activate by setting GT_OTEL_METRICS_URL and/or GT_OTEL_LOGS_URL.
	otelProvider, otelErr := telemetry.Init(ctx, "gastown-daemon", "")
	if otelErr != nil {
		logger.Printf("Warning: telemetry init failed: %v", otelErr)
	}
	var dm *daemonMetrics
	if otelProvider != nil {
		dm, err = newDaemonMetrics()
		if err != nil {
			logger.Printf("Warning: failed to register daemon metrics: %v", err)
			dm = nil
		} else {
			metricsURL := os.Getenv(telemetry.EnvMetricsURL)
			if metricsURL == "" {
				metricsURL = telemetry.DefaultMetricsURL
			}
			logsURL := os.Getenv(telemetry.EnvLogsURL)
			if logsURL == "" {
				logsURL = telemetry.DefaultLogsURL
			}
			logger.Printf("Telemetry active (metrics → %s, logs → %s)",
				metricsURL, logsURL)
		}
	}

	d := &Daemon{
		config:          config,
		patrolConfig:    patrolConfig,
		disabledPatrols: disabledPatrols,
		tmux:            tmux.NewTmux(),
		logger:          logger,
		ctx:             ctx,
		cancel:          cancel,
		doltServer:      doltServer,
		gtPath:          gtPath,
		bdPath:          bdPath,
		notifier:        newDaemonNotifier(gtPath, config.TownRoot),
		otelProvider:    otelProvider,
		metrics:         dm,
		rigPool:         newRigWorkerPool(0, 0, logger), // defaults: 10 workers, 30s timeout
	}

	// A rig whose docked/parked status cannot be read is treated as not
	// operational, which suppresses auto-start for that rig. Escalate it, so
	// the suppression is something the Mayor sees rather than a warning line in
	// a log nobody reads while the rig sits dead (gt-4nu3).
	d.rigStatusAlert = d.escalateAlert
	d.rigStatusClear = d.clearAlerts
	d.checkpointRevertAlert = d.escalateAlert

	// A health-driven Dolt restart waits for an in-flight scheduled gc.
	if doltServer != nil {
		doltServer.SetRestartSuppressor(d.doltRestartHeldForGC)
	}

	return d, nil
}

func applyDoltServerConfigEnv(config *DoltServerConfig) {
	if config == nil {
		return
	}
	if config.Port > 0 {
		portStr := strconv.Itoa(config.Port)
		os.Setenv("GT_DOLT_PORT", portStr)
		os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
		os.Setenv("BEADS_DOLT_PORT", portStr)
	}
	if config.Host != "" {
		os.Setenv("GT_DOLT_HOST", config.Host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", config.Host)
	}
}

func applyConfiguredDoltHostEnv(townRoot string, logf func(format string, v ...interface{})) {
	if host := agentconfig.ResolveConfiguredDoltHost(townRoot); host != "" {
		os.Setenv("GT_DOLT_HOST", host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", host)
		if logf != nil {
			logf("Set BEADS_DOLT_SERVER_HOST=%s from resolved Dolt host", host)
		}
		return
	}
	if _, _, ok := agentconfig.ManagedDoltEndpoint(townRoot); ok {
		os.Unsetenv("GT_DOLT_HOST")
	}
	os.Unsetenv("BEADS_DOLT_SERVER_HOST")
}

func (d *Daemon) cleanupLegacySocketSessions() {
	d.legacySocketCleanupOnce.Do(func() {
		defaultCleaned, baseCleaned := cleanupLegacySocketsForDaemon(d.config.TownRoot)
		if defaultCleaned > 0 {
			d.logger.Printf("legacy_socket_cleanup: cleaned %d session(s) from default socket", defaultCleaned)
		}
		if baseCleaned > 0 {
			d.logger.Printf("legacy_socket_cleanup: cleaned %d session(s) from basename socket", baseCleaned)
		}
	})
}

// Run starts the daemon main loop.
func (d *Daemon) Run() (err error) {
	pid := os.Getpid()
	d.logger.Printf("Daemon starting (PID %d)", pid)
	startupComplete := false
	defer func() {
		if err == nil {
			return
		}
		if startupComplete {
			d.logger.Printf("Daemon exiting with error (PID %d): %v", pid, err)
			return
		}
		d.logger.Printf("Daemon startup failed (PID %d): %v", pid, err)
	}()

	// Acquire exclusive lock to prevent multiple daemons from running.
	// This prevents the TOCTOU race condition where multiple concurrent starts
	// can all pass the IsRunning() check before any writes the PID file.
	// Uses gofrs/flock for cross-platform compatibility (Unix + Windows).
	lockFile := filepath.Join(d.config.TownRoot, "daemon", "daemon.lock")
	fileLock := flock.New(lockFile)

	// Try to acquire exclusive lock (non-blocking)
	locked, err := fileLock.TryLock()
	if err != nil {
		return fmt.Errorf("acquiring lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("daemon already running (lock held by another process)")
	}
	defer func() { _ = fileLock.Unlock() }()

	// Pre-flight check: all rigs must be on Dolt backend.
	if err := d.checkAllRigsDolt(); err != nil {
		return err
	}

	// Repair metadata.json for all rigs on startup.
	// This ensures all rigs have proper Dolt server configuration.
	if _, errs := doltserver.EnsureAllMetadata(d.config.TownRoot); len(errs) > 0 {
		for _, e := range errs {
			d.logger.Printf("Warning: metadata repair: %v", e)
		}
	}

	// Write PID file with nonce for ownership verification
	if _, err := writePIDFile(d.config.PidFile, os.Getpid()); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer func() { _ = os.Remove(d.config.PidFile) }() // best-effort cleanup

	// Update state
	state := &State{
		Running:   true,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
		Commit:    d.resolveOwnCommit(),
	}
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	// Clear a restart marker this binary already covers (the daemon may have
	// been restarted onto it by launchd or an operator). Covered-only: startup
	// never exits for an upgrade; a newer marker waits for the heartbeat.
	d.clearCoveredRestartMarker(time.Now())

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, daemonSignals()...)

	// Fixed recovery-focused heartbeat (no activity-based backoff)
	// Normal wake is handled by feed subscription (bd activity --follow)
	timer := time.NewTimer(d.recoveryHeartbeatInterval())
	defer timer.Stop()

	d.logger.Printf("Daemon running, recovery heartbeat interval %v", d.recoveryHeartbeatInterval())

	// Start feed curator goroutine
	d.curator = feed.NewCurator(d.config.TownRoot)
	if err := d.curator.Start(); err != nil {
		d.logger.Printf("Warning: failed to start feed curator: %v", err)
	} else {
		d.logger.Println("Feed curator started")
	}

	// Start convoy manager (event-driven + periodic stranded scan)
	// Try opening beads stores eagerly; if Dolt isn't ready yet,
	// pass the opener as a callback for lazy retry on each poll tick.
	startupStores, err := d.openBeadsStores()
	if err != nil {
		return err
	}
	d.beadsStores = startupStores.Stores

	// Clean sessions left behind on legacy tmux sockets after daemon startup has
	// passed fatal preflight checks but before any patrol agents can be spawned.
	d.cleanupLegacySocketSessions()

	isRigParked := func(rigName string) bool {
		ok, _ := d.isRigOperational(rigName)
		return !ok
	}
	var storeOpener func() storeOpenResult
	if storeOpenerNeeded(startupStores) {
		storeOpener = func() storeOpenResult {
			stores, err := d.openBeadsStores()
			if err != nil {
				d.logger.Printf("Convoy: beads compatibility check failed: %v", err)
				return storeOpenResult{}
			}
			return stores
		}
	}
	d.convoyManager = NewConvoyManager(d.config.TownRoot, d.logger.Printf, d.gtPath, 0, d.beadsStores, storeOpener, isRigParked)
	d.convoyManager.SetAlertHooks(d.escalateAlert, d.clearAlertsErr)
	if err := d.convoyManager.Start(); err != nil {
		d.logger.Printf("Warning: failed to start convoy manager: %v", err)
	} else {
		d.logger.Println("Convoy manager started")
	}

	// Landing workers (gt-v4ssj.2): opt-in, one goroutine per rig.
	d.startLandingWorkers()

	// Wire a recovery callback so that when Dolt transitions from unhealthy
	// back to healthy, the convoy manager runs a sweep to catch any convoys
	// that completed during the outage and were missed by the event poller.
	if d.doltServer != nil {
		cm := d.convoyManager
		d.doltServer.SetRecoveryCallback(func() {
			d.logger.Printf("Dolt recovery detected: triggering convoy recovery sweep")
			cm.scan()
		})
	}

	// Start KRC pruner for automatic ephemeral data cleanup
	krcPruner, err := NewKRCPruner(d.config.TownRoot, d.logger.Printf)
	if err != nil {
		d.logger.Printf("Warning: failed to create KRC pruner: %v", err)
	} else {
		d.krcPruner = krcPruner
		if err := d.krcPruner.Start(); err != nil {
			d.logger.Printf("Warning: failed to start KRC pruner: %v", err)
		} else {
			d.logger.Println("KRC pruner started")
		}
	}

	// Start dedicated Dolt health check ticker if Dolt server is configured.
	// This runs at a much higher frequency (default 30s) than the general
	// heartbeat (3 min) so Dolt crashes are detected quickly.
	var doltHealthTicker *time.Ticker
	var doltHealthChan <-chan time.Time
	if d.doltServer != nil && d.doltServer.IsEnabled() {
		interval := d.doltServer.HealthCheckInterval()
		doltHealthTicker = time.NewTicker(interval)
		doltHealthChan = doltHealthTicker.C
		defer doltHealthTicker.Stop()
		d.logger.Printf("Dolt health check ticker started (interval %v)", interval)
	}

	// Start dedicated Dolt backup ticker if configured.
	// Runs filesystem backup sync (dolt backup sync) for production databases.
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var doltBackupTicker *time.Ticker
	var doltBackupChan <-chan time.Time
	if d.isPatrolActive("dolt_backup") {
		interval := doltBackupInterval(d.patrolConfig)
		doltBackupTicker = time.NewTicker(shortPatrolCheckTick(interval))
		doltBackupChan = doltBackupTicker.C
		defer doltBackupTicker.Stop()
		d.logger.Printf("Dolt backup ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup (gt-ima2, gt-gxpwc).
		d.triggerDoltBackup()
	}

	// Start JSONL git backup ticker if configured.
	// Exports issues to JSONL, scrubs ephemeral data, pushes to git repo.
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var jsonlGitBackupTicker *time.Ticker
	var jsonlGitBackupChan <-chan time.Time
	if d.isPatrolActive("jsonl_git_backup") {
		interval := jsonlGitBackupInterval(d.patrolConfig)
		jsonlGitBackupTicker = time.NewTicker(shortPatrolCheckTick(interval))
		jsonlGitBackupChan = jsonlGitBackupTicker.C
		defer jsonlGitBackupTicker.Stop()
		d.logger.Printf("JSONL git backup ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup rather than waiting for the first tick: due-ness
		// is a wall-clock question and a restart must not postpone the answer
		// (gt-ima2, gt-gxpwc).
		d.triggerJsonlGitBackup()
	}

	// Start wisp reaper ticker if configured.
	// Closes stale wisps (abandoned molecule steps, old patrol data) across all databases.
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var wispReaperTicker *time.Ticker
	var wispReaperChan <-chan time.Time
	if d.isPatrolActive("wisp_reaper") {
		interval := wispReaperInterval(d.patrolConfig)
		wispReaperTicker = time.NewTicker(shortPatrolCheckTick(interval))
		wispReaperChan = wispReaperTicker.C
		defer wispReaperTicker.Stop()
		d.logger.Printf("Wisp reaper ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup (gt-ima2, gt-gxpwc).
		d.triggerWispReaper()
	}

	// Start doctor dog ticker if configured.
	// Health monitor: TCP check, latency, DB count, gc, zombie detection, backup/disk checks.
	var doctorDogTicker *time.Ticker
	var doctorDogChan <-chan time.Time
	if d.isPatrolActive("doctor_dog") {
		interval := doctorDogInterval(d.patrolConfig)
		doctorDogTicker = time.NewTicker(interval)
		doctorDogChan = doctorDogTicker.C
		defer doctorDogTicker.Stop()
		d.logger.Printf("Doctor dog ticker started (interval %v)", interval)
	}

	// Start compactor dog ticker if configured.
	// Monitors Dolt commit counts and escalates over threshold; compaction
	// itself is operator-only. The ticker is a check cadence — due-ness comes
	// from a persisted last-run time, because a run interval enforced by an
	// in-process ticker resets on every restart (gt-ima2).
	var compactorDogTicker *time.Ticker
	var compactorDogChan <-chan time.Time
	if d.isPatrolActive("compactor_dog") {
		interval := compactorDogInterval(d.patrolConfig)
		compactorDogTicker = time.NewTicker(compactorDogTickInterval)
		compactorDogChan = compactorDogTicker.C
		defer compactorDogTicker.Stop()
		d.logger.Printf("Compactor dog ticker started (check every %v, run interval %v)",
			compactorDogTickInterval, interval)
		// Catch up at startup rather than waiting for the first tick: due-ness
		// is a wall-clock question and a restart must not postpone the answer
		// (gt-ima2). This cycle is a SQL connection to Dolt and this catch-up
		// dispatches before the first heartbeat has started the server, so the
		// trigger brings it up itself (gt-ox6c).
		d.triggerCompactorDog()
	}

	// Start checkpoint dog ticker if configured.
	// Auto-commits WIP changes in active polecat worktrees to prevent data loss.
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var checkpointDogTicker *time.Ticker
	var checkpointDogChan <-chan time.Time
	if d.isPatrolActive("checkpoint_dog") {
		interval := checkpointDogInterval(d.patrolConfig)
		checkpointDogTicker = time.NewTicker(shortPatrolCheckTick(interval))
		checkpointDogChan = checkpointDogTicker.C
		defer checkpointDogTicker.Stop()
		d.logger.Printf("Checkpoint dog ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup (gt-ima2, gt-gxpwc).
		d.triggerCheckpointDog()
	}

	// Start scheduled maintenance ticker if configured.
	// Checks periodically whether we're in the maintenance window and
	// runs `gt maintain --force` when commit counts exceed threshold.
	var scheduledMaintenanceTicker *time.Ticker
	var scheduledMaintenanceChan <-chan time.Time
	if d.isPatrolActive("scheduled_maintenance") {
		interval := maintenanceCheckInterval(d.patrolConfig)
		scheduledMaintenanceTicker = time.NewTicker(interval)
		scheduledMaintenanceChan = scheduledMaintenanceTicker.C
		defer scheduledMaintenanceTicker.Stop()
		window := maintenanceWindow(d.patrolConfig)
		d.logger.Printf("Scheduled maintenance ticker started (check interval %v, window %s)", interval, window)
	}

	// Start main-branch test runner ticker if configured.
	// Periodically runs quality gates on each rig's main branch to catch regressions.
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var mainBranchTestTicker *time.Ticker
	var mainBranchTestChan <-chan time.Time
	if d.isPatrolActive("main_branch_test") {
		interval := mainBranchTestInterval(d.patrolConfig)
		mainBranchTestTicker = time.NewTicker(shortPatrolCheckTick(interval))
		mainBranchTestChan = mainBranchTestTicker.C
		defer mainBranchTestTicker.Stop()
		d.logger.Printf("Main branch test ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup (gt-ima2, gt-gxpwc). triggerMainBranchTests logs
		// its own due-ness decision; the startup log line below just names
		// whether this specific check kicked off a cycle.
		if d.triggerMainBranchTests() {
			d.logger.Printf("Main branch test startup catch-up started a cycle")
		}
	}

	// Start the scheduled_slings ticker if configured. Each tick evaluates
	// every entry; the decision function decides due-ness, so a coarse tick
	// is enough (gt-nj23).
	var scheduledSlingsTicker *time.Ticker
	var scheduledSlingsChan <-chan time.Time
	if d.isPatrolActive("scheduled_slings") {
		scheduledSlingsTicker = time.NewTicker(scheduledSlingsTickInterval)
		scheduledSlingsChan = scheduledSlingsTicker.C
		defer scheduledSlingsTicker.Stop()
		d.logger.Printf("Scheduled slings ticker started (tick %v, %d entries)", scheduledSlingsTickInterval, len(d.patrolConfig.Patrols.ScheduledSlings.Entries))
	}

	// Start quota dog ticker if configured.
	// Scans for rate-limited sessions and automatically rotates credentials.
	var quotaDogTicker *time.Ticker
	var quotaDogChan <-chan time.Time
	if d.isPatrolActive("quota_dog") {
		interval := quotaDogInterval(d.patrolConfig)
		quotaDogTicker = time.NewTicker(interval)
		quotaDogChan = quotaDogTicker.C
		defer quotaDogTicker.Stop()
		d.logger.Printf("Quota dog ticker started (interval %v)", interval)
	}

	// Start quota resume ticker if configured. This runs independently of
	// quota_dog and the account pool — it nudges sessions whose own
	// session-limit reset has passed even on a town with < 2 accounts
	// configured, where quota_dog's rotation path can't run at all (gt-749e).
	var quotaResumeTicker *time.Ticker
	var quotaResumeChan <-chan time.Time
	if d.isPatrolActive("quota_resume") {
		interval := quotaResumeInterval(d.patrolConfig)
		quotaResumeTicker = time.NewTicker(interval)
		quotaResumeChan = quotaResumeTicker.C
		defer quotaResumeTicker.Stop()
		d.logger.Printf("Quota resume ticker started (interval %v)", interval)
	}

	// Start the idle-seat dispatch check ticker if configured. The mayor is
	// event-driven and a "no dispatch" decision opens no slot, so once it
	// declines with no polecats running nothing wakes it again; this ticker is
	// the timer that decision does not self-provide (gt-59o9).
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var mayorDispatchTicker *time.Ticker
	var mayorDispatchChan <-chan time.Time
	if d.isPatrolActive("mayor_dispatch") {
		interval := mayorDispatchInterval(d.patrolConfig)
		mayorDispatchTicker = time.NewTicker(shortPatrolCheckTick(interval))
		mayorDispatchChan = mayorDispatchTicker.C
		defer mayorDispatchTicker.Stop()
		d.logger.Printf("Mayor dispatch ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup (gt-ima2, gt-gxpwc). triggerMayorDispatch logs
		// its own due-ness decision; the startup log line below just names
		// whether this specific check kicked off a cycle.
		if d.triggerMayorDispatch() {
			d.logger.Printf("Mayor dispatch startup catch-up started a cycle")
		}
	}

	// Start the spec dispatcher ticker if enabled (default off, gt-4k3fj.5).
	var specDispatchTicker *time.Ticker
	var specDispatchChan <-chan time.Time
	if d.isPatrolActive("spec_dispatch") {
		interval := specDispatchInterval(d.patrolConfig)
		specDispatchTicker = time.NewTicker(interval)
		specDispatchChan = specDispatchTicker.C
		defer specDispatchTicker.Stop()
		d.logger.Printf("Spec dispatch ticker started (interval %v)", interval)
	}

	// Start the patrol watchdog ticker if configured. Flags a patrol role
	// (witness, deacon, refinery) whose session is alive but whose last
	// COMPLETED patrol cycle is older than N x its cadence (gt-4z3b7).
	// The ticker is a check cadence — due-ness comes from a persisted
	// last-run time, because a run interval enforced by an in-process ticker
	// resets on every restart (gt-ima2, gt-gxpwc).
	var patrolWatchdogTicker *time.Ticker
	var patrolWatchdogChan <-chan time.Time
	if d.isPatrolActive("patrol_watchdog") {
		interval := patrolWatchdogInterval(d.patrolConfig)
		patrolWatchdogTicker = time.NewTicker(shortPatrolCheckTick(interval))
		patrolWatchdogChan = patrolWatchdogTicker.C
		defer patrolWatchdogTicker.Stop()
		d.logger.Printf("Patrol watchdog ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		// Catch up at startup (gt-ima2, gt-gxpwc).
		d.triggerPatrolWatchdog()
	}

	// No tmux pane-died respawn hooks: a dead session is restarted by the
	// heartbeat through the supervisor, within its budget (gt-4k3fj.3).

	// Initial heartbeat
	d.heartbeat(state)
	startupComplete = true
	if err := d.exitForUpgradeIfRequested(state); err != nil {
		return err
	}

	for {
		select {
		case <-d.ctx.Done():
			d.logger.Println("Daemon context canceled, shutting down")
			return d.shutdown(state)

		case sig := <-sigChan:
			if isLifecycleSignal(sig) {
				// Caught so a stray one cannot end the daemon (gt-4k3fj.3).
				d.logger.Println("Received SIGUSR1: ignored")
			} else if isReloadRestartSignal(sig) {
				// 'gt daemon clear-backoff' still sends SIGUSR2. The restart
				// budget lives in each seat's intent record and is read on
				// every restart, so there is nothing to reload.
				d.logger.Println("Received reload-restart signal: restart budgets are read from the intent records, nothing to reload")
			} else {
				d.logger.Printf("Received signal %v, shutting down", sig)
				return d.shutdown(state)
			}

		case <-doltHealthChan:
			// Dedicated Dolt health check — fast crash detection independent
			// of the 3-minute general heartbeat.
			if !d.isShutdownInProgress() {
				d.ensureDoltServerRunning()
			}

		case <-doltBackupChan:
			// Periodic Dolt filesystem backup — syncs production databases to
			// local backup directory. Fires only when the persisted last run
			// is a full interval old (gt-ima2, gt-gxpwc).
			if !d.isShutdownInProgress() {
				d.triggerDoltBackup()
			}

		case <-jsonlGitBackupChan:
			// Periodic JSONL git backup check — exports issues, scrubs ephemeral
			// data, commits and pushes to git repo. Fires only when the
			// persisted last run is a full interval old (gt-ima2, gt-gxpwc).
			if !d.isShutdownInProgress() {
				d.triggerJsonlGitBackup()
			}

		case <-wispReaperChan:
			// Periodic wisp reaper check — closes stale wisps (abandoned
			// molecule steps, old patrol data) to prevent unbounded table
			// growth (Clown Show audit). Fires only when the persisted last
			// run is a full interval old (gt-ima2, gt-gxpwc).
			if !d.isShutdownInProgress() {
				d.triggerWispReaper()
			}

		case <-doctorDogChan:
			// Doctor dog — comprehensive Dolt health monitor: connectivity, latency,
			// gc, zombie detection, backup staleness, and disk usage checks.
			if !d.isShutdownInProgress() {
				// Reap orphaned test 'dolt sql-server' processes on the same
				// cadence, before the molecule-based health checks run (gt-twil).
				d.cleanupOrphanedDoltServers()
				d.runDoctorDog()
				// Deacon self-probe (gt-jmy3): inject a known event through
				// the deacon's own mail path each cycle and read back
				// whether the previous cycle's probe was acked in time.
				d.runDeaconSelfProbe()
			}

		case <-compactorDogChan:
			// Compactor dog — monitors Dolt commit counts across production
			// databases and escalates over threshold. Fires only when the
			// persisted last run is a full interval old (gt-ima2).
			if !d.isShutdownInProgress() {
				d.triggerCompactorDog()
			}

		case <-checkpointDogChan:
			// Checkpoint dog check — auto-commits WIP changes in active
			// polecat worktrees to prevent data loss from session crashes.
			// Fires only when the persisted last run is a full interval old
			// (gt-ima2, gt-gxpwc).
			if !d.isShutdownInProgress() {
				d.triggerCheckpointDog()
			}

		case <-scheduledMaintenanceChan:
			// Scheduled maintenance — checks if we're in the maintenance window
			// and acts on maintenance.mode (monitor escalates, flatten runs
			// `gt maintain --force`, gc dispatches a dolt_gc('--full') cycle).
			if !d.isShutdownInProgress() {
				d.runScheduledMaintenance()
			}

		case <-mainBranchTestChan:
			// Main branch test runner — periodically runs quality gates on each
			// rig's main branch to catch regressions from merges or direct pushes.
			// Dispatched onto its own goroutine (never awaited here): a cycle can
			// block for up to (60m slot wait + 10m test) per rig, and running it
			// inline froze the heartbeat/reaper/dog ticks below for as long as it
			// waited (gt-uvxy).
			if !d.isShutdownInProgress() {
				d.triggerMainBranchTests()
			}

		case <-scheduledSlingsChan:
			// Scheduled slings — dispatches formula runs on an interval.
			// Each entry is evaluated independently; the decision function
			// decides due-ness, so a coarse tick is sufficient (gt-nj23).
			if !d.isShutdownInProgress() {
				d.triggerScheduledSlings()
			}

		case <-quotaDogChan:
			// Quota dog — scans for rate-limited sessions and automatically
			// rotates credentials to available accounts via keychain swap.
			if !d.isShutdownInProgress() {
				d.runQuotaDog()
			}

		case <-quotaResumeChan:
			// Quota resume — nudges sessions whose own session-limit reset
			// has passed, independent of quota_dog / the account pool
			// (gt-749e).
			if !d.isShutdownInProgress() {
				d.runQuotaResume()
			}

		case <-mayorDispatchChan:
			// Idle-seat dispatch check — nudges the mayor when polecat seats
			// are free and a rig has actionable ready work. Dispatched onto its
			// own goroutine (never awaited here): the cycle shells out to the
			// check and then nudges, and the nudge's wait-idle mode alone can
			// hold for 15s (gt-59o9).
			if !d.isShutdownInProgress() {
				d.triggerMayorDispatch()
			}

		case <-specDispatchChan:
			// Spec dispatcher tick — lints ready spec beads and slings clean
			// ones within the seat budget, on its own goroutine (gt-4k3fj.5).
			if !d.isShutdownInProgress() {
				d.triggerSpecDispatch()
			}

		case <-patrolWatchdogChan:
			// Patrol watchdog — flags a role (witness, deacon, refinery)
			// whose session is alive but whose last completed patrol cycle
			// is stale, escalates, and optionally nudges (gt-4z3b7).
			// Dispatched onto its own goroutine for the same reason
			// mayor_dispatch is: bd + tmux reads plus any escalate/nudge
			// subprocess can take real time.
			if !d.isShutdownInProgress() {
				d.triggerPatrolWatchdog()
			}

		case <-timer.C:
			d.heartbeat(state)
			if err := d.exitForUpgradeIfRequested(state); err != nil {
				return err
			}

			// Fixed recovery interval (no activity-based backoff)
			timer.Reset(d.recoveryHeartbeatInterval())
		}
	}
}

// recoveryHeartbeatInterval returns the config-driven recovery heartbeat interval.
// Normal wake is handled by feed subscription (bd activity --follow).
// The daemon is a safety net for dead sessions, GUPP violations, and orphaned work.
// Default: 3 minutes — fast enough to detect stuck agents promptly.
func (d *Daemon) recoveryHeartbeatInterval() time.Duration {
	return d.loadOperationalConfig().GetDaemonConfig().RecoveryHeartbeatIntervalD()
}

// heartbeat performs one heartbeat cycle.
// The daemon is recovery-focused: it ensures agents are running and detects failures.
// Normal wake is handled by feed subscription (bd activity --follow).
// The daemon is the safety net for edge cases:
// - Dead sessions that need restart
// - Agents with work-on-hook not progressing (GUPP violation)
// - Orphaned work (assigned to dead agents)
func (d *Daemon) heartbeat(state *State) {
	// Skip heartbeat if shutdown is in progress.
	// This prevents the daemon from fighting shutdown by auto-restarting killed agents.
	// The shutdown.lock file is created by gt down before terminating sessions.
	if d.isShutdownInProgress() {
		d.logger.Println("Shutdown in progress, skipping heartbeat")
		return
	}

	// Skip agent management if E-stop is active.
	// The daemon stays alive (to maintain Dolt, etc.) but does NOT
	// restart any agents. This prevents fighting the E-stop by auto-spawning
	// sessions that were intentionally frozen.
	if estop.IsActive(d.config.TownRoot) {
		d.logger.Println("E-STOP active, skipping agent management")
		return
	}

	// Before any dispatch: a heartbeat that starts plugins first would make
	// the daemon busy and never let it restart for an upgrade.
	if d.checkUpgradeRestart(time.Now()) {
		return
	}
	heartbeatWorkFn(d, state)
}

// heartbeatWorkFn is the body of a heartbeat; a test seam.
var heartbeatWorkFn = (*Daemon).heartbeatWork

// heartbeatWork is the recovery work of one heartbeat, run after the
// shutdown, E-stop and upgrade-restart guards in heartbeat.
func (d *Daemon) heartbeatWork(state *State) {
	d.metrics.recordHeartbeat(d.ctx)
	d.logger.Println("Heartbeat starting (recovery-focused)")

	// Invalidate the per-tick rigs cache so this heartbeat re-reads from disk.
	// Within a tick the cache coalesces the ~10 getKnownRigs() call sites into
	// a single read; invalidating here ensures we pick up rigs.json changes
	// between ticks.
	d.invalidateKnownRigsCache()

	// 0a. Reload prefix registry so new/changed rigs get correct session names.
	// Without this, rigs added after daemon startup get the "gt" default prefix,
	// causing ghost sessions like gt-witness instead of ti-witness. (hq-ouz, hq-eqf, hq-3i4)
	if err := session.InitRegistry(d.config.TownRoot); err != nil {
		d.logger.Printf("Warning: failed to reload prefix registry: %v", err)
	}

	// 0b. Kill ghost sessions left over from stale registry (default "gt" prefix).
	d.killDefaultPrefixGhosts()

	// 0. Ensure Dolt server is running (if configured)
	// This must happen before beads operations that depend on Dolt.
	d.ensureDoltServerRunning()

	// 1. Ensure Deacon is running (restart if dead)
	// Check patrol config - can be disabled in mayor/daemon.json
	if d.isPatrolActive("deacon") {
		d.ensureDeaconRunning()
	} else {
		d.logger.Printf("Deacon patrol disabled in config, skipping")
		// Kill leftover deacon/boot sessions from before patrol was disabled.
		// Without this, a stale deacon keeps running its own patrol loop,
		// spawning witnesses and refineries despite daemon config. (hq-2mstj)
		d.killDeaconSessions()
	}

	// 2. Poke Boot for intelligent triage (stuck/nudge/interrupt)
	// Boot handles nuanced "is Deacon responsive" decisions
	// Only run if Deacon patrol is enabled
	if d.isPatrolActive("deacon") {
		d.ensureBootRunning()
	}

	// 3. Direct Deacon heartbeat check (belt-and-suspenders)
	// Boot may not detect all stuck states; this provides a fallback
	// Only run if Deacon patrol is enabled
	if d.isPatrolActive("deacon") {
		d.checkDeaconHeartbeat()
	}

	// 4. Ensure Witnesses are running for all rigs (restart if dead)
	// Check patrol config - can be disabled in mayor/daemon.json
	if d.isPatrolActive("witness") {
		d.ensureWitnessesRunning()
	} else {
		d.logger.Printf("Witness patrol disabled in config, skipping")
		// Kill leftover witness sessions from before patrol was disabled. (hq-2mstj)
		d.killWitnessSessions()
	}

	// 5. Ensure Refineries are running for all rigs (restart if dead)
	// Check patrol config - can be disabled in mayor/daemon.json
	// Pressure-gated: refineries consume API credits, defer when system is loaded.
	if d.isPatrolActive("refinery") {
		if p := d.checkPressure("refinery"); !p.OK {
			d.logger.Printf("Deferring refinery spawn: %s", p.Reason)
		} else {
			d.ensureRefineriesRunning()
		}
	} else {
		d.logger.Printf("Refinery patrol disabled in config, skipping")
		// Kill leftover refinery sessions from before patrol was disabled. (hq-2mstj)
		d.killRefinerySessions()
	}

	// 6. Ensure Mayor is running (restart if dead)
	d.ensureMayorRunning()

	// 6.5. Handle Dog lifecycle: cleanup stuck dogs and dispatch plugins
	// Pressure-gated: dog dispatch spawns new agent sessions.
	if d.isPatrolActive("handler") {
		if p := d.checkPressure("dog"); !p.OK {
			d.logger.Printf("Deferring dog dispatch: %s", p.Reason)
			// Still run cleanup phases (stuck/stale/idle) — only skip dispatch
			d.handleDogsCleanupOnly()
		} else {
			d.handleDogs()
		}
	} else {
		d.logger.Printf("Handler patrol disabled in config, skipping")
	}

	// 12. Check polecat session health (proactive crash detection)
	// This validates tmux sessions are still alive for polecats with work-on-hook
	d.checkPolecatSessionHealth()

	// 12b. Reap idle polecat sessions to prevent API slot burn.
	// Polecats transition to IDLE after gt done but sessions stay alive.
	// Kill sessions that have been idle longer than the configured threshold.
	d.reapIdlePolecats()

	// 13. Clean up orphaned claude subagent processes (memory leak prevention)
	// These are Task tool subagents that didn't clean up after completion.
	// This is a safety net - Deacon patrol also does this more frequently.
	d.cleanupOrphanedProcesses()

	// 13. Prune stale local polecat tracking branches across all rig clones.
	// When polecats push branches to origin, other clones create local tracking
	// branches via git fetch. After merge, remote branches are deleted but local
	// branches persist indefinitely. This cleans them up periodically.
	d.pruneStaleBranches()

	// 14. Dispatch scheduled work (capacity-controlled polecat dispatch).
	// Shells out to `gt scheduler run` to avoid circular import between daemon and cmd.
	// Pressure-gated: polecats are the primary resource consumers.
	if p := d.checkPressure("polecat"); !p.OK {
		d.logger.Printf("Deferring polecat dispatch: %s", p.Reason)
	} else {
		d.dispatchQueuedWork()
	}

	// 15. Rotate oversized Dolt logs (copytruncate for child process fds).
	// daemon.log uses lumberjack for automatic rotation; this handles Dolt server logs.
	d.rotateOversizedLogs()

	// Update state
	state.LastHeartbeat = time.Now()
	state.HeartbeatCount++
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	d.logger.Printf("Heartbeat complete (#%d)", state.HeartbeatCount)
}

// rotateOversizedLogs checks Dolt server log files and rotates any that exceed
// the size threshold. Uses copytruncate which is safe for logs held open by
// child processes. Runs every heartbeat but is cheap (just stat calls).
func (d *Daemon) rotateOversizedLogs() {
	result := RotateLogs(d.config.TownRoot)
	for _, path := range result.Rotated {
		d.logger.Printf("log_rotation: rotated %s", path)
	}
	for _, err := range result.Errors {
		d.logger.Printf("log_rotation: error: %v", err)
	}
}

// ensureDoltServerRunning ensures the Dolt SQL server is running if configured.
// This provides the backend for beads database access in server mode.
// Option B throttling: pours a mol-dog-doctor molecule only when health check
// warnings are detected, with a 5-minute cooldown to avoid wisp spam.
func (d *Daemon) ensureDoltServerRunning() {
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return
	}

	if err := d.doltServer.EnsureRunning(); err != nil {
		d.logger.Printf("Error ensuring Dolt server is running: %v", err)
	}

	// Option B throttling: pour mol-dog-doctor only on anomaly with cooldown.
	if warnings := d.doltServer.LastWarnings(); len(warnings) > 0 {
		if time.Since(d.lastDoctorMolTime) >= doctorMolCooldown {
			d.lastDoctorMolTime = time.Now()
			go d.pourDoctorMolecule(warnings)
		}
	}

	// Update OTel gauges with the latest Dolt health snapshot.
	if d.metrics != nil {
		h := doltserver.GetHealthMetrics(d.config.TownRoot)
		d.metrics.updateDoltHealth(
			int64(h.Connections),
			int64(h.MaxConnections),
			float64(h.QueryLatency.Milliseconds()),
			h.DiskUsageBytes,
			h.Healthy,
		)
	}
}

// ensureDoltServerUp brings the Dolt server up and reports a failure to do so.
//
// ensureDoltServerRunning is the heartbeat's step 0 and does more than this: it
// pours the doctor molecule and reads the OTel gauges from state the heartbeat
// goroutine owns. A patrol cycle running on its own goroutine takes this bare
// bring-up instead of reaching into that state (gt-ox6c).
func (d *Daemon) ensureDoltServerUp() error {
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return nil
	}
	return d.doltServer.EnsureRunning()
}

// pourDoctorMolecule creates a mol-dog-doctor molecule to track a health anomaly.
// Runs asynchronously — molecule lifecycle is observability, not control flow.
func (d *Daemon) pourDoctorMolecule(warnings []string) {
	mol := d.pourDogMolecule(constants.MolDogDoctor, map[string]string{
		"port": strconv.Itoa(d.doltServer.config.Port),
	})
	defer mol.close()

	// Step 1: probe — connectivity was already checked (we got here because it passed).
	mol.closeStep("probe")

	// Step 2: inspect — resource checks produced the warnings.
	mol.closeStep("inspect")

	// Step 3: report — log the warning summary.
	summary := strings.Join(warnings, "; ")
	d.logger.Printf("Doctor molecule: %d warning(s): %s", len(warnings), summary)
	mol.closeStep("report")
}

// checkAllRigsDolt verifies all rigs are using the Dolt backend.
func (d *Daemon) checkAllRigsDolt() error {
	var problems []string

	// Check town-level beads
	townBeadsDir := filepath.Join(d.config.TownRoot, ".beads")
	if backend := readBeadsBackend(townBeadsDir); backend != "" && backend != "dolt" {
		problems = append(problems, fmt.Sprintf(
			"Rig %q is using %s backend.\n  Gas Town requires Dolt. Run: cd %s && bd migrate dolt",
			"town-root", backend, d.config.TownRoot))
	}

	// Check each registered rig
	for _, rigName := range d.getKnownRigs() {
		rigBeadsDir := filepath.Join(d.config.TownRoot, rigName, "mayor", "rig", ".beads")
		if backend := readBeadsBackend(rigBeadsDir); backend != "" && backend != "dolt" {
			rigPath := filepath.Join(d.config.TownRoot, rigName)
			problems = append(problems, fmt.Sprintf(
				"Rig %q is using %s backend.\n  Gas Town requires Dolt. Run: cd %s && bd migrate dolt",
				rigName, backend, rigPath))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("daemon startup blocked: %d rig(s) not on Dolt backend\n\n  %s",
		len(problems), strings.Join(problems, "\n\n  "))
}

// readBeadsBackend reads the backend field from metadata.json in a beads directory.
// Returns empty string if the directory or metadata doesn't exist.
func readBeadsBackend(beadsDir string) string {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return ""
	}

	var metadata struct {
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return ""
	}

	return metadata.Backend
}

type beadsDBAccessor interface {
	DB() *sql.DB
}

// bdSchemaLevel returns the schema level the bd on PATH migrates a database
// to (bd version --json db_schema_version). Zero with a nil error means bd
// did not report one: a build from before the machine surface. Tests
// replace it.
var bdSchemaLevel = func(ctx context.Context, townRoot string) (int, error) {
	stdout, stderr, err := deps.NewBDProcessRunner(townRoot)(ctx, nil, "version", "--json")
	if err != nil {
		return 0, fmt.Errorf("bd version --json: %w (%s)", err, strings.TrimSpace(string(stderr)))
	}
	info, err := deps.ParseBDVersionJSON(stdout)
	if err != nil {
		return 0, err
	}
	return info.DBSchemaVersion, nil
}

// verifyBeadsStores is the daemon's startup gate on opened stores: bd must
// report the schema level it migrates to, and every store must be at that
// level with a journal bd can tail. Every read goes through bd (probeFor;
// gt-7iwy0.2), not the store handles. On refusal it closes every store.
func verifyBeadsStores(ctx context.Context, logger *log.Logger, townRoot string, stores map[string]beadsdk.Storage, probeFor func(townRoot, name string) (storeProbe, error)) error {
	bdSchema, err := bdSchemaLevel(ctx, townRoot)
	if err == nil && bdSchema <= 0 {
		err = fmt.Errorf("bd version --json reports no db_schema_version (a bd build from before the machine surface); install the beads fork's bd with make safe-install")
	}
	if err != nil {
		closeBeadsStores(logger, stores)
		return fmt.Errorf("daemon startup blocked: cannot read bd's schema level: %w", err)
	}
	names := make([]string, 0, len(stores))
	for name, store := range stores {
		if store != nil {
			names = append(names, name)
		}
	}
	warn := func(w string) {
		if logger != nil {
			logger.Printf("Convoy: %s", w)
		}
	}
	if err := checkBeadsStoreCompatibility(ctx, townRoot, names, bdSchema, probeFor, warn); err != nil {
		closeBeadsStores(logger, stores)
		return err
	}
	return nil
}

// storeProbe is what the compatibility check reads about one store, all
// through bd.
type storeProbe interface {
	// SchemaLevel is the database's highest applied migration.
	SchemaLevel(ctx context.Context) (int, error)
	// EventsTail reads the events journal; the check reads one record.
	EventsTail(since int64, limit int) (*beads.EventsPage, error)
	// JournalConfig is events-journal as the store's config.yaml sets it,
	// read without gastown's BD_EVENTS_JOURNAL override.
	JournalConfig() (string, error)
}

// bdStoreProbe reads a store's compatibility facts through the bd on PATH,
// pinned to the store's beads directory.
type bdStoreProbe struct {
	dir    string
	run    deps.BDRunner
	client *beads.Beads
}

// newBDStoreProbe returns the probe for the named store ("hq" or a rig).
func newBDStoreProbe(townRoot, name string) (storeProbe, error) {
	dir := doltserver.FindRigBeadsDir(townRoot, name)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for store %q", name)
	}
	return &bdStoreProbe{dir: dir, run: deps.NewBDProcessRunner(filepath.Dir(dir)), client: beads.NewWithBeadsDir(townRoot, dir)}, nil
}

// pinned runs bd with BEADS_DIR set to the store's directory and
// BD_EVENTS_JOURNAL cleared, so bd reports the workspace's own settings.
func (p *bdStoreProbe) pinned(ctx context.Context, extraEnv []string, args ...string) ([]byte, []byte, error) {
	env := append([]string{"BEADS_DIR=" + p.dir, "BD_EVENTS_JOURNAL="}, extraEnv...)
	return p.run(ctx, env, args...)
}

func (p *bdStoreProbe) SchemaLevel(ctx context.Context) (int, error) {
	return deps.ReadDBSchemaLevel(ctx, p.pinned)
}

func (p *bdStoreProbe) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	return p.client.EventsTail(since, limit)
}

func (p *bdStoreProbe) JournalConfig() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), beads.ResolveSubprocessTimeout())
	defer cancel()
	stdout, stderr, err := p.pinned(ctx, []string{"BD_MACHINE=1"}, "config", "get", "events-journal", "--json")
	if err != nil {
		return "", fmt.Errorf("bd config get events-journal: %w (%s)", err, strings.TrimSpace(string(stderr)))
	}
	return parseConfigGetValue(stdout)
}

// parseConfigGetValue reads the value from bd config get --json: the
// machine envelope's data, or the legacy {key, value, location} object.
func parseConfigGetValue(out []byte) (string, error) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	body := bytes.TrimSpace(out)
	if json.Unmarshal(body, &env) == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		body = env.Data
	}
	var kv struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(body, &kv); err != nil || kv.Value == nil {
		return "", fmt.Errorf("bd config get --json: no value in %q", util.FirstLine(string(out)))
	}
	return strings.TrimSpace(*kv.Value), nil
}

// checkBeadsStoreCompatibility refuses stores whose database schema level
// is not the level bd migrates to (bdSchema), or whose events journal bd
// cannot read. It compares schema integers read from schema_migrations, the
// table bd advances (B5-02). A journal left off in a store's config.yaml is
// passed to warn, not refused: gastown's own bd calls journal regardless.
func checkBeadsStoreCompatibility(ctx context.Context, townRoot string, names []string, bdSchema int, probeFor func(townRoot, name string) (storeProbe, error), warn func(string)) error {
	if len(names) == 0 {
		return nil
	}
	names = append([]string(nil), names...)
	sort.Strings(names)

	var problems []string
	for _, name := range names {
		problem, warning := checkSingleBeadsStoreCompatibility(ctx, townRoot, name, bdSchema, probeFor)
		if problem != "" {
			problems = append(problems, problem)
		}
		if warning != "" && warn != nil {
			warn(warning)
		}
	}
	if len(problems) == 0 {
		return nil
	}

	remediation := "Install the beads fork's bd at the database's schema level (make safe-install in the beads repository), check `gt doctor`, then retry `gt daemon start`."

	return fmt.Errorf("daemon startup blocked: incompatible beads workspace / gt binary combination\n\n  %s\n\n%s",
		strings.Join(problems, "\n  "), remediation)
}

func checkSingleBeadsStoreCompatibility(ctx context.Context, townRoot, name string, bdSchema int, probeFor func(townRoot, name string) (storeProbe, error)) (problem, warning string) {
	label := displayBeadsStoreName(name)
	probe, err := probeFor(townRoot, name)
	if err != nil {
		return fmt.Sprintf("%s: %v", label, err), ""
	}

	var reasons []string
	level, err := probe.SchemaLevel(ctx)
	if p := schemaLevelProblem(level, err, bdSchema); p != "" {
		reasons = append(reasons, p)
	}
	if _, err := probe.EventsTail(0, 1); err != nil {
		// A journal pruned below seq 1 is readable; bd says so with a
		// window that holds records. Anything else is a failed probe.
		var trunc *beads.EventsTruncatedError
		if !errors.As(err, &trunc) || trunc.Floor <= 0 || trunc.Head < trunc.Floor {
			reasons = append(reasons, fmt.Sprintf("events journal probe failed: %v", err))
		}
	}
	if len(reasons) > 0 {
		problem = fmt.Sprintf("%s: %s", label, strings.Join(reasons, "; "))
	}

	switch v, err := probe.JournalConfig(); {
	case err != nil:
		warning = fmt.Sprintf("%s: cannot read events-journal from its config (%v); bd calls made outside gt may not journal, so convoy closes they make wait for the stranded scan. Check with bd config get events-journal; enable with bd config set events-journal true", label, err)
	case v != "true":
		warning = fmt.Sprintf("%s: events journal is off in its config.yaml (events-journal=%q): closes made by bd calls outside gt are not journaled and wait for the stranded scan. Enable with bd config set events-journal true in that beads directory and commit the config.yaml", label, v)
	}
	return problem, warning
}

// schemaLevelProblem is the verdict on one store: "" when the database is at
// exactly bd's level, otherwise the reason naming both integers. A failed
// read or an unknown bd level is a problem, never a pass.
func schemaLevelProblem(level int, readErr error, bdSchema int) string {
	switch {
	case readErr != nil:
		return fmt.Sprintf("cannot read schema_migrations: %v", readErr)
	case bdSchema <= 0:
		return fmt.Sprintf("bd schema level unknown (database schema %d); bd version --json reports no db_schema_version", level)
	case level != bdSchema:
		return fmt.Sprintf("database schema %d does not match bd schema %d", level, bdSchema)
	}
	return ""
}

func displayBeadsStoreName(name string) string {
	if name == "hq" {
		return "town-root beads store"
	}
	return fmt.Sprintf("rig %q beads store", name)
}

func closeBeadsStores(logger *log.Logger, stores map[string]beadsdk.Storage) {
	for name, store := range stores {
		if store == nil {
			continue
		}
		if err := store.Close(); err != nil {
			if logger != nil {
				logger.Printf("Convoy: error closing beads store (%s): %v", name, err)
			}
			continue
		}
		if logger != nil {
			logger.Printf("Convoy: closed beads store (%s)", name)
		}
	}
}

// DeaconRole is the role name for the Deacon's handoff bead.
const DeaconRole = "deacon"

// getDeaconSessionName returns the Deacon session name for the daemon's town.
func (d *Daemon) getDeaconSessionName() string {
	return session.DeaconSessionName()
}

// bootSpawnCooldown returns the config-driven boot spawn cooldown.
func (d *Daemon) bootSpawnCooldown() time.Duration {
	return d.loadOperationalConfig().GetDaemonConfig().BootSpawnCooldownD()
}

// bootSessionStart returns when Boot's current session started, and whether it
// could be dated. tmux owns the start, so it survives a daemon restart; the
// spawn stamp covers a session tmux cannot date (gt-w28o).
func (d *Daemon) bootSessionStart() (time.Time, bool) {
	if created, err := d.tmux.GetSessionCreatedTime(session.BootSessionName()); err == nil && !created.IsZero() {
		return created, true
	}
	if !d.bootLastSpawned.IsZero() {
		return d.bootLastSpawned, true
	}
	return time.Time{}, false
}

// bootSessionWorking reports whether a live Boot session should be left alone,
// with a phrase naming the reason for the log. Boot idles at its prompt after a
// triage run, so its age alone cannot tell a working session from a spent one:
// a session whose triage already completed, or one that cannot be dated, is
// reaped; only a session with no completion stamp gets the turn budget (gt-w28o).
func (d *Daemon) bootSessionWorking(b *boot.Boot) (string, bool) {
	start, dated := d.bootSessionStart()
	if !dated {
		return "alive but undated", false
	}
	if status, err := b.LoadStatus(); err == nil && status.CompletedAt.After(start) {
		return fmt.Sprintf("finished its triage run %s ago", time.Since(status.CompletedAt).Round(time.Second)), false
	}
	budget := d.loadOperationalConfig().GetDaemonConfig().BootTurnBudgetD()
	age := time.Since(start)
	if age >= budget {
		return fmt.Sprintf("alive %s, past the turn budget (%s)", age.Round(time.Second), budget), false
	}
	return fmt.Sprintf("alive %s, within the turn budget (%s)", age.Round(time.Second), budget), true
}

// ensureBootRunning spawns Boot to triage the Deacon. Boot is a fresh-each-tick
// watchdog that decides whether to start/wake/nudge the Deacon, centralizing the
// "when to wake" decision in an agent; with no tmux it falls back to mechanical
// checks.
func (d *Daemon) ensureBootRunning() {
	// Idle guard: skip if Deacon is healthy AND no beads are actively in flight.
	//
	// Boot's job is to triage a stuck or unresponsive Deacon and to flag stuck
	// in_progress/hooked work. If Deacon has written a fresh heartbeat and no
	// beads are in_progress or hooked, there is nothing to triage.
	//
	// We deliberately do NOT update bootLastSpawned on an idle skip: the cooldown
	// is about rate-limiting real spawns; the idle check should re-run every
	// heartbeat so Boot fires promptly when work actually appears.
	hb := deacon.ReadHeartbeat(d.config.TownRoot)
	if hb != nil && hb.IsFresh() && !d.hasActiveWork() {
		d.logger.Println("Boot spawn skipped: Deacon is healthy and no active work in flight")
		return
	}

	b := boot.New(d.config.TownRoot)

	// Idle suppression: if Boot's last run found deacon healthy ("nothing"),
	// suppress spawning for longer to avoid burning API calls. (fixes gt-qu883c)
	idleSuppression := d.loadOperationalConfig().GetDaemonConfig().BootIdleSuppressionD()
	if status, err := b.LoadStatus(); err == nil && status.LastAction == "nothing" {
		if !status.CompletedAt.IsZero() && time.Since(status.CompletedAt) < idleSuppression {
			d.logger.Printf("Boot last reported 'nothing' %s ago, within idle suppression (%s), skipping",
				time.Since(status.CompletedAt).Round(time.Second), idleSuppression)
			return
		}
	}

	// Check for degraded mode
	degraded := os.Getenv("GT_DEGRADED") == "true"
	if degraded || !d.tmux.IsAvailable() {
		// In degraded mode, run mechanical triage directly
		d.logger.Println("Degraded mode: running mechanical Boot triage")
		d.runDegradedBootTriage(b)
		return
	}

	// Mechanical boot mode (the default, gt-fo2k): run `gt boot triage`
	// ourselves instead of spawning a Boot agent to run it for us. It is
	// the same command the agent ran, so warrants, the shutdown check and
	// the status file all behave as before — minus the ~22k-token session.
	if d.bootUsesMechanicalTriage() {
		d.runMechanicalBootTriage()
		return
	}

	// Cooldown gate: a Boot agent spawn costs a ~23k-token prefill, so one that
	// just ran skips this heartbeat (fixes #2084). Mechanical, degraded and
	// idle-check triage are in-process and pay no prefill, which is why they
	// return above rather than through this gate.
	if !d.bootLastSpawned.IsZero() && time.Since(d.bootLastSpawned) < d.bootSpawnCooldown() {
		d.logger.Printf("Boot spawned %s ago, within cooldown (%s), skipping",
			time.Since(d.bootLastSpawned).Round(time.Second), d.bootSpawnCooldown())
		return
	}

	// Idle check: run gt-idle-check to see if the system needs waking.
	// If idle (all rigs parked, no polecats, deacon alive), skip the expensive
	// Claude Boot session and use degraded mechanical triage instead.
	// This saves ~480 Claude sessions/day when Gas Town is not in active use.
	idleCheckBin := filepath.Join(d.config.TownRoot, "bin", "gt-idle-check")
	if _, err := os.Stat(idleCheckBin); err == nil {
		//nolint:gosec // G204: path is constructed from config
		cmd := exec.Command(idleCheckBin)
		cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s:%s",
			filepath.Join(d.config.TownRoot, "bin"), os.Getenv("PATH")))
		if output, err := cmd.CombinedOutput(); err == nil {
			// Exit 0 = idle, use degraded triage (zero tokens)
			d.runDegradedBootTriage(b)
			return
		} else {
			// Exit 1 = needs waking, proceed to full Claude Boot
			d.logger.Printf("Idle check: waking — %s", strings.TrimSpace(string(output)))
		}
	}

	// Leave a Boot that is still working alone (gt-w28o): its turn outlasts a
	// heartbeat on a slow model, and killing it mid-prefill so its replacement
	// pays the same prefill again is the tax this guard exists to stop. A
	// session the guard does not keep falls through to be reaped, which is how
	// Boot reaches the Deacon again.
	if alive, err := d.tmux.HasSession(session.BootSessionName()); err == nil && alive {
		reason, working := d.bootSessionWorking(b)
		if working {
			d.logger.Printf("Boot session %s %s, leaving it to finish", session.BootSessionName(), reason)
			return
		}
		d.logger.Printf("Boot session %s %s, reaping", session.BootSessionName(), reason)
	}

	// Spawn Boot in a fresh tmux session
	d.logger.Println("Spawning Boot for triage...")
	if err := d.spawnBoot(b); err != nil {
		d.logger.Printf("Error spawning Boot: %v, falling back to direct Deacon check", err)
		// Fallback: ensure Deacon is running directly
		d.ensureDeaconRunning()
		return
	}

	d.bootLastSpawned = time.Now()
	d.logger.Println("Boot spawned successfully")
}

// hasActiveWork returns true if any bead store has in_progress or hooked beads.
// These are the only states Boot can meaningfully act on: in_progress work may be
// stuck, and hooked work is waiting on a polecat that may have died.
//
// Returns true conservatively on error or when no stores are available, so the
// caller falls through to spawn Boot rather than suppressing it incorrectly.
func (d *Daemon) hasActiveWork() bool {
	if len(d.beadsStores) == 0 {
		// No stores open — cannot inspect; let Boot run to be safe.
		return true
	}

	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()

	for name, store := range d.beadsStores {
		for _, rawStatus := range []string{"in_progress"} {
			s := beadsdk.Status(rawStatus)
			filter := beadsdk.IssueFilter{Status: &s, Limit: 1}
			issues, err := store.SearchIssues(ctx, "", filter)
			if err != nil {
				d.logger.Printf("hasActiveWork: %s/%s query failed: %v — assuming work present",
					name, rawStatus, err)
				return true // conservative: don't suppress Boot on query failure
			}
			if len(issues) > 0 {
				return true
			}
		}
	}
	return false
}

// runMechanicalBootTriage runs `gt boot triage` as a subprocess of the
// daemon (gt-fo2k). It is exactly the command the Boot agent used to run
// on our behalf, so warrant execution, the shutdown check and the status
// file that drives idle suppression all behave as before; the difference
// is no Claude session, no ~22k-token prefill, and no tmux window. The
// installed gt is used (os.Executable), never a PATH lookup, so a stale
// PATH cannot pick a different build than the daemon itself.
func (d *Daemon) runMechanicalBootTriage() {
	// One triage at a time, and never on the heartbeat goroutine: the old
	// agent spawn returned immediately, and a wedged `gt boot triage` must
	// not stall plugin dispatch or the deacon checks behind it.
	if !d.bootTriageInFlight.CompareAndSwap(false, true) {
		d.logger.Println("Boot: mechanical triage still running, skipping")
		return
	}
	// Stamp the attempt at start, so the daemon's record of Boot's last run
	// does not depend on the triage finishing.
	d.bootLastSpawned = time.Now()
	exe, err := bootTriageExecutable()
	if err != nil {
		d.bootTriageInFlight.Store(false)
		d.logger.Printf("Boot: cannot resolve gt binary for mechanical triage: %v; falling back to direct Deacon check", err)
		d.ensureDeaconRunning()
		return
	}
	if strings.HasSuffix(exe, ".test") {
		// Under `go test` the executable is the test binary; exec'ing it
		// with "boot triage" would run the whole suite again, recursively.
		d.bootTriageInFlight.Store(false)
		d.logger.Printf("Boot: mechanical triage skipped: executable %s is a test binary", filepath.Base(exe))
		return
	}
	townRoot := d.config.TownRoot
	go func() {
		defer d.bootTriageInFlight.Store(false)
		ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "boot", "triage") //nolint:gosec // G204: our own binary, fixed args
		cmd.Dir = filepath.Join(townRoot, "deacon")
		cmd.Env = append(os.Environ(), "GT_ROOT="+townRoot, "GT_TOWN_ROOT="+townRoot, "GT_ROLE=deacon/boot", "BD_ACTOR=boot")
		util.SetProcessGroup(cmd) // its Cancel hook kills the whole group on timeout
		out, err := cmd.CombinedOutput()
		summary := strings.TrimSpace(string(out))
		if len(summary) > 400 {
			summary = summary[len(summary)-400:]
		}
		if err != nil {
			d.logger.Printf("Boot: mechanical triage failed (%v): %s", err, summary)
			return
		}
		d.logger.Printf("Boot: mechanical triage: %s", summary)
	}()
}

// bootTriageExecutable resolves the gt binary that runs `boot triage` in
// mechanical mode. A variable so tests can point it at a stub.
var bootTriageExecutable = os.Executable

// bootUsesMechanicalTriage reports whether Boot triage runs in-process
// (operational.daemon.boot_mode unset or "mechanical") rather than as a
// spawned Boot agent session ("agent").
func (d *Daemon) bootUsesMechanicalTriage() bool {
	return d.loadOperationalConfig().GetDaemonConfig().BootModeValue() == agentconfig.BootModeMechanical
}

// runDegradedBootTriage performs mechanical Boot logic without AI reasoning.
func (d *Daemon) runDegradedBootTriage(b *boot.Boot) {
	startTime := time.Now()
	status := &boot.Status{
		StartedAt: startTime,
	}

	// Simple check: is Deacon session alive?
	hasDeacon, err := d.tmux.HasSession(d.getDeaconSessionName())
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		status.LastAction = "error"
		status.Error = err.Error()
	} else if !hasDeacon {
		d.logger.Println("Deacon not running, starting...")
		d.ensureDeaconRunning()
		status.LastAction = "start"
		status.Target = "deacon"
	} else {
		status.LastAction = "nothing"
	}

	status.CompletedAt = time.Now()

	if err := b.SaveStatus(status); err != nil {
		d.logger.Printf("Warning: failed to save Boot status: %v", err)
	}
}

// deaconSeat is the Deacon's seat.
var deaconSeat = supervisor.SeatFor("", constants.RoleDeacon, "")

// ensureDeaconRunning restarts the Deacon when the liveness function says it
// is dead: no session, or a session whose agent process is gone. An Unknown
// verdict is left alone (G1-09), and the restart goes through the
// supervisor, which enforces pause, e-stop and the persisted restart budget
// in place of the old in-memory backoff.
func (d *Daemon) ensureDeaconRunning() {
	res := d.assessSeat(deaconSeat, liveness.Input{Session: d.getDeaconSessionName()})
	switch res.Verdict {
	case liveness.Unknown:
		d.logger.Printf("Deacon liveness unknown (%v); not restarting this tick", res.Err)
		return
	case liveness.Alive, liveness.Stalled:
		// Running. A stall is checkDeaconHeartbeat's to act on.
		return
	}

	if err := d.sup().Restart(deaconSeat, "deacon "+res.Reason, "daemon/ensure-deacon"); err != nil {
		if errors.Is(err, supervisor.ErrRefused) {
			d.logger.Printf("Not starting Deacon: %v", err)
		} else {
			d.logger.Printf("Error starting Deacon: %v", err)
		}
		return
	}

	d.metrics.recordRestart(d.ctx, "deacon")
	telemetry.RecordDaemonRestart(d.ctx, "deacon")
	d.logger.Println("Deacon started successfully")
}

// spawnBoot starts a fresh Boot session (boot.Boot.Spawn replaces any live
// one), or runs spawnBootFn when a test set one.
func (d *Daemon) spawnBoot(b *boot.Boot) error {
	if d.spawnBootFn != nil {
		return d.spawnBootFn(b)
	}
	return b.Spawn("")
}

// startDeacon starts the Deacon session through deacon.Manager, or through
// startDeaconFn when a test set one.
func (d *Daemon) startDeacon() error {
	if d.startDeaconFn != nil {
		return d.startDeaconFn()
	}
	return deacon.NewManager(d.config.TownRoot).Start("")
}

// checkDeaconHeartbeat judges whether a running Deacon is making progress.
// It is a belt-and-suspenders fallback behind Boot.
//
// Progress is measured by change between two liveness samples persisted in
// the Deacon's intent record: its heartbeat cycle number (never the file's
// timestamp, which the heartbeat poller refreshes on a timer, gt-t3cw), its
// pane's work region and its transcript. Because the previous sample lives on
// disk, a daemon restarted every few minutes still accumulates stall evidence
// (G1-04), and a first sample is always a baseline, never a verdict.
//
// Two tiers: quiet for HeartbeatStaleThreshold (5m) with work in flight earns
// a nudge; Stalled at HeartbeatVeryStaleThreshold (20m, longer than the
// patrol's await-signal backoff) earns a restart through the supervisor.
func (d *Daemon) checkDeaconHeartbeat() {
	sessionName := d.getDeaconSessionName()
	in := liveness.Input{Session: sessionName, StallAfter: deacon.HeartbeatVeryStaleThreshold}
	if hb := deacon.ReadHeartbeat(d.config.TownRoot); hb != nil {
		in.Heartbeat = &liveness.Heartbeat{Cycle: hb.Cycle}
	}
	res := d.assessSeat(deaconSeat, in)

	switch res.Verdict {
	case liveness.Unknown:
		d.logger.Printf("Deacon liveness unknown (%v); skipping the progress check", res.Err)
		return
	case liveness.Dead:
		// ensureDeaconRunning, earlier in this heartbeat, owns a dead Deacon.
		return
	case liveness.Stalled:
		d.restartStuckDeacon(sessionName, fmt.Sprintf("no progress for %s", res.QuietFor.Round(time.Minute)))
		return
	}

	if res.QuietFor < deacon.HeartbeatStaleThreshold {
		return
	}

	// Quiet but not stalled - nudge to wake up (unless idle).
	//
	// Idle guard: skip the nudge if no beads are actively in flight. When the
	// Deacon is sleeping in an await-signal backoff, a nudge interrupts the
	// backoff for no reason; it will wake at its next timeout. Conservative:
	// on store errors hasActiveWork returns true, so the nudge fires. See also
	// runtime/runtime.go: the session-started nudge was removed for the same
	// reason.
	if !d.hasActiveWork() {
		d.logger.Println("Deacon nudge skipped: no active work in flight, await-signal will fire naturally")
		return
	}
	d.logger.Printf("Deacon quiet for %s (no progress evidence changed) - nudging session", res.QuietFor.Round(time.Minute))
	if err := d.tmux.NudgeSession(sessionName, "HEALTH_CHECK: heartbeat stale, respond to confirm responsiveness"); err != nil {
		d.logger.Printf("Error nudging stuck Deacon: %v", err)
	}
}

// restartStuckDeacon restarts a stalled Deacon through the supervisor.
// Notifies via gt-notify (zero token cost) if the notify script exists.
func (d *Daemon) restartStuckDeacon(sessionName, reason string) {
	// Distinguish a usage-limit pause from a true stall. If Claude is sitting
	// at a rate-limit prompt its progress stops, looking identical to a
	// stall, but a restart won't help (the new session hits the same limit)
	// and would spend the restart budget. quota_dog rotates accounts.
	if pane, err := d.tmux.CapturePane(sessionName, 30); err == nil && IsClaudeUsageLimit(pane) {
		d.logger.Printf("Deacon paused — Claude usage-limit detected, not restarting (quota_dog will rotate accounts). Reason: %s", reason)
		return
	}

	d.logger.Printf("STUCK DEACON: %s, session %s needs restart", reason, sessionName)
	if err := d.sup().Restart(deaconSeat, reason, "daemon/deacon-heartbeat"); err != nil {
		if errors.Is(err, supervisor.ErrRefused) {
			d.logger.Printf("Not restarting stuck Deacon: %v", err)
			return
		}
		d.logger.Printf("Deacon restart FAILED: %v", err)
		d.notifySlack("admin", "critical", fmt.Sprintf("Deacon restart FAILED: %v. Reason: %s", err, reason))
		return
	}

	d.metrics.recordRestart(d.ctx, "deacon")
	telemetry.RecordDaemonRestart(d.ctx, "deacon")
	d.logger.Printf("Deacon restarted: %s", reason)
	d.notifySlack("admin", "high", fmt.Sprintf("Deacon was stuck (%s) — auto-restarted", reason))
}

// notifySlack sends a notification via gt-notify (zero token cost).
// Channel: "admin" or "status". Priority: "critical", "high", "info", "success".
// Silently fails if gt-notify is not found — notification is best-effort.
func (d *Daemon) notifySlack(channel, priority, message string) {
	notifyBin := filepath.Join(d.config.TownRoot, "bin", "gt-notify")
	if _, err := os.Stat(notifyBin); err != nil {
		d.logger.Printf("Stuck-agent-dog: gt-notify not found at %s, skipping notification", notifyBin)
		return
	}

	//nolint:gosec // G204: args are constructed internally
	cmd := exec.Command(notifyBin, "--channel", channel, "--priority", priority, message)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s:%s", filepath.Join(d.config.TownRoot, "bin"), os.Getenv("PATH")))
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("Stuck-agent-dog: gt-notify failed: %v (output: %s)", err, string(output))
	}
}

// ensureWitnessesRunning ensures witnesses are running for configured rigs.
// Called on each heartbeat to maintain witness patrol loops.
// Respects the rigs filter in daemon.json patrol config.
func (d *Daemon) ensureWitnessesRunning() {
	rigs := d.getPatrolRigs("witness")
	d.rigPool.runPerRig(d.ctx, rigs, func(ctx context.Context, rigName string) error {
		d.ensureWitnessRunning(rigName)
		return nil
	})
}

// hasPendingEvents checks if there are pending .event files in the given channel directory.
// Used to gate agent spawning: don't burn API credits starting a Claude session when
// there's nothing to process. The agent's await-event handles the actual consumption.
// rig scopes the check for per-rig channels (events/<channel>/<rig>/); it is
// ignored for town-global channels.
func (d *Daemon) hasPendingEvents(channel, rig string) bool {
	eventDir := channelevents.Dir(d.config.TownRoot, channel, rig)
	entries, err := os.ReadDir(eventDir)
	if err != nil {
		return false // Directory doesn't exist or unreadable = no pending events
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".event") {
			return true
		}
	}
	return false
}

// ensureWitnessRunning keeps the witness for a rig running: the liveness
// function decides, and a dead witness is restarted through the supervisor.
// In a rig that is not operational (docked/parked) a leftover witness is
// killed through the supervisor instead (hq-snx61).
func (d *Daemon) ensureWitnessRunning(rigName string) {
	seat := supervisor.SeatFor(rigName, constants.RoleWitness, "")
	if operational, reason := d.isRigOperational(rigName); !operational {
		d.logger.Printf("Skipping witness auto-start for %s: %s", rigName, reason)
		d.killLeftover(seat, "rig "+reason, "daemon/rig-state")
		return
	}

	// NOTE: no stall restart for witnesses (serial killer bug): an idle
	// witness legitimately produces no output while it waits for work.
	res := d.assessSeat(seat, liveness.Input{})
	switch res.Verdict {
	case liveness.Unknown:
		d.logger.Printf("Witness for %s: liveness unknown (%v); not acting this tick", rigName, res.Err)
		return
	case liveness.Alive, liveness.Stalled:
		d.logger.Printf("Witness for %s already running, skipping spawn", rigName)
		// "Running" is decided by session and process existence, which a
		// session that consumes no input also satisfies. Probe it, so a
		// wedged witness is not skipped indefinitely (gt-eigw).
		d.probeRunningWitness(rigName)
		return
	}

	if err := d.sup().Restart(seat, "witness "+res.Reason, "daemon/ensure-witness"); err != nil {
		d.logStartOutcome("witness", rigName, err)
		return
	}
	d.metrics.recordRestart(d.ctx, "witness")
	telemetry.RecordDaemonRestart(d.ctx, "witness-"+rigName)
	d.logger.Printf("Witness session for %s started successfully", rigName)
}

// killLeftover kills a seat's session, if it has one, through the
// supervisor. Used where a role must not run (docked rig, disabled patrol,
// safety stop).
func (d *Daemon) killLeftover(seat supervisor.Seat, reason, actor string) {
	name := seat.SessionName()
	exists, err := d.tmux.HasSession(name)
	if err != nil {
		d.logger.Printf("Not killing leftover %s (%s): session query failed: %v", name, reason, err)
		return
	}
	if !exists {
		return
	}
	d.logger.Printf("Killing leftover %s (%s)", name, reason)
	if err := d.sup().Kill(seat, reason, actor); err != nil {
		d.logRefusal("Killing leftover "+name, err)
	}
}

// logStartOutcome logs a supervisor Restart that did not start a role.
func (d *Daemon) logStartOutcome(role, rigName string, err error) {
	switch {
	case errors.Is(err, supervisor.ErrRefused):
		d.logger.Printf("Not starting %s for %s: %v", role, rigName, err)
	case errors.Is(err, supervisor.ErrDeclined):
		d.logger.Printf("Skipping %s auto-start for %s: %v", role, rigName, err)
	default:
		d.logger.Printf("Error starting %s for %s: %v", role, rigName, err)
	}
}

// ensureRefineriesRunning ensures refineries are running for configured rigs.
// Called on each heartbeat to maintain refinery merge queue processing.
// Respects the rigs filter in daemon.json patrol config.
func (d *Daemon) ensureRefineriesRunning() {
	rigs := d.getPatrolRigs("refinery")
	d.rigPool.runPerRig(d.ctx, rigs, func(ctx context.Context, rigName string) error {
		d.ensureRefineryRunning(rigName)
		return nil
	})
}

// ensureRefineryRunning keeps the refinery for a rig running when it has
// work: the liveness function decides, and a dead refinery with pending
// events is restarted through the supervisor. A leftover refinery in a rig
// that is not operational or is under a safety stop is killed through the
// supervisor.
func (d *Daemon) ensureRefineryRunning(rigName string) {
	seat := supervisor.SeatFor(rigName, constants.RoleRefinery, "")
	if operational, reason := d.isRigOperational(rigName); !operational {
		d.logger.Printf("Skipping refinery auto-start for %s: %s", rigName, reason)
		d.killLeftover(seat, "rig "+reason, "daemon/rig-state")
		return
	}
	if stop, err := refinery.ActiveSafetyStop(d.config.TownRoot, rigName); err != nil {
		d.logger.Printf("Skipping refinery auto-start for %s: cannot verify safety stop: %v", rigName, err)
		return
	} else if stop != nil {
		d.logger.Printf("Skipping refinery auto-start for %s: %s", rigName, stop.Reason())
		d.killLeftover(seat, stop.Reason(), "daemon/safety-stop")
		return
	}

	// NOTE: no stall restart for refineries (serial killer bug): an idle
	// refinery legitimately produces no output while it waits for MRs.
	res := d.assessSeat(seat, liveness.Input{})
	switch res.Verdict {
	case liveness.Unknown:
		d.logger.Printf("Refinery for %s: liveness unknown (%v); not acting this tick", rigName, res.Err)
		return
	case liveness.Alive, liveness.Stalled:
		d.logger.Printf("Refinery for %s already running, skipping spawn", rigName)
		// The check above cannot see a session that is alive but consuming
		// no input -- exactly the state be-refinery was in for ~15 minutes
		// while three MRs aged behind it (gt-eigw). Probe it.
		d.probeRunningRefinery(rigName)
		return
	}

	// Event gate: a new Claude session with an empty queue burns API
	// credits for nothing; the refinery formula's await-event wakes a
	// running one when events appear.
	if !d.hasPendingEvents("refinery", rigName) {
		d.logger.Printf("No pending refinery events and no session running for %s, skipping spawn", rigName)
		return
	}

	if err := d.sup().Restart(seat, "refinery "+res.Reason+" with pending events", "daemon/ensure-refinery"); err != nil {
		d.logStartOutcome("refinery", rigName, err)
		return
	}
	d.metrics.recordRestart(d.ctx, "refinery")
	telemetry.RecordDaemonRestart(d.ctx, "refinery-"+rigName)
	d.logger.Printf("Refinery session for %s started successfully", rigName)
}

// mayorDeadSamples is how many consecutive dead-agent samples the Mayor
// needs before a restart: during a handoff its agent is briefly
// undetectable. The count lives in the Mayor's intent record, so it
// survives daemon restarts.
const mayorDeadSamples = 3

// mayorSeat is the Mayor's seat.
var mayorSeat = supervisor.SeatFor("", constants.RoleMayor, "")

// ensureMayorRunning keeps the Mayor running. A missing session is restarted
// at once; a session whose agent is gone only after mayorDeadSamples
// consecutive samples. Unknown is never acted on.
func (d *Daemon) ensureMayorRunning() {
	res := d.assessSeat(mayorSeat, liveness.Input{})
	switch res.Verdict {
	case liveness.Unknown:
		d.logger.Printf("Mayor agent liveness unknown (%v); not counted as a zombie cycle", res.Err)
		return
	case liveness.Alive, liveness.Stalled:
		return
	}
	if res.Reason == liveness.ReasonAgentGone && res.Sample != nil && res.Sample.DeadSamples < mayorDeadSamples {
		d.logger.Printf("Mayor agent not detected (sample %d/%d), waiting before restart", res.Sample.DeadSamples, mayorDeadSamples)
		return
	}
	if err := d.sup().Restart(mayorSeat, "mayor "+res.Reason, "daemon/ensure-mayor"); err != nil {
		d.logStartOutcome("mayor", "town", err)
		return
	}
	d.logger.Println("Mayor started successfully")
}

// killDeaconSessions kills leftover deacon and boot tmux sessions through
// the supervisor. Called when the deacon patrol is disabled to prevent stale
// deacons from running their own patrol loops and spawning agents. (hq-2mstj)
func (d *Daemon) killDeaconSessions() {
	for _, seat := range []supervisor.Seat{deaconSeat, supervisor.SeatFor("", constants.RoleDeacon, "boot")} {
		d.killLeftover(seat, "patrol disabled", "daemon/patrol-disabled")
	}
}

// killWitnessSessions kills leftover witness sessions for all rigs through
// the supervisor. Called when the witness patrol is disabled. (hq-2mstj)
func (d *Daemon) killWitnessSessions() {
	d.rigPool.runPerRig(d.ctx, d.getKnownRigs(), func(ctx context.Context, rigName string) error {
		d.killLeftover(supervisor.SeatFor(rigName, constants.RoleWitness, ""), "patrol disabled", "daemon/patrol-disabled")
		return nil
	})
}

// killRefinerySessions kills leftover refinery sessions for all rigs through
// the supervisor. Called when the refinery patrol is disabled. (hq-2mstj)
func (d *Daemon) killRefinerySessions() {
	d.rigPool.runPerRig(d.ctx, d.getKnownRigs(), func(ctx context.Context, rigName string) error {
		d.killLeftover(supervisor.SeatFor(rigName, constants.RoleRefinery, ""), "patrol disabled", "daemon/patrol-disabled")
		return nil
	})
}

// killDefaultPrefixGhosts kills tmux sessions that use the default "gt" prefix
// for roles that should use a rig-specific prefix. These ghost sessions appear
// when the daemon starts before a rig is registered or when the registry was
// stale. After a registry reload, any "gt-witness", "gt-refinery", or "gt-*"
// sessions that correspond to rigs with their own prefix are stale duplicates.
// Fix for: hq-ouz, hq-eqf, hq-3i4.
func (d *Daemon) killDefaultPrefixGhosts() {
	reg := session.DefaultRegistry()
	allRigs := reg.AllRigs() // rigName → shortPrefix
	if len(allRigs) == 0 {
		return
	}

	// Check if any rig actually has "gt" as its registered prefix.
	// If so, gt-witness is legitimate for that rig — don't kill it.
	gtIsLegitimate := false
	for _, prefix := range allRigs {
		if prefix == session.DefaultPrefix {
			gtIsLegitimate = true
			break
		}
	}
	if gtIsLegitimate {
		return
	}

	// Kill ghost sessions using the default "gt" prefix for patrol roles.
	for _, role := range []string{"witness", "refinery"} {
		ghostName := fmt.Sprintf("%s-%s", session.DefaultPrefix, role)
		exists, _ := d.tmux.HasSession(ghostName)
		if exists {
			d.logger.Printf("Killing ghost session %s (default prefix, stale registry artifact)", ghostName)
			if err := d.sup().KillStray(ghostName, "default-prefix ghost (stale registry artifact)", "daemon/ghosts"); err != nil {
				d.logRefusal("Killing ghost session "+ghostName, err)
			}
		}
	}

	// Also check for ghost polecat sessions: gt-<polecatName> where the polecat
	// actually belongs to a rig with a different prefix.
	for _, rigName := range d.getKnownRigs() {
		rigPrefix := session.PrefixFor(rigName)
		if rigPrefix == session.DefaultPrefix {
			continue // This rig uses "gt" — its sessions are fine
		}
		rigPath := filepath.Join(d.config.TownRoot, rigName, "polecats")
		entries, err := os.ReadDir(rigPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			polecatName := entry.Name()
			ghostName := fmt.Sprintf("%s-%s", session.DefaultPrefix, polecatName)
			exists, _ := d.tmux.HasSession(ghostName)
			if exists {
				// Verify the correct session isn't also running (avoid killing legit sessions)
				correctName := session.PolecatSessionName(rigPrefix, polecatName)
				correctExists, _ := d.tmux.HasSession(correctName)
				if !correctExists {
					// Ghost is the only session — it might be doing real work.
					// Log but don't kill; the registry reload will prevent new ghosts.
					d.logger.Printf("Ghost polecat session %s found (should be %s), not killing (may have active work)", ghostName, correctName)
				} else {
					// Both exist — ghost is definitely a duplicate, kill it.
					d.logger.Printf("Killing duplicate ghost polecat session %s (correct session %s exists)", ghostName, correctName)
					if err := d.sup().KillStray(ghostName, "duplicate of "+correctName, "daemon/ghosts"); err != nil {
						d.logRefusal("Killing ghost session "+ghostName, err)
					}
				}
			}
		}
	}
}

// openBeadsStores opens beads stores for the town (hq) and all known rigs.
// It returns the stores that opened — keyed by "hq" for town-level and by rig
// name for per-rig stores — plus the names that were wanted but would not open.
// Stores that fail to open are logged and skipped. Successfully opened stores
// are compatibility-checked before being returned to Convoy polling.
//
// The names that failed are returned rather than only logged: a store missed
// while Dolt is restarting has to be retried, and the convoy manager cannot
// retry what it was never told was wanted (gt-i36h).
func (d *Daemon) openBeadsStores() (storeOpenResult, error) {
	stores := make(map[string]beadsdk.Storage)
	var missing []string

	// Town-level store (hq)
	hqBeadsDir := filepath.Join(d.config.TownRoot, ".beads")
	if store, err := beads.OpenStoreFromConfig(d.ctx, hqBeadsDir); err == nil {
		stores["hq"] = store
	} else {
		d.logger.Printf("Convoy: hq beads store unavailable: %s", util.FirstLine(err.Error()))
		missing = append(missing, "hq")
	}

	// Per-rig stores
	for _, rigName := range d.getKnownRigs() {
		beadsDir := doltserver.FindRigBeadsDir(d.config.TownRoot, rigName)
		if beadsDir == "" {
			continue
		}
		store, err := beads.OpenStoreFromConfig(d.ctx, beadsDir)
		if err != nil {
			d.logger.Printf("Convoy: %s beads store unavailable: %s", rigName, util.FirstLine(err.Error()))
			missing = append(missing, rigName)
			continue
		}
		stores[rigName] = store
	}

	if len(stores) == 0 {
		d.logger.Printf("Convoy: no beads stores available, event polling disabled")
		return storeOpenResult{Missing: missing}, nil
	}

	if err := verifyBeadsStores(d.ctx, d.logger, d.config.TownRoot, stores, newBDStoreProbe); err != nil {
		return storeOpenResult{Missing: missing}, err
	}

	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	d.logger.Printf("Convoy: opened %d beads store(s): %v", len(stores), names)
	return storeOpenResult{Stores: stores, Missing: missing}, nil
}

// storeOpenerNeeded reports whether the convoy manager must be handed the
// store opener to complete the set this startup walk produced.
//
// A walk that came up short is not only one that came up empty. Dolt restarting
// mid-walk drops whichever stores it had not reached — hq is opened first, so a
// restart landing there loses hq while the rigs that follow open fine — and
// with no opener that partial map read as complete: every convoy lookup was
// skipped for the life of the daemon, silently, while the rigs kept polling
// (gt-i36h).
func storeOpenerNeeded(res storeOpenResult) bool {
	return len(res.Stores) == 0 || len(res.Missing) > 0
}

// getKnownRigs returns list of registered rig names.
// Results are memoized per heartbeat tick to coalesce the ~10 per-tick callers
// into a single mayor/rigs.json read. The cache is invalidated at the start of
// each heartbeat. Callable from any goroutine (gt-f18v).
//
// The lock is held across the disk read rather than only around the fields:
// the file is a few hundred bytes, the callers are a handful per tick, and a
// second caller arriving mid-read wants the value that read produces.
func (d *Daemon) getKnownRigs() []string {
	d.knownRigsMu.Lock()
	defer d.knownRigsMu.Unlock()
	if d.knownRigsCacheValid {
		return d.knownRigsCache
	}
	d.knownRigsCache = d.readKnownRigsFromDisk()
	d.knownRigsCacheValid = true
	return d.knownRigsCache
}

// invalidateKnownRigsCache clears the per-tick cache so the next
// getKnownRigs() call re-reads mayor/rigs.json from disk.
func (d *Daemon) invalidateKnownRigsCache() {
	d.knownRigsMu.Lock()
	defer d.knownRigsMu.Unlock()
	d.knownRigsCache = nil
	d.knownRigsCacheValid = false
}

// readKnownRigsFromDisk reads and parses mayor/rigs.json.
func (d *Daemon) readKnownRigsFromDisk() []string {
	rigsPath := filepath.Join(d.config.TownRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		return nil
	}

	var parsed struct {
		Rigs map[string]interface{} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}

	var rigs []string
	for name := range parsed.Rigs {
		rigs = append(rigs, name)
	}
	return rigs
}

// getPatrolRigs returns the list of operational rigs for a patrol.
// If the patrol config specifies a rigs filter, only those rigs are returned.
// Otherwise, all known rigs are returned. In both cases, non-operational
// rigs (parked/docked) are filtered out at list-building time. (Fixes upstream #2082)
func (d *Daemon) getPatrolRigs(patrol string) []string {
	configRigs := GetPatrolRigs(d.patrolConfig, patrol)
	var candidates []string
	if len(configRigs) > 0 {
		candidates = configRigs
	} else {
		candidates = d.getKnownRigs()
	}

	// Filter out non-operational rigs early to avoid per-rig skip noise
	var operational []string
	for _, rigName := range candidates {
		if ok, reason := d.isRigOperational(rigName); ok {
			operational = append(operational, rigName)
		} else {
			d.logger.Printf("Excluding %s from %s patrol: %s", rigName, patrol, reason)
		}
	}
	return operational
}

// isRigOperational checks if a rig is in an operational state.
// Returns true if the rig can have agents auto-started.
// Returns false (with reason) if the rig is parked, docked, or has auto_restart blocked/disabled.
//
// The state is read from two layers. The wisp layer is local and cheap, so it is
// evaluated here on every call; the identity bead costs a bd subprocess, so that
// read is memoized for a short window (rigOperationalCacheTTL) - see
// internal/daemon/rig_status.go for why. This function is evaluated per rig per
// heartbeat by the patrol rig filters, witness and refinery auto-start, and the
// convoy manager, and each of those used to repeat the subprocess read, whose
// 60s budget a CPU-starved host spends in full (gt-4nu3).
//
// A failed bead read still fails closed: the rig is reported not operational, so
// nothing is auto-started for a rig whose state could not be verified. It is
// logged with the failure's category (timeout vs missing identity bead) and
// escalated, because that suppression is otherwise invisible.
//
// TODO(#2120): This duplicates parked/docked checking logic from
// cmd.IsRigParkedOrDocked and cmd.hasRigBeadLabel. Consolidating into a
// shared package (e.g. internal/rig) would eliminate the third implementation
// and reduce drift risk. Not done here due to circular import constraints
// (daemon cannot import cmd).
func (d *Daemon) isRigOperational(rigName string) (bool, string) {
	cfg := wisp.NewConfig(d.config.TownRoot, rigName)

	// A rig that has never been parked or docked legitimately has no wisp
	// config - that's the default state, not data loss. Log it once per rig
	// per process (not on every patrol-candidate evaluation) so it stays
	// available for debugging without drowning the log (gt-k07).
	if _, err := os.Stat(cfg.ConfigPath()); os.IsNotExist(err) {
		if d.rigOperational.markWispConfigWarned(rigName) {
			d.logger.Printf("no wisp config for %s (rig has never been parked or docked)", rigName)
		}
	}

	// Check wisp layer first (local/ephemeral overrides). Reading it is a stat
	// plus a small file, so this layer is never memoized: `gt rig park` takes
	// effect on the next evaluation, not on the next memo expiry.
	switch cfg.GetString("status") {
	case "parked":
		return false, "rig is parked"
	case "docked":
		return false, "rig is docked"
	}

	// Check the rig bead labels (global/synced docked status), the persistent
	// docked state set by 'gt rig dock'. This is the memoized read.
	entry, ok := d.rigBeadVerdict(rigName)
	if !ok {
		// FAIL-SAFE: When we can't verify docked status (Dolt down, host starved
		// past the subprocess budget, network issue), assume the rig is NOT
		// operational. This prevents wasting API credits starting witnesses that
		// might be docked. Better to delay work than burn credits unnecessarily.
		return false, "cannot verify rig status (" + entry.failed + ")"
	}
	switch entry.verdict {
	case rigBeadDocked:
		return false, "rig is docked (global)"
	case rigBeadParked:
		return false, "rig is parked (global)"
	}

	// Check auto_restart config. Also local and cheap, so also not memoized: a
	// `gt rig config set <rig> auto_restart false` used to quiesce a rig takes
	// effect now rather than at the next expiry.
	//
	// If explicitly blocked (nil), auto-restart is disabled
	if cfg.IsBlocked("auto_restart") {
		return false, "auto_restart is blocked"
	}

	// If explicitly set to false, auto-restart is disabled
	if autoRestartDisabled(cfg.Get("auto_restart")) {
		return false, "auto_restart is disabled"
	}

	return true, ""
}

// rigBeadVerdict returns the rig's identity-bead verdict, from the memo when one
// is fresh and from a fresh bd read otherwise. ok is false when the read could
// not answer, in which case the entry carries the failure category.
//
// The window a read opens starts when the read answers, not when it was issued.
// Measuring from before the read would hand a starved host an entry that is
// already stale on arrival - a 60s read with a 60s window buys nothing, and
// every call site in the tick re-pays the budget the memo exists to save.
func (d *Daemon) rigBeadVerdict(rigName string) (rigBeadEntry, bool) {
	if entry, ok := d.rigOperational.get(rigName, d.clk().Now()); ok {
		return entry, entry.failed == ""
	}

	rigBeadID, verdict, err := d.queryRigBead(rigName)
	now := d.clk().Now()
	if err != nil {
		// The category is in the line on purpose: "assuming not operational"
		// alone left a starved-host timeout and a missing identity bead looking
		// identical in daemon.log, and they call for different responses
		// (gt-4nu3).
		category := rigStatusFailureCategory(err)
		d.logger.Printf("Warning: failed to check rig bead %s for docked/parked status: %s — assuming not operational (rig %s): %v",
			rigBeadID, category, rigName, err)
		if d.rigOperational.store(rigName, verdict, category, rigOperationalFailureCacheTTL, now).open {
			d.alertRigStatusUnverified(rigName, category, err)
		}
		return rigBeadEntry{failed: category}, false
	}

	if d.rigOperational.store(rigName, verdict, "", rigOperationalCacheTTL, now).clear {
		d.clearRigStatusUnverified(rigName)
	}
	return rigBeadEntry{verdict: verdict}, true
}

// queryRigBead reads the rig's identity bead and turns its status labels into a
// verdict. It logs nothing and reports no escalation - rigBeadVerdict owns both,
// so this stays a pure query.
func (d *Daemon) queryRigBead(rigName string) (string, rigBeadVerdict, error) {
	rigPath := filepath.Join(d.config.TownRoot, rigName)

	// Try to get prefix from rig config.json, fall back to rigs.json registry
	var prefix string
	if rigCfg, err := rig.LoadRigConfig(rigPath); err == nil && rigCfg.Beads != nil {
		prefix = rigCfg.Beads.Prefix
	} else {
		// Fall back to registry (mayor/rigs.json) when config.json is missing
		prefix = agentconfig.GetRigPrefix(d.config.TownRoot, rigName)
	}

	rigBeadID := fmt.Sprintf("%s-rig-%s", prefix, rigName)
	issue, err := d.showRigBead(rigPath, rigBeadID)
	if err != nil {
		return rigBeadID, rigBeadActive, err
	}

	for _, label := range issue.Labels {
		if label == "status:docked" {
			return rigBeadID, rigBeadDocked, nil
		}
		if label == "status:parked" {
			return rigBeadID, rigBeadParked, nil
		}
	}
	return rigBeadID, rigBeadActive, nil
}

// showRigBead reads a rig's identity bead with bd show, or through
// rigBeadShowFn when a test set one.
func (d *Daemon) showRigBead(rigPath, rigBeadID string) (*beads.Issue, error) {
	if d.rigBeadShowFn != nil {
		return d.rigBeadShowFn(rigPath, rigBeadID)
	}
	return beads.NewWithBeadsDir(rigPath, beads.ResolveBeadsDir(rigPath)).Show(rigBeadID)
}

// autoRestartDisabled reports whether a resolved auto_restart value turns
// auto-restart off. Note that wisp.Get returns false for unset keys, so nil
// (never configured) must stay enabled - only an explicit off-switch disables it.
//
// The value is coerced rather than type-asserted: wisp values round-trip through
// JSON, where numbers load back as float64, so a stored 0 (written by hand, by an
// older `gt rig config set`, or for a key with no declared type) has to read as
// false instead of being ignored by a `val.(bool)` assertion.
func autoRestartDisabled(val interface{}) bool {
	return val != nil && !rig.CoerceBool(val)
}

// shutdown performs graceful shutdown.
func (d *Daemon) shutdown(state *State) error { //nolint:unparam // error return kept for future use
	d.logger.Println("Daemon shutting down")

	// Stop feed curator
	if d.curator != nil {
		d.curator.Stop()
		d.logger.Println("Feed curator stopped")
	}

	// Stop convoy manager (also closes beads stores)
	if d.convoyManager != nil {
		d.convoyManager.Stop()
		d.logger.Println("Convoy manager stopped")
	}
	d.beadsStores = nil

	// Stop KRC pruner
	if d.krcPruner != nil {
		d.krcPruner.Stop()
		d.logger.Println("KRC pruner stopped")
	}

	// Stop Dolt server if we're managing it. An upgrade restart leaves Dolt
	// running: the server is detached and the next daemon adopts it via
	// dolt.pid + port probe (isRunning). Bouncing the data plane several
	// times an hour for a binary swap is not safe.
	if d.upgradeRestartRequested.Load() {
		d.logger.Println("Upgrade restart: leaving Dolt server running for the next daemon to adopt")
	} else if d.doltServer != nil && d.doltServer.IsEnabled() && !d.doltServer.IsExternal() {
		if err := d.doltServer.Stop(); err != nil {
			d.logger.Printf("Warning: failed to stop Dolt server: %v", err)
		} else {
			d.logger.Println("Dolt server stopped")
		}
	}

	// Flush and stop OTel providers. Bounded so it cannot block shutdown; part
	// of ShutdownBudget.
	if d.otelProvider != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), otelShutdownBudget)
		defer cancel()
		if err := d.otelProvider.Shutdown(shutCtx); err != nil {
			d.logger.Printf("Warning: telemetry shutdown: %v", err)
		}
	}

	state.Running = false
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save final state: %v", err)
	}

	d.logger.Println("Daemon stopped")
	return nil
}

// Stop signals the daemon to stop.
func (d *Daemon) Stop() {
	d.cancel()
}

// isShutdownInProgress checks if a shutdown is currently in progress.
// The shutdown.lock file is created by gt down before terminating sessions.
// This prevents the daemon from fighting shutdown by auto-restarting killed agents.
//
// Uses flock to check actual lock status rather than file existence, since
// the lock file persists after shutdown completes. The file is intentionally
// never removed: flock works on file descriptors, not paths, and removing
// the file while another process waits on the flock defeats mutual exclusion.
func (d *Daemon) isShutdownInProgress() bool {
	lockPath := filepath.Join(d.config.TownRoot, "daemon", "shutdown.lock")

	// If file doesn't exist, no shutdown in progress
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false
	}

	// Try non-blocking lock acquisition to check if shutdown holds the lock
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		// Error acquiring lock - assume shutdown in progress to be safe
		return true
	}

	if locked {
		// We acquired the lock, so no shutdown is holding it
		// Release immediately; leave the file in place so all
		// concurrent callers flock the same inode.
		_ = lock.Unlock()
		return false
	}

	// Could not acquire lock - shutdown is in progress
	return true
}

// IsShutdownInProgress checks if a shutdown is currently in progress for the given town.
// This is the exported version of isShutdownInProgress for use by other packages
// (e.g., Boot triage) that need to avoid restarting sessions during shutdown.
func IsShutdownInProgress(townRoot string) bool {
	lockPath := filepath.Join(townRoot, "daemon", "shutdown.lock")

	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return true
	}

	if locked {
		_ = lock.Unlock()
		return false
	}

	return true
}

// IsRunning checks if a daemon is running for the given town.
// Uses the daemon.lock flock as the authoritative signal — if the lock is held,
// the daemon is running. Falls back to PID file for the process ID.
// This avoids fragile ps string matching for process identity (ZFC fix: gt-utuk).
func IsRunning(townRoot string) (bool, int, error) {
	// Primary check: is the daemon lock held?
	lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false, 0, nil
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		// Can't check lock — fall back to PID file + signal check
		return isRunningFromPID(townRoot)
	}

	if locked {
		// We acquired the lock, so no daemon holds it
		_ = lock.Unlock()
		// Clean up stale PID file if present
		pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
		_ = os.Remove(pidFile)
		return false, 0, nil
	}

	// Lock is held — daemon is running. Read PID from file.
	// Use readPIDFile to handle the "PID\nNONCE" format introduced alongside
	// nonce-based ownership verification. A plain Atoi on the raw file content
	// fails when a nonce line is present, returning PID 0.
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	pid, _, err := readPIDFile(pidFile)
	if err != nil {
		// Lock held but no readable PID file — daemon running, PID unknown
		return true, 0, nil
	}

	return true, pid, nil
}

// isRunningFromPID is the fallback when flock check fails. Uses PID file + signal.
func isRunningFromPID(townRoot string) (bool, int, error) {
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")

	pid, alive, err := verifyPIDOwnership(pidFile)
	if err != nil {
		return false, 0, fmt.Errorf("checking PID file: %w", err)
	}

	if pid == 0 {
		// No PID file
		return false, 0, nil
	}

	if !alive {
		// Process not running, clean up stale PID file.
		// This is a successful recovery, not an error — the caller can
		// proceed as if no daemon is running (fixes #2107).
		os.Remove(pidFile) // best-effort cleanup
		return false, 0, nil
	}

	return true, pid, nil
}

// StopDaemon stops the running daemon for the given town.
// Note: The file lock in Run() prevents multiple daemons per town, so we only
// need to kill the process from the PID file.
func StopDaemon(townRoot string) error {
	running, pid, err := IsRunning(townRoot)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	if pid <= 0 {
		// Lock is held but PID is unknown (race: daemon starting, or stale lock).
		// Clean up the lock file so the next gt up can start fresh.
		lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
		_ = os.Remove(lockPath)
		pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
		_ = os.Remove(pidFile)
		return nil
	}

	// The lock proves a daemon is running; the pid file only claims which PID
	// it is. Never signal that PID until it is shown to be `gt daemon run`
	// (gt-p7zy0). Nothing is removed on refusal: the lock holder is live.
	if err := verifyGTDaemonPID(townRoot, pid); err != nil {
		return fmt.Errorf("refusing to signal PID %d from %s: %w",
			pid, filepath.Join(townRoot, "daemon", "daemon.pid"), err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process: %w", err)
	}

	// Send termination signal for graceful shutdown
	if err := sendTermSignal(process); err != nil {
		return fmt.Errorf("sending termination signal: %w", err)
	}

	// Wait a bit for graceful shutdown
	time.Sleep(constants.ShutdownNotifyDelay)

	// Check if still running
	if isProcessAlive(process) {
		// Still running, force kill — re-verified: the PID may have exited
		// and been reused during the wait (gt-p7zy0).
		if verifyGTDaemonPID(townRoot, pid) == nil {
			_ = sendKillSignal(process)
		}
	}

	// Clean up PID file
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	_ = os.Remove(pidFile)

	return nil
}

// FindOrphanedDaemons detects daemon processes not tracked by the PID file.
// Uses flock on daemon.lock to detect running daemons without relying on
// pgrep or ps string matching (ZFC fix: gt-utuk).
//
// With flock-based daemon management, only one daemon can hold the lock.
// An "orphan" is detected when the lock is held but the PID file is stale
// (process dead) or missing. Returns the stale PID if available.
func FindOrphanedDaemons(townRoot string) ([]int, error) {
	lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return nil, nil // No lock file — no daemon has ever run
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, nil // Can't check lock — assume no orphans
	}

	if locked {
		// We acquired the lock — no daemon holds it, no orphans possible
		_ = lock.Unlock()
		return nil, nil
	}

	// Lock is held — a daemon is running. Check if it's tracked.
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	trackedPID, _, err := readPIDFile(pidFile)
	if err != nil {
		// Lock held but no/invalid PID file — daemon is running but untracked.
		// We can't determine its PID without ps/pgrep, so return empty.
		// The caller (start.go) should use IsRunning() which handles this case.
		return nil, nil
	}

	// Check if the tracked PID is actually alive
	process, findErr := os.FindProcess(trackedPID)
	if findErr != nil {
		return nil, nil
	}
	if !isProcessAlive(process) {
		// PID file exists but process is dead — stale PID file with held lock.
		// This shouldn't happen (lock should release on process death), but
		// report the stale PID for cleanup.
		return []int{trackedPID}, nil
	}

	// Lock held, PID alive, PID tracked — daemon is properly running, not orphaned.
	return nil, nil
}

// KillOrphanedDaemons finds and kills any orphaned gt daemon processes.
// Returns number of processes killed.
func KillOrphanedDaemons(townRoot string) (int, error) {
	pids, err := FindOrphanedDaemons(townRoot)
	if err != nil {
		return 0, err
	}

	killed := 0
	for _, pid := range pids {
		// FindOrphanedDaemons reports PIDs whose process looked dead: any
		// live process now holding that number is someone else unless it
		// is provably `gt daemon run` (gt-p7zy0).
		if verifyGTDaemonPID(townRoot, pid) != nil {
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}

		// Try termination signal first
		if err := sendTermSignal(process); err != nil {
			continue
		}

		// Wait for graceful shutdown
		time.Sleep(200 * time.Millisecond)

		// Check if still alive
		if isProcessAlive(process) {
			// Still alive, force kill — re-verified first.
			if verifyGTDaemonPID(townRoot, pid) == nil {
				_ = sendKillSignal(process)
			}
		}

		killed++
	}

	return killed, nil
}

// checkPolecatSessionHealth proactively validates polecat tmux sessions.
// This detects crashed polecats that:
// 1. Have work-on-hook (assigned work)
// 2. Report state=running/working in their agent bead
// 3. But the tmux session is actually dead
//
// When a crash is detected, the polecat is automatically restarted.
// This provides faster recovery than waiting for GUPP timeout or Witness detection.
func (d *Daemon) checkPolecatSessionHealth() {
	d.rigPool.runPerRig(d.ctx, d.getKnownRigs(), func(ctx context.Context, rigName string) error {
		d.checkRigPolecatHealth(rigName)
		return nil
	})
}

// checkRigPolecatHealth checks polecat session health for a specific rig.
func (d *Daemon) checkRigPolecatHealth(rigName string) {
	// Get polecat directories for this rig
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		return // No polecats directory - rig might not have polecats
	}

	for _, polecatName := range polecats {
		d.checkPolecatHealth(rigName, polecatName)
	}
}

func listPolecatWorktrees(polecatsDir string) ([]string, error) {
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return nil, err
	}

	polecats := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		polecats = append(polecats, name)
	}

	return polecats, nil
}

// checkPolecatHealth checks a single polecat's session health.
// If the polecat has work-on-hook but the tmux session is dead, it's restarted.
func (d *Daemon) checkPolecatHealth(rigName, polecatName string) {
	// The seat's intent record says whether it is held and, when a writer
	// set it, what work it holds. It is read before tmux and never from
	// Dolt, and an unreadable record is a hold (fail closed). A polecat the
	// operator parked — gt agent pause, or the deliberate stop gt session
	// stop records (gt-fojqs) — has a dead session on purpose.
	seat := supervisor.SeatFor(rigName, constants.RolePolecat, polecatName)
	rec, err := intent.Read(d.config.TownRoot, supervisor.IntentSeat(seat))
	if err != nil || rec.Held() {
		d.logger.Printf("Skipping crash detection for %s/%s: agent is parked (%s)",
			rigName, polecatName, rec.HoldReason())
		return
	}

	// gt done recorded desired=submitted: the branch is on origin for the
	// landing worker and the session ended on purpose. The hook still holds
	// the bead until it lands, which would read as a crash (gt-obbx2).
	if rec.Submitted() {
		d.logger.Printf("Skipping crash detection for %s/%s: work %s is submitted for landing",
			rigName, polecatName, rec.WorkBead)
		return
	}

	// Build the expected tmux session name
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

	// Check if tmux session exists
	sessionAlive, err := d.tmux.HasSession(sessionName)
	if err != nil {
		d.logger.Printf("Error checking session %s: %v", sessionName, err)
		return
	}

	if sessionAlive {
		// Session is alive - nothing to do
		return
	}

	// Session is dead. Find the seat's work: the intent record's work_bead
	// when a writer set it, otherwise the work bead assigned to the polecat
	// with status hooked or in_progress. Agent beads are display mirrors and
	// are not read here (gt-4k3fj.1, G1-01).
	hookBead := rec.WorkBead
	var workUpdated time.Time
	if hookBead == "" {
		assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
		work, updated, werr := d.assignedActiveWorkBead(rigName, assignee)
		if werr != nil {
			d.logger.Printf("UNKNOWN: crash detection for %s/%s skipped: session %s is dead and assigned work could not be read: %v",
				rigName, polecatName, sessionName, werr)
			return
		}
		if work == "" {
			// No hooked work: a finished or idle polecat whose session ended
			// is not a crash.
			return
		}
		hookBead, workUpdated = work, updated
	}

	// Finished work: gt done closes the work bead before the session stops,
	// so a dead session holding closed work completed normally.
	closed, submitted := d.beadFinished(hookBead)
	if closed {
		d.logger.Printf("Skipping crash detection for %s/%s: hook_bead %s is already closed (work completed normally)",
			rigName, polecatName, hookBead)
		return
	}
	// Submitted work: gt done pushed the branch and labeled the bead
	// gt:ready-to-land; the hook stays on it until the landing worker lands
	// it. This is the label half of the intent-record check above, for a seat
	// whose record was never written (gt-obbx2).
	if submitted {
		d.logger.Printf("Skipping crash detection for %s/%s: hook_bead %s is submitted for landing",
			rigName, polecatName, hookBead)
		return
	}

	// Spawn grace: gt sling hooks the work bead before the tmux session
	// exists, so a bead hooked moments ago with no session yet is a polecat
	// starting up. Reporting it would double-spawn (issue #1752).
	if !workUpdated.IsZero() {
		if age := time.Since(workUpdated); age < polecatSpawnGrace {
			d.logger.Printf("Skipping crash detection for %s/%s: work %s hooked %s ago, polecat may be spawning",
				rigName, polecatName, hookBead, age.Round(time.Second))
			return
		}
	}

	// TOCTOU guard: re-verify session is still dead before restarting.
	// Between the initial check and now, the session may have been restarted
	// by another heartbeat cycle, witness, or the polecat itself.
	sessionRevived, err := d.tmux.HasSession(sessionName)
	if err == nil && sessionRevived {
		return // Session came back - no restart needed
	}

	// Polecat has work but session is dead - this is a crash!
	d.logger.Printf("CRASH DETECTED: polecat %s/%s has hook_bead=%s but session %s is dead",
		rigName, polecatName, hookBead, sessionName)

	// Track this death for mass death detection
	d.recordSessionDeath(sessionName)

	// Emit session_death event for audit trail / feed visibility
	_ = events.LogFeedTo(d.config.TownRoot, events.TypeSessionDeath, sessionName,
		events.SessionDeathPayload(sessionName, rigName+"/polecats/"+polecatName, "crash detected by daemon health check", events.CallerDaemon))

	// Notify witness — stuck-agent-dog plugin handles context-aware restart
	d.notifyWitnessOfCrashedPolecat(rigName, polecatName, hookBead)
}

// recordSessionDeath records a session death and checks for mass death pattern.
func (d *Daemon) recordSessionDeath(sessionName string) {
	d.deathsMu.Lock()
	defer d.deathsMu.Unlock()

	now := time.Now()

	// Add this death
	d.recentDeaths = append(d.recentDeaths, sessionDeath{
		sessionName: sessionName,
		timestamp:   now,
	})

	// Prune deaths outside the window
	cutoff := now.Add(-massDeathWindow)
	var recent []sessionDeath
	for _, death := range d.recentDeaths {
		if death.timestamp.After(cutoff) {
			recent = append(recent, death)
		}
	}
	d.recentDeaths = recent

	// Check for mass death
	if len(d.recentDeaths) >= massDeathThreshold {
		d.emitMassDeathEvent()
	}
}

// emitMassDeathEvent logs a mass death event when multiple sessions die in a short window.
func (d *Daemon) emitMassDeathEvent() {
	// Collect session names
	var sessions []string
	for _, death := range d.recentDeaths {
		sessions = append(sessions, death.sessionName)
	}

	count := len(sessions)
	window := massDeathWindow.String()

	d.logger.Printf("MASS DEATH DETECTED: %d sessions died in %s: %v", count, window, sessions)

	// Emit feed event
	_ = events.LogFeedTo(d.config.TownRoot, events.TypeMassDeath, events.ActorDaemon,
		events.MassDeathPayload(count, window, sessions, ""))

	// Clear the deaths to avoid repeated alerts
	d.recentDeaths = nil
}

// beadFinished reads a bead with bd show --json and reports whether its work
// is over for crash detection: closed (status "closed"), or submitted for
// landing (label gt:ready-to-land, not closed). On any error (bead not found,
// bd failure) both are false, erring toward crash detection rather than
// silently suppressing alerts.
func (d *Daemon) beadFinished(beadID string) (closed, submitted bool) {
	cmd := beads.CommandWithPath(d.bdPath, d.config.TownRoot, bdReadOnlyRoutingEnv(d.config.TownRoot), "show", beadID, "--json")
	setSysProcAttr(cmd.Cmd)

	output, err := cmd.Output()
	if err != nil {
		return false, false
	}

	var issues []struct {
		Status string   `json:"status"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return false, false
	}

	if issues[0].Status == "closed" {
		return true, false
	}
	return false, slices.Contains(issues[0].Labels, land.LabelReadyToLand)
}

// hasAssignedOpenWork checks if any work bead is assigned to the given polecat
// with a non-terminal status (hooked, in_progress, or open). This is the
// authoritative source of polecat work — the sling code sets status=hooked +
// assignee on the work bead, but no longer maintains the agent bead's hook_bead
// field (updateAgentHookBead is a no-op). Without this fallback, the idle reaper
// kills working polecats whose agent bead hook_bead is stale.
func (d *Daemon) hasAssignedOpenWork(rigName, assignee string) bool {
	rigDir := beads.GetRigDirForName(d.config.TownRoot, rigName)

	for _, status := range []string{"hooked", "in_progress", "open"} {
		args := beads.InjectFlatForListJSON([]string{"list", "--assignee=" + assignee, "--status=" + status, "--json"})
		env := bdReadOnlyRoutingEnv(d.config.TownRoot)
		if rigDir != "" {
			env = bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(rigDir))
		}
		cmd := beads.CommandWithPath(d.bdPath, d.config.TownRoot, env, args...)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		var issues []json.RawMessage
		if json.Unmarshal(output, &issues) == nil && len(issues) > 0 {
			return true
		}
	}
	return false
}

// assignedActiveWorkBead returns the ID and updated_at of a work bead
// assigned to the polecat with status hooked or in_progress, read from the
// rig's database the way hasAssignedOpenWork does, or "" when there is none.
// A bead found by any query is returned; otherwise any failed query makes the
// answer unknown (an error), since the failed status may be the one holding
// the work. An updated_at that does not parse is returned as zero.
func (d *Daemon) assignedActiveWorkBead(rigName, assignee string) (string, time.Time, error) {
	rigDir := beads.GetRigDirForName(d.config.TownRoot, rigName)
	var lastErr error
	for _, status := range []string{"hooked", "in_progress"} {
		args := beads.InjectFlatForListJSON([]string{"list", "--assignee=" + assignee, "--status=" + status, "--json"})
		env := bdReadOnlyRoutingEnv(d.config.TownRoot)
		if rigDir != "" {
			env = bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(rigDir))
		}
		cmd := beads.CommandWithPath(d.bdPath, d.config.TownRoot, env, args...)
		output, err := cmd.Output()
		if err != nil {
			lastErr = fmt.Errorf("bd list --status=%s: %w", status, err)
			continue
		}
		var issues []struct {
			ID        string `json:"id"`
			UpdatedAt string `json:"updated_at"`
		}
		if err := json.Unmarshal(output, &issues); err != nil {
			lastErr = fmt.Errorf("parsing bd list --status=%s output: %w", status, err)
			continue
		}
		for _, issue := range issues {
			if issue.ID != "" {
				updated, _ := time.Parse(time.RFC3339, issue.UpdatedAt)
				return issue.ID, updated, nil
			}
		}
	}
	return "", time.Time{}, lastErr
}

// polecatSpawnGrace is how long after its work bead was hooked a polecat
// with no session is taken to be starting up rather than crashed.
const polecatSpawnGrace = 5 * time.Minute

// notifyWitnessOfCrashedPolecat notifies the witness when a polecat crash is detected.
// The stuck-agent-dog plugin handles context-aware restart decisions.
func (d *Daemon) notifyWitnessOfCrashedPolecat(rigName, polecatName, hookBead string) {
	witnessAddr := rigName + "/witness"
	subject := fmt.Sprintf("CRASHED_POLECAT: %s/%s detected", rigName, polecatName)
	body := fmt.Sprintf(`Polecat %s crash detected (session dead, work on hook).

hook_bead: %s

Restart deferred to stuck-agent-dog plugin for context-aware recovery.`,
		polecatName, hookBead)

	if err := d.notify().MailSend(context.Background(), witnessAddr, subject, body); err != nil {
		d.logger.Printf("Warning: failed to notify witness of crashed polecat: %v", err)
	}
}

// reapIdlePolecats kills polecat tmux sessions that have been idle too long.
// The persistent polecat model (gt-4ac) keeps sessions alive after gt done for reuse,
// but idle sessions consume API slots (Claude Code process stays alive at 0% CPU).
// This reaper checks heartbeat state and kills sessions idle longer than the threshold.
func (d *Daemon) reapIdlePolecats() {
	opCfg := d.loadOperationalConfig().GetDaemonConfig()
	idleTimeout := opCfg.PolecatIdleSessionTimeoutD()

	d.rigPool.runPerRig(d.ctx, d.getKnownRigs(), func(ctx context.Context, rigName string) error {
		d.reapRigIdlePolecats(rigName, idleTimeout)
		return nil
	})
}

// reapRigIdlePolecats checks all polecats in a rig and kills idle sessions.
func (d *Daemon) reapRigIdlePolecats(rigName string, timeout time.Duration) {
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		return // No polecats directory
	}

	for _, polecatName := range polecats {
		d.reapIdlePolecat(rigName, polecatName, timeout)
	}
}

// reapIdlePolecat checks a single polecat and kills it if idle too long.
// A polecat is considered idle if:
//   - Heartbeat state is "exiting" or "idle" and timestamp exceeds threshold, OR
//   - Heartbeat state is "working" but timestamp is stale AND the polecat has no
//     hooked work (agent_state=idle in beads). This catches polecats that completed
//     gt done — persistentPreRun resets heartbeat to "working" on every gt sub-command,
//     so after gt done finishes the heartbeat shows "working" with a stale timestamp.
func (d *Daemon) reapIdlePolecat(rigName, polecatName string, timeout time.Duration) {
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

	// Only check sessions that are actually alive
	alive, err := d.tmux.HasSession(sessionName)
	if err != nil || !alive {
		return
	}

	// Never reap a session younger than the idle threshold, regardless of what
	// the heartbeat file says. A reused polecat name can inherit a heartbeat
	// file written by a PREVIOUS incarnation (stale timestamp, state=exiting)
	// before the new session gets a chance to write its own heartbeat — see
	// gt-5mkr. Session age, taken straight from tmux, is authoritative for
	// "how long has THIS incarnation existed" in a way the heartbeat file
	// is not.
	if created, err := d.tmux.GetSessionCreatedTime(sessionName); err == nil {
		if time.Since(created) < timeout {
			return
		}
	}

	// Read heartbeat to check state and idle duration
	hb := polecat.ReadSessionHeartbeat(d.config.TownRoot, sessionName)
	if hb == nil {
		return // No heartbeat file — can't determine state
	}

	staleDuration := time.Since(hb.Timestamp)
	if staleDuration < timeout {
		return // Heartbeat is fresh — polecat is active
	}

	state := hb.EffectiveState()

	// Explicitly idle or exiting — safe to reap
	if state == polecat.HeartbeatIdle || state == polecat.HeartbeatExiting {
		d.killIdlePolecat(rigName, polecatName, sessionName, staleDuration, timeout, string(state))
		return
	}

	// Heartbeat says "working" but is stale. persistentPreRun resets it to
	// "working" on every gt sub-command, so a polecat that finished gt done
	// can look like this. It is idle only if no work bead is assigned to it
	// (the authoritative work record; agent beads are display mirrors and are
	// not read, gt-4k3fj.1) and its agent process is confirmed gone (a failed
	// gt sling rollback can clear the hook while the agent is still working,
	// GH#3342).
	if state == polecat.HeartbeatWorking {
		assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
		if d.hasAssignedOpenWork(rigName, assignee) {
			return
		}
		seat := supervisor.SeatFor(rigName, constants.RolePolecat, polecatName)
		res := d.assessSeat(seat, liveness.Input{Session: sessionName})
		switch res.Verdict {
		case liveness.Unknown:
			// Never acted on (gt-fcxe9.1).
			d.logger.Printf("Not reaping %s/%s: agent liveness unknown (%v)", rigName, polecatName, res.Err)
			return
		case liveness.Dead:
			d.killIdlePolecat(rigName, polecatName, sessionName, staleDuration, timeout, "working-no-hook")
		}
	}
}

// killIdlePolecat terminates an idle polecat session through the supervisor,
// which refuses it for a paused or e-stopped seat (G1-07, G1-08), and cleans
// up after it.
func (d *Daemon) killIdlePolecat(rigName, polecatName, sessionName string, idleDuration, timeout time.Duration, reason string) {
	seat := supervisor.SeatFor(rigName, constants.RolePolecat, polecatName)
	why := fmt.Sprintf("idle-reap: %s, idle %v (threshold %v)", reason, idleDuration.Truncate(time.Second), timeout)
	if err := d.sup().Kill(seat, why, "daemon/idle-reaper"); err != nil {
		d.logRefusal(fmt.Sprintf("Not reaping idle polecat %s/%s", rigName, polecatName), err)
		return
	}
	d.logger.Printf("Reaping idle polecat %s/%s (state=%s, idle %v, threshold %v)",
		rigName, polecatName, reason, idleDuration.Truncate(time.Second), timeout)

	// Clean up heartbeat file
	polecat.RemoveSessionHeartbeat(d.config.TownRoot, sessionName)

	d.logger.Printf("Reaped idle polecat %s/%s — session killed, API slot freed", rigName, polecatName)

	// Emit feed event so the activity feed shows the reap
	_ = events.LogFeedTo(d.config.TownRoot, events.TypeSessionDeath, fmt.Sprintf("%s/%s", rigName, polecatName),
		events.SessionDeathPayload(sessionName, fmt.Sprintf("%s/polecats/%s", rigName, polecatName), why, events.CallerDaemon))
}

// cleanupOrphanedProcesses kills orphaned claude subagent processes.
// These are Task tool subagents that didn't clean up after completion.
// Detection uses TTY column: processes with TTY "?" have no controlling terminal.
// This is a safety net fallback - Deacon patrol also runs this more frequently.
func (d *Daemon) cleanupOrphanedProcesses() {
	results, err := util.CleanupOrphanedClaudeProcesses()
	if err != nil {
		d.logger.Printf("Warning: orphan process cleanup failed: %v", err)
		return
	}

	if len(results) > 0 {
		d.logger.Printf("Orphan cleanup: processed %d process(es)", len(results))
		for _, r := range results {
			// ppid makes the kill attributable: ppid 0/1 is a genuine orphan
			// reparented to launchd/init, anything else means the parent died
			// between the scan and the signal (gt-h1tq).
			if r.Signal == "UNKILLABLE" {
				d.logger.Printf("  WARNING: PID %d (%s) ppid=%d survived SIGKILL", r.Process.PID, r.Process.Cmd, r.Process.PPID)
			} else {
				d.logger.Printf("  Sent %s to PID %d (%s) ppid=%d", r.Signal, r.Process.PID, r.Process.Cmd, r.Process.PPID)
			}
		}
	}
}

// pruneStaleBranches removes stale local polecat tracking branches from all rig clones.
// This runs in every heartbeat but is very fast when there are no stale branches.
func (d *Daemon) pruneStaleBranches() {
	// pruneInDir prunes stale polecat branches in a single git directory.
	pruneInDir := func(dir, label string) {
		g := gitpkg.NewGit(dir)
		if !g.IsRepo() {
			return
		}

		// Fetch --prune first to clean up stale remote tracking refs
		_ = g.FetchPrune("origin")

		pruned, err := g.PruneStaleBranches("polecat/*", false)
		if err != nil {
			d.logger.Printf("Warning: branch prune failed for %s: %v", label, err)
			return
		}

		if len(pruned) > 0 {
			d.logger.Printf("Branch prune: removed %d stale polecat branch(es) in %s", len(pruned), label)
			for _, b := range pruned {
				d.logger.Printf("  %s (%s)", b.Name, b.Reason)
			}
		}
	}

	// Prune in each rig's git directory (parallel — each rig is independent).
	d.rigPool.runPerRig(d.ctx, d.getKnownRigs(), func(ctx context.Context, rigName string) error {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		pruneInDir(rigPath, rigName)
		return nil
	})

	// Also prune in the town root itself (mayor clone)
	pruneInDir(d.config.TownRoot, "town-root")
}

// dispatchQueuedWork shells out to `gt scheduler run` to dispatch scheduled beads.
// This avoids circular import between the daemon and cmd packages.
// Uses a 5m timeout to allow multi-bead dispatch with formula cooking and hook retries.
//
// Timeout safety: if the timeout fires mid-dispatch, a bead may be left with
// metadata written but label not yet swapped (or vice versa). The dispatch flock
// is released on process death, and dispatchSingleBead's label swap retry logic
// prevents double-dispatch on the next cycle. The batch_size config (default: 1)
// limits how many beads are in-flight per heartbeat, reducing the timeout window.
func (d *Daemon) dispatchQueuedWork() {
	// `gt scheduler run` slings queued beads (executeSling); the operator's
	// town-wide hold parks it like every other automatic dispatcher
	// (gt-ifijm). ESTOP already stops the heartbeat before this step; the
	// hold file does not, so it is checked here.
	reason := dispatch.OperatorHold(d.config.TownRoot)
	if d.queuedWorkHold.Changed(reason) {
		if reason != "" {
			d.logger.Printf("Deferring scheduler dispatch: %s", reason)
		} else {
			d.logger.Printf("Resuming scheduler dispatch: operator dispatch hold lifted")
		}
	}
	if reason != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gt", "scheduler", "run")
	setSysProcAttr(cmd)
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(beads.BuildMutationRoutingBDEnv(os.Environ(), filepath.Join(d.config.TownRoot, ".beads")), "GT_DAEMON=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		d.logger.Printf("Scheduler dispatch timed out after 5m")
	} else if err != nil {
		d.logger.Printf("Scheduler dispatch failed: %v (output: %s)", err, string(out))
	} else if len(out) > 0 {
		d.logger.Printf("Scheduler dispatch: %s", string(out))
	}
}
