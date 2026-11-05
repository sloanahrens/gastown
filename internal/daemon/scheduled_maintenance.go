package daemon

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// scheduled_maintenance is the town's one Dolt GC actor (gt-8z769.3): in the
// configured window it runs CALL dolt_gc('--full') on each database that is
// due — weekly, or sooner when its old generation grew more than 20% since its
// last gc (maintenance_gc.go). Nothing else in gt runs a manual gc, and
// nothing rewrites history: flatten is an offline operator procedure
// (docs/dolt-history-offline.md). Dolt's own auto-GC stays on.
//
// Before the gc, the same window appends each database's new dolt_log commits
// and events rows to the history archive (scheduled_maintenance_archive.go,
// gt-7nxj4) and takes the nightly backup of every database to
// ~/gt-backups/dolt (maintenance_backup.go, gt-8z769.5); the gc runs only once
// that night's backup is on disk.

const (
	// defaultMaintenanceCheckInterval is how often the daemon checks if it's
	// within the maintenance window. Short interval (5 min) ensures we don't
	// miss a narrow window, but the actual maintenance only runs once per window.
	defaultMaintenanceCheckInterval = 5 * time.Minute

	// maintenanceCycleGap is the minimum time between two completed cycles:
	// one per daily window. Slightly under 24h so the run time cannot drift
	// out of a one-hour window. Which databases a cycle gc's is decided per
	// database (shouldGCDatabase); the cycle itself only measures.
	maintenanceCycleGap = 20 * time.Hour
)

// maintenanceCheckInterval returns the check interval (5m). It is internal:
// the patrol only needs to poll often enough to catch the window.
func maintenanceCheckInterval(config *DaemonPatrolConfig) time.Duration {
	return defaultMaintenanceCheckInterval
}

// maintenanceWindow returns the configured window start time (HH:MM), or empty string.
func maintenanceWindow(config *DaemonPatrolConfig) string {
	if config != nil && config.Patrols != nil && config.Patrols.ScheduledMaintenance != nil {
		return config.Patrols.ScheduledMaintenance.Window
	}
	return ""
}

// parseWindowTime parses an HH:MM string and returns the hour and minute.
func parseWindowTime(window string) (hour, minute int, err error) {
	parts := strings.SplitN(window, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid window format %q: expected HH:MM", window)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid hour in window %q: expected 0-23", window)
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid minute in window %q: expected 0-59", window)
	}
	return hour, minute, nil
}

// maintenanceWindowLength is how long the maintenance window stays open after
// its configured HH:MM start.
const maintenanceWindowLength = time.Hour

// maintenanceWindowBounds returns the start and end of the window containing
// now, or of now's day's window when none is open. A window that starts late
// in the evening runs past midnight, so yesterday's window is checked first.
// Both isInMaintenanceWindow and maintenanceWindowEnd derive from it, so the
// deferral streak can never disagree with the window it counts.
func maintenanceWindowBounds(now time.Time, window string) (start, end time.Time, err error) {
	hour, minute, err := parseWindowTime(window)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	prev := time.Date(now.Year(), now.Month(), now.Day()-1, hour, minute, 0, 0, now.Location())
	if now.Before(prev.Add(maintenanceWindowLength)) && !now.Before(prev) {
		return prev, prev.Add(maintenanceWindowLength), nil
	}
	start = time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	return start, start.Add(maintenanceWindowLength), nil
}

// isInMaintenanceWindow checks if the given time falls within the maintenance
// window: maintenanceWindowLength starting at the configured HH:MM.
func isInMaintenanceWindow(now time.Time, window string) bool {
	start, end, err := maintenanceWindowBounds(now, window)
	if err != nil {
		return false
	}
	return !now.Before(start) && now.Before(end)
}

// maintenanceWindowEnd returns when the window containing now closes. Only
// meaningful in-window.
func maintenanceWindowEnd(now time.Time, window string) time.Time {
	_, end, err := maintenanceWindowBounds(now, window)
	if err != nil {
		return now
	}
	return end
}

// deprecatedMaintenanceKeys names the retired scheduled_maintenance keys a
// daemon patrol config still sets. They are parsed and ignored.
func deprecatedMaintenanceKeys(config *DaemonPatrolConfig) []string {
	if config == nil || config.Patrols == nil || config.Patrols.ScheduledMaintenance == nil {
		return nil
	}
	mc := config.Patrols.ScheduledMaintenance
	var keys []string
	if mc.Interval != "" {
		keys = append(keys, "interval")
	}
	if mc.Threshold != nil {
		keys = append(keys, "threshold")
	}
	if mc.Mode != "" {
		keys = append(keys, "mode")
	}
	if mc.GCMinBytes != nil {
		keys = append(keys, "gc_min_bytes")
	}
	if mc.GCGrowthRatio != nil {
		keys = append(keys, "gc_growth_ratio")
	}
	return keys
}

// shouldRunMaintenanceCycle reports whether a cycle is due: none has completed
// yet, or the last one is at least maintenanceCycleGap old.
func shouldRunMaintenanceCycle(now, lastRun time.Time) bool {
	return lastRun.IsZero() || now.Sub(lastRun) >= maintenanceCycleGap
}

// runScheduledMaintenance checks if we're in the maintenance window and, once
// per window, dispatches a backup-then-gc cycle over every database in the
// data dir. The stale-gc alerts are settled before the tick does any window
// work, in the window and out of it (maintenance_gc_stale.go); an archive-now
// request is served only after that check. A request file from the update job
// runs a cycle at any hour and is answered through its result file
// (maintenance_request.go). An archive-now request runs only the history
// archive pass and answers it (scheduled_maintenance_archive_request.go).
func (d *Daemon) runScheduledMaintenance() {
	files := osRequestFiles{}
	dir := update.Dir(d.config.TownRoot)

	// The archive-now request runs the history archive pass and nothing else,
	// so a tick serving one returns before any window work. A malformed or
	// invalid-id archive request is settled and the tick carries on as if there
	// were none; an unreadable one is left for the next tick. The request is
	// served after the stale-gc check below, so it rides the tick like every
	// other request.
	archiveRequest, archiveRequested, err := readArchiveRequest(files, dir)
	switch {
	case errors.Is(err, errMalformedRequest):
		d.dropUnanswerableRequest(files, archiveRequestPath(dir), "archive", err)
	case errors.Is(err, errInvalidRunID):
		d.answerInvalidArchiveRequest(files, dir, err)
	case err != nil:
		d.logger.Printf("scheduled_maintenance: WARNING: %v — leaving the archive request for the next tick", err)
	}

	request, requested, err := readMaintenanceRequest(files, dir)
	switch {
	case errors.Is(err, errMalformedRequest):
		d.dropMalformedRequest(files, dir, err)
		requested = false
	case errors.Is(err, errInvalidRunID):
		d.answerInvalidRequest(files, dir, err)
		requested = false
	case err != nil:
		// A request that cannot be read is not a missing one: leave it for the
		// next tick, and let this tick do only its scheduled work.
		d.logger.Printf("scheduled_maintenance: WARNING: %v — leaving the request for the next tick", err)
		requested = false
	}

	// A request whose cycle already ran but whose result write failed is
	// answered from the remembered outcome, never rerun. An unreadable state
	// is answered failed for the same reason: neither may start a cycle.
	if requested && d.reanswerRememberedRequest(files, dir, request) {
		return
	}

	if !d.isPatrolActive("scheduled_maintenance") {
		if requested {
			d.answerRequest(request, gcOutcomeFailed, "the scheduled_maintenance patrol is not active")
		}
		if archiveRequested {
			d.answerArchive(archiveRequest, "failed", "the scheduled_maintenance patrol is not active")
		}
		return
	}

	now := d.maintenance().now()

	// The stale-gc check rides every tick, in the window and out of it: the
	// failure it exists for is the window never opening at all, which an
	// in-window check would sleep through (gt-2l9zd). It reads the state file,
	// not the window, so it runs before the window is parsed.
	d.checkStaleMaintenanceGC(now)

	// An archive-now request runs the history archive pass and nothing else,
	// and returns before any window work. It is reached only after the check
	// above, so a tick that serves one still settles the stale-gc alerts.
	if archiveRequested {
		d.serveArchiveRequest(archiveRequest)
		return
	}

	// The update job owns the nightly cycle when its trigger is on: this tick
	// starts none of its own. A B11 request is an instruction rather than this
	// tick's decision, so it still runs the cycle; the stale-gc check above
	// and the catch-up paths outside the window are the safety net and still
	// run too.
	cycleDeferred := !requested && d.updateTriggerEnabled()

	window := maintenanceWindow(d.patrolConfig)
	if window == "" && !requested {
		d.logger.Printf("scheduled_maintenance: no window configured, skipping")
		return
	}

	// Count a window that closed with gc still deferred (and escalate a
	// streak) before anything else, including outside the window. A requested
	// cycle with no window configured touches no window bookkeeping at all.
	if window != "" && !d.maintenanceGCRunning.Load() {
		d.closeDeferredGCWindow(now)
	}

	// A request runs the cycle at any time of day, so it skips the window and
	// the gap since the last cycle; the catch-up work is for a tick without
	// one, and a requested cycle replaces it.
	if !requested && !isInMaintenanceWindow(now, window) {
		// Outside the window the work is a missed backup (gt-wne04) and, for a
		// database whose windows kept deferring, the gc that never ran
		// (gt-uux5b). A database with nothing else to do is a silent skip (this
		// fires every 5 minutes).
		// A malformed window also reads as "outside"; it must not enable
		// catch-ups and hide that the window (and its gc) never runs.
		if _, _, err := maintenanceWindowBounds(now, window); err == nil {
			d.maybeCatchUpBackup(now)
			// The gc's own freshness check still wants a backup, so a
			// catch-up one dispatched just above defers the gc to a later
			// tick rather than letting it run uncovered.
			d.maybeCatchUpGC(now)
		}
		return
	}

	// A gc cycle runs on its own goroutine; fold its completion time into
	// lastMaintenanceRun here, on the loop goroutine that owns the field.
	if finished := d.maintenanceGCFinishedAt.Swap(0); finished != 0 {
		if at := time.Unix(0, finished); at.After(d.lastMaintenanceRun) {
			d.lastMaintenanceRun = at
		}
	}

	if !requested && !shouldRunMaintenanceCycle(now, d.lastMaintenanceRun) {
		return // Already ran this window
	}

	// The update job asks for the nightly cycle through the maintenance
	// request, so a tick with no request starts nothing here.
	if cycleDeferred {
		return
	}

	// A standby (the town ESTOP or a Waned Ring) starts no gc, but the
	// night's backup still runs (startStandbyBackup). The window is consumed
	// only once that backup ends, so the gc waits for the catch-up. A request
	// is answered at once instead of being left to the job's timeout.
	reason := d.standbyReason(now)
	d.logStandby(&d.standbyMaintenance, "scheduled_maintenance", reason)
	if reason != "" {
		if requested {
			d.deferRequest(request, reason)
		}
		if window != "" {
			d.startStandbyBackup(window)
		}
		return
	}

	// A cycle that defers (town busy) leaves lastMaintenanceRun alone, so
	// the next 5-minute tick in the window retries. A request this tick cannot
	// serve because a cycle is already running is answered (maintenance_request.go)
	// rather than left to the job's timeout.
	if d.maintenanceGCRunning.Load() {
		if requested {
			d.answerBusyRequest(request)
		}
		return
	}
	if external, why := d.maintenance().gcExternal(d); external {
		// The old-gen trigger reads the server's data dir from this host's
		// disk; against a remote server it would read the wrong one. The skip
		// is the scheduled window's, so only a scheduled tick consumes it: a
		// request that fails here changed nothing a later request must wait
		// on.
		d.logger.Printf("scheduled_maintenance: skipped: %s — gc needs a local server", why)
		if requested {
			d.answerRequest(request, gcOutcomeFailed, "no local server can run the cycle: "+why)
		} else {
			d.lastMaintenanceRun = now
		}
		return
	}
	dataDir := d.maintenanceDataDir()
	databases, err := d.maintenance().gcDatabases(dataDir)
	if err != nil || len(databases) == 0 {
		d.logger.Printf("scheduled_maintenance: no databases discovered in %s (err=%v)", dataDir, err)
		if requested {
			detail := fmt.Sprintf("no databases discovered in %s", dataDir)
			if err != nil {
				detail = fmt.Sprintf("%s: %v", detail, err)
			}
			d.answerRequest(request, gcOutcomeFailed, detail)
		}
		return
	}
	where := fmt.Sprintf("in window %s", window)
	if requested {
		switch {
		case window == "":
			where = fmt.Sprintf("on the request for run %s (no window configured)", request.RunID)
		case !isInMaintenanceWindow(now, window):
			where = fmt.Sprintf("outside the window on the request for run %s", request.RunID)
		}
	}
	d.logger.Printf("scheduled_maintenance: %s, %d database(s) discovered", where, len(databases))
	if keys := deprecatedMaintenanceKeys(d.patrolConfig); len(keys) > 0 {
		d.logger.Printf("scheduled_maintenance: WARNING: deprecated config ignored — patrols.scheduled_maintenance.{%s} "+
			"have no effect; the schedule is weekly gc plus the old-gen trigger (gt-8z769.3)", strings.Join(keys, ","))
	}
	var requestedCycle *update.RequestFile
	if requested {
		requestedCycle = &request
	}
	// No window means no window end to record a deferral against; the cycle
	// still runs on the request.
	var windowEnd time.Time
	if window != "" {
		windowEnd = maintenanceWindowEnd(now, window)
	}
	d.startMaintenanceGC(databases, windowEnd, requestedCycle)
	if finished := d.maintenanceGCFinishedAt.Swap(0); finished != 0 {
		d.lastMaintenanceRun = time.Unix(0, finished)
	}
}
