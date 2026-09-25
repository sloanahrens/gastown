package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Guards around the scheduled_maintenance gc cycle (claude-05o fix round 1):
//
//   - doltMaintMu serializes a dolt_gc('--full') call against the daemon's own
//     Dolt tasks. The tasks take the read side with TryRLock and skip their tick
//     while a gc holds the write side; the gc takes the write side with TryLock
//     and defers while any task holds the read side. Neither side ever blocks,
//     so the select loop never waits on a gc.
//   - The ConvoyManager's event poll and stranded scan are paused around each
//     database's gc (a live reader racing gc; design doc, Problem).
//   - The Dolt health check defers a restart while a gc call is in flight and
//     under its timeout (doltRestartHeldForGC).
//   - Windows that close with gc still deferred are counted, and escalated
//     after a streak, so a town that is never quiet at 03:00 is not silent.
//   - Databases are discovered from the Dolt data dir, not from the
//     compactor_dog / wisp_reaper lists (whose fallback is ["hq"]).

// maintenanceGCRestartGrace is how far past maintenanceGCTimeout a gc call may
// run before the health check stops deferring restarts for it. The call's own
// context cancels at the timeout; a call still in flight after the grace is
// stuck, and a stuck server is what the health restart exists for.
const maintenanceGCRestartGrace = 2 * time.Minute

// maintenanceConvoyPauseTimeout bounds how long the gc waits for an in-flight
// Convoy poll or scan to finish before it defers.
const maintenanceConvoyPauseTimeout = 60 * time.Second

// tryDoltTask takes the read side of doltMaintMu for one of the daemon's Dolt
// tasks. When a gc holds the write side it logs "<name>: skipped: gc in
// flight" and reports false; the task skips this tick. Never blocks.
func (d *Daemon) tryDoltTask(name string) (release func(), ok bool) {
	if !d.doltMaintMu.TryRLock() {
		d.logger.Printf("%s: skipped: gc in flight", name)
		return nil, false
	}
	return d.doltMaintMu.RUnlock, true
}

// maintenanceConvoyPauseFn pauses the ConvoyManager's Dolt reads for one
// database's gc and returns the resume function. Seamed for tests.
var maintenanceConvoyPauseFn = func(d *Daemon) (resume func(), ok bool) {
	cm := d.convoyManager
	if cm == nil {
		return func() {}, true
	}
	if !cm.Pause(maintenanceConvoyPauseTimeout) {
		return nil, false
	}
	return cm.Resume, true
}

// doltRestartHeldForGC is the Dolt manager's restart suppressor: true while a
// dolt_gc('--full') call is in flight and within its timeout plus grace. Past
// that it escalates once for the call and returns false, letting the health
// check restart a server that is evidently stuck.
func (d *Daemon) doltRestartHeldForGC() bool {
	if !d.maintenanceGCRunning.Load() {
		return false
	}
	started := d.maintenanceGCCallStartedAt.Load()
	if started == 0 {
		return false // between calls: nothing in flight to protect
	}
	elapsed := time.Since(time.Unix(0, started))
	if elapsed <= maintenanceGCTimeout+maintenanceGCRestartGrace {
		return true
	}
	if d.maintenanceGCOverdueEscalated.CompareAndSwap(false, true) {
		msg := fmt.Sprintf(
			"scheduled_maintenance: a CALL dolt_gc('--full') has been in flight for %v, past its %v timeout, "+
				"and the Dolt health check is failing. No longer deferring the health restart. "+
				"Collect gt dolt status / gt dolt dump now.",
			elapsed.Round(time.Second), maintenanceGCTimeout)
		// Dispatched: this runs under the Dolt manager's lock on the daemon
		// loop, and gt escalate can take minutes (60s x retries). The restart
		// it is releasing must not wait on the alert.
		go maintenanceEscalateFn(d, "scheduled_maintenance", msg)
	}
	return false
}

// --- database discovery -----------------------------------------------------------

// discoverMaintenanceDatabases lists every Dolt database under dataDir: a
// non-hidden directory with a .dolt subdirectory and a valid name. This is
// gc mode's database set — not compactor_dog.databases or
// wisp_reaper.databases, whose fallback is ["hq"] alone.
func discoverMaintenanceDatabases(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || validMaintenanceDBName(e.Name()) != nil {
			continue
		}
		if info, err := os.Stat(filepath.Join(dataDir, e.Name(), ".dolt")); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// maintenanceGCDatabasesFn discovers gc mode's databases. Seamed for tests.
var maintenanceGCDatabasesFn = discoverMaintenanceDatabases

// --- external server -------------------------------------------------------------

// maintenanceGCExternalFn reports whether the Dolt server is not one this
// daemon can measure on local disk. Seamed for tests.
var maintenanceGCExternalFn = func(d *Daemon) (bool, string) { return d.maintenanceGCExternal() }

// maintenanceGCExternal is true when the Dolt server is externally managed or
// not on a loopback host: its data dir is not this host's, so the size
// trigger would be reading the wrong disk.
func (d *Daemon) maintenanceGCExternal() (bool, string) {
	if d.doltServer != nil && d.doltServer.IsEnabled() && d.doltServer.IsExternal() {
		return true, "Dolt server is externally managed"
	}
	host := d.doltServerHost()
	if !isLoopbackHost(host) {
		return true, fmt.Sprintf("Dolt server host %q is not local", host)
	}
	return false, ""
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- skipped-window streak ------------------------------------------------------

// maintenanceDeferredWindowsBeforeEscalation is the streak length that
// escalates: 3 windows for a daily (or shorter) interval, 2 for weekly,
// monthly or any interval of six days or more. After the first escalation the
// streak re-escalates at most every further 3 windows.
func maintenanceDeferredWindowsBeforeEscalation(interval string) int {
	switch interval {
	case "weekly", "monthly":
		return 2
	case "daily", "":
		return 3
	}
	if dur, err := time.ParseDuration(interval); err == nil && dur >= 6*24*time.Hour {
		return 2
	}
	return 3
}

func shouldEscalateDeferredWindows(count, threshold int) bool {
	if count < threshold {
		return false
	}
	return (count-threshold)%3 == 0
}

// recordGCDeferral stores the current window's deferral. The latest deferral
// in a window wins; the window is counted when it closes.
func (d *Daemon) recordGCDeferral(windowEnd time.Time, reason string, pending []gcCandidate) {
	dbs := make([]gcDeferredDB, 0, len(pending))
	for _, c := range pending {
		dbs = append(dbs, gcDeferredDB{Name: c.name, Bytes: c.size})
	}
	_, err := updateMaintenanceGCState(d.config.TownRoot, func(st *maintenanceGCState) {
		st.PendingDeferral = &gcDeferral{WindowEnd: windowEnd, Reason: reason, Databases: dbs}
		st.LastDeferralReason = reason
		if st.DeferralReasons == nil {
			st.DeferralReasons = map[string]int{}
		}
		st.DeferralReasons[reason]++
	})
	if err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot record gc deferral: %v", err)
	}
}

// resetGCDeferralStreak clears the streak after a run that completed or failed
// (a failure escalates on its own).
func (d *Daemon) resetGCDeferralStreak() {
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err == nil && st.PendingDeferral == nil && st.ConsecutiveDeferredWindows == 0 && len(st.DeferralReasons) == 0 {
		return // nothing to reset; skip the write
	}
	_, err = updateMaintenanceGCState(d.config.TownRoot, func(st *maintenanceGCState) {
		st.PendingDeferral = nil
		st.ConsecutiveDeferredWindows = 0
		st.DeferralReasons = nil
	})
	if err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot reset gc deferral streak: %v", err)
	}
}

// closeDeferredGCWindow counts a pending deferral whose window has closed and
// escalates when the streak reaches the threshold for interval. Called on
// every scheduled_maintenance tick in gc mode; a no-op when nothing is pending
// or the window is still open.
func (d *Daemon) closeDeferredGCWindow(now time.Time, interval string) {
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil || st.PendingDeferral == nil || now.Before(st.PendingDeferral.WindowEnd) {
		return
	}
	var closed gcDeferral
	st, err = updateMaintenanceGCState(d.config.TownRoot, func(s *maintenanceGCState) {
		if s.PendingDeferral == nil {
			return
		}
		closed = *s.PendingDeferral
		s.PendingDeferral = nil
		s.ConsecutiveDeferredWindows++
	})
	if err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot record closed deferred window: %v", err)
		return
	}
	if closed.WindowEnd.IsZero() {
		return
	}
	threshold := maintenanceDeferredWindowsBeforeEscalation(interval)
	d.logger.Printf("scheduled_maintenance: gc window closed still deferred (%s) — %d consecutive window(s), escalation at %d",
		closed.Reason, st.ConsecutiveDeferredWindows, threshold)
	if !shouldEscalateDeferredWindows(st.ConsecutiveDeferredWindows, threshold) {
		return
	}
	maintenanceEscalateFn(d, "scheduled_maintenance", deferredWindowsMessage(st, closed))
}

// deferredWindowsMessage renders the skipped-window escalation.
func deferredWindowsMessage(st maintenanceGCState, closed gcDeferral) string {
	var b strings.Builder
	fmt.Fprintf(&b, "scheduled_maintenance: gc deferred for %d consecutive maintenance window(s) — the town was never quiet. "+
		"Nothing was rewritten.\n", st.ConsecutiveDeferredWindows)
	b.WriteString("Waiting databases:\n")
	for _, db := range closed.Databases {
		fmt.Fprintf(&b, "  %s: %s\n", db.Name, formatBytes(db.Bytes))
	}
	type rc struct {
		reason string
		n      int
	}
	var reasons []rc
	for r, n := range st.DeferralReasons {
		reasons = append(reasons, rc{r, n})
	}
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].n != reasons[j].n {
			return reasons[i].n > reasons[j].n
		}
		return reasons[i].reason < reasons[j].reason
	})
	if len(reasons) > 3 {
		reasons = reasons[:3]
	}
	b.WriteString("Top deferral reasons (deferred attempts, up to one per 5-minute tick):\n")
	for _, r := range reasons {
		fmt.Fprintf(&b, "  %dx %s\n", r.n, r.reason)
	}
	b.WriteString("Operator options: run the gc by hand in a quiet moment, or move maintenance.window to a quieter hour.")
	return b.String()
}
