package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
)

// scheduled_maintenance mode "gc": history-preserving garbage collection.
//
// Per database, in the maintenance window: when its on-disk size is at least
// gc_min_bytes AND has grown by gc_growth_ratio since the size recorded after
// its last gc (or no such record exists yet), run CALL dolt_gc('--full') on it
// over the daemon's SQL connection to the running server. It never flattens,
// never rewrites a commit, never pushes — see
// docs/plans/2026-09-25-dolt-gc-maintenance-design.md (claude-05o).
//
// The trigger is size, not commit count; the measurements behind that are in
// the design doc's Problem section.

const (
	// MaintenanceModeGC runs CALL dolt_gc('--full') on each database that
	// crossed the size trigger. History is kept.
	MaintenanceModeGC = "gc"

	// DefaultGCMinBytes is the size floor below which a database is never
	// gc'd by the patrol: a small database is not worth a --full pass.
	DefaultGCMinBytes = int64(256 * 1024 * 1024)

	// DefaultGCGrowthRatio is how much a database must have grown since its
	// last post-gc size before the next gc.
	DefaultGCGrowthRatio = 2.0

	// maintenanceGCTimeout bounds one CALL dolt_gc('--full'). A prod --full
	// gc takes seconds per database (design doc, Problem); ten minutes is a
	// hang, not a slow gc.
	maintenanceGCTimeout = 10 * time.Minute

	// maintenancePolecatFreshness is how recent a polecat's "working"
	// heartbeat must be to count as work in flight. Polecats renew it on
	// every gt sub-command; an older one is a stalled or finished session.
	maintenancePolecatFreshness = 15 * time.Minute

	maintenanceGCStateFileName = "maintenance_state.json"
)

// maintenanceGCPolicy is the resolved size trigger.
type maintenanceGCPolicy struct {
	minBytes    int64
	growthRatio float64
}

// maintenanceGCPolicyFor resolves gc_min_bytes and gc_growth_ratio, replacing
// invalid values with the defaults. The returned warnings name every value
// that was replaced, for the log: a hand-edited daemon.json must not fail
// silently toward a different trigger than the file says.
func maintenanceGCPolicyFor(config *DaemonPatrolConfig) (maintenanceGCPolicy, []string) {
	p := maintenanceGCPolicy{minBytes: DefaultGCMinBytes, growthRatio: DefaultGCGrowthRatio}
	var warnings []string
	if config == nil || config.Patrols == nil || config.Patrols.ScheduledMaintenance == nil {
		return p, nil
	}
	mc := config.Patrols.ScheduledMaintenance
	if mc.GCMinBytes != nil {
		if *mc.GCMinBytes > 0 {
			p.minBytes = *mc.GCMinBytes
		} else {
			warnings = append(warnings, fmt.Sprintf("gc_min_bytes=%d is not positive; using %d", *mc.GCMinBytes, DefaultGCMinBytes))
		}
	}
	if mc.GCGrowthRatio != nil {
		r := *mc.GCGrowthRatio
		if r >= 1.0 && !math.IsInf(r, 0) && !math.IsNaN(r) {
			p.growthRatio = r
		} else {
			warnings = append(warnings, fmt.Sprintf("gc_growth_ratio=%v is not a finite number >= 1; using %v", r, DefaultGCGrowthRatio))
		}
	}
	return p, warnings
}

// shouldGCDatabase applies the size trigger. baseline is the size recorded
// after the database's last patrol gc, 0 when there is none.
func shouldGCDatabase(size, baseline int64, p maintenanceGCPolicy) (bool, string) {
	if size < p.minBytes {
		return false, fmt.Sprintf("below gc_min_bytes %s", formatBytes(p.minBytes))
	}
	if baseline <= 0 {
		return true, "no post-gc baseline yet"
	}
	ratio := float64(size) / float64(baseline)
	if ratio < p.growthRatio {
		return false, fmt.Sprintf("%.2fx last post-gc size %s, under gc_growth_ratio %v", ratio, formatBytes(baseline), p.growthRatio)
	}
	return true, fmt.Sprintf("%.2fx last post-gc size %s", ratio, formatBytes(baseline))
}

// formatBytes renders a byte count in MiB for logs and escalations.
func formatBytes(n int64) string {
	return fmt.Sprintf("%.1fMiB", float64(n)/(1024*1024))
}

// --- state file ----------------------------------------------------------------

// maintenanceGCState is <town>/daemon/maintenance_state.json: the size each
// database had right after its last patrol gc, which is the growth baseline.
//
// It also carries the skipped-window streak (see maintenance_gc_guard.go): a
// window that closes with gc still deferred on at least one eligible database
// counts once, and a completed or failed run resets the streak.
type maintenanceGCState struct {
	PostGCBytes map[string]int64     `json:"post_gc_bytes"`
	LastGC      map[string]time.Time `json:"last_gc"`

	// PendingDeferral is the current window's latest deferral, if any. It is
	// counted into ConsecutiveDeferredWindows once its window has closed.
	PendingDeferral *gcDeferral `json:"pending_deferral,omitempty"`
	// ConsecutiveDeferredWindows is how many windows in a row closed with gc
	// deferred.
	ConsecutiveDeferredWindows int `json:"consecutive_deferred_windows"`
	// LastDeferralReason is the most recent quiet-guard (or lock) reason.
	LastDeferralReason string `json:"last_deferral_reason,omitempty"`
	// DeferralReasons counts deferred attempts per reason over the current
	// streak. It counts attempts, not windows: a busy window can defer on
	// every 5-minute tick, so one window can add up to ~12 to a reason.
	DeferralReasons map[string]int `json:"deferral_reasons,omitempty"`
}

// gcDeferral records one window's deferral: when the window closes, why the
// cycle stopped, and the databases still waiting.
type gcDeferral struct {
	WindowEnd time.Time      `json:"window_end"`
	Reason    string         `json:"reason"`
	Databases []gcDeferredDB `json:"databases"`
}

// gcDeferredDB is a database a deferred cycle did not reach.
type gcDeferredDB struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// maintenanceGCStateMu serializes the state file's read-modify-write.
var maintenanceGCStateMu sync.Mutex

func maintenanceGCStatePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", maintenanceGCStateFileName)
}

// loadMaintenanceGCState reads the baselines. A missing file is empty state; a
// corrupt one is an error the caller logs before treating it as empty (gc is
// non-destructive, so "no baseline" is a safe reading).
func loadMaintenanceGCState(townRoot string) (maintenanceGCState, error) {
	st := maintenanceGCState{PostGCBytes: map[string]int64{}, LastGC: map[string]time.Time{}}
	data, err := os.ReadFile(maintenanceGCStatePath(townRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	var parsed maintenanceGCState
	if err := json.Unmarshal(data, &parsed); err != nil {
		return st, fmt.Errorf("parse %s: %w", maintenanceGCStateFileName, err)
	}
	for k, v := range parsed.PostGCBytes {
		st.PostGCBytes[k] = v
	}
	for k, v := range parsed.LastGC {
		st.LastGC[k] = v
	}
	st.PendingDeferral = parsed.PendingDeferral
	st.ConsecutiveDeferredWindows = parsed.ConsecutiveDeferredWindows
	st.LastDeferralReason = parsed.LastDeferralReason
	st.DeferralReasons = parsed.DeferralReasons
	return st, nil
}

// updateMaintenanceGCState applies fn to the state under the write lock and
// writes it back atomically. A corrupt file is replaced rather than failing
// forever. fn's view is the state fn writes; the returned copy is what landed.
func updateMaintenanceGCState(townRoot string, fn func(*maintenanceGCState)) (maintenanceGCState, error) {
	maintenanceGCStateMu.Lock()
	defer maintenanceGCStateMu.Unlock()

	st, _ := loadMaintenanceGCState(townRoot)
	fn(&st)

	path := maintenanceGCStatePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return st, err
	}
	return st, atomicfile.WriteJSON(path, st)
}

// recordMaintenanceGCBaseline records one database's post-gc size, keeping the
// other entries. A corrupt file is replaced rather than failing forever.
func recordMaintenanceGCBaseline(townRoot, db string, size int64, at time.Time) error {
	_, err := updateMaintenanceGCState(townRoot, func(st *maintenanceGCState) {
		st.PostGCBytes[db] = size
		st.LastGC[db] = at
	})
	return err
}

// --- probes (seams) --------------------------------------------------------------

// maintenanceDBSize sums the regular files under <dataDir>/<db>, including the
// old generation under .dolt/noms/oldgen that only gc --full empties. It only
// stats; it never opens a file inside .dolt.
func maintenanceDBSize(dataDir, db string) (int64, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return 0, err
	}
	root := filepath.Join(dataDir, db)
	if _, err := os.Stat(root); err != nil {
		return 0, err
	}
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A file removed mid-walk (a live table-file swap) is not an error.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// validMaintenanceDBName refuses a database name that is empty, hidden, or
// not a single path element — it is both a directory under the data dir and a
// DSN path component, and must be neither a traversal nor a DSN injection.
func validMaintenanceDBName(db string) error {
	if db == "" || db != filepath.Base(db) || strings.HasPrefix(db, ".") ||
		strings.ContainsAny(db, "?/\\`'\" \t\n") {
		return fmt.Errorf("invalid database name %q", db)
	}
	return nil
}

// maintenanceDBSizeFn measures a database's on-disk size. Seamed so tests never
// walk a real .dolt directory.
var maintenanceDBSizeFn = maintenanceDBSize

// maintenanceGCExecFn runs CALL dolt_gc('--full') on one database. Seamed so
// tests never reach a Dolt server.
var maintenanceGCExecFn = func(ctx context.Context, d *Daemon, db string) error {
	return d.doltGCFull(ctx, db)
}

// maintenanceQuietFn is the quiet-window guard, re-checked before each
// database. Seamed for the cycle tests; maintenanceQuiet has its own.
var maintenanceQuietFn = func(d *Daemon) (bool, string) { return d.maintenanceQuiet() }

// maintenanceSlotHoldersFn lists the roles holding a container-gate slot or an
// in-flight marker. Locks-only: no docker ps.
var maintenanceSlotHoldersFn = func(townRoot string) ([]string, error) {
	cg := config.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	rep, err := slot.StatusPoolLocksOnly(townRoot, slot.Pool{Slots: cg.SlotsV(), ReservedForGate: cg.ReservedForGateV()})
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, s := range rep.Slots {
		if !s.Held {
			continue
		}
		role := "unknown holder"
		if s.Owner != nil && s.Owner.Role != "" {
			role = s.Owner.Role
		}
		holders = append(holders, role)
	}
	return holders, nil
}

// maintenanceWorkingPolecatsFn lists polecats with a fresh "working" heartbeat.
// An error means some rig's polecats could not be listed.
var maintenanceWorkingPolecatsFn = func(d *Daemon) ([]string, error) { return d.workingPolecats() }

// maintenanceGCDispatchFn runs a gc cycle off the daemon's select loop: a
// cycle can hold a --full gc for up to maintenanceGCTimeout per database, and
// running that inline would freeze the heartbeat (gt-uvxy). Tests run inline.
var maintenanceGCDispatchFn = func(fn func()) { go fn() }

// --- quiet-window guard -----------------------------------------------------------

// maintenanceQuiet reports whether gc may run now, with the reason when not.
// A --full gc is the one step that has correlated with a Dolt panic (design
// doc, Problem), so it runs only when nothing else in the town is doing work:
//   - the daemon's own in-flight work is idle (the upgrade-restart predicate),
//   - no main_branch_test, including one still waiting for a slot,
//   - no container-gate slot or in-flight marker held by anyone (refinery gate,
//     batch gate, polecat verification suite, review),
//   - no polecat with a fresh "working" heartbeat.
//
// A probe that cannot answer counts as busy.
func (d *Daemon) maintenanceQuiet() (bool, string) {
	if !d.daemonWorkIdle() {
		return false, "daemon has work in flight (scripts, compactor, triage, slings, dispatch, watchdog, main_branch_test or install)"
	}
	if d.mainBranchTestRunning.Load() {
		return false, "main_branch_test running (waiting for or holding a slot)"
	}
	holders, err := maintenanceSlotHoldersFn(d.config.TownRoot)
	if err != nil {
		return false, fmt.Sprintf("cannot read slot status: %v", err)
	}
	if len(holders) > 0 {
		return false, "slot held by " + strings.Join(holders, ", ")
	}
	working, err := maintenanceWorkingPolecatsFn(d)
	if err != nil {
		return false, fmt.Sprintf("cannot list polecats: %v", err)
	}
	if len(working) > 0 {
		return false, "polecats working: " + strings.Join(working, ", ")
	}
	return true, ""
}

// workingPolecats lists "<rig>/<polecat>" for every polecat worktree whose
// session heartbeat says working and is fresher than
// maintenancePolecatFreshness. File reads only — no tmux, no bd.
//
// A rig with no polecats directory has no polecats. Any other listing error
// is returned: the caller cannot tell whether a polecat is working in that
// rig, so it must count the town as busy.
func (d *Daemon) workingPolecats() ([]string, error) {
	var out []string
	now := time.Now()
	for _, rigName := range d.getKnownRigs() {
		polecats, err := listPolecatWorktrees(filepath.Join(d.config.TownRoot, rigName, "polecats"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("rig %s: %w", rigName, err)
		}
		for _, name := range polecats {
			hb := polecat.ReadSessionHeartbeat(d.config.TownRoot,
				session.PolecatSessionName(session.PrefixFor(rigName), name))
			if hb == nil || hb.EffectiveState() != polecat.HeartbeatWorking {
				continue
			}
			if now.Sub(hb.Timestamp) > maintenancePolecatFreshness {
				continue
			}
			out = append(out, rigName+"/"+name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// --- the gc call -------------------------------------------------------------------

// doltGCFull runs CALL dolt_gc('--full') on db through the running server. The
// connection's read timeout matches the gc bound; compactorOpenDB's 30s would
// cut a slow gc off mid-flight.
func (d *Daemon) doltGCFull(ctx context.Context, db string) error {
	if err := validMaintenanceDBName(db); err != nil {
		return err
	}
	dsn := fmt.Sprintf("root@tcp(%s)/%s?timeout=5s&readTimeout=%s&writeTimeout=30s",
		net.JoinHostPort(d.doltServerHost(), strconv.Itoa(d.doltServerPort())), db, maintenanceGCTimeout)
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	// ExecContext, as internal/cmd/maintain.go runs CALL dolt_gc(): the
	// procedure reports failure as a SQL error, and its status row carries
	// nothing more. TestDoltGCFullAgainstRealServer runs this on a server.
	if _, err := conn.ExecContext(ctx, "CALL dolt_gc('--full')"); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("dolt_gc --full: timeout: %w", err)
		}
		return fmt.Errorf("dolt_gc --full: %w", err)
	}
	return nil
}

// --- the cycle ------------------------------------------------------------------------

type maintenanceGCOutcome int

const (
	// gcOutcomeCompleted: every eligible database was gc'd (or none was eligible).
	gcOutcomeCompleted maintenanceGCOutcome = iota
	// gcOutcomeDeferred: the town was busy before some database; retry on a
	// later tick. Databases already gc'd have fresh baselines and drop out.
	gcOutcomeDeferred
	// gcOutcomeFailed: a gc failed; the run stopped and escalated once.
	gcOutcomeFailed
)

func (o maintenanceGCOutcome) String() string {
	switch o {
	case gcOutcomeCompleted:
		return "completed"
	case gcOutcomeDeferred:
		return "deferred"
	case gcOutcomeFailed:
		return "failed"
	}
	return "unknown"
}

type gcCandidate struct {
	name string
	size int64
}

// maintenanceGCResult is a cycle's outcome plus, for a deferral, why and what
// is still waiting.
type maintenanceGCResult struct {
	outcome maintenanceGCOutcome
	reason  string
	pending []gcCandidate
}

// maintenanceGCCycle measures every database, gc's the eligible ones smallest
// first, and re-checks the quiet-window guard before each.
func (d *Daemon) maintenanceGCCycle(databases []string, dataDir string, p maintenanceGCPolicy) maintenanceGCResult {
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: %v — treating every database as having no post-gc baseline", err)
	}

	var eligible []gcCandidate
	for _, db := range databases {
		size, err := maintenanceDBSizeFn(dataDir, db)
		if err != nil {
			d.logger.Printf("scheduled_maintenance: %s: cannot measure size: %v — skipping", db, err)
			continue
		}
		ok, reason := shouldGCDatabase(size, st.PostGCBytes[db], p)
		if !ok {
			d.logger.Printf("scheduled_maintenance: %s: %s, %s — no gc", db, formatBytes(size), reason)
			continue
		}
		d.logger.Printf("scheduled_maintenance: %s: %s, %s — gc eligible", db, formatBytes(size), reason)
		eligible = append(eligible, gcCandidate{name: db, size: size})
	}
	if len(eligible) == 0 {
		d.logger.Printf("scheduled_maintenance: mode=%s — no database needs gc", MaintenanceModeGC)
		return maintenanceGCResult{outcome: gcOutcomeCompleted}
	}
	// Smallest first: the cheap databases finish before the town can get
	// busy, and the operator runbook proved the order on prod.
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].size < eligible[j].size })

	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}

	defer_ := func(i int, why string) maintenanceGCResult {
		var rest []string
		for _, r := range eligible[i:] {
			rest = append(rest, r.name)
		}
		d.logger.Printf("scheduled_maintenance: town not quiet (%s) — deferring gc of %s to a later tick",
			why, strings.Join(rest, ", "))
		return maintenanceGCResult{outcome: gcOutcomeDeferred, reason: why, pending: eligible[i:]}
	}

	for i, c := range eligible {
		if quiet, why := maintenanceQuietFn(d); !quiet {
			return defer_(i, why)
		}
		// Exclusive against the daemon's own Dolt tasks (backups, remote push,
		// wisp reaper, JSONL export, compactor), which hold the read side.
		if !d.doltMaintMu.TryLock() {
			return defer_(i, "daemon Dolt task in flight")
		}
		resume, paused := maintenanceConvoyPauseFn(d)
		if !paused {
			d.doltMaintMu.Unlock()
			return defer_(i, "convoy poll busy")
		}

		d.logger.Printf("scheduled_maintenance: gc %s: CALL dolt_gc('--full') (before %s)", c.name, formatBytes(c.size))
		start := time.Now()
		d.maintenanceGCOverdueEscalated.Store(false)
		d.maintenanceGCCallStartedAt.Store(start.UnixNano())
		ctx, cancel := context.WithTimeout(parent, maintenanceGCTimeout)
		err := maintenanceGCExecFn(ctx, d, c.name)
		cancel()
		d.maintenanceGCCallStartedAt.Store(0)
		resume()
		d.doltMaintMu.Unlock()
		elapsed := time.Since(start).Round(time.Millisecond)

		if err != nil {
			d.logger.Printf("scheduled_maintenance: gc %s FAILED after %v: %v — stopping this run", c.name, elapsed, err)
			maintenanceEscalateFn(d, "scheduled_maintenance", fmt.Sprintf(
				"scheduled_maintenance: CALL dolt_gc('--full') failed on %s after %v (size before %s): %v\n"+
					"The gc run stopped; remaining databases were not touched. History is intact "+
					"(gc mode never flattens). Collect gt dolt status before any Dolt restart.",
				c.name, elapsed, formatBytes(c.size), err))
			return maintenanceGCResult{outcome: gcOutcomeFailed, reason: err.Error()}
		}

		after, sizeErr := maintenanceDBSizeFn(dataDir, c.name)
		if sizeErr != nil {
			// No baseline means the next interval treats this database as
			// never gc'd and, if it is over gc_min_bytes, gc's it again.
			d.logger.Printf("scheduled_maintenance: gc %s: done in %v, but cannot re-measure: %v — no baseline recorded",
				c.name, elapsed, sizeErr)
			continue
		}
		d.logger.Printf("scheduled_maintenance: gc %s: %s -> %s in %v", c.name, formatBytes(c.size), formatBytes(after), elapsed)
		if err := recordMaintenanceGCBaseline(d.config.TownRoot, c.name, after, time.Now()); err != nil {
			d.logger.Printf("scheduled_maintenance: WARNING: cannot record post-gc baseline for %s: %v", c.name, err)
		}
	}
	return maintenanceGCResult{outcome: gcOutcomeCompleted}
}

// maintenanceDataDir is the Dolt data directory the server serves: the
// managed server's configured data dir, else <town>/.dolt-data.
func (d *Daemon) maintenanceDataDir() string {
	if d.doltServer != nil && d.doltServer.IsEnabled() && d.doltServer.config.DataDir != "" {
		return d.doltServer.config.DataDir
	}
	return filepath.Join(d.config.TownRoot, ".dolt-data")
}

// startMaintenanceGC dispatches a gc cycle unless one is already running. The
// cycle's completion time is handed back through maintenanceGCFinishedAt and
// folded into lastMaintenanceRun on the loop goroutine; a deferred cycle
// leaves it untouched so the next 5-minute tick retries.
//
// windowEnd is when the current maintenance window closes; a deferral is
// recorded against it and counted toward the skipped-window streak once it
// has passed.
func (d *Daemon) startMaintenanceGC(databases []string, windowEnd time.Time) {
	if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
		d.logger.Printf("scheduled_maintenance: previous gc cycle still running — skipping this tick")
		return
	}
	p, warnings := maintenanceGCPolicyFor(d.patrolConfig)
	for _, w := range warnings {
		d.logger.Printf("scheduled_maintenance: WARNING: %s", w)
	}
	dataDir := d.maintenanceDataDir()
	d.logger.Printf("scheduled_maintenance: mode=%s — gc_min_bytes=%s gc_growth_ratio=%v data_dir=%s",
		MaintenanceModeGC, formatBytes(p.minBytes), p.growthRatio, dataDir)

	maintenanceGCDispatchFn(func() {
		defer d.maintenanceGCRunning.Store(false)
		res := d.maintenanceGCCycle(databases, dataDir, p)
		d.logger.Printf("scheduled_maintenance: gc cycle %s", res.outcome)
		if res.outcome == gcOutcomeDeferred {
			d.recordGCDeferral(windowEnd, res.reason, res.pending)
			return
		}
		d.resetGCDeferralStreak()
		d.maintenanceGCFinishedAt.Store(time.Now().UnixNano())
	})
}
