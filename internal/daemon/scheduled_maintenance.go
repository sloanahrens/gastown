package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

const (
	// defaultMaintenanceCheckInterval is how often the daemon checks if it's
	// within the maintenance window. Short interval (5 min) ensures we don't
	// miss a narrow window, but the actual maintenance only runs once per window.
	defaultMaintenanceCheckInterval = 5 * time.Minute

	// defaultMaintenanceThreshold is the minimum commit count before maintenance
	// triggers. Lower than compactor_dog (10k) since this is user-configured
	// scheduled maintenance, not emergency compaction.
	defaultMaintenanceThreshold = 1000

	// MaintenanceModeMonitor reports over-threshold databases and rewrites
	// nothing. It is the default: an unattended path that squashes commit
	// history is the failure mode gt-e14c and gt-nfu7 removed from the
	// compactor_dog daemon, and this patrol is the same shape of risk.
	MaintenanceModeMonitor = "monitor"

	// MaintenanceModeFlatten runs `gt maintain --force`, which flattens the
	// commit history of every database over threshold. Operator opt-in only.
	MaintenanceModeFlatten = "flatten"

	// maintenanceTailLines is how much of `gt maintain`'s output is logged and
	// escalated on failure. The interesting line is at the end; the full output
	// can be hundreds of lines of per-database progress.
	maintenanceTailLines = 5
)

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

// maintenanceCheckInterval returns the configured check interval, or the default (5m).
func maintenanceCheckInterval(config *DaemonPatrolConfig) time.Duration {
	// The check interval is not user-configurable — it's internal.
	// We just need to poll often enough to catch the window.
	return defaultMaintenanceCheckInterval
}

// maintenanceThreshold returns the configured commit threshold, or the default (1000).
func maintenanceThreshold(config *DaemonPatrolConfig) int {
	if config != nil && config.Patrols != nil && config.Patrols.ScheduledMaintenance != nil {
		if config.Patrols.ScheduledMaintenance.Threshold != nil {
			return *config.Patrols.ScheduledMaintenance.Threshold
		}
	}
	return defaultMaintenanceThreshold
}

// maintenanceWindow returns the configured window start time (HH:MM), or empty string.
func maintenanceWindow(config *DaemonPatrolConfig) string {
	if config != nil && config.Patrols != nil && config.Patrols.ScheduledMaintenance != nil {
		return config.Patrols.ScheduledMaintenance.Window
	}
	return ""
}

// maintenanceInterval returns the configured interval string, or "daily".
func maintenanceInterval(config *DaemonPatrolConfig) string {
	if config != nil && config.Patrols != nil && config.Patrols.ScheduledMaintenance != nil {
		if config.Patrols.ScheduledMaintenance.Interval != "" {
			return config.Patrols.ScheduledMaintenance.Interval
		}
	}
	return "daily"
}

// maintenanceMode returns the configured compaction mode.
//
// Only the trimmed string "flatten" selects the destructive path and only the
// trimmed string "gc" selects garbage collection; everything else — an empty
// field, a typo, a case variation like "Flatten", a missing config — is
// MaintenanceModeMonitor. The negative test is deliberate: a misspelling or a
// hand-edited case difference must fail toward escalation, never toward
// rewriting every database's history at 03:00. This matches setMaintenanceConfig
// (internal/cmd/config.go), which writes only the exact lowercase constants and
// rejects everything else at `gt config set` time — the two entry points must
// agree on what arms the destructive path, since daemon.json can also be
// hand-edited directly.
func maintenanceMode(config *DaemonPatrolConfig) string {
	if config != nil && config.Patrols != nil && config.Patrols.ScheduledMaintenance != nil {
		switch strings.TrimSpace(config.Patrols.ScheduledMaintenance.Mode) {
		case MaintenanceModeFlatten:
			return MaintenanceModeFlatten
		case MaintenanceModeGC:
			return MaintenanceModeGC
		}
	}
	return MaintenanceModeMonitor
}

// maintenanceExecFn runs `gt maintain --force --threshold N`. A package
// variable, following this package's *Fn seam convention (wispTreeFn,
// closeStaleWispFn, listOriginBranchesFn), so a test can drive the flatten
// branch without a real gt binary and a real town — and, more importantly, can
// assert that monitor mode never reaches it at all.
var maintenanceExecFn = func(ctx context.Context, gtPath, dir string, threshold int) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gtPath, "maintain", "--force", "--threshold", strconv.Itoa(threshold))
	cmd.Dir = dir
	util.SetDetachedProcessGroup(cmd)
	return cmd.CombinedOutput()
}

// maintenanceEscalateFn reports maintenance findings to the mayor. Seamed for
// the same reason as maintenanceExecFn: monitor mode's whole contract is that
// it escalates instead of compacting, and that contract needs a test.
var maintenanceEscalateFn = func(d *Daemon, source, message string) {
	d.escalate(source, message)
}

// maintenanceTarget is a database at or above the maintenance threshold.
type maintenanceTarget struct {
	name    string
	commits int
}

// maintenanceMonitorMessage renders the monitor-mode escalation body: what
// crossed the line, and the two operator paths that compact.
func maintenanceMonitorMessage(targets []maintenanceTarget, threshold int) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"scheduled_maintenance: %d database(s) at or above the %d-commit threshold. "+
			"Nothing was rewritten — maintenance.mode is %s.\n",
		len(targets), threshold, MaintenanceModeMonitor)
	for _, t := range targets {
		fmt.Fprintf(&b, "  %s: %d commits\n", t.name, t.commits)
	}
	fmt.Fprintf(&b,
		"To compact, run plugins/compactor-dog/run.sh --compact (operator), "+
			"or set maintenance.mode=flatten to let this patrol flatten in-window.")
	return b.String()
}

// tailLines returns the last n lines of output, for logs and escalations where
// the interesting failure is at the end.
func tailLines(output []byte, n int) []string {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
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

// shouldRunMaintenance checks if maintenance should run based on the interval
// and the last run time. Returns true if enough time has passed since the last run.
func shouldRunMaintenance(now time.Time, lastRun time.Time, interval string) bool {
	if lastRun.IsZero() {
		return true // Never run before
	}

	var minGap time.Duration
	switch interval {
	case "daily":
		minGap = 20 * time.Hour // Slightly less than 24h to avoid drift
	case "weekly":
		minGap = 6 * 24 * time.Hour
	case "monthly":
		minGap = 27 * 24 * time.Hour
	default:
		// Try parsing as Go duration
		d, err := time.ParseDuration(interval)
		if err != nil || d <= 0 {
			minGap = 20 * time.Hour // Fall back to daily
		} else {
			minGap = d - (d / 10) // 90% of configured interval to avoid drift
		}
	}

	return now.Sub(lastRun) >= minGap
}

// runScheduledMaintenance checks if we're in the maintenance window and acts
// on the mode: monitor escalates over-threshold commit counts, flatten runs
// `gt maintain --force`, gc runs a size-triggered dolt_gc('--full') cycle.
func (d *Daemon) runScheduledMaintenance() {
	if !d.isPatrolActive("scheduled_maintenance") {
		return
	}

	window := maintenanceWindow(d.patrolConfig)
	if window == "" {
		d.logger.Printf("scheduled_maintenance: no window configured, skipping")
		return
	}

	now := time.Now()

	// gc mode: count a window that closed with gc still deferred (and escalate
	// a streak) before anything else, including outside the window.
	if maintenanceMode(d.patrolConfig) == MaintenanceModeGC && !d.maintenanceGCRunning.Load() {
		d.closeDeferredGCWindow(now, maintenanceInterval(d.patrolConfig))
	}

	// Check if we're in the maintenance window.
	if !isInMaintenanceWindow(now, window) {
		return // Not in window — silent skip (this fires every 5 minutes)
	}

	// A gc cycle runs on its own goroutine; fold its completion time into
	// lastMaintenanceRun here, on the loop goroutine that owns the field.
	if finished := d.maintenanceGCFinishedAt.Swap(0); finished != 0 {
		if at := time.Unix(0, finished); at.After(d.lastMaintenanceRun) {
			d.lastMaintenanceRun = at
		}
	}

	// Check if we already ran recently (respect interval).
	interval := maintenanceInterval(d.patrolConfig)
	if !shouldRunMaintenance(now, d.lastMaintenanceRun, interval) {
		return // Already ran this window
	}

	// gc mode is size-triggered and never counts commits or reaches the
	// flatten path. A cycle that defers (town busy) leaves lastMaintenanceRun
	// alone, so the next 5-minute tick in the window retries.
	if maintenanceMode(d.patrolConfig) == MaintenanceModeGC {
		if d.maintenanceGCRunning.Load() {
			return
		}
		if external, why := maintenanceGCExternalFn(d); external {
			// The size trigger reads the server's data dir from this host's
			// disk; against a remote server it would read the wrong one.
			d.logger.Printf("scheduled_maintenance: mode=%s skipped: %s — gc mode needs a local server", MaintenanceModeGC, why)
			d.lastMaintenanceRun = now
			return
		}
		dataDir := d.maintenanceDataDir()
		databases, err := maintenanceGCDatabasesFn(dataDir)
		if err != nil || len(databases) == 0 {
			d.logger.Printf("scheduled_maintenance: mode=%s: no databases discovered in %s (err=%v)", MaintenanceModeGC, dataDir, err)
			return
		}
		d.logger.Printf("scheduled_maintenance: in window %s, mode=%s, %d database(s) discovered", window, MaintenanceModeGC, len(databases))
		d.startMaintenanceGC(databases, maintenanceWindowEnd(now, window))
		if finished := d.maintenanceGCFinishedAt.Swap(0); finished != 0 {
			d.lastMaintenanceRun = time.Unix(0, finished)
		}
		return
	}

	d.logger.Printf("scheduled_maintenance: in window %s, checking commit counts", window)

	// Check if any database exceeds the threshold.
	threshold := maintenanceThreshold(d.patrolConfig)
	databases := d.compactorDatabases() // Reuse the same DB discovery
	if len(databases) == 0 {
		d.logger.Printf("scheduled_maintenance: no databases found")
		return
	}

	// Collect every database over threshold rather than stopping at the first:
	// the escalation should name all of them, not the alphabetically first.
	var targets []maintenanceTarget
	for _, dbName := range databases {
		commitCount, err := d.compactorCountCommits(dbName)
		if err != nil {
			d.logger.Printf("scheduled_maintenance: %s: error counting commits: %v", dbName, err)
			continue
		}
		if commitCount >= threshold {
			d.logger.Printf("scheduled_maintenance: %s: %d commits >= threshold %d — maintenance needed",
				dbName, commitCount, threshold)
			targets = append(targets, maintenanceTarget{name: dbName, commits: commitCount})
			continue
		}
		d.logger.Printf("scheduled_maintenance: %s: %d commits (below threshold %d)",
			dbName, commitCount, threshold)
	}

	if len(targets) == 0 {
		d.logger.Printf("scheduled_maintenance: all databases below threshold, skipping")
		d.lastMaintenanceRun = now // Don't re-check until next interval
		return
	}

	if maintenanceMode(d.patrolConfig) == MaintenanceModeFlatten {
		d.maintenanceFlatten(threshold)
	} else {
		d.logger.Printf("scheduled_maintenance: mode=%s — escalating %d database(s), rewriting nothing",
			MaintenanceModeMonitor, len(targets))
		maintenanceEscalateFn(d, "scheduled_maintenance", maintenanceMonitorMessage(targets, threshold))
	}

	d.lastMaintenanceRun = now
}

// maintenanceFlatten runs the destructive maintenance path: `gt maintain
// --force`, which flattens every database over threshold. Only reachable when
// maintenance.mode is explicitly "flatten".
//
// `gt maintain` applies its own remote-divergence pre-flight and refuses a
// database whose remote has moved on, so a non-zero exit here can mean "nothing
// was flattened on purpose" rather than a crash — the output tail is included
// in the escalation for that reason.
func (d *Daemon) maintenanceFlatten(threshold int) {
	d.logger.Printf("scheduled_maintenance: mode=%s — running gt maintain --force --threshold %d",
		MaintenanceModeFlatten, threshold)

	output, err := maintenanceExecFn(d.ctx, d.gtPath, d.config.TownRoot, threshold)
	if err != nil {
		d.logger.Printf("scheduled_maintenance: gt maintain failed: %v\nOutput: %s", err, string(output))
		detail := fmt.Sprintf("gt maintain --force failed: %v", err)
		if tail := tailLines(output, maintenanceTailLines); len(tail) > 0 {
			detail += "\n" + strings.Join(tail, "\n")
		}
		maintenanceEscalateFn(d, "scheduled_maintenance", detail)
		return
	}

	d.logger.Printf("scheduled_maintenance: gt maintain completed successfully")
	for _, line := range tailLines(output, maintenanceTailLines) {
		d.logger.Printf("scheduled_maintenance: %s", line)
	}
}
