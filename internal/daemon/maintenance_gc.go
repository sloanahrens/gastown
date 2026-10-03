package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
)

// scheduled_maintenance's gc: history-preserving garbage collection, the
// town's one GC actor (gt-8z769.3).
//
// Per database, in the maintenance window, run CALL dolt_gc('--full') over the
// daemon's SQL connection to the running server when either holds:
//   - weekly: its last patrol gc is at least maintenanceGCWeekly old, or it
//     has none recorded;
//   - old-gen growth: its old generation (.dolt/noms/oldgen) is more than
//     maintenanceOldGenGrowth larger than right after its last gc.
//
// Old-gen is the trigger because it is the growth: the 2026-09-29 offline
// measurement (gt-8z769) showed a full gc reclaims almost nothing on a
// database whose size is commit history, so a total-size trigger fires on
// growth gc cannot remove. A pause marker (internal/doltpause) covers each gc
// call. It never flattens, never rewrites a commit, never pushes.

const (
	// maintenanceGCWeekly is the routine full-gc interval per database,
	// short of seven days by a window's slack so a weekly run does not drift
	// a day later each week.
	maintenanceGCWeekly = 7*24*time.Hour - 4*time.Hour

	// maintenanceOldGenGrowth is the old-gen growth since the last gc that
	// triggers an early gc: more than 20%.
	maintenanceOldGenGrowth = 1.20

	// maintenanceGCTimeout bounds one CALL dolt_gc('--full'). A prod --full
	// gc takes seconds per database (design doc, Problem); ten minutes is a
	// hang, not a slow gc.
	maintenanceGCTimeout = 10 * time.Minute

	// maintenancePauseSlack is how long the pause marker outlives the gc
	// timeout, so a gc cut off at its bound is still covered while the
	// cycle removes the marker. A daemon that dies mid-gc leaves a marker
	// that lapses on its own at this horizon.
	maintenancePauseSlack = 5 * time.Minute

	// maintenancePauseActor is the actor the gc's pause marker names.
	maintenancePauseActor = "daemon/scheduled_maintenance"

	// maintenancePolecatFreshness is how recent a polecat's "working"
	// heartbeat must be to count as work in flight. Polecats renew it on
	// every gt sub-command; an older one is a stalled or finished session.
	maintenancePolecatFreshness = 15 * time.Minute

	maintenanceGCStateFileName = "maintenance_state.json"
)

// gcMeasure is one database's sizes before or after a gc.
type gcMeasure struct {
	total  int64
	oldGen int64
}

// shouldGCDatabase applies the two triggers to one database. lastGC and
// oldGenBaseline come from its last patrol gc (zero when none is recorded).
func shouldGCDatabase(now, lastGC time.Time, oldGen, oldGenBaseline int64) (bool, string) {
	if lastGC.IsZero() {
		return true, "no patrol gc recorded yet"
	}
	since := now.Sub(lastGC).Round(time.Hour)
	if now.Sub(lastGC) >= maintenanceGCWeekly {
		return true, fmt.Sprintf("weekly: last gc %v ago", since)
	}
	if float64(oldGen) > float64(oldGenBaseline)*maintenanceOldGenGrowth {
		return true, fmt.Sprintf("old-gen grew %s -> %s since last gc %v ago (trigger >%.0f%%)",
			formatBytes(oldGenBaseline), formatBytes(oldGen), since, (maintenanceOldGenGrowth-1)*100)
	}
	return false, fmt.Sprintf("last gc %v ago, old-gen %s vs %s after it", since, formatBytes(oldGen), formatBytes(oldGenBaseline))
}

// formatBytes renders a byte count in MiB for logs and escalations.
func formatBytes(n int64) string {
	return fmt.Sprintf("%.1fMiB", float64(n)/(1024*1024))
}

// --- state file ----------------------------------------------------------------

// maintenanceGCState is <town>/daemon/maintenance_state.json: per database,
// when its last patrol gc ran, its size and old-gen size right after it (the
// old-gen size is the growth baseline), and how much that gc reclaimed.
//
// It also carries the skipped-window streak (see maintenance_gc_guard.go): a
// window that closes with gc still deferred on at least one eligible database
// counts once, and a completed or failed run resets the streak.
type maintenanceGCState struct {
	PostGCBytes       map[string]int64     `json:"post_gc_bytes"`
	PostGCOldGenBytes map[string]int64     `json:"post_gc_oldgen_bytes"`
	ReclaimedBytes    map[string]int64     `json:"reclaimed_bytes"`
	LastGC            map[string]time.Time `json:"last_gc"`

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
	st := maintenanceGCState{
		PostGCBytes:       map[string]int64{},
		PostGCOldGenBytes: map[string]int64{},
		ReclaimedBytes:    map[string]int64{},
		LastGC:            map[string]time.Time{},
	}
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
	for k, v := range parsed.PostGCOldGenBytes {
		st.PostGCOldGenBytes[k] = v
	}
	for k, v := range parsed.ReclaimedBytes {
		st.ReclaimedBytes[k] = v
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

// recordMaintenanceGCRun records one database's gc: when it ran, its sizes
// after, and what it reclaimed, keeping the other entries. A corrupt file is
// replaced rather than failing forever.
func recordMaintenanceGCRun(townRoot, db string, before, after gcMeasure, at time.Time) error {
	_, err := updateMaintenanceGCState(townRoot, func(st *maintenanceGCState) {
		st.PostGCBytes[db] = after.total
		st.PostGCOldGenBytes[db] = after.oldGen
		st.ReclaimedBytes[db] = before.total - after.total
		st.LastGC[db] = at
	})
	return err
}

// --- probes (seams) --------------------------------------------------------------

// maintenanceMeasure measures <dataDir>/<db> on disk: its total size and the
// old generation under .dolt/noms/oldgen that only gc --full writes. A
// database with no oldgen directory has an old-gen size of 0. It only stats;
// it never opens a file inside .dolt.
func maintenanceMeasure(dataDir, db string) (gcMeasure, error) {
	if err := validMaintenanceDBName(db); err != nil {
		return gcMeasure{}, err
	}
	root := filepath.Join(dataDir, db)
	total, err := dirSize(root)
	if err != nil {
		return gcMeasure{}, err
	}
	oldGen, err := dirSize(filepath.Join(root, ".dolt", "noms", "oldgen"))
	if errors.Is(err, fs.ErrNotExist) {
		oldGen, err = 0, nil
	}
	if err != nil {
		return gcMeasure{}, err
	}
	return gcMeasure{total: total, oldGen: oldGen}, nil
}

// dirSize sums the regular files under root, which must exist.
func dirSize(root string) (int64, error) {
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

// maintenanceSlotHolders lists the roles holding a container-gate slot or an
// in-flight marker. Locks-only: no docker ps.
func maintenanceSlotHolders(townRoot string) ([]string, error) {
	cg := config.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	rep, err := slot.StatusPoolLocksOnly(townRoot, slot.PoolFromConfig(cg))
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

// --- quiet-window guard -----------------------------------------------------------

// maintenanceQuiet reports whether gc may run now, with the reason when not.
// A --full gc is the one step that has correlated with a Dolt panic (design
// doc, Problem), so it runs only when nothing else in the town is doing work:
//   - the daemon's own in-flight work is idle (the upgrade-restart predicate),
//   - no container-gate slot or in-flight marker held by anyone (refinery gate,
//     batch gate, polecat verification suite, review),
//   - no polecat with a fresh "working" heartbeat.
//
// A probe that cannot answer counts as busy.
func (d *Daemon) maintenanceQuiet() (bool, string) {
	if !d.daemonWorkIdle() {
		return false, "daemon has work in flight (scripts, compactor, triage, slings, dispatch, watchdog or install)"
	}
	holders, err := d.maintenance().slotHolders(d.config.TownRoot)
	if err != nil {
		return false, fmt.Sprintf("cannot read slot status: %v", err)
	}
	if len(holders) > 0 {
		return false, "slot held by " + strings.Join(holders, ", ")
	}
	working, err := d.maintenance().workingPolecats(d)
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
				session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rigName), name))
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
	conn, err := sql.Open("mysql", d.maintenanceDSN(db, maintenanceGCTimeout))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	// ExecContext: the procedure reports failure as a SQL error, and its
	// status row carries nothing more. TestIntegrationDoltGCFullAgainstRealServer
	// runs this on a server.
	if _, err := conn.ExecContext(ctx, "CALL dolt_gc('--full')"); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("dolt_gc --full: timeout: %w", err)
		}
		return fmt.Errorf("dolt_gc --full: %w", err)
	}
	return nil
}

// maintenanceDSN is the DSN for one maintenance call on db, whose read timeout
// matches the call's own bound.
func (d *Daemon) maintenanceDSN(db string, readTimeout time.Duration) string {
	return fmt.Sprintf("root@tcp(%s)/%s?timeout=5s&readTimeout=%s&writeTimeout=30s",
		net.JoinHostPort(d.doltServerHost(), strconv.Itoa(d.doltServerPort())), db, readTimeout)
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
	name   string
	before gcMeasure
}

// maintenanceGCResult is a cycle's outcome plus, for a deferral, why and what
// is still waiting.
type maintenanceGCResult struct {
	outcome maintenanceGCOutcome
	reason  string
	pending []gcCandidate
}

// maintenanceGCCycle measures every database, gc's the due ones smallest
// first, and re-checks the quiet-window guard and the pause marker before
// each.
func (d *Daemon) maintenanceGCCycle(databases []string, dataDir string) maintenanceGCResult {
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: %v — treating every database as never gc'd", err)
	}

	now := d.maintenance().now()
	var eligible []gcCandidate
	for _, db := range databases {
		m, err := d.maintenance().measure(dataDir, db)
		if err != nil {
			d.logger.Printf("scheduled_maintenance: %s: cannot measure size: %v — skipping", db, err)
			continue
		}
		ok, reason := shouldGCDatabase(now, st.LastGC[db], m.oldGen, st.PostGCOldGenBytes[db])
		if !ok {
			d.logger.Printf("scheduled_maintenance: %s: %s, %s — no gc", db, formatBytes(m.total), reason)
			continue
		}
		d.logger.Printf("scheduled_maintenance: %s: %s, %s — gc due", db, formatBytes(m.total), reason)
		eligible = append(eligible, gcCandidate{name: db, before: m})
	}
	if len(eligible) == 0 {
		d.logger.Printf("scheduled_maintenance: no database needs gc")
		return maintenanceGCResult{outcome: gcOutcomeCompleted}
	}
	// Smallest first: the cheap databases finish before the town can get
	// busy, and the operator runbook proved the order on prod.
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].before.total < eligible[j].before.total })

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
		if quiet, why := d.maintenance().quiet(d); !quiet {
			return defer_(i, why)
		}
		// Someone else's deliberate outage (the backup, an operator) is not
		// ours to overwrite or end.
		if m := d.maintenance().pauseCurrent(d.config.TownRoot, d.maintenance().now()); m != nil {
			return defer_(i, m.Message())
		}
		// Exclusive against the daemon's own Dolt tasks (backups, wisp
		// reaper, JSONL export, compactor), which hold the read side.
		if !d.doltMaintMu.TryLock() {
			return defer_(i, "daemon Dolt task in flight")
		}

		err := d.maintenanceGCOne(parent, dataDir, c)
		d.doltMaintMu.Unlock()
		if err != nil {
			d.maintenance().escalate(d, "scheduled_maintenance", fmt.Sprintf(
				"scheduled_maintenance: CALL dolt_gc('--full') on %s (size before %s): %v\n"+
					"The gc run stopped; remaining databases were not touched. History is intact "+
					"(gc never flattens). Collect gt dolt status before any Dolt restart.",
				c.name, formatBytes(c.before.total), err))
			return maintenanceGCResult{outcome: gcOutcomeFailed, reason: err.Error()}
		}
	}
	return maintenanceGCResult{outcome: gcOutcomeCompleted}
}

// maintenanceGCOne pauses Dolt, runs one database's gc, unpauses, and records
// the run. The pause marker is written before the gc call and removed after
// it whatever the outcome; one the daemon cannot remove lapses at its until.
func (d *Daemon) maintenanceGCOne(parent context.Context, dataDir string, c gcCandidate) error {
	s := d.maintenance()
	start := s.now()
	marker := doltpause.Marker{
		Actor:  maintenancePauseActor,
		Reason: fmt.Sprintf("dolt_gc --full on %s", c.name),
		Since:  start,
		Until:  start.Add(maintenanceGCTimeout + maintenancePauseSlack),
	}
	if err := s.pauseWrite(d.config.TownRoot, marker); err != nil {
		d.logger.Printf("scheduled_maintenance: gc %s: cannot write pause marker: %v — not running gc", c.name, err)
		return fmt.Errorf("cannot write pause marker, gc not run: %w", err)
	}
	defer func() {
		if _, err := s.pauseRemove(d.config.TownRoot); err != nil {
			d.logger.Printf("scheduled_maintenance: WARNING: cannot remove pause marker: %v — it lapses at %s",
				err, marker.Until.Format(time.RFC3339))
		}
	}()

	d.logger.Printf("scheduled_maintenance: gc %s: CALL dolt_gc('--full') (before %s, old-gen %s)",
		c.name, formatBytes(c.before.total), formatBytes(c.before.oldGen))
	d.maintenanceGCOverdueEscalated.Store(false)
	d.maintenanceGCCallStartedAt.Store(time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(parent, maintenanceGCTimeout)
	err := s.gcExec(ctx, d, c.name)
	cancel()
	d.maintenanceGCCallStartedAt.Store(0)
	elapsed := s.now().Sub(start).Round(time.Millisecond)
	if err != nil {
		d.logger.Printf("scheduled_maintenance: gc %s FAILED after %v: %v — stopping this run", c.name, elapsed, err)
		return fmt.Errorf("failed after %v: %w", elapsed, err)
	}

	after, err := s.measure(dataDir, c.name)
	if err != nil {
		// Recording no run means the next window treats this database as
		// never gc'd and gc's it again.
		d.logger.Printf("scheduled_maintenance: gc %s: done in %v, but cannot re-measure: %v — no run recorded",
			c.name, elapsed, err)
		return nil
	}
	d.logger.Printf("scheduled_maintenance: gc %s: %s -> %s (old-gen %s -> %s) in %v", c.name,
		formatBytes(c.before.total), formatBytes(after.total), formatBytes(c.before.oldGen), formatBytes(after.oldGen), elapsed)
	if err := recordMaintenanceGCRun(d.config.TownRoot, c.name, c.before, after, s.now()); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot record gc of %s: %v", c.name, err)
	}
	return nil
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
	dataDir := d.maintenanceDataDir()
	d.logger.Printf("scheduled_maintenance: gc cycle — weekly, or old-gen >%.0f%% since last gc; data_dir=%s",
		(maintenanceOldGenGrowth-1)*100, dataDir)

	d.maintenance().dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		// Backup first (maintenance_backup.go); the gc only runs once the
		// night's backup is on disk.
		res := d.maintenanceBackup(databases)
		if res.outcome == gcOutcomeCompleted {
			res = d.maintenanceGCCycle(databases, dataDir)
		}
		d.logger.Printf("scheduled_maintenance: cycle %s", res.outcome)
		if res.outcome == gcOutcomeDeferred {
			d.recordGCDeferral(windowEnd, res.reason, res.pending)
			return
		}
		d.resetGCDeferralStreak()
		d.maintenanceGCFinishedAt.Store(d.maintenance().now().UnixNano())
	})
}
