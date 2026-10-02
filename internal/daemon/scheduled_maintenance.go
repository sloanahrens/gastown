package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// scheduled_maintenance is the town's one Dolt GC actor (gt-8z769.3): in the
// configured window it runs CALL dolt_gc('--full') on each database that is
// due — weekly, or sooner when its old generation grew more than 20% since its
// last gc (maintenance_gc.go). Nothing else in gt runs a manual gc, and
// nothing rewrites history: flatten is an offline operator procedure
// (docs/dolt-history-offline.md). Dolt's own auto-GC stays on.
//
// Before the gc, the same window takes the nightly backup of every database
// to ~/gt-backups/dolt (maintenance_backup.go, gt-8z769.5); the gc runs only
// once that night's backup is on disk.

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

// maintenanceWindowBounds returns the start and end of the window on now's
// day. Both isInMaintenanceWindow and maintenanceWindowEnd derive from it, so
// the deferral streak can never disagree with the window it counts.
func maintenanceWindowBounds(now time.Time, window string) (start, end time.Time, err error) {
	hour, minute, err := parseWindowTime(window)
	if err != nil {
		return time.Time{}, time.Time{}, err
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
// daemon.json still sets. They are parsed and ignored.
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
// data dir.
func (d *Daemon) runScheduledMaintenance() {
	if !d.isPatrolActive("scheduled_maintenance") {
		return
	}

	window := maintenanceWindow(d.patrolConfig)
	if window == "" {
		d.logger.Printf("scheduled_maintenance: no window configured, skipping")
		return
	}

	now := d.maintenance().now()

	// Count a window that closed with gc still deferred (and escalate a
	// streak) before anything else, including outside the window.
	if !d.maintenanceGCRunning.Load() {
		d.closeDeferredGCWindow(now)
	}

	if !isInMaintenanceWindow(now, window) {
		// Outside the window the only work is a missed backup (gt-wne04);
		// otherwise a silent skip (this fires every 5 minutes).
		// A malformed window also reads as "outside"; it must not enable
		// catch-ups and hide that the window (and its gc) never runs.
		if _, _, err := maintenanceWindowBounds(now, window); err == nil {
			d.maybeCatchUpBackup(now)
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

	if !shouldRunMaintenanceCycle(now, d.lastMaintenanceRun) {
		return // Already ran this window
	}

	// A cycle that defers (town busy) leaves lastMaintenanceRun alone, so
	// the next 5-minute tick in the window retries.
	if d.maintenanceGCRunning.Load() {
		return
	}
	if external, why := d.maintenance().gcExternal(d); external {
		// The old-gen trigger reads the server's data dir from this host's
		// disk; against a remote server it would read the wrong one.
		d.logger.Printf("scheduled_maintenance: skipped: %s — gc needs a local server", why)
		d.lastMaintenanceRun = now
		return
	}
	dataDir := d.maintenanceDataDir()
	databases, err := d.maintenance().gcDatabases(dataDir)
	if err != nil || len(databases) == 0 {
		d.logger.Printf("scheduled_maintenance: no databases discovered in %s (err=%v)", dataDir, err)
		return
	}
	d.logger.Printf("scheduled_maintenance: in window %s, %d database(s) discovered", window, len(databases))
	if keys := deprecatedMaintenanceKeys(d.patrolConfig); len(keys) > 0 {
		d.logger.Printf("scheduled_maintenance: WARNING: deprecated config ignored — patrols.scheduled_maintenance.{%s} "+
			"have no effect; the schedule is weekly gc plus the old-gen trigger (gt-8z769.3)", strings.Join(keys, ","))
	}
	d.startMaintenanceGC(databases, maintenanceWindowEnd(now, window))
	if finished := d.maintenanceGCFinishedAt.Swap(0); finished != 0 {
		d.lastMaintenanceRun = time.Unix(0, finished)
	}
}
