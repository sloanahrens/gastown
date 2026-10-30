package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/intent"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	jevconflict "github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	jevdispatch "github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/jev/drift"
	"github.com/steveyegge/gastown/internal/jev/failcause"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
	"github.com/steveyegge/gastown/internal/jev/progress"
	"github.com/steveyegge/gastown/internal/jev/rework"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/jev/stale"
	"github.com/steveyegge/gastown/internal/jev/submission"
	jevthrash "github.com/steveyegge/gastown/internal/jev/thrash"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/schedulerrun"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/steveyegge/gastown/internal/wisp"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Daemon is the town-level background service.
// It ensures patrol agents (Deacon, Witnesses) are running and detects failures.
// This is recovery-focused: normal wake is handled by feed subscription (bd activity --follow).
// The daemon is the safety net for dead sessions, GUPP violations, and orphaned work.
type Daemon struct {
	// crashSkipLog logs a polecat's "Skipping crash detection" reason once
	// per state, not every heartbeat (gt tail noise).
	crashSkipLog logLatch

	// sandboxRecordCursor is the name the last sandbox record pass ended on.
	// The next pass starts after it, so the walk rotates over the records and
	// survives records coming and going between ticks.
	sandboxRecordCursor string

	config       *Config
	patrolConfig *DaemonPatrolConfig
	tmux         sessionTmux
	logger       *log.Logger
	ctx          context.Context
	cancel       context.CancelFunc
	doltServer   *DoltServerManager

	// disabledPatrols is loaded from town settings (disabled_patrols field).
	// Provides a simple way to disable individual patrol dogs without editing
	// the daemon patrol config (settings/config.json's "daemon" section; the
	// legacy mayor/daemon.json on a town gt config migrate has not moved).
	// Checked by
	// isPatrolActive alongside patrolConfig.
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

	// execCmd runs the gt, bd and helper subprocesses the daemon builds (see
	// runCmd); nil runs them for real. Tests set it to a fakeCLI.
	execCmd cmdRunFunc

	// pluginRuns is the plugin run ledger dispatchPlugins reads cooldown gates
	// from and records runs to; nil reads the town's beads database
	// (plugin.NewRecorder). Tests set it to count a tick's ledger reads
	// (gt-3bqfy).
	pluginRuns pluginRunSource

	// getenv reads the environment the start names its owner from
	// (DetectSelf); nil reads the process environment. Tests set it so a
	// start runs under an environment fixture.
	getenv func(string) string

	// hostname names the host a manual owner's id carries (see
	// hostnameOrOS); nil reads os.Hostname. Tests set it to drive the failed
	// read, which refuses a manual start.
	hostname func() (string, error)

	// ownerProbe reads whether the owner a state.json records is running (see
	// guardStartup); nil reads docker, launchd and systemd for real. Tests set
	// it so a start never runs a supervisor command.
	ownerProbe OwnerProbe

	// logPreexisted is whether daemon.log existed before New wrote to it. New
	// logs the patrol config and PATH before Run starts, so the startup guard
	// reads this snapshot rather than the file's presence (gt-qpeei.27).
	logPreexisted bool

	// owner is what started this daemon (DetectSelf), named once at the start
	// and stamped onto every state this process saves (see saveState), so no
	// writer drops it. nil before the start names it.
	owner *Owner

	// cpuIdleFn replaces the production read of the host's CPU idle percent
	// in checkPressure (pressure.go); nil runs top. It returns an error for a
	// reading that could not be taken.
	cpuIdleFn func() (float64, error)

	// hostTop replaces the top probe hostCPUIdle samples and parses; nil runs
	// top on the host. A test injects a canned sample so its outcome does not
	// depend on the host's load (gt-fbf5n).
	hostTop func(context.Context) ([]byte, error)

	// pressureScaleWarn makes checkPressure warn once that the configured
	// CPU threshold is past the 1.0 a busy share can reach.
	pressureScaleWarn sync.Once

	// openWorkBeads opens the beads client the daemon's work-bead reads go
	// through, given bd's whole environment (see workBeads) and the deadline
	// for one read; nil is bd at bdPath run from the town root. Tests answer
	// from a beadsfake.
	openWorkBeads func(env []string, timeout time.Duration) workBeadReader

	// openRecoveryBeads opens the agent-bead client the crash path's stale
	// hook reap reads and resets (see patrolScanHost.recoveryBeads); nil is
	// the routed bd. Tests answer from a beadsfake.
	openRecoveryBeads func(env []string) beads.Client

	// openOverrunBeads opens the store the overrun stop's ownership re-check
	// reads the stopped seat's bead through (overrunStopSeams); nil is the
	// routed bd. Tests answer from a beadsfake.
	openOverrunBeads func() overrunHandBackStore

	// sandboxExists asks a gateway whether it has a sandbox of that name (see
	// patrolScanHost.sandboxStillExists); nil asks the real gateway. Tests
	// answer from a stub.
	sandboxExists func(gateway, name string) (bool, error)

	// schedulerDeps are the collaborators scheduled dispatch runs on (the
	// sling and the polecat-capacity probe, both in package cmd). Set through
	// SetSchedulerDeps; empty means this daemon does not dispatch.
	schedulerDeps schedulerrun.Deps

	// lastTownHealth is the previous health report, the baseline for the
	// heartbeat advance check. Heartbeat goroutine only.
	lastTownHealth *townhealth.Report

	// lastAttention is the previous attention tick: the queue state it wrote
	// and the rejection-note cache (see attentionCache). Heartbeat goroutine
	// only.
	lastAttention *attentionCache

	// hostProbes replaces the production reads of the attention host
	// collectors (attention_host.go); nil is production.
	hostProbes hostProbes

	// deployW is the deploy watch (deploy_watch.go), stored once it starts.
	// Its collector runs on the heartbeat goroutine and the watch itself on
	// its own, so the pointer is what they share. deployWatchMu guards the
	// start, which a town that cannot build a watch at first is allowed to
	// retry; deployWatchHold is the reason it last could not, logged once.
	deployW         atomic.Pointer[deployWatch]
	deployWatchMu   sync.Mutex
	deployWatchHold string

	// deployWatchBuild replaces the production builder (deploy_watch.go) in
	// tests; nil builds the real one.
	deployWatchBuild func() (*deployWatch, string)

	// townHealthSources replaces the health report's outside probes (Dolt
	// ping, backup root, slot status) in tests; nil probes for real.
	townHealthSources func(s *healthSources)

	// notifyRun runs the operator's health notify command with its argv (see
	// runNotifyCommand); nil execs it. Tests record the argv.
	notifyRun func(ctx context.Context, argv []string) error

	// openNoticeBeads opens the client the health-transition notice bead is
	// created through (see noticeBeads); nil is bd against the town database.
	openNoticeBeads func() notifyBeadWriter

	// prefixRegistryFn replaces the process-wide rig-prefix registry (see
	// prefixRegistry) in tests; nil reads session.DefaultRegistry().
	prefixRegistryFn func() *session.PrefixRegistry

	// openGitFn opens the git repository at a directory (see gitAt); nil
	// opens a *git.Git. Tests hand it a gitfake world.
	openGitFn func(dir string) daemonGit

	// endSandboxSeat is the step that ends a finished sandbox seat (see
	// sandboxSeatEnder); nil builds one over the daemon's own seams. Tests
	// record the seats it was asked to end.
	endSandboxSeat func(rig, name string, landing sandboxSeatLanding) bool

	// rulesCursorAt reads the stamp of the jev_rules battery's cursor (see
	// readRulesCursor) when set; nil reads it with battery.ReadCursor. A seam
	// for the unit tier, and for the unreadable cursor a real file need not
	// produce (gt-381oj).
	rulesCursorAt func(path string) (time.Time, bool, error)

	// sandboxSeatEndMu guards sandboxSeatEndStopped together with the count
	// below, so no seat end is added to the wait shutdown joins (see
	// startSandboxSeatEnd, stopSandboxSeatEnds).
	sandboxSeatEndMu      sync.Mutex
	sandboxSeatEndStopped bool
	// sandboxSeatEndCycles counts the sandbox seat ends in flight (see
	// startSandboxSeatEnd); shutdown joins them and tests wait on it.
	sandboxSeatEndCycles sync.WaitGroup

	// buildCommitFn replaces the daemon's own build commit (see buildCommit)
	// in tests; nil reads the version package's.
	buildCommitFn func() string

	// seams replaces the heartbeat, upgrade and main-branch
	// collaborators in tests (see daemonSeams); the zero value is production.
	seams daemonSeams

	// maint replaces scheduled maintenance's side effects in tests (see
	// maintenanceSeams); the zero value is production.
	maint maintenanceSeams

	// diskMaint replaces the disk_maintenance patrol's side effects in tests
	// (see diskMaintSeams); the zero value is production.
	diskMaint diskMaintSeams
	// diskMaintRunning is that patrol's single-flight guard.
	diskMaintRunning atomic.Bool
	// diskMaintSettled is when it last settled a window, as UnixNano: the
	// pass runs off the loop goroutine, so the marker it writes is atomic and
	// the loop only reads it.
	diskMaintSettled atomic.Int64
	// diskMaintSchedule is when each target with a cadence of its own last
	// concluded a run, and the settings the patrol refused to schedule
	// (gt-z6qre.17).
	diskMaintSchedule diskMaintSchedule
	// diskMaintAlerts is the set of disk alert keys the patrol has open: it
	// alerts a disk once, and a clear needs a reading that says so.
	diskMaintAlerts diskMaintAlerts

	// hmSync replaces the hm_sync patrol's clock and run seam in tests (see
	// hmSyncSeams); the zero value is production.
	hmSync hmSyncSeams
	// hmSyncRunning is that patrol's single-flight guard.
	hmSyncRunning atomic.Bool
	// hmSyncLastRun is when it last finished a sync, as UnixNano: the pass runs
	// off the loop goroutine, so the marker it writes is atomic and the loop
	// only reads it.
	hmSyncLastRun atomic.Int64
	// hmSyncAlerts is that patrol's memory of whether hmsync:stale is open, and
	// the alert functions a test replaces (see hm_sync_alert.go).
	hmSyncAlerts hmSyncAlerts

	// rigOperational memoizes each rig's docked/parked determination for a short
	// window, so the many per-rig-per-heartbeat call sites share one lookup
	// instead of each paying a bd subprocess - which, on a CPU-starved host,
	// costs its entire 60s budget (gt-4nu3).
	//
	// Concurrent by design: isRigOperational runs on the heartbeat goroutine
	// (patrol rig filters) and on rigPool workers (witness/refinery auto-start).
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

	// scripts tracks script-type plugins whose run.sh is executing
	// in-process (gt-fo2k). Lazily created; safe for concurrent use.
	scripts     *scriptRunner
	scriptsOnce sync.Once

	// supervisor holds the only Kill and Restart the daemon uses (see
	// sup()); tests may set it, otherwise it is built on first use.
	supOnce    sync.Once
	supervisor *supervisor.Supervisor
	// restartSeatFn replaces restartSeat, the supervisor's restart executor,
	// in tests that cannot run the role managers against a fake tmux.
	restartSeatFn func(supervisor.Seat) error

	// patrolRestartFails holds each seat's run of consecutive failed
	// patrol-scan restarts under its alert key, so a seat whose restart
	// keeps failing the same way is escalated once instead of retried
	// silently (gt-6hhp8). patrolRestartTried holds the keys a scan of the
	// current rig tried to restart, so the scan's end can break the runs it
	// did not continue. Created on first write; guarded by the mutex.
	// patrolRestartCleared holds the keys whose alert this process has cleared
	// since the seat's last failed restart, so a steady success does not ask
	// the town to clear again (gt-7tz5f).
	patrolRestartFailsMu sync.Mutex
	patrolRestartFails   map[string]patrolRestartFailure
	patrolRestartTried   map[string]bool
	patrolRestartCleared map[string]bool

	// jsonlPushFailures tracks consecutive git push failures for JSONL backup.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	jsonlPushFailures int

	// jsonlExportAlert is the partial export failure this process last raised
	// the export alert for, so an unchanged failure is not raised again every
	// tick (jsonl_git_backup.go, gt-ga9hx). Only the jsonl_git_backup goroutine
	// touches it — jsonlGitBackupRunning below keeps one cycle in flight.
	jsonlExportAlert string

	// jsonlPushFileLimit is the per-file size the JSONL backup treats as too
	// large for its remote, in tests that drive the oversize-push recovery
	// with a small file instead of a real 100 MB export; 0 is GitHub's
	// limit (jsonlPushFileLimitBytes).
	jsonlPushFileLimit int64

	// jsonlExportFn replaces exportDatabaseToJsonl in tests, so a JSONL backup
	// cycle's own bookkeeping (counts, failures, alerts) can be driven without
	// running dolt; nil is the real export.
	jsonlExportFn func(db, gitRepo, dataDir string, scrub bool) (int, error)

	// jsonlTableExportFn replaces exportTableToJsonl in tests, so
	// exportDatabaseToJsonl's handling of a failing table can be driven without
	// running dolt; nil is the real export. budget is the time the export has
	// been given, the shared supplemental bound included.
	jsonlTableExportFn func(table string, q beadsql.Query, dir, dataDir string, budget time.Duration) (int, error)

	// jsonlTrackerExportFn replaces the tracker client behind the JSONL
	// backup's tracker branch in tests, so a tracker rig's export is driven
	// with records a test wrote; nil is the client built from the environment
	// (jsonl_tracker_export.go).
	jsonlTrackerExportFn trackerRigExporter

	// jsonlTrackerDBs holds the databases the current backup cycle exported
	// through the tracker, so the commit subject can mark their counts and the
	// spike guard can tell a tracker-measured count from a Dolt one
	// (jsonlTrackerCountToken). The cycle fills it before the export loop;
	// empty or nil means no database was read through the tracker. Only the
	// jsonl_git_backup goroutine touches it.
	jsonlTrackerDBs map[string]bool

	// dogFeedFn records a dog cycle's failed outcome in the town feed. Nil
	// writes the real feed; tests capture the event instead.
	dogFeedFn func(eventType string, payload map[string]interface{}) error

	// rebuildGTStaleFn reads binary staleness for the rebuild_gt job (see
	// rebuild_gt.go); nil is version.CheckStaleBinaryFresh, which shells out
	// to git. rebuildGTGateFn reads the container-gate picture; nil is the
	// slot package, which shells out to docker. Tests answer both.
	rebuildGTStaleFn func(repoRoot string) *version.StaleBinaryInfo
	rebuildGTGateFn  func() (busy string, reservable bool)
	// slotGate is the container gate the quiet check reads its report through;
	// nil is slot.NewGate(), which lists the host's docker. A test supplies a
	// gate over a fake runtime, so the quiet check's container classification
	// (internal/scratch) runs through the same code the host's docker feeds
	// rather than a report a test built by hand (gt-4gixm).
	slotGate *slot.Gate
	// rebuildGTInstallYieldFn answers the pool's install cap, the duration at
	// which the job escalates a binary that stays due; nil is the town pool's
	// Pool.InstallYield. Tests set a cap so the alarm can be driven at a value
	// other than the default (gt-pa4tg).
	rebuildGTInstallYieldFn func() time.Duration
	// rebuildGTPendingFn answers where the pending-install marker lives; nil is
	// installpending.StoreFor(townRoot), which honors INSTALL_GT_DAEMON_DIR the
	// way the Go installer does. Tests point it at their own temp file, so
	// no test can reach the operator's marker through that variable (gt-vc34c).
	rebuildGTPendingFn func() installpending.Store

	// omVersionFn reads the installed om's version sha and omHeadFn reads the
	// om rig's origin/main tip, the two readings the om-skew check compares
	// (see om_skew.go); nil is the real `om version` call and the real git
	// reads. Tests answer both, so no binary and no remote runs.
	omVersionFn func(omPath string) (string, error)
	omHeadFn    func(townRoot string) (string, error)

	// omSkew is the om-skew monitor's start guard and alert state (see
	// om_skew.go).
	omSkew omSkewMonitor

	// lastDoltWarningTime is when Dolt health warnings were last reported.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	lastDoltWarningTime time.Time

	// lastMaintenanceRun tracks when scheduled maintenance last ran.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	lastMaintenanceRun time.Time

	// maintenanceGCRunning is set while a scheduled_maintenance gc cycle runs
	// on its own goroutine; it blocks a second cycle and an upgrade-restart.
	maintenanceGCRunning atomic.Bool
	// catchUpAttemptAt is the UnixNano time of the last out-of-window
	// catch-up backup attempt (or unreadable-backup-root report); it bounds
	// retries to one per maintenanceCatchUpRetry (gt-wne04).
	catchUpAttemptAt atomic.Int64
	// catchUpGCAttemptAt is the UnixNano time of the last out-of-window
	// catch-up gc attempt. It is the gc's own throttle (gt-uux5b): a failing
	// gc escalates once and the outside-the-window check fires every 5
	// minutes, and sharing catchUpAttemptAt would let it starve the backup
	// catch-up.
	catchUpGCAttemptAt atomic.Int64
	// catchUpDeferMu guards the two fields below: the last catch-up backup
	// deferral logged and when. A held lock keeps the deferral true on every
	// 5-minute tick, so a reason is logged once per maintenanceCatchUpRetry
	// instead of on every tick.
	catchUpDeferMu       sync.Mutex
	catchUpDeferWhy      string
	catchUpDeferLoggedAt time.Time
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
	// gcStateUnreadableRaised is set while this process holds the
	// dolt-gc-state-unreadable alert open; the state file that would record it
	// is the one that cannot be read (maintenance_gc_stale.go).
	gcStateUnreadableRaised atomic.Bool
	// staleGCCheckRunning is set while a stale-gc check is off the select loop
	// delivering its alerts; a tick that finds it set skips rather than stack a
	// second check behind a slow gt escalate (gt-3v0sf).
	staleGCCheckRunning atomic.Bool
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

	// compactorDogCycles tracks the in-flight cycle goroutine, so a caller can
	// wait for one to finish (its last-run record included).
	compactorDogCycles sync.WaitGroup

	// compactorDogCycleFn replaces the compactor's monitoring cycle (see
	// compactorDogCycle) in tests; nil runs the real one.
	compactorDogCycleFn func() bool

	// reaperWriterForFn resolves the bd writer a live reaper run writes
	// through (see reaperWriter); nil uses reaper.WriterForDatabase. Tests set
	// it so no bd runs.
	reaperWriterForFn func(townRoot, dbName string) (reaper.Writer, error)

	// rigPool runs per-rig heartbeat operations (witness checks, refinery checks,
	// polecat health, idle reaping, branch pruning) with bounded concurrency and
	// per-rig context timeouts so one slow rig cannot block all others.
	rigPool *RigWorkerPool

	// knownRigsCache memoizes the result of reading the rig registry
	// (mayor/town.json's "registry" section on a two-file town,
	// mayor/rigs.json on a five-file one) for the duration of a single
	// heartbeat tick. ~10 call sites per tick otherwise re-read and re-parse
	// the same document. Invalidated at the start of each heartbeat so
	// registry changes between ticks are picked up.
	//
	// knownRigsMu guards both fields. The memo is per-tick, but background
	// patrols run on their own goroutines (gt-uvxy) and read the cache, so
	// the heartbeat's invalidation and those reads would otherwise touch the
	// fields concurrently (gt-f18v).
	knownRigsMu         sync.Mutex
	knownRigsCache      []string
	knownRigsCacheValid bool

	// legacySocketCleanupOnce ensures upgrade cleanup only runs once per daemon
	// lifetime, before any patrol agent can be started on the current socket.
	legacySocketCleanupOnce sync.Once

	// upgradeRestartRequested is set by checkUpgradeRestart; the run loop
	// then shuts down (leaving Dolt running) and Run returns
	// ErrRestartForUpgrade.
	upgradeRestartRequested atomic.Bool
	// upgradeWait* track the pending restart marker for the stuck and
	// no-effect alarms. Run-loop goroutine only (startup and heartbeat).
	upgradeWaitCommit    string
	upgradeWaitSince     time.Time
	upgradeSlingFrozen   bool      // the drain froze the sling wait (gt-nosod): only the slings in flight then hold the restart
	upgradeSlingSince    time.Time // the youngest in-flight sling's start when the drain began, which the restart's sling wait runs from (zero = no drain)
	upgradeGCHoldSince   time.Time // when the drain's clock for the maintenance gc call in flight started: the call's start, moved forward over hard-hold time (zero = none)
	upgradeGCCallStart   int64     // UnixNano start of the gc call upgradeGCHoldSince times (0 = none)
	upgradeGCBeat        time.Time // the last heartbeat that read the gc hold
	upgradeWaitEscalated bool
	upgradeWaitLogged    string    // the hold the last wait line named (the hold's label while it holds the restart)
	upgradeDrainLogged   bool      // the once-per-marker drain line has been logged
	upgradeDrainSince    time.Time // when the drain line was written, so its end can say how long landings were held (zero = no drain)
	upgradeDrainWait     string    // the last hold a wait line named while the drain was on, for the drain end line

	// scheduledSlingsRunning is the single-flight guard for the scheduled_slings
	// patrol, on its own goroutine so a slow sling never
	// blocks the select loop (gt-nj23).
	scheduledSlingsRunning atomic.Bool

	// scheduledSlingRunner is nil in production (an exec runner is built on
	// first use) and a fake in tests.
	scheduledSlingRunner scheduledSlingRunner

	// scheduledSlingTimeout overrides scheduledSlingCommandTimeout; tests set
	// it so a sling can hit its deadline without waiting five minutes.
	scheduledSlingTimeout time.Duration

	// jsonlLocalPhaseTimeout overrides gitLocalPhaseTimeout, the budget for one
	// backup tick's local git staging; tests set it to a spent phase.
	jsonlLocalPhaseTimeout time.Duration

	// jsonlSupplementalTimeout overrides jsonlSupplementalExportTimeout, the
	// bound on one database's supplemental tables together; tests set it to a
	// short bound instead of exporting six tables over a real five minutes.
	jsonlSupplementalTimeout time.Duration

	// scheduledSlingFailures counts consecutive failures per entry name; the
	// third escalates once, success resets. Touched only by the patrol goroutine.
	scheduledSlingFailures map[string]int

	// scheduledSlingEscalate defaults to d.escalate; tests capture it.
	scheduledSlingEscalate func(source, message string)

	// scheduledGateFailures counts consecutive gate_command failures per entry
	// name. The patrol goroutine writes it and the attention tick reads it.
	scheduledGateFailures scheduledGateCounts

	// scheduledSlingsHold and queuedWorkHold remember the operator dispatch
	// hold each dispatcher last saw, so a hold is logged once when it appears,
	// changes, or lifts rather than on every tick (gt-ifijm).
	scheduledSlingsHold dispatch.HoldLatch
	queuedWorkHold      dispatch.HoldLatch

	// specDispatchRunning / specDispatchCycles are the spec_dispatch ticker's
	// single-flight guard and cycle count (gt-4k3fj.5, spec_dispatch.go), and
	// specDispatchHold remembers the town hold the ticker last saw so a
	// parked dispatcher is logged once per hold (gt-y6ovz).
	specDispatchRunning atomic.Bool
	specDispatchCycles  sync.WaitGroup
	specDispatchHold    dispatch.HoldLatch

	// specCensusRemove removes the spec dispatcher's census file before a tick
	// (spec_dispatch.go); nil removes the real file. A test whose clear fails
	// sets it (gt-139dp).
	specCensusRemove func(path string) error

	// specDispatchTick is when the running tick started, the clock an upgrade
	// restart waits on (upgrade_restart.go, gt-cbaoy), published before
	// specDispatchRunning is set and cleared with it (gt-32j1y);
	// specDispatchDeferLogged latches the line that says a pending restart is
	// holding new ticks.
	specDispatchTick        atomic.Pointer[specDispatchTickRun]
	specDispatchDeferLogged atomic.Bool

	// specSlings is the slings the ticks planned and the daemon still has
	// running in the background (spec_slings.go, gt-5h70s).
	specSlings specSlings

	// specSeats is the seat settings the ticker read last, and specSeatHold
	// remembers the read error it last saw, so a seat file that does not parse
	// parks the ticker with one line per distinct error (spec_seats.go,
	// gt-jdnab).
	specSeats    specSeatTracker
	specSeatHold dispatch.HoldLatch

	// specPatrolHold remembers the patrol config read error the tick bound
	// last fell back on, so an unreadable block is logged once per distinct
	// error (spec_dispatch.go, gt-zeqqg).
	specPatrolHold dispatch.HoldLatch

	// dispatchTicks is the spec_dispatch ticker's recent tick decisions,
	// oldest first, which townhealth's dispatch field judges to tell a
	// dispatcher that is off from one that is stalled (gt-xiw7o). The tick's
	// seq is what a background sling's outcome names to correct the counts it
	// was included in (gt-pk2q8).
	dispatchTicksMu sync.Mutex
	dispatchTicks   []recordedDispatchTick
	dispatchTickSeq atomic.Uint64

	// dispatchIdle counts the ticks that had a free seat and a dispatchable
	// bead and started nothing, which the dispatch.idle rule judges (gt-wil0h).
	dispatchIdle dispatchIdleTracker

	// specUnshapedWarned remembers the beads the spec_dispatch ticker has
	// warned about as unshaped, so a bead held unshaped every tick warns on the
	// first tick that sees it, not on each (gt-gzav5).
	specUnshapedWarnedMu sync.Mutex
	specUnshapedWarned   map[string]bool

	// overseerReportRunning / overseerReportCycles are the overseer_report
	// patrol's single-flight guard and cycle count (gt-a6fg7,
	// overseer_report.go); overseerReportSeams replaces its sources in tests.
	overseerReportRunning atomic.Bool
	overseerReportCycles  sync.WaitGroup
	overseerReportSeams   *overseerReportSeams

	// patrolScanRunning / patrolScanCycles are the patrol_scan tick's
	// single-flight guard and cycle count (gt-4k3fj.6, patrol_scan.go).
	patrolScanRunning atomic.Bool
	patrolScanCycles  sync.WaitGroup

	// reapAlertState is the blocked-seat alert pass's memory between ticks,
	// and reapAlertSink is where it sends; a nil sink uses
	// escalateAlert/clearAlerts (patrol_scan_reap_alert.go). The tick is
	// single-flight, so only its goroutine touches the state.
	reapAlertState reapAlertState
	reapAlertSink  reapAlertSink

	// landingPasses counts landing-worker passes in flight, one per rig at
	// most: a restart mid-pass kills the merged-tree gate of the bead being
	// landed (gt-u641b), so isIdleForUpgrade waits for it to reach zero.
	landingPasses atomic.Int32

	// landingPassFn, when non-nil, is called on the landing loop's goroutine
	// once a pass's report has been applied, before the next pass starts. Test
	// seam (gt-v0t6k): the wait helpers in landing_drain_test.go block on it
	// rather than on a runtime.Gosched budget, which runs out under load
	// before the landing goroutine is scheduled. It must not block; nil in
	// production.
	landingPassFn func()

	// upgradeRestartPending is true while a restart marker the daemon must
	// act on exists and the landing drain is on. The landing workers drain on it:
	// they finish the pass in flight and start no new one, so the restart finds
	// an idle moment (gt-nxvpe). Cleared when the marker is gone or covered.
	upgradeRestartPending atomic.Bool

	// upgradeTickWaiting is true while the restart waits on the spec dispatch
	// tick in flight with the drain deferred, where upgradeRestartPending is
	// false and cannot keep a new tick from starting behind the one it waits on
	// (gt-u1fzb). The ticker is parked while either flag is set, so the
	// transition between them sets the arriving flag before it clears the
	// leaving one; see deferUpgradeDrain and endDispatchTickWait.
	upgradeTickWaiting atomic.Bool

	// landingDrainedCh carries one wake per drained landing pass, so the run
	// loop restarts at the pass's end instead of at the next heartbeat
	// (gt-fzwcd). See landingDrained.
	landingDrainedOnce sync.Once
	landingDrainedCh   chan struct{}

	// landingStates maps rig -> its landing pass: the bead being landed and
	// when it started. The restart's wait line reads the bead; the attention
	// queue's landing-stuck collector reads the times (gt-vsct7.3). The
	// queue-stuck collector judges the waiting beads' own ages instead
	// (gt-m36as).
	landingStates landingStates

	// postLandRuns counts rigs whose post-landing runner has a run in flight
	// or queued. It runs outside the landing pass, so checkUpgradeRestart
	// holds a restart for it separately, up to postLandRestartCap (gt-gb4ij).
	postLandRuns atomic.Int32

	// jsonlGitBackupRunning, wispReaperRunning, and checkpointDogRunning are
	// the single-flight guards for their patrols, on their own goroutines —
	// the same gt-ima2 shape as compactor_dog
	// above, applied by gt-gxpwc so a restart cannot starve these patrols by
	// resetting an in-process ticker's countdown. Due-ness for all three is
	// decided against the persisted last-run time (patrol_last_run.go), not
	// against the ticker.
	jsonlGitBackupRunning atomic.Bool
	wispReaperRunning     atomic.Bool
	checkpointDogRunning  atomic.Bool

	// overrunGuardRunning is the overrun_guard patrol's single-flight guard
	// (overrun_guard.go).
	overrunGuardRunning atomic.Bool

	// gitHygieneRunning is the git_hygiene patrol's single-flight guard
	// (git_hygiene.go).
	gitHygieneRunning atomic.Bool

	// landingInfra is the landing_infra probe's last observation, read by the
	// town health computation (townhealth.go) and written by its patrol
	// (landing_infra.go, gt-fn9e6.11); landingInfraRunning is the patrol's
	// single-flight guard.
	landingInfra        landingInfraState
	landingInfraRunning atomic.Bool
	// landingInfraRigs lists the rigs the probe covers, and
	// landingInfraProber makes its two HTTP calls; nil means the real ones.
	// Test seams (landing_infra.go).
	landingInfraRigs   func() []landingInfraTarget
	landingInfraProber infraProber

	// rebuildGTRunning is the rebuild_gt job's single-flight guard,
	// rebuildGTRequested is the landing worker's sticky request to install at
	// the next heartbeat, and rebuildGTBlock is how long the current "due and
	// not installed" episode has run (rebuild_gt.go).
	rebuildGTRunning   atomic.Bool
	rebuildGTRequested atomic.Bool
	rebuildGTBlock     rebuildGTBlock
	// rebuildGTRequestMu makes rebuildGTRequested and rebuildGTInstallNow one
	// request rather than two flags: a gt:install-now marker is armed and
	// consumed with the request it belongs to, so it can neither be lost with
	// a request a cycle is taking nor be left behind for a later plain one.
	rebuildGTRequestMu sync.Mutex
	// rebuildGTInstallNow marks the pending request as one a gt:install-now
	// landing made: it skips the minimum install interval a plain landing's
	// request waits out.
	rebuildGTInstallNow atomic.Bool
	// rebuildGTWindowLogged is the latch on the "waiting for the window" log
	// line, so one held request logs once rather than once per heartbeat; a
	// cycle that runs resets it.
	rebuildGTWindowLogged atomic.Bool

	// rebuildGTCycleDoneFn, when non-nil, is called on the cycle's own
	// goroutine as it ends, after rebuildGTRunning clears. Test seam
	// (gt-v0t6k): awaitRebuildGTIdle blocks on it rather than on a
	// runtime.Gosched budget, which runs out under load before the cycle
	// goroutine is scheduled. It must not block; nil in production.
	rebuildGTCycleDoneFn func()

	// rebuildOMRunning is the rebuild_om job's single-flight guard, and
	// rebuildOMBlock is how long the current "om stale and not installed"
	// episode has run (rebuild_om.go). rebuildOMCycleDoneFn is the test seam
	// rebuildGTCycleDoneFn is for rebuild_gt.
	rebuildOMRunning     atomic.Bool
	rebuildOMBlock       rebuildGTBlock
	rebuildOMCycleDoneFn func()

	// rebuildOMInstallFn replaces the in-process install in tests; nil runs
	// ominstaller.Run (rebuild_om.go).
	rebuildOMInstallFn rebuildOMInstallFunc

	// jevDedupeRunning is the jev_dedupe pass's single-flight guard
	// (jev_dedupe.go). A pass makes network calls, so it runs on its own
	// goroutine and the heartbeat never waits for it.
	jevDedupeRunning atomic.Bool
	// jevDedupeOpen builds the pass a run executes. Nil means dedupe.Open,
	// which wires the live Jev client, hm and the rig's beads; a test sets it
	// to hand the run a pass over fakes.
	jevDedupeOpen func(townRoot string, s dedupe.Settings, warnings io.Writer) (*dedupe.Pass, error)
	// jevDedupeRun is the completion time of the last pass this process ran
	// (jev_dedupe.go), which can be newer than the persisted record when the
	// write failed, and stands in for it when the write left no record at all.
	// Zero until the first pass completes.
	jevDedupeRun patrolLastRunMemory

	// jevAttentionRunning is the jev_attention pass's single-flight guard
	// (jev_attention.go), kept apart from jev_dedupe's so the two passes run
	// independently.
	jevAttentionRunning atomic.Bool
	// jevAttentionOpen builds the pass a run executes. Nil means
	// jevattention.Open; a test sets it to hand the run a pass over fakes.
	jevAttentionOpen func(townRoot string, s jevattention.Settings, warnings io.Writer) (*jevattention.Pass, error)

	// jevLandingRunning and jevLandingOpen are the jev_landing pass's
	// single-flight guard and test seam (jev_landing.go), as jevDedupeRunning
	// and jevDedupeOpen are for jev_dedupe.
	jevLandingRunning atomic.Bool
	jevLandingOpen    func(townRoot string, s landing.Settings, warnings io.Writer) (*landing.Pass, error)

	// jevSubmissionRunning and jevSubmissionOpen are the jev_submission
	// pass's single-flight guard and test seam (jev_submission.go), as
	// jevLandingRunning and jevLandingOpen are for jev_landing. jevSubmissionRun
	// is its in-memory last-run time, as jevDedupeRun is jev_dedupe's.
	jevSubmissionRunning atomic.Bool
	jevSubmissionOpen    func(townRoot string, s submission.Settings, warnings io.Writer) (*submission.Pass, error)
	jevSubmissionRun     patrolLastRunMemory

	// jevDriftRunning and jevDriftOpen are the jev_drift pass's single-flight
	// guard and test seam (jev_drift.go), as jevLandingRunning and
	// jevLandingOpen are for jev_landing. jevDriftRun is its in-memory last-run
	// time, as jevDedupeRun is jev_dedupe's.
	jevDriftRunning atomic.Bool
	jevDriftOpen    func(townRoot string, s drift.Settings, warnings io.Writer) (*drift.Pass, error)
	jevDriftRun     patrolLastRunMemory

	// jevLogtriageRunning and jevLogtriageOpen are the jev_logtriage pass's
	// single-flight guard and test seam (jev_logtriage.go), as jevDriftRunning
	// and jevDriftOpen are for jev_drift.
	jevLogtriageRunning atomic.Bool
	jevLogtriageOpen    func(townRoot string, s logtriage.Settings, warnings io.Writer) (*logtriage.Pass, error)

	// jevReworkRunning and jevReworkOpen are the jev_rework pass's
	// single-flight guard and test seam (jev_rework.go), as
	// jevLogtriageRunning and jevLogtriageOpen are for jev_logtriage.
	jevReworkRunning atomic.Bool
	jevReworkOpen    func(townRoot string, s rework.Settings, warnings io.Writer) (*rework.Pass, error)

	// jevFailcauseOpen builds the cause battery's pass for a rig's candidate
	// gate (jev_failcause.go). Nil means failcause.Open; a test sets it to use
	// a fake asker.
	jevFailcauseOpen func(townRoot string, s failcause.Settings, warnings io.Writer) (*failcause.Pass, error)

	// jevDispatchRunning is the jev_dispatch pass's single-flight guard
	// (jev_dispatch.go), for the same reason as jevDedupeRunning.
	jevDispatchRunning atomic.Bool
	// jevDispatchOpen builds the pass a run executes. Nil means jevdispatch.Open;
	// a test sets it to hand the run a pass over fakes.
	jevDispatchOpen func(townRoot string, s jevdispatch.Settings, warnings io.Writer) (*jevdispatch.Pass, error)

	// jevThrashRunning is the jev_thrash pass's single-flight guard
	// (jev_thrash.go), for the same reason as jevDispatchRunning.
	jevThrashRunning atomic.Bool
	// jevThrashOpen builds the pass a run executes. Nil means jevthrash.Open;
	// a test sets it to hand the run a pass over fakes.
	jevThrashOpen func(townRoot string, s jevthrash.Settings, warnings io.Writer) (*jevthrash.Pass, error)

	// jevRulesRunning is the jev_rules pass's single-flight guard
	// (jev_rules.go), for the same reason as jevDispatchRunning.
	jevRulesRunning atomic.Bool
	// jevRulesOpen builds the pass a run executes. Nil means jevrules.Open; a
	// test sets it to hand the run a pass over fakes.
	jevRulesOpen func(townRoot string, s jevrules.Settings, warnings io.Writer) (*jevrules.Pass, error)

	// jevConflictRunning is the jev_conflict pass's single-flight guard
	// (jev_conflict.go), and jevConflictOpen builds the pass a run executes.
	// Nil means jevconflict.Open; a test sets it to hand the run fakes.
	jevConflictRunning atomic.Bool
	jevConflictOpen    func(townRoot string, s jevconflict.Settings, warnings io.Writer) (*jevconflict.Pass, error)

	// jevProgressQueue holds the nudges the overrun guard delivered and the
	// progress pass has not yet asked about (jev_progress.go). jevProgressRunning
	// is the pass's single-flight guard. jevProgressOpen builds the pass a run
	// executes; nil means progress.Open, and a test sets it to use fakes.
	jevProgressQueue   progress.Queue
	jevProgressRunning atomic.Bool
	jevProgressOpen    func(townRoot string, q *progress.Queue, s progress.Settings, warnings io.Writer) (*progress.Pass, error)
	// jevProgressLatest finds a seat's live transcript from its worktree when a
	// nudge is queued. Nil means agentlog.LatestTranscript for the town root; a
	// test sets it so the lookup does not reach the Claude config dir.
	jevProgressLatest func(workDir string) (string, error)

	// jevStaleRunning is the jev_stale read's single-flight guard
	// (jev_stale.go). jevStaleOpen builds the pass a run executes; nil means
	// stale.Open, and a test sets it to use fakes. jevStaleRun is its in-memory
	// last-run time, as jevDedupeRun is jev_dedupe's.
	jevStaleRunning atomic.Bool
	jevStaleOpen    func(townRoot string, rigs []string, s stale.Settings, warnings io.Writer) (*stale.Pass, error)
	jevStaleRun     patrolLastRunMemory

	// jevNotesRunning is the jev_notes read's single-flight guard
	// (jev_notes.go). jevNotesOpen builds the pass a run executes; nil means
	// jevnotes.Open, and a test sets it to use fakes.
	jevNotesRunning atomic.Bool
	jevNotesOpen    jevNotesOpener

	// The in-memory last-run times of the patrols below, as jevDedupeRun is
	// jev_dedupe's: each outranks the persisted record when a save failed, so
	// an unwritable daemon directory cannot make the patrol due on every
	// heartbeat (gt-th2am, gt-cekkh).
	jevAttentionRun   patrolLastRunMemory
	jevLandingRun     patrolLastRunMemory
	jevLogtriageRun   patrolLastRunMemory
	jevReworkRun      patrolLastRunMemory
	jevConflictRun    patrolLastRunMemory
	jevNotesRun       patrolLastRunMemory
	jevRulesRun       patrolLastRunMemory
	jevDispatchRun    patrolLastRunMemory
	jevThrashRun      patrolLastRunMemory
	wispReaperRun     patrolLastRunMemory
	tierSweepRun      patrolLastRunMemory
	rebuildOMRun      patrolLastRunMemory
	jsonlGitBackupRun patrolLastRunMemory
	overrunGuardRun   patrolLastRunMemory
	eventsPruneRun    patrolLastRunMemory
	gitHygieneRun     patrolLastRunMemory
	checkpointDogRun  patrolLastRunMemory
	rogueBDRun        patrolLastRunMemory
	// ghGatesRun holds one *patrolLastRunMemory per database's gh gate check,
	// keyed by its last-run key (patrol_scan.go).
	ghGatesRun sync.Map

	// samplerPatrol is the resource_sampler patrol's sampler and pacing,
	// built on its first tick. newResourceSampler builds it: nil means the
	// host's own ps, and a test sets it to hand the patrol a fake
	// (resource_sampler.go).
	samplerPatrol      *resourceSamplerPatrol
	resourceSamplerMu  sync.Mutex
	newResourceSampler newResourceSamplerFn

	// burst is the burst sampler (burst_sampler.go), built on its first
	// start; burstSeams replaces its sample, its wait and its Env in tests,
	// and the zero value is production.
	burst      *burstSampler
	burstMu    sync.Mutex
	burstSeams burstSamplerSeams
	// landingQueue is the ready-to-land count each rig's landing worker
	// published at the top of its last pass, which the burst sampler reads in
	// place of listing every rig's board on every sample (landing_worker.go,
	// gt-hw6ih).
	landingQueue landingQueueState

	// containerMode says this daemon runs inside the town's container, where
	// top and ps read the container's own process tree rather than the host's.
	// New settles it once from config.ContainerModeEnv (the rendered compose
	// file sets it); a Daemon a test builds by hand keeps it false, so the
	// process running the tests cannot change what an existing burst test
	// samples (burst_sampler.go).
	containerMode bool

	// tierSweepRunning is the tier_sweep job's single-flight guard
	// (tier_sweep.go). A cycle runs on its own goroutine outside the idle
	// predicate, so checkUpgradeRestart holds a restart for it separately, up
	// to tierSweepRestartCap (gt-ccyw0).
	tierSweepRunning atomic.Bool
	// tierSweepStartedAt is the UnixNano start of the cycle running now, 0 when
	// none is. The restart's sweep hold is measured from it as well as from the
	// marker's wait clock, so a sweep the stale bound let through while the
	// restart had already waited gets its own bound instead of being cut on the
	// heartbeat that follows (gt-zvqn8).
	tierSweepStartedAt atomic.Int64
	// tierSweepSkipLogged is true once the job has logged that a pending
	// upgrade restart deferred its cycle; triggerTierSweep clears it when no
	// restart is pending, so the line appears once per restart.
	tierSweepSkipLogged atomic.Bool
	// tierSweepStaleLogged is the same, for the line saying the owed sweep is
	// past the stale bound and so runs despite the restart. Shared with the
	// skip line's latch, a restart that already deferred a younger sweep would
	// swallow it (gt-icmdq).
	tierSweepStaleLogged atomic.Bool
	// tierSweepSeams replace the tier_sweep job's outside effects in tests;
	// the zero value is production (see tier_sweep.go).
	tierSweepSeams tierSweepSeams

	// goos is the platform the platform-gated patrols decide on (see
	// platform); "" is runtime.GOOS. Tests set it to reach a gated path.
	goos string
}

// platform is the GOOS the daemon's platform-gated patrols decide on.
func (d *Daemon) platform() string {
	if d.goos != "" {
		return d.goos
	}
	return runtime.GOOS
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

	// doltWarningCooldown is the minimum interval between reports of Dolt
	// health warnings.
	doltWarningCooldown = 5 * time.Minute
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
		processEnv{}.Setenv("PATH", newPath)
		logger.Printf("PATCH-007: augmented daemon PATH with user/local bin dirs (was=%q, now=%q)", current, newPath)
	}
}

// cleanupLegacySockets removes the town's legacy default- and base-socket
// sessions (see daemonSeams.cleanupLegacySocketsFor).
func cleanupLegacySockets(reg *session.PrefixRegistry, townRoot string) (int, int) {
	defaultCleaned := session.CleanupLegacyDefaultSocket(reg)
	baseCleaned := session.CleanupLegacyBaseSocket(reg, townRoot)
	return defaultCleaned, baseCleaned
}

// New creates a new daemon instance.
func New(config *Config) (*Daemon, error) {
	// Ensure daemon directory exists
	daemonDir := filepath.Dir(config.LogFile)
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		return nil, fmt.Errorf("creating daemon directory: %w", err)
	}
	// Taken before the first log line below, which creates daemon.log on a
	// town that has none (gt-qpeei.27).
	_, logStatErr := os.Stat(config.LogFile)
	logPreexisted := !os.IsNotExist(logStatErr)

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
	if town, err := townconfig.Load(config.TownRoot); err == nil {
		if msg := town.LiteralSecretsWarning(); msg != "" {
			logger.Print(msg)
		}
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
	// sessionPrefixPattern) can read it without a cwd walk.
	processEnv{}.Setenv("GT_TOWN_ROOT", config.TownRoot)

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

	// Load the daemon patrol config (settings/config.json's "daemon" section;
	// the legacy mayor/daemon.json on a town gt config migrate has not moved),
	// ensuring
	// lifecycle defaults are populated for any missing data maintenance
	// tickers. Without this, opt-in patrols (compactor, reaper, doctor, JSONL
	// backup, dolt backup) remain disabled if the config was created before
	// they were implemented.
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
		// Name the file the config was read from: on a migrated town it is
		// settings/config.json, not the retired mayor/daemon.json (gt-y3pgh.12).
		source := PatrolConfigSource(config.TownRoot)
		logger.Printf("Loaded patrol config from %s", source)
		// Propagate the daemon config's env to this process and all spawned sessions.
		for k, v := range patrolConfig.Env {
			processEnv{}.Setenv(k, v)
			if agentconfig.LooksLikeSecret(k, v) {
				logger.Printf("Set env %s=<redacted> from %s", k, source)
			} else {
				logger.Printf("Set env %s=%s from %s", k, v, source)
			}
		}
	}
	doltEnv := agentconfig.ConfiguredDoltEnv(config.TownRoot)
	for _, key := range agentconfig.DoltEndpointEnvKeys {
		processEnv{}.Unsetenv(key)
	}
	for key, value := range doltEnv {
		processEnv{}.Setenv(key, value)
	}

	// Load disabled_patrols from town settings (settings/config.json).
	// This provides a simpler way to disable patrols than editing the "daemon" section of settings/config.json.
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
		if root, err := doltbackup.DefaultRoot(); err == nil {
			doltServer.backupRoot = root
		}
		if doltServer.config.Port == 0 {
			// The endpoint comes from the town's config only (gt-y3pgh.9);
			// the daemon section's dolt_server port/host are ignored. With no
			// endpoint there is nothing to manage, and guessing one would
			// point the daemon at some other server.
			logger.Printf("Dolt server management disabled: town has no Dolt endpoint (set it with gt config set dolt.port)")
			doltServer = nil
		} else if doltServer.IsEnabled() {
			logger.Printf("Dolt server management enabled (port %d)", doltServer.config.Port)
		}
	}

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

	d := &Daemon{
		config:          config,
		patrolConfig:    patrolConfig,
		disabledPatrols: disabledPatrols,
		tmux:            tmux.NewTmux(),
		logger:          logger,
		logPreexisted:   logPreexisted,
		ctx:             ctx,
		cancel:          cancel,
		doltServer:      doltServer,
		gtPath:          gtPath,
		bdPath:          bdPath,
		notifier:        newDaemonNotifier(gtPath, config.TownRoot),
		rigPool:         newRigWorkerPool(0, 0, logger), // defaults: 10 workers, 30s timeout
		// The container's compose file sets this beside the daemon; asking the
		// process environment here, in the one constructor production runs,
		// keeps the burst sampler from reading it (burst_sampler.go).
		containerMode: containerModeFromEnv(os.Getenv),
	}

	// A rig whose docked/parked status cannot be read is treated as not
	// operational, which suppresses auto-start for that rig. Escalate it, so
	// the suppression is visible rather than a warning line in a log nobody
	// reads while the rig sits dead (gt-4nu3).
	d.rigStatusAlert = d.escalateAlert
	d.rigStatusClear = d.clearAlerts
	d.checkpointRevertAlert = d.escalateAlert

	// A health-driven Dolt restart waits for an in-flight scheduled gc.
	if doltServer != nil {
		doltServer.SetRestartSuppressor(d.doltRestartHeldForGC)
	}

	return d, nil
}

// processEnv writes the daemon's own process environment. It is the one
// place the daemon does: PublishIdentity publishes the daemon's identity and
// New the town root, the daemon patrol config's env, the augmented PATH and the Dolt
// endpoint here so every session, bd and gt it starts inherits them, and the
// libraries that build those children (config.AgentEnv, internal/beads) read
// the process environment.
type processEnv struct{}

func (processEnv) Setenv(key, value string) {
	//testpolicy:allow prod-no-setenv — the daemon process publishes its environment to every child it starts (see processEnv)
	_ = os.Setenv(key, value)
}

func (processEnv) Unsetenv(key string) {
	//testpolicy:allow prod-no-setenv — the daemon process publishes its environment to every child it starts (see processEnv)
	_ = os.Unsetenv(key)
}

func (d *Daemon) cleanupLegacySocketSessions() {
	d.legacySocketCleanupOnce.Do(func() {
		defaultCleaned, baseCleaned := d.seams.cleanupLegacySocketsFor(d.prefixRegistry(), d.config.TownRoot)
		if defaultCleaned > 0 {
			d.logger.Printf("legacy_socket_cleanup: cleaned %d session(s) from default socket", defaultCleaned)
		}
		if baseCleaned > 0 {
			d.logger.Printf("legacy_socket_cleanup: cleaned %d session(s) from basename socket", baseCleaned)
		}
	})
}

// unrequestedStopLogLine is what the daemon logs when it shuts down on a
// signal gt did not send. A launchctl bootout unloads the job, so nothing
// relaunches after one: only KeepAlive {SuccessfulExit: false} does, and only
// for a job still loaded (gt-2gctr, gt-swsqm).
const unrequestedStopLogLine = "Stop was not requested through gt: exiting 75; this was a bootout or a signal from outside gt, and launchd relaunches only under KeepAlive"

// recordStopForHeal leaves the trace the upgrade heal helper reads, before the
// shutdown so the record is on disk whichever way the stop was requested. The
// stop marker is read here, not consumed: StopDaemon wrote it before the
// signal, and exitAfterSignal takes it only after the shutdown (gt-518hq). A
// failure costs the helper its one clue about a bootout, never the stop
// (gt-r8ux8).
func (d *Daemon) recordStopForHeal(now time.Time) {
	requested := stopRequested(d.config.TownRoot, os.Getpid(), now)
	if err := recordSignalStop(d.config.TownRoot, now, requested); err != nil {
		d.logger.Printf("could not record the signal stop for the upgrade heal helper: %v", err)
	}
}

// shutdownForSignal ends the daemon on a signal and returns what Run should
// exit with: nil when gt requested the stop, ErrUnrequestedStop otherwise. The
// record goes down before the shutdown, and the exit reads the marker after it
// (gt-518hq, gt-swsqm).
func (d *Daemon) shutdownForSignal(sig os.Signal, state *State) error {
	d.logger.Printf("Received signal %v, shutting down", sig)
	d.recordStopForHeal(time.Now())
	// Exit 0 only when gt asked for this stop; any other signal exits 75,
	// which launchd turns into a relaunch only for a job still loaded under
	// KeepAlive (gt-swsqm, gt-2gctr).
	err := exitAfterSignal(d.config.TownRoot, os.Getpid(), time.Now(), d.shutdown(state))
	if errors.Is(err, ErrUnrequestedStop) {
		d.logger.Println(unrequestedStopLogLine)
	}
	return err
}

// clearSpentSignalStop drops the record of the daemon that ran before this
// one: a record says the town was stopped, and a daemon starting says it is
// not (gt-r8ux8).
func (d *Daemon) clearSpentSignalStop() {
	clearSignalStop(d.config.TownRoot)
}

// daemonLockWaitBudget is how long Run keeps trying daemon.lock before it
// concludes another daemon holds it, and daemonLockPollInterval is the gap
// between those attempts.
//
// A probe of the same lock (probeDaemonLock) holds it only for the two
// syscalls a look takes, so a start that loses the race to one waits it out
// and starts; a real daemon holds the lock for its whole life and still loses
// after the budget (gt-fsgb1).
const (
	daemonLockWaitBudget   = 2 * time.Second
	daemonLockPollInterval = 25 * time.Millisecond
)

// acquireDaemonLock takes the exclusive daemon.lock flock that makes this
// process the town's one daemon, retrying for budget so a probe holding the
// lock shared cannot end a legitimate start (gt-fsgb1).
func acquireDaemonLock(clk clockwork.Clock, lockPath string, budget, poll time.Duration) (*flock.Flock, error) {
	fileLock := flock.New(lockPath)
	deadline := clk.Now().Add(budget)
	for {
		locked, err := fileLock.TryLock()
		if err != nil {
			return nil, fmt.Errorf("acquiring lock: %w", err)
		}
		if locked {
			return fileLock, nil
		}
		if !clk.Now().Before(deadline) {
			return nil, fmt.Errorf("daemon already running (lock held by another process)")
		}
		clk.Sleep(poll)
	}
}

// Run starts the daemon main loop.
func (d *Daemon) Run() (err error) {
	pid := os.Getpid()
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

	// Name this daemon's owner before anything is written: every state this
	// process saves carries it, and a start that cannot name itself never
	// touches the town (gt-qpeei.12).
	self, err := DetectSelf(d.env, d.hostnameOrOS)
	if err != nil {
		return err
	}

	// Refuse a start another owner may still hold before any file is written.
	// This runs ahead of the start line below, so the start writes no log line
	// the guard could read as a prior run.
	probe := d.ownerProbe
	if probe == nil {
		probe = startupOwnerProbe(templates.RunDockerInspect, d.hostnameOrOS, nil)
	}
	takeover := d.env(agentconfig.TakeoverEnv) == "1"
	if err := guardStartup(d.config.TownRoot, self, takeover, probe, d.clk().Now(), d.logPreexisted); err != nil {
		return err
	}

	d.owner = &self
	d.logger.Printf("Daemon starting (PID %d)", pid)
	d.logger.Printf("Daemon owner: %s %q", self.Kind, self.ID)

	// Acquire exclusive lock to prevent multiple daemons from running.
	// This prevents the TOCTOU race condition where multiple concurrent starts
	// can all pass the IsRunning() check before any writes the PID file.
	// Uses gofrs/flock for cross-platform compatibility (Unix + Windows).
	lockFile := filepath.Join(d.config.TownRoot, "daemon", "daemon.lock")
	fileLock, err := acquireDaemonLock(d.clk(), lockFile, daemonLockWaitBudget, daemonLockPollInterval)
	if err != nil {
		return err
	}
	defer func() { _ = fileLock.Unlock() }()

	// Pre-flight check: all rigs must be on Dolt backend.
	if err := d.checkAllRigsDolt(); err != nil {
		return err
	}

	// The seat map must read cleanly before anything is dispatched: a daemon
	// that ran on a partial seat list would spend seats the operator closed.
	if err := checkSeatMap(d.config.TownRoot); err != nil {
		return err
	}

	d.warnPressureScale(d.loadOperationalConfig().GetDaemonConfig().PressureCPUThresholdV())

	// Repair metadata.json for all rigs on startup.
	// This ensures all rigs have proper Dolt server configuration.
	if _, errs := doltserver.EnsureAllMetadata(d.config.TownRoot); len(errs) > 0 {
		for _, e := range errs {
			d.logger.Printf("Warning: metadata repair: %v", e)
		}
	}

	// Write PID file with nonce for ownership verification. A pid from the
	// container's namespace means nothing on the host, so container mode
	// writes none and leaves the host to the supervisor's state (gt-qpeei.12).
	if !containerMode(d.env) {
		if _, err := writePIDFile(d.config.PidFile, os.Getpid()); err != nil {
			return fmt.Errorf("writing PID file: %w", err)
		}
		defer func() { _ = os.Remove(d.config.PidFile) }() // best-effort cleanup
	}

	// Update state
	state := startState(os.Getpid(), time.Now(), d.resolveOwnCommit())
	if err := d.saveState(state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	// Clear a restart marker this binary already covers (the daemon may have
	// been restarted onto it by launchd or an operator). Covered-only: startup
	// never exits for an upgrade; a newer marker waits for the heartbeat.
	d.clearCoveredRestartMarker(time.Now())
	d.clearSpentSignalStop()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, daemonSignals()...)

	// Fixed recovery-focused heartbeat (no activity-based backoff)
	// Normal wake is handled by feed subscription (bd activity --follow)
	timer := time.NewTimer(d.recoveryHeartbeatInterval())
	defer timer.Stop()

	d.logger.Printf("Daemon running, recovery heartbeat interval %v", d.recoveryHeartbeatInterval())

	// bd call census (gt-7h3vm): this process is most of the town's bd calls,
	// so it names them once a minute where call-volume cuts are chosen.
	stopCensus := d.startCensusLog()
	defer stopCensus()

	// Clean sessions left behind on legacy tmux sockets after daemon startup has
	// passed fatal preflight checks but before any patrol agents can be spawned.
	d.cleanupLegacySocketSessions()

	// Landing workers (gt-v4ssj.2): opt-in, one goroutine per rig.
	d.startLandingWorkers()

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

	// Start the overrun guard ticker if configured. It nudges a polecat whose
	// bead runs long and, once stopping is built, stops one that runs over.
	// Due-ness comes from the persisted last-run time, as for checkpoint_dog.
	var overrunGuardTicker *time.Ticker
	var overrunGuardChan <-chan time.Time
	if d.isPatrolActive("overrun_guard") {
		interval := overrunGuardInterval(d.patrolConfig)
		overrunGuardTicker = time.NewTicker(shortPatrolCheckTick(interval))
		overrunGuardChan = overrunGuardTicker.C
		defer overrunGuardTicker.Stop()
		d.logger.Printf("Overrun guard ticker started (check every %v, run interval %v)",
			shortPatrolCheckTick(interval), interval)
		d.triggerOverrunGuard()
	}

	// Start the landing-infrastructure probe ticker (gt-fn9e6.11): it tells
	// the town health line whether the Forgejo instance the landing path uses
	// answers and whether the rigs' CI has a runner online, so an outage is
	// visible instead of only a landing that never finishes. It does nothing
	// where no rig lands through Forgejo.
	var landingInfraTicker *time.Ticker
	var landingInfraChan <-chan time.Time
	if d.isPatrolActive("landing_infra") {
		interval := landingInfraInterval(d.patrolConfig)
		landingInfraTicker = time.NewTicker(interval)
		landingInfraChan = landingInfraTicker.C
		defer landingInfraTicker.Stop()
		d.logger.Printf("Landing infra probe ticker started (interval %v)", interval)
		// Catch up at startup: the first observation is what makes the two
		// fields appear at all, and a restart must not blank them for a whole
		// interval (gt-fn9e6.11).
		d.triggerLandingInfraProbe()
	}

	// Start scheduled maintenance ticker if configured.
	// Checks periodically whether we're in the maintenance window and runs
	// the town's one Dolt GC cycle there (scheduled_maintenance.go).
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

	// Start the disk_maintenance ticker if configured. It plans the town's
	// registry in its own quiet window, once a day, and any target with a
	// cadence of its own on that cadence; it prunes nothing unless the mode is
	// apply (disk_maintenance.go, gt-dl4nv.15, gt-z6qre.17).
	var diskMaintTicker *time.Ticker
	var diskMaintChan <-chan time.Time
	if d.isPatrolActive("disk_maintenance") {
		diskMaintTicker = time.NewTicker(diskMaintCheckInterval)
		diskMaintChan = diskMaintTicker.C
		defer diskMaintTicker.Stop()
		d.logger.Printf("Disk maintenance ticker started (check interval %v, window %s, mode %s)",
			diskMaintCheckInterval, diskMaintenanceWindow(d.patrolConfig), diskMaintenanceModeOrDefault(d.patrolConfig))
	}

	// Start the hm_sync ticker if configured. It runs the town's `hm sync`
	// once per interval, so history-man's index no longer depends on someone
	// running it by hand (hm_sync.go, gt-z6qre.15).
	var hmSyncTicker *time.Ticker
	var hmSyncChan <-chan time.Time
	if d.isPatrolActive("hm_sync") {
		hmSyncTicker = d.startHMSyncTicker()
		if hmSyncTicker != nil {
			hmSyncChan = hmSyncTicker.C
			defer hmSyncTicker.Stop()
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
		// Catch up at startup rather than waiting for the first tick: the
		// ticker is fresh on every start, so restarts closer together than
		// scheduledSlingsTickInterval would otherwise never evaluate a
		// schedule. Due-ness comes from bead history, so an extra evaluation
		// cannot double-dispatch (gt-ima2, gt-gxpwc).
		d.catchUpScheduledSlings()
	}

	// Start the spec dispatcher ticker unless the daemon patrol config
	// (settings/config.json's "daemon" section; the legacy mayor/daemon.json
	// on an unmigrated town) sets enabled:false (default on, gt-1gnq9).
	var specDispatchTicker *time.Ticker
	var specDispatchChan <-chan time.Time
	if d.isPatrolActive("spec_dispatch") {
		interval := specDispatchInterval(d.patrolConfig)
		// The idle rule's consecutive-tick gap is a multiple of the cadence the
		// ticker actually runs at (gt-icmdq).
		d.dispatchIdle.setInterval(interval)
		specDispatchTicker = time.NewTicker(interval)
		specDispatchChan = specDispatchTicker.C
		defer specDispatchTicker.Stop()
		d.logger.Printf("Spec dispatch ticker started (interval %v)", interval)
	}

	// Start the overseer_report ticker if enabled (default off, gt-a6fg7). The
	// report is one file per interval slot, so the run at startup writes the
	// current slot's file unless an earlier run already did. The ticker fires
	// several times a slot because it is not aligned to the slots: one tick per
	// interval, a late one could land in the slot after the one it was meant for
	// and leave that slot without a file. A run in a slot that has its file
	// writes nothing, so the extra ticks cost one directory read.
	var overseerReportTicker *time.Ticker
	var overseerReportChan <-chan time.Time
	if d.isPatrolActive("overseer_report") {
		interval := overseerReportInterval(d.patrolConfig)
		overseerReportTicker = time.NewTicker(overseerReportTickEvery(interval))
		overseerReportChan = overseerReportTicker.C
		defer overseerReportTicker.Stop()
		d.logger.Printf("Overseer report ticker started (interval %v)", interval)
		d.triggerOverseerReport()
	}

	// Start the patrol_scan tick if enabled (default off, gt-4k3fj.6): the
	// deterministic replacement for the witness LLM's patrol.
	var patrolScanTicker *time.Ticker
	var patrolScanChan <-chan time.Time
	if d.isPatrolActive("patrol_scan") {
		interval := patrolScanInterval(d.patrolConfig)
		patrolScanTicker = time.NewTicker(interval)
		patrolScanChan = patrolScanTicker.C
		defer patrolScanTicker.Stop()
		d.logger.Printf("Patrol scan ticker started (interval %v)", interval)
	}

	// Start the resource sampler ticker if enabled (default on, gt-qqwey). Its
	// own ticker, not the heartbeat: the heartbeat is minutes apart and a row's
	// cpu_pct is the delta over the interval since the previous row.
	var resourceSamplerTicker *time.Ticker
	var resourceSamplerChan <-chan time.Time
	if d.isPatrolActive("resource_sampler") {
		interval := resourceSamplerInterval(d.patrolConfig)
		resourceSamplerTicker = time.NewTicker(interval)
		resourceSamplerChan = resourceSamplerTicker.C
		defer resourceSamplerTicker.Stop()
		d.logger.Printf("Resource sampler ticker started (interval %v)", interval)
	}

	// The burst sampler runs beside it (gt-ztem6): its own goroutine, paced by
	// the row's own cadence, so bursts.jsonl fills while the host is pressed.
	// stopBurst cancels and joins the loop, so Run's return leaves no sample
	// behind it.
	stopBurst := d.startBurstSamplerIfEnabled()
	defer stopBurst()

	// No tmux pane-died respawn hooks: a dead session is restarted by the
	// heartbeat through the supervisor, within its budget (gt-4k3fj.3).

	// Initial heartbeat
	d.heartbeat(state)
	startupComplete = true
	if err := d.exitForUpgradeIfRequested(state, time.Now()); err != nil {
		return err
	}

	// The landing workers wake the loop here when a drained pass ends, so a
	// pending upgrade restart fires at that moment (gt-fzwcd).
	landingDrainedChan := d.landingDrained()

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
				return d.shutdownForSignal(sig, state)
			}

		case <-doltHealthChan:
			// Dedicated Dolt health check — fast crash detection independent
			// of the 3-minute general heartbeat.
			if !d.isShutdownInProgress() {
				d.ensureDoltServerRunning()
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
				// The test-pollution sweep runs first, on the same cadence
				// (gt-4k3fj.6.2): it is the deacon's test-pollution-cleanup
				// step, now one daemon job.
				d.runTestPollutionSweep()
				d.runDoctorDog()
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

		case <-overrunGuardChan:
			// Overrun guard — nudges polecats whose bead runs long. Fires
			// only when the persisted last run is a full interval old.
			if !d.isShutdownInProgress() {
				d.triggerOverrunGuard()
			}

		case <-scheduledMaintenanceChan:
			// Scheduled maintenance — checks if we're in the maintenance window
			// and dispatches a dolt_gc('--full') cycle for the databases due.
			if !d.isShutdownInProgress() {
				d.runScheduledMaintenance()
			}

		case <-diskMaintChan:
			// Disk maintenance — plans the due targets: the registry in its own
			// quiet window, once a day, and any target with a cadence of its
			// own on that cadence; it applies a plan only in apply mode.
			if !d.isShutdownInProgress() {
				d.runDiskMaintenance()
			}

		case <-hmSyncChan:
			// HM sync — runs the configured sync once per interval, off the
			// loop goroutine.
			if !d.isShutdownInProgress() {
				d.runHMSync()
			}

		case <-scheduledSlingsChan:
			// Scheduled slings — dispatches formula runs on an interval.
			// Each entry is evaluated independently; the decision function
			// decides due-ness, so a coarse tick is sufficient (gt-nj23).
			if !d.isShutdownInProgress() {
				d.triggerScheduledSlings()
			}

		case <-specDispatchChan:
			// Spec dispatcher tick — lints ready spec beads and slings clean
			// ones within the seat budget, on its own goroutine (gt-4k3fj.5).
			if !d.isShutdownInProgress() {
				d.triggerSpecDispatch()
			}

		case <-overseerReportChan:
			// Overseer report tick — writes the digest for the current slot on
			// its own goroutine (gt-a6fg7).
			if !d.isShutdownInProgress() {
				d.triggerOverseerReport()
			}

		case <-resourceSamplerChan:
			// Resource sampler — appends the host's CPU and memory rows for
			// the metrics snapshot (gt-qqwey).
			if !d.isShutdownInProgress() {
				d.triggerResourceSampler()
			}

		case <-patrolScanChan:
			// Patrol scan tick — restarts confirmed-dead polecats through the
			// supervisor, closes orphaned work molecules and reports stranded
			// work, per rig, on its own goroutine (gt-4k3fj.6).
			if !d.isShutdownInProgress() {
				d.triggerPatrolScan()
			}

		case <-landingInfraChan:
			// Landing-infrastructure probe — asks the Forgejo instance and the
			// rigs' CI runners how they are, on its own goroutine: the probes
			// are HTTP calls that can each take the probe timeout
			// (gt-fn9e6.11).
			if !d.isShutdownInProgress() {
				d.triggerLandingInfraProbe()
			}

		case <-landingDrainedChan:
			// A drained landing pass ended while an upgrade restart was
			// pending: that is the idle moment it waited for, so check now
			// rather than at the next heartbeat, up to 3 min away (gt-fzwcd).
			if err := d.restartOnDrainedLanding(state); err != nil {
				return err
			}

		case <-timer.C:
			d.heartbeat(state)
			if err := d.exitForUpgradeIfRequested(state, time.Now()); err != nil {
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

	// The town E-stop holds dispatch and restarts, not upkeep: Dolt stays
	// supervised and the logs keep rotating while it is on (gt-4k3fj.8).
	// The steps it holds are the lifecycle ones in heartbeatSteps. No plugin
	// script kills or restarts a session any more (the stuck-agent dog was
	// deleted in 339ac0ce), and every daemon kill and restart goes through
	// the supervisor, which refuses them under the e-stop as well.
	if estop.IsActive(d.config.TownRoot) {
		d.logger.Println("E-STOP active, holding dispatch and restarts; Dolt and upkeep still run")
		d.seams.runHeartbeatWork(d, state, false)
		return
	}

	// Before any dispatch: a heartbeat that starts plugins first would make
	// the daemon busy and never let it restart for an upgrade.
	if d.checkUpgradeRestart(time.Now()) {
		return
	}
	d.seams.runHeartbeatWork(d, state, true)
}

// heartbeatStep is one step of a heartbeat. A lifecycle step dispatches
// work, kills or restarts sessions or processes; the town E-stop holds it.
type heartbeatStep struct {
	name      string
	lifecycle bool
	run       func(d *Daemon)
}

// heartbeatSteps is the recovery work of one heartbeat, in order.
var heartbeatSteps = []heartbeatStep{
	// Invalidate the per-tick rigs cache so this heartbeat re-reads from disk.
	// Within a tick the cache coalesces the ~10 getKnownRigs() call sites into
	// a single read; invalidating here ensures we pick up rigs.json changes
	// between ticks.
	{name: "rigs-cache", run: (*Daemon).invalidateKnownRigsCache},

	// Reload prefix registry so new/changed rigs get correct session names.
	// Without this, rigs added after daemon startup get the "gt" default prefix,
	// causing ghost sessions like gt-witness instead of ti-witness. (hq-ouz, hq-eqf, hq-3i4)
	{name: "prefix-registry", run: func(d *Daemon) {
		if err := session.InitRegistry(d.config.TownRoot); err != nil {
			d.logger.Printf("Warning: failed to reload prefix registry: %v", err)
		}
	}},

	// Ensure Dolt server is running (if configured).
	// This must happen before beads operations that depend on Dolt.
	{name: "dolt", run: (*Daemon).ensureDoltServerRunning},

	// Kill ghost sessions left over from stale registry (default "gt" prefix).
	{name: "ghost-sessions", lifecycle: true, run: (*Daemon).killDefaultPrefixGhosts},

	// Run due plugins. Pressure-gated: a plugin run is new work the
	// town can put off while it is loaded.
	{name: "plugins", lifecycle: true, run: func(d *Daemon) {
		if !d.isPatrolActive("handler") {
			d.logger.Printf("Handler patrol disabled in config, skipping")
			return
		}
		if p := d.checkPressure("plugin"); !p.OK {
			d.logger.Printf("Deferring plugin runs: %s", p.Reason)
			return
		}
		d.handlePlugins()
	}},

	// Check polecat session health (proactive crash detection)
	// This validates tmux sessions are still alive for polecats with work-on-hook
	{name: "polecat-health", lifecycle: true, run: (*Daemon).checkPolecatSessionHealth},

	// Reap idle polecat sessions to prevent API slot burn.
	// Polecats transition to IDLE after gt done but sessions stay alive.
	// Kill sessions that have been idle longer than the configured threshold.
	{name: "idle-reap", lifecycle: true, run: (*Daemon).reapIdlePolecats},

	// Clean up orphaned claude subagent processes (memory leak prevention)
	// These are Task tool subagents that didn't clean up after completion.
	{name: "orphan-processes", lifecycle: true, run: (*Daemon).cleanupOrphanedProcesses},

	// Prune stale local polecat tracking branches across all rig clones.
	// When polecats push branches to origin, other clones create local tracking
	// branches via git fetch. After merge, remote branches are deleted but local
	// branches persist indefinitely. This cleans them up periodically.
	{name: "branch-prune", run: (*Daemon).pruneStaleBranches},

	// Offer every sandbox seat whose end waits for the jev_rules battery to
	// ask about its submission again, so the seat goes once the ask has
	// happened or its one-hour window has closed (gt-381oj). Non-lifecycle:
	// it removes leftovers, and an E-stopped town still wants them gone.
	{name: "sandbox-seat-ends", run: (*Daemon).retryPendingSandboxSeatEnds},

	// Dispatch scheduled work (capacity-controlled polecat dispatch).
	// Runs internal/schedulerrun in process; package cmd installs the deps.
	// Pressure-gated: polecats are the primary resource consumers.
	{name: "dispatch", lifecycle: true, run: func(d *Daemon) {
		if p := d.checkPressure("polecat"); !p.OK {
			d.logger.Printf("Deferring polecat dispatch: %s", p.Reason)
			return
		}
		d.dispatchQueuedWork()
	}},

	// Rotate oversized Dolt logs (copytruncate for child process fds).
	// daemon.log uses lumberjack for automatic rotation; this handles Dolt server logs.
	{name: "log-rotation", run: (*Daemon).rotateOversizedLogs},

	// Prune the raw event log (.events.jsonl) when due (gt-ori5j).
	{name: "events-prune", run: (*Daemon).pruneEventsLog},

	// Clean merged and orphaned branches and gc the rig repos when due
	// (was the git-hygiene plugin, gt-4k3fj.8.5).
	{name: "git-hygiene", run: (*Daemon).triggerGitHygiene},

	// Bring the installed gt binary in force from main when it falls behind
	// (was the rebuild-gt plugin, gt-4k3fj.8.6). lifecycle: the install makes
	// the daemon restart, which the town E-stop holds.
	{name: "rebuild-gt", lifecycle: true, run: (*Daemon).triggerRebuildGT},

	// Keep the installed om at the om origin's main (gt-0f228). lifecycle: an
	// install changes a binary on the machine, which the town E-stop holds.
	{name: "rebuild-om", lifecycle: true, run: (*Daemon).triggerRebuildOM},

	// Suggest the bead each new finding duplicates, and log every suggestion
	// (internal/jev/dedupe). Opt-in, and lifecycle: each pass spends on Jev, so
	// the town E-stop holds it along with the other paid work.
	{name: "jev-dedupe", lifecycle: true, run: (*Daemon).triggerJevDedupe},

	// Ask Jev the attention question set about each new attention item and
	// log every answer (internal/jev/attention). Opt-in, and lifecycle for the
	// same reason as jev-dedupe: each pass spends on Jev.
	{name: "jev-attention", lifecycle: true, run: (*Daemon).triggerJevAttention},

	// Ask Jev a second opinion on each risk-path landing and log it
	// (internal/jev/landing). Opt-in and lifecycle, like jev-dedupe.
	{name: "jev-landing", lifecycle: true, run: (*Daemon).triggerJevLanding},

	// Ask Jev about each branch submitted for landing and log every answer
	// (internal/jev/submission). Opt-in and lifecycle, like jev-dedupe.
	{name: "jev-submission", lifecycle: true, run: (*Daemon).triggerJevSubmission},

	// Ask Jev whether each landing left the doc sections closest to its diff
	// stale, and log every answer (internal/jev/drift). Opt-in and lifecycle,
	// like jev-dedupe.
	{name: "jev-drift", lifecycle: true, run: (*Daemon).triggerJevDrift},

	// Ask Jev about each group of the daemon log's warning and error lines
	// every fifteen minutes, and log every answer (internal/jev/logtriage).
	// Opt-in and lifecycle, like jev-dedupe.
	{name: "jev-logtriage", lifecycle: true, run: (*Daemon).triggerJevLogtriage},

	// Ask Jev, for each om review that refused a landing, why it failed and
	// whether the landing will need a rework commit, and log both answers
	// (internal/jev/rework). Opt-in and lifecycle, like jev-dedupe: the pass
	// changes no landing.
	{name: "jev-rework", lifecycle: true, run: (*Daemon).triggerJevRework},

	// Predict, for each bead just placed on a seat, how it will land, and log
	// every prediction (internal/jev/dispatch). Opt-in and lifecycle, like
	// jev-dedupe, and under the same town-wide daily cap.
	{name: "jev-dispatch", lifecycle: true, run: (*Daemon).triggerJevDispatch},

	// Ask Jev, for each bead just placed on a seat, whether the polecat will be
	// respawned on it and whether the overrun guard will stop it, and log both
	// answers (internal/jev/thrash). Opt-in and lifecycle, like jev-dispatch,
	// and under the same town-wide daily cap. It reads placements and writes
	// only its own log.
	{name: "jev-thrash", lifecycle: true, run: (*Daemon).triggerJevThrash},

	// Ask Jev, for each submission gt done made (a READY TO LAND block or a
	// revert-guard refusal), whether the session broke a standing rule and
	// which rule, and log both answers (internal/jev/rules). Opt-in and
	// lifecycle, like jev-thrash, and under the same town-wide daily cap. It
	// reads submissions and transcripts and writes only its own log.
	{name: "jev-rules", lifecycle: true, run: (*Daemon).triggerJevRules},

	// Ask Jev whether each bead just placed would touch the same files and
	// function as the beads in flight beside it, and log every answer
	// (internal/jev/conflict). Opt-in and lifecycle, like jev-dispatch, and
	// under the same town-wide daily cap. It reads placements and writes only
	// its own log, so dispatch is the same with it on or off.
	{name: "jev-conflict", lifecycle: true, run: (*Daemon).triggerJevConflict},

	// Ask Jev whether each polecat the overrun guard nudged is making progress,
	// and log every answer (internal/jev/progress). Opt-in and lifecycle, like
	// jev-dedupe. It runs on the heartbeat that finds a nudge queued, not on an
	// interval, so the answer lands while the seat is still inside its clock.
	{name: "jev-progress", lifecycle: true, run: (*Daemon).triggerJevProgress},

	// Ask Jev about each open bead untouched for fourteen days, once a week,
	// and log every answer (internal/jev/stale). Opt-in and lifecycle, like
	// jev-dedupe.
	{name: "jev-stale", lifecycle: true, run: (*Daemon).triggerJevStale},

	// Ask Jev whether each memory note and the operator handbook still
	// describes the town, once a week, and log every answer beside a
	// mechanical label (internal/jev/notes). Opt-in and lifecycle, like
	// jev-dedupe.
	{name: "jev-notes", lifecycle: true, run: (*Daemon).triggerJevNotes},

	// Sweep the tiers `make gate` does not run against origin/main
	// (gt-vsct7.5). Non-lifecycle: it runs tests and files beads, and an
	// E-stopped town still wants to know its main is red.
	{name: "tier-sweep", run: (*Daemon).triggerTierSweep},

	// Compute the town health report and write the one health file gt
	// status --line reads (gt-s3rec.2). Last, so it sees this tick's Dolt
	// check and the steps' records.
	{name: "townhealth", run: (*Daemon).writeTownHealth},

	// Write the attention queue from the collectors (gt-vsct7.2). After
	// townhealth, so the bd-slow collector can read this tick's Dolt verdict;
	// non-lifecycle, so an E-stopped town still gets its queue.
	{name: "attention", run: (*Daemon).writeAttention},

	// Tell the human once about each escalation the overseer left open past
	// its unhandled window (gt-71714). Last, so it reads the escalations the
	// steps above filed this tick.
	{name: "unhandled-escalations", run: (*Daemon).pushUnhandledEscalations},
}

// OwnerKindManual is the owner of a daemon nobody's service manager started,
// identified by the host it runs on and its pid there.
const OwnerKindManual = "manual"

// DetectSelf names the owner of a daemon started from this process: the
// container (ContainerNameEnv), the launchd job (LaunchdJobEnv), the systemd
// unit (SystemdInvocationEnv), or manual ("hostname:pid"). Every environment
// read goes through env so a test sets the fixture.
//
// hostname is read only for the manual owner: the other kinds name themselves
// without the host, so an unreadable host name must not refuse them. Container
// mode with no container name is an error, never a guessed id.
func DetectSelf(env func(string) string, hostname func() (string, error)) (Owner, error) {
	if containerMode(env) {
		name := env(agentconfig.ContainerNameEnv)
		if name == "" {
			return Owner{}, fmt.Errorf("container mode (%s=%s) without %s: the owner id is the container name the compose file sets",
				agentconfig.ContainerModeEnv, agentconfig.SupervisorContainer, agentconfig.ContainerNameEnv)
		}
		return Owner{Kind: agentconfig.SupervisorContainer, ID: name}, nil
	}
	if label := launchdJob(env); label != "" {
		return Owner{Kind: agentconfig.SupervisorLaunchd, ID: label}, nil
	}
	if invocation := env(agentconfig.SystemdInvocationEnv); invocation != "" {
		return Owner{Kind: agentconfig.SupervisorSystemd, ID: invocation}, nil
	}
	host, err := hostname()
	if err != nil {
		return Owner{}, fmt.Errorf("naming the daemon's owner: %w", err)
	}
	return Owner{Kind: OwnerKindManual, ID: fmt.Sprintf("%s:%d", host, os.Getpid())}, nil
}

// launchdJob returns the launchd job label that started this process, or ""
// when launchd did not start it. Only the town's own job counts: macOS sets
// LaunchdJobEnv to "0" in an ordinary user session, and an app-launched
// process carries its own XPC service name, so either one recorded as a
// launchd owner would name a job that is not the daemon's (gt-qpeei.27).
func launchdJob(env func(string) string) string {
	label := env(agentconfig.LaunchdJobEnv)
	if label != templates.LaunchdLabel {
		return ""
	}
	return label
}

// containerMode reports whether the compose file started this process as the
// town's daemon in its container.
func containerMode(env func(string) string) bool {
	return env(agentconfig.ContainerModeEnv) == agentconfig.SupervisorContainer
}

// env answers one environment variable the start inspects (see the getenv
// field); nil getenv reads the process environment.
func (d *Daemon) env(key string) string {
	if d.getenv != nil {
		return d.getenv(key)
	}
	return os.Getenv(key)
}

// hostnameOrOS names the host a manual owner's id carries (see the hostname
// field); a nil seam reads it from the OS.
func (d *Daemon) hostnameOrOS() (string, error) {
	if d.hostname != nil {
		return d.hostname()
	}
	return os.Hostname()
}

// startState is the state record a daemon writes as it starts. The first
// heartbeat completes about a minute later, so LastHeartbeat starts at the
// start time with count 0: the health line then reads a young daemon as
// fresh instead of "no heartbeat recorded", and a daemon that never beats
// still ages past the heartbeat threshold from its start (gt-v9oug). The
// owner comes from saveState (gt-qpeei.12).
func startState(pid int, now time.Time, commit string) *State {
	return &State{
		Running:       true,
		PID:           pid,
		StartedAt:     now,
		LastHeartbeat: now,
		Commit:        commit,
	}
}

// saveState writes the daemon's state under the owner it named at its start,
// so no writer can drop it: the start, every heartbeat and the shutdown save
// here (gt-qpeei.12). A daemon that named no owner writes the state as given.
func (d *Daemon) saveState(state *State) error {
	if d.owner != nil {
		state.Owner = d.owner
	}
	return SaveState(d.config.TownRoot, state)
}

// heartbeatWork is the recovery work of one heartbeat, run after the
// shutdown, E-stop and upgrade-restart guards in heartbeat. lifecycle false
// (the town E-stop) skips the steps that dispatch, kill or restart.
func (d *Daemon) heartbeatWork(state *State, lifecycle bool) {
	d.logger.Println("Heartbeat starting (recovery-focused)")

	for _, step := range heartbeatSteps {
		if step.lifecycle && !lifecycle {
			continue
		}
		d.seams.runHeartbeatStep(d, step)
	}

	// Update state
	state.LastHeartbeat = time.Now()
	state.HeartbeatCount++
	if err := d.saveState(state); err != nil {
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
// Health check warnings are reported as a failed doctor_dog cycle, at most once
// per doltWarningCooldown.
func (d *Daemon) ensureDoltServerRunning() {
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return
	}

	if err := d.doltServer.EnsureRunning(); err != nil {
		d.logger.Printf("Error ensuring Dolt server is running: %v", err)
	}

	if warnings := d.doltServer.LastWarnings(); len(warnings) > 0 {
		if time.Since(d.lastDoltWarningTime) >= doltWarningCooldown {
			d.lastDoltWarningTime = time.Now()
			d.reportDoltWarnings(warnings)
		}
	}

}

// errDoltBringUpSkipped is what ensureDoltServerUp returns when it left Dolt
// alone because the daemon is shutting down. It is neither a failure to start
// the server nor "Dolt is up": a caller that needs the server skips its pass
// on it (gt-84o0a).
var errDoltBringUpSkipped = errors.New("dolt bring-up skipped: daemon is shutting down")

// ensureDoltServerUp brings the Dolt server up and reports a failure to do so.
// It returns errDoltBringUpSkipped, not nil, when the daemon is shutting down.
//
// ensureDoltServerRunning is the heartbeat's step 0 and does more than this: it
// reports Dolt warnings from state the heartbeat goroutine owns. A patrol cycle running on its own goroutine takes this bare
// bring-up instead of reaching into that state (gt-ox6c).
func (d *Daemon) ensureDoltServerUp() error {
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return nil
	}
	// shutdown() cancels d.ctx before it stops Dolt, so a canceled context
	// says the daemon is leaving: a server started here is one no daemon is
	// left to manage or adopt (gt-govth).
	if d.shuttingDown() {
		d.logger.Println("Dolt server not started: daemon is shutting down")
		return errDoltBringUpSkipped
	}
	// A daemon started while `gt down` holds shutdown.lock has a live context,
	// yet the town is being torn down: the startup catch-ups reach here before
	// the first heartbeat, which is the step that checks the lock itself.
	if d.isShutdownInProgress() {
		d.logger.Println("Dolt server not started: town shutdown in progress")
		return errDoltBringUpSkipped
	}
	return d.doltServer.EnsureRunning()
}

// shuttingDown reports whether the daemon has begun shutting down. It reads
// the context shutdown() cancels first, the same signal the background workers
// wait on (d.ctx.Done()).
func (d *Daemon) shuttingDown() bool {
	return d.ctx != nil && d.ctx.Err() != nil
}

// reportDoltWarnings records the Dolt health check's warnings as a failed
// doctor_dog cycle: a log line and a feed event.
func (d *Daemon) reportDoltWarnings(warnings []string) {
	cycle := d.startDogCycle("doctor_dog")
	defer cycle.close()
	cycle.failStep("dolt-health", strings.Join(warnings, "; "))
}

// checkSeatMap loads the town settings and the seat map in them through the
// loader, and returns the loader's message when either is rejected (gt-mukgk).
// A town with no seats key passes.
func checkSeatMap(townRoot string) error {
	ts, err := agentconfig.LoadOrCreateTownSettings(agentconfig.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("seat map: loading town settings: %w", err)
	}
	if _, err := agentconfig.Seats(ts, agentconfig.AgentResolver(townRoot, ts)); err != nil {
		return fmt.Errorf("seat map: %w", err)
	}
	return nil
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

// killDefaultPrefixGhosts kills tmux sessions that use the default "gt" prefix
// for roles that should use a rig-specific prefix. These ghost sessions appear
// when the daemon starts before a rig is registered or when the registry was
// stale. After a registry reload, any "gt-witness", "gt-refinery", or "gt-*"
// sessions that correspond to rigs with their own prefix are stale duplicates.
// Fix for: hq-ouz, hq-eqf, hq-3i4.
func (d *Daemon) killDefaultPrefixGhosts() {
	reg := d.prefixRegistry()
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
	for _, role := range []string{"witness"} {
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
		rigPrefix := reg.PrefixForRig(rigName)
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

// prefixRegistry is the rig-prefix registry the daemon reads: prefixRegistryFn's
// when a test set one, else the process-wide session registry.
func (d *Daemon) prefixRegistry() *session.PrefixRegistry {
	if d.prefixRegistryFn != nil {
		return d.prefixRegistryFn()
	}
	return session.DefaultRegistry()
}

// getKnownRigs returns list of registered rig names.
// Results are memoized per heartbeat tick to coalesce the ~10 per-tick callers
// into a single rig registry read (mayor/town.json's "registry" section on a
// two-file town, mayor/rigs.json on a five-file one). The cache is invalidated
// at the start of each heartbeat. Callable from any goroutine (gt-f18v).
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
// getKnownRigs() call re-reads the rig registry from disk.
func (d *Daemon) invalidateKnownRigsCache() {
	d.knownRigsMu.Lock()
	defer d.knownRigsMu.Unlock()
	d.knownRigsCache = nil
	d.knownRigsCacheValid = false
}

// readKnownRigsFromDisk reads and parses the rig registry: mayor/town.json's
// "registry" section on a two-file town, mayor/rigs.json on a five-file one.
func (d *Daemon) readKnownRigsFromDisk() []string {
	parsed, err := d.loadRigsConfig()
	if err != nil {
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
// Parked is the rig's record in the rig registry (mayor/town.json's
// "registry" section on a two-file town, mayor/rigs.json on a five-file
// one), read through the config kernel on every call (a few small files, no
// subprocess), so `gt rig park` takes effect on the next evaluation. It
// fails closed: a rig whose park state cannot be read is not operational
// (gt-y3pgh.4).
//
// Docked is the identity bead's status:docked label, and reading it costs a
// bd subprocess, so that read is memoized for a window (rigOperationalCacheTTL)
// - see internal/daemon/rig_status.go for why. This function is evaluated per
// rig per heartbeat by the patrol rig filters and witness and refinery
// auto-start, and each of those used to repeat the subprocess read, whose 60s
// budget a CPU-starved host spends in full (gt-4nu3). `gt rig dock` and
// `gt rig undock` stamp the rig's dock stamp, which drops the memo, so the
// window only bounds a label edited on the bead by hand.
//
// A failed bead read still fails closed: the rig is reported not operational, so
// nothing is auto-started for a rig whose state could not be verified. It is
// logged with the failure's category (timeout vs missing identity bead) and
// escalated, because that suppression is otherwise invisible.
func (d *Daemon) isRigOperational(rigName string) (bool, string) {
	if parked, why := townconfig.IsParked(d.config.TownRoot, rigName); parked {
		return false, why
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
	if entry.verdict == rigBeadDocked {
		return false, "rig is docked (global)"
	}

	cfg := wisp.NewConfig(d.config.TownRoot, rigName)

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
// Fresh also requires the rig's dock stamp to not be newer than the one this
// entry recorded: see rigOperationalCache.get and internal/rig.DockStamp.
//
// The window a read opens starts when the read answers, not when it was issued.
// Measuring from before the read would hand a starved host an entry that is
// already stale on arrival - a 60s read with a 60s window buys nothing, and
// every call site in the tick re-pays the budget the memo exists to save.
func (d *Daemon) rigBeadVerdict(rigName string) (rigBeadEntry, bool) {
	// The stamp is read before the bead, so the stamp an entry records is never
	// newer than the read it describes. A dock that lands while the bd read is
	// in flight then leaves a stamp the next call sees as newer than the entry,
	// and the entry is dropped; stamping after the read would record the new
	// stamp against the old verdict and hold a docked rig as active for the rest
	// of the window.
	now := d.clk().Now()
	stampAt, stampErr := rig.DockStamp(d.config.TownRoot, rigName)
	stamp := rigDockStamp{at: stampAt, known: stampErr == nil}
	if stampErr != nil && d.rigOperational.noteStampError(rigName, now, rigOperationalCacheTTL) {
		// The memo still answers (see get); only the notice is rate-limited, so
		// a stamp that stays unreadable does not turn every call site into a log
		// line.
		d.logger.Printf("Warning: cannot read the dock stamp for rig %s, so a dock is not noticed until its memoized read expires: %v", rigName, stampErr)
	}
	if entry, ok := d.rigOperational.get(rigName, now, stamp); ok {
		return entry, entry.failed == ""
	}

	rigBeadID, verdict, err := d.queryRigBead(rigName)
	answeredAt := d.clk().Now()
	if err != nil {
		// The category is in the line on purpose: "assuming not operational"
		// alone left a starved-host timeout and a missing identity bead looking
		// identical in daemon.log, and they call for different responses
		// (gt-4nu3).
		category := rigStatusFailureCategory(err)
		d.logger.Printf("Warning: failed to check rig bead %s for docked status: %s — assuming not operational (rig %s): %v",
			rigBeadID, category, rigName, err)
		if d.rigOperational.store(rigName, verdict, category, rigOperationalFailureCacheTTL, answeredAt, stamp).open {
			d.alertRigStatusUnverified(rigName, category, err)
		}
		return rigBeadEntry{failed: category}, false
	}

	if d.rigOperational.store(rigName, verdict, "", rigOperationalCacheTTL, answeredAt, stamp).clear {
		d.clearRigStatusUnverified(rigName)
	}
	return rigBeadEntry{verdict: verdict}, true
}

// queryRigBead reads the rig's identity bead and turns its status labels into a
// verdict. It logs nothing and reports no escalation - rigBeadVerdict owns both,
// so this stays a pure query.
func (d *Daemon) queryRigBead(rigName string) (string, rigBeadVerdict, error) {
	rigPath := filepath.Join(d.config.TownRoot, rigName)

	// Try to get prefix from rig config.json, fall back to rigs.json registry.
	// A config.json that does not decode is reported once rather than read as
	// unconfigured (gt-8xk9k).
	var prefix string
	rigCfg, cfgErr := rig.LoadRigConfigIfPresent(rigPath)
	if cfgErr != nil {
		rig.WarnRigConfigOnce(rigPath, cfgErr)
	}
	if rigCfg != nil && rigCfg.Beads != nil {
		prefix = rigCfg.Beads.Prefix
	} else {
		// Fall back to the rig registry (mayor/town.json's "registry"
		// section on a two-file town, mayor/rigs.json on a five-file one)
		// when the rig's config.json is missing
		prefix = agentconfig.GetRigPrefix(d.config.TownRoot, rigName)
	}

	if prefix == "" {
		return "", rigBeadActive, fmt.Errorf("rig %s: no beads prefix (rigs.json unreadable)", rigName)
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

	// Cancel before the Dolt stop, not after: the landing worker, spec
	// dispatch, the patrol scans and plugin scripts stop on d.ctx.Done(), and
	// the compactor dog and scheduled slings bring Dolt up themselves, so a
	// cancel that waited for the stop would leave them starting passes against
	// a server on its way down for the whole doltServerStopBudget (gt-govth).
	if d.cancel != nil {
		d.cancel()
	}

	// Join the sandbox seat ends the cancel just stopped (their remaining
	// reads go through Dolt, so this comes before the stop) and refuse any
	// new one. The wait is bounded; a seat end left over keeps its seat.
	d.stopSandboxSeatEnds()

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

	state.Running = false
	if err := d.saveState(state); err != nil {
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

// probeDaemonLock asks whether a daemon holds the town's daemon.lock. held is
// the answer; a non-nil probeLock is a *shared* lock on the file that the
// caller owns until it has settled what it read under it.
//
// Shared, not exclusive (gt-fsgb1): an exclusive probe is indistinguishable
// from a daemon to every other probe, so two checks racing each other each
// report a daemon that is not there. A daemon's exclusive hold still fails a
// shared probe, which is the answer a probe is after.
func probeDaemonLock(lockPath string) (held bool, probeLock *flock.Flock, err error) {
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false, nil, nil // No lock file — no daemon has ever run
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryRLock()
	if err != nil {
		return false, nil, err
	}

	if !locked {
		// A daemon holds the lock exclusively
		return true, nil, nil
	}

	// We hold the lock shared
	return false, lock, nil
}

// settleFreeDaemonLock removes the pid file of a town no daemon holds the lock
// of, and releases the probe lock only afterwards.
//
// The order is the point (gt-fsgb1): released first, the two steps leave a
// window for a starting daemon to take the lock and write its pid file, which
// the removal then deletes — after which nothing rewrites the file until that
// daemon restarts, and StopDaemon refuses with "pid is not known yet".
func settleFreeDaemonLock(pidFile string, release func() error) {
	_ = os.Remove(pidFile)
	_ = release()
}

// containerTown reports whether townRoot's daemon runs in a container. A
// readable daemon.supervisor naming container says so, and one naming any other
// kind says it does not. A town that sets no kind is not a container town and
// takes the host path it had before this check existed (gt-qpeei.19).
//
// A setting that cannot be read, or names a kind this binary does not know,
// cannot say either way, and both answers fail somewhere: reading it as a host
// kind lets a container town's stop signal a pid from another namespace, and
// reading it as a container refuses every host town its stop over a settings
// file the stop never needed. The compose file a container town's supervisor
// drives does not depend on the settings, so it decides: a town with the file
// may be a container town and is treated as one, and a town without it is a
// host town and keeps the host path (config.MayBeContainerTown).
//
// Even then a host path is not a license to signal a container's PID: the
// signal still runs through verifyGTDaemonPID, which proves the process is this
// town's `gt daemon run` on this host. The cmd layer reads the kind to decide
// the direct signal itself and refuses it on an unreadable setting (gt-qpeei.10).
func containerTown(townRoot string) bool {
	cfg, err := ReadPatrolConfig(townRoot)
	if err != nil {
		return agentconfig.MayBeContainerTown(townRoot)
	}
	kind, err := cfg.SupervisorKind()
	if err != nil {
		return agentconfig.MayBeContainerTown(townRoot)
	}
	return kind == agentconfig.SupervisorContainer
}

// IsRunning checks if a daemon is running for the given town.
// Uses the daemon.lock flock as the authoritative signal — if the lock is held,
// the daemon is running. Falls back to PID file for the process ID.
// This avoids fragile ps string matching for process identity (ZFC fix: gt-utuk).
//
// A town whose daemon.supervisor is container runs its daemon in another
// namespace, where a pid means nothing here: the answer still comes from the
// lock, but the PID is always 0 and the pid file is never consulted
// (gt-qpeei.19).
//
// A running container town answers (true, 0): 0 is not a daemon PID here, and
// as a signal target it names this process group, so a caller that gets it
// must not signal it.
func IsRunning(townRoot string) (bool, int, error) {
	return isRunningOn(townRoot, containerTown(townRoot), probeDaemonLock)
}

// isRunningOn is IsRunning on the container answer and the lock probe the
// caller supplies. The container town's pid refusal has to hold when the probe
// fails as well, so a test drives that branch with a probe that returns an
// error; StopDaemon passes the answer it already read, so the town's settings
// are read once on that path.
func isRunningOn(townRoot string, container bool, probe func(lockPath string) (bool, *flock.Flock, error)) (bool, int, error) {
	// Primary check: is the daemon lock held?
	lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
	held, probeLock, err := probe(lockPath)
	if err != nil {
		if container {
			// The pid-file fallback is the one place a pid for another
			// namespace could come from, so a container town has none:
			// the unreadable lock is reported instead (gt-qpeei.19).
			return false, 0, fmt.Errorf("checking %s: %w", lockPath, err)
		}
		// Can't check lock — fall back to PID file + signal check
		return isRunningFromPID(townRoot)
	}

	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")

	if !held {
		// No daemon holds the lock: clean up any stale PID file, still
		// holding the lock we read it under
		if probeLock != nil {
			settleFreeDaemonLock(pidFile, probeLock.Unlock)
		}
		return false, 0, nil
	}

	// A container town's daemon runs in another namespace and writes no pid
	// file at all (gt-qpeei.12), so whatever daemon.pid holds is stale and
	// names a process on this host only by accident. The lock still answers
	// that the daemon runs; the pid stays unknown (gt-qpeei.19).
	if container {
		return true, 0, nil
	}

	// Lock is held — daemon is running. Read PID from file.
	// Use readPIDFile to handle the "PID\nNONCE" format introduced alongside
	// nonce-based ownership verification. A plain Atoi on the raw file content
	// fails when a nonce line is present, returning PID 0.
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

// StopWaitBudget is how long StopDaemon lets a SIGTERMed daemon finish
// shutdown() before it sends SIGKILL: ShutdownBudget, the ceiling on that
// shutdown, plus stopWaitMargin for the steps that run after Dolt's stop.
// Killing sooner lands before SaveState(Running=false) and the dolt.pid
// cleanup, leaving a stale state file behind (gt-dhlm4).
const StopWaitBudget = ShutdownBudget + stopWaitMargin

// stopWaitMargin covers the in-process steps shutdown() runs after the Dolt
// stop returns: they are quick, but not part of ShutdownBudget.
const stopWaitMargin = 5 * time.Second

// stopPollInterval is how often StopDaemon re-checks that the daemon exited.
const stopPollInterval = 100 * time.Millisecond

// daemonStopWait waits for a SIGTERMed daemon to exit, then SIGKILLs it if it
// outlives budget. Its effects are fields so a test can drive it with a fake
// clock and a fake process.
type daemonStopWait struct {
	clock  clockwork.Clock
	out    io.Writer
	pid    int
	budget time.Duration
	poll   time.Duration
	alive  func() bool
	verify func() error // re-checks pid is still the daemon before SIGKILL
	kill   func() error
}

// run returns once the daemon has exited or SIGKILL has been sent, and reports
// whether it is gone. The false cases are the ones the pid file has to survive:
// a daemon that outlives its budget with a PID that no longer verifies
// (gt-p7zy0), or one that has not gone in the grace after the SIGKILL.
func (w *daemonStopWait) run() bool {
	if !w.alive() {
		return true
	}
	fmt.Fprintf(w.out, "  Waiting up to %s for the daemon (PID %d) to finish shutting down...\n",
		w.budget, w.pid)
	deadline := w.clock.Now().Add(w.budget)
	for w.alive() {
		if !w.clock.Now().Before(deadline) {
			if w.verify() != nil {
				return false
			}
			fmt.Fprintf(w.out, "  Daemon (PID %d) still running after %s; sending SIGKILL\n",
				w.pid, w.budget)
			_ = w.kill()
			return w.awaitExit(stopKillGrace)
		}
		w.clock.Sleep(w.poll)
	}
	return true
}

// stopKillGrace is how long run waits for a SIGKILLed daemon to disappear
// before it reports the daemon still running.
const stopKillGrace = 2 * time.Second

// awaitExit waits up to grace for the process to go.
func (w *daemonStopWait) awaitExit(grace time.Duration) bool {
	deadline := w.clock.Now().Add(grace)
	for w.alive() {
		if !w.clock.Now().Before(deadline) {
			return false
		}
		w.clock.Sleep(w.poll)
	}
	return true
}

// errContainerStopDaemon is StopDaemon's refusal for a container town. The
// daemon it would signal runs in the container's namespace, where a pid names
// nothing on this host, so the stop is the supervisor's and never a signal.
var errContainerStopDaemon = errors.New("refusing to stop the daemon with a signal: this town's daemon runs, or may run, under the container supervisor (daemon.supervisor is container, or the setting cannot be read and the town has a container compose file), so no PID on this host is known to be it; stop it through that supervisor instead")

// daemonStopSignals is the pair of signals StopDaemon sends the daemon's
// process. It is a field set so a test can supply a fake and prove a refusal
// never reaches one.
type daemonStopSignals struct {
	term func(*os.Process) error
	kill func(*os.Process) error
}

// hostDaemonStopSignals is the pair the real machine sends.
func hostDaemonStopSignals() daemonStopSignals {
	return daemonStopSignals{term: sendTermSignal, kill: sendKillSignal}
}

// StopDaemon stops the running daemon for the given town.
// Note: The file lock in Run() prevents multiple daemons per town, so we only
// need to kill the process from the PID file.
func StopDaemon(townRoot string) error {
	return stopDaemonWith(townRoot, hostDaemonStopSignals())
}

// stopDaemonWith is StopDaemon on the signals the caller supplies.
func stopDaemonWith(townRoot string, sigs daemonStopSignals) error {
	// The kind comes first, and the refusal is unconditional: a container
	// town's daemon is not a process on this host at all, and neither file
	// can say otherwise — daemon.lock's flock does not cross a Docker Desktop
	// bind mount, and daemon.pid names a process in the container's
	// namespace. Answering "not running" here would be a claim about the
	// container this has no evidence for, and answering with the lock's PID
	// is how a host process gets signaled in the container's place
	// (gt-qpeei.19).
	container := containerTown(townRoot)
	if container {
		return errContainerStopDaemon
	}

	running, pid, err := isRunningOn(townRoot, container, probeDaemonLock)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	if pid <= 0 {
		// The lock is held, and an flock is released by the kernel when its
		// holder dies, so a live process owns it: a daemon still starting (the
		// lock is taken before the pid file is written) or one whose pid file
		// is damaged. Unlinking the lock file would let the next gt up lock a
		// fresh inode and start a second daemon beside it, so leave both files
		// alone and let the caller retry (gt-dicyp).
		return fmt.Errorf("daemon holds %s but its pid is not known yet (still starting, or its pid file is damaged); retry in a few seconds",
			filepath.Join(townRoot, "daemon", "daemon.lock"))
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

	// Tell the daemon this stop is deliberate before signaling it: a signal
	// without the marker makes it exit 75 so launchd relaunches it (gt-swsqm).
	if err := writeStopRequested(townRoot, pid, time.Now()); err != nil {
		return fmt.Errorf("recording the stop request: %w", err)
	}

	// Send termination signal for graceful shutdown
	if err := sigs.term(process); err != nil {
		removeStopRequested(townRoot)
		return fmt.Errorf("sending termination signal: %w", err)
	}

	// Give shutdown() the time it is budgeted before forcing the issue.
	// StopDaemon runs outside any Daemon, so it waits on the real clock.
	gone := (&daemonStopWait{
		clock:  clockwork.NewRealClock(),
		out:    os.Stdout,
		pid:    pid,
		budget: StopWaitBudget,
		poll:   stopPollInterval,
		alive:  func() bool { return isProcessAlive(process) },
		// Re-verified: the PID may have exited and been reused during the
		// wait (gt-p7zy0).
		verify: func() error { return verifyGTDaemonPID(townRoot, pid) },
		kill:   func() error { return sigs.kill(process) },
	}).run()

	return finishStopDaemon(townRoot, pid, gone)
}

// finishStopDaemon clears the pid file of a daemon whose stop has settled, and
// reports a daemon that is still running instead of clearing it (gt-fsgb1).
//
// A pid file removed while its daemon still runs takes the PID with it:
// IsRunning then answers (true, 0), and every later stop refuses with "pid is
// not known yet" until that daemon restarts, because nothing rewrites the file
// after startup. A pid file written by a new daemon — it started as soon as the
// old one let go of the lock — stays too, and daemon.lock is what tells them
// apart: the file is removed only while the probe holds the lock shared, so a
// daemon cannot take the lock and write its pid file between the probe and the
// removal (a check made unlocked would delete that fresh file). A lock that is
// held, or cannot be probed, leaves the file alone.
func finishStopDaemon(townRoot string, pid int, gone bool) error {
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	if !gone {
		return fmt.Errorf("stopping daemon (PID %d) did not finish within %s; left %s in place so a later stop can still find it",
			pid, StopWaitBudget, pidFile)
	}
	held, probeLock, err := probeDaemonLock(filepath.Join(townRoot, "daemon", "daemon.lock"))
	if err != nil || held {
		return nil
	}
	if probeLock == nil {
		// No daemon.lock file: no daemon has run here, so there is no owner to
		// protect and nothing to race.
		_ = os.Remove(pidFile)
		return nil
	}
	settleFreeDaemonLock(pidFile, probeLock.Unlock)
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
	held, probeLock, err := probeDaemonLock(lockPath)
	if err != nil {
		return nil, nil // Can't check lock — assume no orphans
	}

	if !held {
		// No daemon holds the lock, no orphans possible
		if probeLock != nil {
			_ = probeLock.Unlock()
		}
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

		// Wait for graceful shutdown (outside any Daemon: the real clock).
		clockwork.NewRealClock().Sleep(200 * time.Millisecond)

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

// parkedAge renders how long a seat has been parked: whole minutes under an
// hour, hours and minutes after.
func parkedAge(d time.Duration) string {
	m := int(d.Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}

// The rig LED glyphs the crash-skip line quotes, as `gt rig list` prints them.
// Their meanings are GetRigLED's (internal/cmd); this package cannot import
// that one without a cycle, so the two states a paused seat can be confused
// with are named once here instead of spelled into the log's format string
// (gt-v7jod).
const (
	rigLEDParked  = "🅿️"
	rigLEDStopped = "⚫"
)

// rigLEDLegend spells out the two rig states a paused polecat seat can leave
// behind, for the crash-skip line that must tell a paused seat from a stopped
// rig on its own.
func rigLEDLegend() string {
	return rigLEDParked + " parked, " + rigLEDStopped + " nothing running"
}

// checkPolecatHealth checks a single polecat's session health.
// If the polecat has work-on-hook but the tmux session is dead, it's restarted.
func (d *Daemon) checkPolecatHealth(rigName, polecatName string) {
	// The seat's intent record says whether it is held and, when a writer
	// set it, what work it holds. It is read before tmux and never from
	// Dolt, and an unreadable record is a hold (fail closed). A polecat the
	// operator parked — gt agent pause, or the deliberate stop gt session
	// stop records (gt-fojqs) — has a dead session on purpose.
	seat := supervisor.SeatIn(d.prefixRegistry(), rigName, constants.RolePolecat, polecatName)
	// The name is computed here, not at the tmux check below, because the
	// parked branch asks tmux whether the seat still has a session.
	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rigName), polecatName)
	// A seat that reaches any outcome other than a logged skip has left the
	// skip state; its next skip is news and is logged again.
	skipped := false
	defer func() {
		if !skipped {
			d.crashSkipLog.forget(rigName + "/" + polecatName)
		}
	}()
	rec, err := intent.Read(d.config.TownRoot, supervisor.IntentSeat(seat))
	if err != nil || rec.Held() {
		skipped = true
		// The pool releases a stale park's seat only when its session is gone
		// (parkedStale in internal/cmd), so this says released only there; an
		// unanswered tmux read is no proof, and the park itself stays until an
		// operator recovers it.
		now := time.Now()
		if parked, st, perr := agentpause.IsPaused(d.config.TownRoot, rigName, constants.RolePolecat, polecatName); err == nil && perr == nil && parked && agentpause.ParkedStale(st, now) {
			if alive, serr := d.tmux.HasSession(sessionName); serr == nil && !alive {
				age := parkedAge(now.Sub(st.PausedAt))
				d.crashSkipLog.logAs(d.logger.Printf, rigName+"/"+polecatName, "parked-released",
					fmt.Sprintf("Skipping crash detection for %s/%s: polecat seat paused %s ago (%s); seat released; recover with gt polecat check-recovery %s/%s; this is the seat, not the rig (gt rig status %s: %s)",
						rigName, polecatName, age, rec.HoldReason(), rigName, polecatName, rigName, rigLEDLegend()))
				return
			}
		}
		d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: polecat seat paused (%s)",
			rigName, polecatName, rec.HoldReason())
		return
	}

	// gt done recorded desired=submitted: the branch is on origin for the
	// landing worker and the session ended on purpose. The hook still holds
	// the bead until it lands, which would read as a crash (gt-obbx2).
	//
	// The record is a wait, and the wait is real only while the bead it names
	// still reads as submitted. gt done writes the label before the record,
	// so a bead that no longer carries it was pulled after the submission —
	// for rework, or by a human — and only the record outlived it. That seat
	// has to fall through to the ordinary path, or nothing ever restarts it
	// (gt-xs1ni). An unreadable bead keeps the skip: a failed read proves
	// nothing, and falling through on it would call a landing still in flight
	// a crash.
	if rec.Submitted() {
		if still, known := d.workBeadStillSubmitted(rigName, rec.WorkBead); !known || still {
			skipped = true
			d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: work %s is submitted for landing",
				rigName, polecatName, rec.WorkBead)
			return
		}
	}

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
	// do not hold the work (gt-4k3fj.1, G1-01): they are read on this path only
	// to refuse a report, first for the spawn grace below and again for a seat
	// that stopped on its own escalation, and never to find the work.
	hookBead := rec.WorkBead
	if hookBead == "" {
		assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
		work, werr := d.assignedActiveWorkBead(rigName, assignee)
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
		hookBead = work
	}

	// Finished work: gt done closes the work bead before the session stops,
	// so a dead session holding closed work completed normally. The same read
	// dates the bead, which is what the spawn grace below measures.
	closed, submitted, workUpdated := d.beadFinished(hookBead)
	if closed {
		skipped = true
		d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: hook_bead %s is already closed (work completed normally)",
			rigName, polecatName, hookBead)
		return
	}
	// Submitted work: gt done pushed the branch and labeled the bead
	// gt:ready-to-land; the hook stays on it until the landing worker lands
	// it. This is the label half of the intent-record check above, for a seat
	// whose record was never written (gt-obbx2).
	if submitted {
		skipped = true
		d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: hook_bead %s is submitted for landing",
			rigName, polecatName, hookBead)
		return
	}

	// A seat whose own last event is a `done` for the bead it holds has ended
	// its turn; the bead's close trails that event, and a report-only run that
	// skipped its close leaves the bead hooked over a dead session (gt-0p9pv).
	// The window is bounded, so a done whose bead never closes falls through to
	// the crash below rather than hiding the seat for good.
	if age, ok := d.seatLastDoneAge(rigName, polecatName, hookBead, d.clk().Now()); ok && age < polecatDoneCloseGrace {
		skipped = true
		d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: it ran gt done for %s %s ago and the bead has not closed yet",
			rigName, polecatName, hookBead, age.Round(time.Second))
		return
	}

	// Spawn grace (gt-6hby3): gt sling hooks the work bead before the tmux
	// session exists, so a bead hooked moments ago with no session yet may be
	// a polecat starting up, and reporting it would double-spawn (issue
	// #1752). Only an explicit spawning agent state earns the grace
	// (polecat.SpawnGrace): a running polecat keeps writing its own work
	// bead, so that write says nothing about a dispatch. The agent record is
	// read lazily, inside the window only, and never to find the work
	// (G1-01); one that cannot be read proves no dispatch. Its own last write
	// dates the spawn, so a record left at spawning by a polecat that has run
	// longer than the window earns nothing (gt-i3ky2).
	if !workUpdated.IsZero() {
		if age := time.Since(workUpdated); age < polecatSpawnGrace {
			if rec, ok := d.polecatAgentRecord(rigName, polecatName); ok &&
				polecat.SpawnGrace(rec.State, workUpdated, time.Now(), polecatSpawnGrace) &&
				polecat.SpawnGrace(rec.State, rec.UpdatedAt, time.Now(), polecatSpawnGrace) {
				skipped = true
				d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: work %s hooked %s ago and agent_state=%s, polecat may be spawning",
					rigName, polecatName, hookBead, age.Round(time.Second), rec.State)
				return
			}
		}
	}

	// TOCTOU guard: re-verify session is still dead before restarting.
	// Between the initial check and now, the session may have been restarted
	// by another heartbeat cycle, witness, or the polecat itself.
	sessionRevived, err := d.tmux.HasSession(sessionName)
	if err == nil && sessionRevived {
		return // Session came back - no restart needed
	}

	// A polecat that stopped on purpose to raise a decision is not a crash: it
	// exited ESCALATED, and the bead it holds is waiting on the escalation that
	// turn raised for it (gt-4hduq). patrol_scan owns the seat and resumes it
	// on its preserved branch once that escalation closes, so this pass says so
	// once and stops there: reporting it every heartbeat buried the log, and
	// nothing here restarts a seat. The record is read only to refuse.
	if held, detail := d.seatHeldOnEscalation(rigName, polecatName, hookBead); held {
		skipped = true
		d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName,
			"Skipping crash detection for %s/%s: %s", rigName, polecatName, detail)
		return
	}

	// Polecat has work but session is dead - this is a crash!
	d.logger.Printf("CRASH DETECTED: polecat %s/%s has hook_bead=%s but session %s is dead",
		rigName, polecatName, hookBead, sessionName)

	// Track this death for mass death detection
	d.recordSessionDeath(sessionName)

	// Emit session_death event for audit trail / feed visibility
	_ = events.LogFeedTo(d.config.TownRoot, events.TypeSessionDeath, sessionName,
		events.SessionDeathPayload(sessionName, rigName+"/polecats/"+polecatName, "crash detected by daemon health check", events.CallerDaemon))

	// A sandbox seat whose sandbox is gone and whose work was handed back
	// keeps a hook nothing releases; clear it so the roster stops counting the
	// seat as dead-hooked.
	stale := &patrolScanHost{d: d, openRecoveryBeads: d.openRecoveryBeads, sandboxExists: d.sandboxExists}
	if stale.reapStaleSandboxHook(rigName, polecatName, sessionName, hookBead) {
		return
	}

	// The patrol_scan tick owns the restart (gt-4k3fj.6); a rig it does not
	// cover gets no automatic restart.
	if d.patrolScanActiveForRig(rigName) {
		d.logger.Printf("Crash of %s/%s left to patrol_scan (restart through the supervisor)", rigName, polecatName)
		return
	}
	d.logger.Printf("Crash of %s/%s not restarted: patrol_scan does not cover rig %s", rigName, polecatName, rigName)
}

// seatHeldOnEscalation reports whether a polecat's own record says it stopped
// on an escalation (agent_state stuck, exit ESCALATED) that is not resolved,
// and the line to say so: such a seat is waiting on a human decision, not
// crashed, and patrol_scan releases it once the escalation closes (gt-4hduq).
//
// Anything else is not held and crash detection goes on as before: another
// exit type, a seat that never escalated, or a record that could not be read.
// The rule is the tick's own (patrolscan.SeatEscalationHold), shared with the
// overrun guard, so the sites cannot disagree about which seat is waiting on a
// decision.
func (d *Daemon) seatHeldOnEscalation(rigName, polecatName, hookBead string) (bool, string) {
	h := &patrolScanHost{d: d}
	rec, err := h.AgentRecord(rigName, polecatName)
	if err != nil {
		return false, ""
	}
	v, err := patrolscan.SeatEscalationHold(rec, rigName, polecatName, hookBead, func() ([]patrolscan.Escalation, error) {
		return h.PolecatEscalations(rigName)
	})
	if err == nil && v.Verdict == patrolscan.EscalationNotStopped {
		return false, ""
	}
	if err != nil {
		// An unreadable read is the tick's Unknown too: nothing is restarted or
		// reported as a crash on a guess.
		return true, "exit ESCALATED and its escalations could not be read: " + err.Error()
	}
	if v.Verdict == patrolscan.EscalationResolved {
		return true, fmt.Sprintf("exit ESCALATED and %s for %s is closed; patrol_scan resumes the seat", v.ID, hookBead)
	}
	return true, v.Detail
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

// workBeadReader is the slice of the beads client the daemon's work-bead
// reads use.
type workBeadReader interface {
	Show(id string) (*beads.Issue, error)
	List(opts beads.ListOptions) ([]*beads.Issue, error)
}

// workBeadReadTimeout bounds one of the idle reaper's work-bead reads. Those
// reads run on the daemon's per-rig pool worker, so a bd that has hung would
// hold the whole reap tick for the rig open until it returned. At the deadline
// bd is killed and the read fails, which the reaper reads as "unknown, do not
// reap" (gt-vfr9r).
const workBeadReadTimeout = 30 * time.Second

// workBeads returns the client for one work-bead read: the daemon's resolved
// bd run from the town root with exactly env, which pins or routes the read,
// killed after timeout (zero waits).
func (d *Daemon) workBeads(env []string, timeout time.Duration) workBeadReader {
	if d.openWorkBeads != nil {
		return d.openWorkBeads(env, timeout)
	}
	b := beads.NewPlain(d.config.TownRoot, env, beads.WithBin(d.bdPathOrDefault()))
	if timeout > 0 {
		b = b.WithTimeout(timeout)
	}
	return b
}

// workBeadsEnv is the read-only environment for a read of rigName's work
// beads: pinned to the rig's database, or routed from the town root when the
// rig has no directory.
func (d *Daemon) workBeadsEnv(rigName string) []string {
	if rigDir := beads.GetRigDirForName(d.config.TownRoot, rigName); rigDir != "" {
		return bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(rigDir))
	}
	return bdReadOnlyRoutingEnv(d.config.TownRoot)
}

// beadFinished reads a bead and reports whether its work is over for crash
// detection: closed (status "closed"), or submitted for landing (label
// gt:ready-to-land, not closed). On any error (bead not found, bd failure)
// both are false, erring toward crash detection rather than silently
// suppressing alerts. It also returns the bead's last write, zero when the
// read failed or the timestamp does not parse, which is what the spawn grace
// dates the dispatch against (gt-6hby3).
func (d *Daemon) beadFinished(beadID string) (closed, submitted bool, updated time.Time) {
	closed, submitted, updated, _ = d.beadFinishedLabels(beadID)
	return closed, submitted, updated
}

// beadFinishedLabels is beadFinished plus the bead's labels, from the same
// read, for a caller that also needs to know what kind of bead it is. The
// labels are nil when the read failed.
func (d *Daemon) beadFinishedLabels(beadID string) (closed, submitted bool, updated time.Time, labels []string) {
	st, err := d.workBeadState(beadID)
	if err != nil {
		return false, false, time.Time{}, nil
	}
	return st.closed, st.submitted, st.updated, st.labels
}

// workBeadState is what one read of a work bead says about it: the crash
// detectors' finished answer (beadFinished), its labels, and when it was last
// submitted for landing. A caller that must not act on a bead it could not read
// takes the error instead of the zero value.
type workBeadState struct {
	closed, submitted bool
	updated           time.Time
	labels            []string
	// submittedAt is the time on the bead's last READY TO LAND block, zero when
	// it carries none. It is the end of the run that made the submission, which
	// the overrun guard dates a rework from (gt-chd44).
	submittedAt time.Time
}

// workBeadState reads beadID once through the town's routing environment.
func (d *Daemon) workBeadState(beadID string) (workBeadState, error) {
	issue, err := d.workBeads(bdReadOnlyRoutingEnv(d.config.TownRoot), 0).Show(beadID)
	if err != nil {
		return workBeadState{}, err
	}
	var st workBeadState
	st.updated, _ = time.Parse(time.RFC3339, issue.UpdatedAt)
	st.labels = issue.Labels
	st.submittedAt = land.ReadyBlockTime(issue.Notes)
	if issue.Status == "closed" {
		st.closed = true
		return st, nil
	}
	st.submitted = slices.Contains(issue.Labels, land.LabelReadyToLand)
	return st, nil
}

// workBeadStillSubmitted reports whether the work bead a submitted record names is
// still submitted for landing (gt:ready-to-land, not terminal), and whether
// that could be answered at all. A bead bd says does not exist is an answer —
// it cannot be waiting to land — but an empty ID or a failed read is not:
// known=false, which every caller must read as "leave the seat alone". The
// record is the wait, and a read that failed is no evidence it ended.
func (d *Daemon) workBeadStillSubmitted(rigName, beadID string) (still, known bool) {
	if beadID == "" {
		return false, false
	}
	issue, err := d.workBeads(d.workBeadsEnv(rigName), 0).Show(beadID)
	if errors.Is(err, beads.ErrNotFound) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	return polecat.IsSubmittedWork(issue), true
}

// assignedWork lists assignee's work beads in status from rigName's database,
// under the reaper's read budget so a hung bd cannot hold the rig pool worker.
func (d *Daemon) assignedWork(rigName, assignee, status string) ([]*beads.Issue, error) {
	return d.workBeads(d.workBeadsEnv(rigName), workBeadReadTimeout).List(beads.ListOptions{Assignee: assignee, Status: status, Priority: -1})
}

// hasAssignedOpenWork checks if any work bead is assigned to the given polecat
// with a non-terminal status (hooked, in_progress, or open). This is the
// authoritative source of polecat work — the sling code sets status=hooked +
// assignee on the work bead, but no longer maintains the agent bead's hook_bead
// field (updateAgentHookBead is a no-op). Without this fallback, the idle reaper
// kills working polecats whose agent bead hook_bead is stale.
//
// A query that fails and finds nothing is an error, not "no work": the failed
// status may be the one holding the work, so a caller must read the error as
// unknown and not reap (gt-vfr9r). assignedActiveWorkBead answers by the same
// rule. A bead found by any query is work, whatever else failed.
func (d *Daemon) hasAssignedOpenWork(rigName, assignee string) (bool, error) {
	var lastErr error
	for _, status := range []string{"hooked", "in_progress", "open"} {
		issues, err := d.assignedWork(rigName, assignee, status)
		if err != nil {
			lastErr = fmt.Errorf("bd list --status=%s: %w", status, err)
			continue
		}
		if len(issues) > 0 {
			return true, nil
		}
	}
	return false, lastErr
}

// assignedActiveWorkBead returns the ID of a work bead assigned to the
// polecat with status hooked or in_progress, read from the rig's database the
// way hasAssignedOpenWork does, or "" when there is none. A bead found by any
// query is returned; otherwise any failed query makes the answer unknown (an
// error), since the failed status may be the one holding the work. Callers
// that need the bead's own record read it back by ID (beadFinished), which is
// where its last write comes from.
func (d *Daemon) assignedActiveWorkBead(rigName, assignee string) (string, error) {
	ids, err := d.assignedActiveWorkBeads(rigName, assignee, 1)
	if len(ids) > 0 {
		return ids[0], nil
	}
	return "", err
}

// assignedActiveWorkBeads lists the IDs of the work beads assigned to the
// polecat with status hooked or in_progress, hooked first, and stops querying
// once limit are found. The error is the last failed query's: a caller that
// needs every assignment reads it as unknown, since the failed status may be
// the one holding the rest.
func (d *Daemon) assignedActiveWorkBeads(rigName, assignee string, limit int) ([]string, error) {
	var ids []string
	var lastErr error
	for _, status := range []string{"hooked", "in_progress"} {
		issues, err := d.assignedWork(rigName, assignee, status)
		if err != nil {
			lastErr = fmt.Errorf("bd list --status=%s: %w", status, err)
			continue
		}
		for _, issue := range issues {
			if issue != nil && issue.ID != "" && !slices.Contains(ids, issue.ID) {
				ids = append(ids, issue.ID)
			}
		}
		if len(ids) >= limit {
			break
		}
	}
	return ids, lastErr
}

// polecatSpawnGrace is how long after its work bead was hooked a polecat
// whose agent record still says spawning is taken to be starting up rather
// than crashed.
const polecatSpawnGrace = 5 * time.Minute

// polecatDoneCloseGrace is how long after its own `done` event a seat over a
// bead that has not closed is read as a finished turn rather than a crash: the
// window the bead's close has to arrive in (gt-0p9pv).
const polecatDoneCloseGrace = 5 * time.Minute

// seatLastDoneAge returns how long ago the seat's own most recent event was,
// when that event is a `done` naming hookBead. The event log is a best-effort
// record, so a log that cannot be read, a seat with no events, and a newer
// event of the seat's all answer false: none of them shows a finished turn.
func (d *Daemon) seatLastDoneAge(rigName, polecatName, hookBead string, now time.Time) (time.Duration, bool) {
	evs, err := events.Read(d.config.TownRoot)
	if err != nil {
		// The events read so far are still used: a partial log dates a turn
		// that a missing one cannot.
		d.logger.Printf("crash detection: reading the event log for %s/%s: %v", rigName, polecatName, err)
	}
	seat := rigName + "/polecats/" + polecatName
	var last events.Event
	var lastAt time.Time
	for _, ev := range evs {
		if ev.Actor != seat {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil {
			continue
		}
		if ts.After(lastAt) {
			last, lastAt = ev, ts
		}
	}
	if lastAt.IsZero() || last.Type != events.TypeDone {
		return 0, false
	}
	bead, _ := last.Payload["bead"].(string)
	if bead != hookBead {
		return 0, false
	}
	// An event dated ahead of now is a clock skew, not an age; reading it as 0
	// keeps the seat inside the grace rather than past it.
	return max(now.Sub(lastAt), 0), true
}

// polecatAgentRecord reads a seat's agent bead, and reports whether that could
// be read at all: false for a seat with no bead and for a failed read, either
// of which proves no dispatch. It reads through the patrol scan's own reader,
// so the crash detector and the tick that owns the restart ask the same
// question of the same record (gt-6hby3).
func (d *Daemon) polecatAgentRecord(rigName, polecatName string) (patrolscan.AgentRecord, bool) {
	rec, err := (&patrolScanHost{d: d}).AgentRecord(rigName, polecatName)
	if err != nil {
		return patrolscan.AgentRecord{}, false
	}
	return rec, true
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
//
// Unfinished assigned work suspends the reap in every state. A heartbeat cannot
// separate a finished polecat from one that is deliberately waiting, so the reap
// reads the work bead (gt-7lwft). A work read that fails suspends it too: an
// unanswerable read is unknown, and killing on unknown strands live work
// (gt-vfr9r).
func (d *Daemon) reapIdlePolecat(rigName, polecatName string, timeout time.Duration) {
	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rigName), polecatName)

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

	// Explicitly idle or exiting — reapable only with no work left to strand.
	// gt done writes state="exiting" at the top of its flow and a run that
	// exits 15 on a red local gate (or 10/11/14 on a failed push or rebase)
	// never writes anything else on the way out, so "exiting" covers both the
	// polecat that submitted and the one sitting on committed, unpushed work
	// waiting for the gate to go green (gt-7lwft). The work bead is the
	// discriminator the rest of the town already applies to this question:
	// patrol scan will not retire a seat that holds work, and the "working"
	// branch below has always stopped here. The trade is that a session wedged
	// inside gt done with work on its hook waits for the crash path instead.
	if state == polecat.HeartbeatIdle || state == polecat.HeartbeatExiting {
		assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
		hasWork, err := d.hasAssignedOpenWork(rigName, assignee)
		if err != nil {
			d.logger.Printf("Not reaping %s/%s: cannot read assigned work (idle %v): %v",
				rigName, polecatName, staleDuration.Truncate(time.Second), err)
			return
		}
		if hasWork {
			d.logger.Printf("Not reaping %s/%s: %s heartbeat but still holds assigned work (idle %v)",
				rigName, polecatName, state, staleDuration.Truncate(time.Second))
			return
		}
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
		hasWork, err := d.hasAssignedOpenWork(rigName, assignee)
		if err != nil {
			d.logger.Printf("Not reaping %s/%s: cannot read assigned work (idle %v): %v",
				rigName, polecatName, staleDuration.Truncate(time.Second), err)
			return
		}
		if hasWork {
			return
		}
		seat := supervisor.SeatIn(d.prefixRegistry(), rigName, constants.RolePolecat, polecatName)
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
	seat := supervisor.SeatIn(d.prefixRegistry(), rigName, constants.RolePolecat, polecatName)
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
		// No tmux server is the sweep refusing to signal, not a failure (gt-r2kvf).
		if errors.Is(err, util.ErrTmuxNoServer) {
			return
		}
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

// dispatchQueuedWork dispatches scheduled beads through internal/schedulerrun,
// the same code `gt scheduler run` runs, in this process (gt-638go.9). The
// deps it needs live in package cmd and arrive through SetSchedulerDeps.
//
// Timeout safety: if the timeout fires mid-dispatch, a bead may be left with
// metadata written but label not yet swapped (or vice versa). The dispatch flock
// is released on process death, and the sling path's label swap retry logic
// prevents double-dispatch on the next cycle. The batch_size config (default: 1)
// limits how many beads are in-flight per heartbeat, reducing the timeout window.
func (d *Daemon) dispatchQueuedWork() {
	// Scheduled dispatch slings queued beads (executeSling); the operator's
	// town-wide hold parks it like every other automatic dispatcher
	// (gt-ifijm). ESTOP holds this heartbeat step (heartbeatSteps); the
	// hold file does not, so it is checked here.
	reason := dispatch.OperatorHold(d.config.TownRoot)
	if d.queuedWorkHold.Changed(reason) {
		if reason != "" {
			d.logger.Printf("Deferring scheduler dispatch: %s", reason)
		} else {
			d.logger.Printf("Resuming scheduler dispatch: operator dispatch hold lifted (%s)", d.autoDispatchers())
		}
	}
	if reason != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulerDispatchTimeout)
	defer cancel()
	d.dispatchScheduledWork(ctx)
}

// autoDispatchers names the automatic dispatchers the operator hold parks and
// whether each is on, for the hold-lifted line: an operator reading it learns
// what came back without a second lookup (gt-xiw7o). The spec dispatcher is
// the only one (gt-gzhin.6).
func (d *Daemon) autoDispatchers() string {
	return fmt.Sprintf("spec_dispatch %s", onOff(d.isPatrolActive("spec_dispatch")))
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
