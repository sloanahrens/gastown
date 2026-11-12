package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/reaper"
)

const (
	// defaultWispReaperInterval is the patrol interval. Set to 1h since reaping
	// is cleanup work, not latency-sensitive.
	defaultWispReaperInterval = 1 * time.Hour
	// Wisps older than this are reaped (closed). Configurable via formula var max_age.
	defaultWispMaxAge = 24 * time.Hour
	// Closed wisps older than this are permanently deleted. Formula var: purge_age.
	defaultWispDeleteAge = 7 * 24 * time.Hour
	// Closed mail older than this is permanently deleted.
	defaultMailDeleteAge = 7 * 24 * time.Hour
	// Issues stale longer than this are auto-closed.
	//
	// It was 7d until gt-2qzr: the daemon injected the shorter value on the dog
	// path, overriding the formula's 720h, and the inline fallback used it
	// directly. Agent beads are idle
	// by design and were eight days old, so a 7d threshold swept 102 durable
	// beads — every agent, dog, and patrol-molecule bead in the town. Auto-close
	// is for work that was abandoned, and abandonment does not look like a week.
	defaultStaleIssueAge = 30 * 24 * time.Hour
	// defaultEventsPurgeBatch and defaultEventsPurgeMaxBatches bound one events
	// retention pass (gt-hk21d): at most this many DELETEs of this many rows, so
	// a large first backlog drains over several cycles instead of one long write.
	defaultEventsPurgeBatch      = 5000
	defaultEventsPurgeMaxBatches = 20
	// eventsRetentionTimeout bounds one database's retention pass. It is longer
	// than maxBatches DELETEs at the 30s write timeout each, so the budget
	// cannot cut a pass short before the server refuses a delete itself.
	eventsRetentionTimeout = 15 * time.Minute
)

// wispReaperInterval returns the configured interval, or the default (1h).
func wispReaperInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.IntervalStr != "" {
			if d, err := agentconfig.ParseAgeDuration(config.Patrols.WispReaper.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispReaperInterval
}

// wispReaperMaxAge returns the configured max age, or the default (24h).
func wispReaperMaxAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.MaxAgeStr != "" {
			if d, err := agentconfig.ParseAgeDuration(config.Patrols.WispReaper.MaxAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispMaxAge
}

// wispDeleteAge returns the configured delete age, or the default (7 days).
func wispDeleteAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.DeleteAgeStr != "" {
			if d, err := agentconfig.ParseAgeDuration(config.Patrols.WispReaper.DeleteAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispDeleteAge
}

// wispReaperStaleIssueAge returns the configured stale-issue age, or the
// formula default (30d).
func wispReaperStaleIssueAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.StaleIssueAgeStr != "" {
			if d, err := agentconfig.ParseAgeDuration(config.Patrols.WispReaper.StaleIssueAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultStaleIssueAge
}

// WispReaperAutoCloseDisarmed reports whether the daemon patrol config explicitly disarms
// the wisp_reaper auto-close step (patrols.wisp_reaper.auto_close=false).
//
// This is read by `gt reaper auto-close` as well, so the setting disarms a
// hand-run sweep too, not just the daemon's own. A nil knob is "unset", not
// "disarmed".
func WispReaperAutoCloseDisarmed(config *DaemonPatrolConfig) bool {
	if config == nil || config.Patrols == nil || config.Patrols.WispReaper == nil {
		return false
	}
	knob := config.Patrols.WispReaper.AutoClose
	return knob != nil && !*knob
}

// wispReaperAutoCloseEnabled reports whether the daemon's sweep may
// auto-close. Auto-close writes to durable issues, unlike reap/purge which
// only retire ephemeral wisps, so it needs an explicit opt-in: an unset knob
// leaves it off (gt-2qzr). The sweep used to run in a dog by default, with
// this inline path as the fallback; the dogs were retired (gt-ckunw) and the
// inline sweep kept the fallback's stricter rule.
func wispReaperAutoCloseEnabled(config *DaemonPatrolConfig) bool {
	if config == nil || config.Patrols == nil || config.Patrols.WispReaper == nil {
		return false
	}
	knob := config.Patrols.WispReaper.AutoClose
	return knob != nil && *knob
}

// triggerWispReaper runs a wisp_reaper cycle when the patrol is due, on its
// own goroutine.
//
// The ticker that drives this call is a check cadence, not a run cadence: an
// in-process ticker resets its countdown on every daemon restart, so due-ness
// is instead decided from the persisted last-run time in
// daemon/patrol_last_run.json, which survives a restart (gt-ima2, gt-gxpwc).
//
// Run on its own goroutine, like the gt-ima2 fix for compactor_dog: a sweep
// of every database would stall every other tick behind it.
func (d *Daemon) triggerWispReaper() {
	if !d.isPatrolActive("wisp_reaper") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "wisp_reaper", d.wispReaperRun.last(), time.Now(), wispReaperInterval(d.patrol()))
	if !dec.due {
		d.logger.Printf("wisp_reaper: not due — %s", dec.note)
		return
	}

	if !d.wispReaperRunning.CompareAndSwap(false, true) {
		d.logger.Printf("wisp_reaper: previous cycle still running — skipping this check")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("wisp_reaper: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("wisp_reaper: due — %s", dec.note)
	}

	go func() {
		defer d.wispReaperRunning.Store(false)
		d.reapWisps()
	}()
}

// reapWisps is the thin orchestrator for the wisp_reaper patrol: it runs the
// sweep inline and records each step on the cycle's receipt.
func (d *Daemon) reapWisps() {
	if !d.isPatrolActive("wisp_reaper") {
		return
	}
	release, ok := d.tryDoltTask("wisp_reaper")
	if !ok {
		return
	}
	defer release()

	// Record that a cycle was attempted, regardless of outcome below — the
	// same "attempted" semantics as the ticker firing before gt-ima2/gt-gxpwc.
	defer func() {
		d.recordPatrolRun(&d.wispReaperRun, "wisp_reaper", time.Now())
	}()

	config := d.patrol().Patrols.WispReaper
	maxAge := wispReaperMaxAge(d.patrol())
	deleteAge := wispDeleteAge(d.patrol())
	staleIssueAge := wispReaperStaleIssueAge(d.patrol())

	cycle := d.startDogCycle("wisp_reaper")
	defer cycle.close()

	if config.DryRun {
		d.logger.Printf("wisp_reaper: DRY RUN — reporting only, no changes will be made")
	}

	d.reapWispsInline(config, maxAge, deleteAge, staleIssueAge, cycle)
}

// errNoDatabases is the refusal for a pass that found no database to sweep.
var errNoDatabases = errors.New("no databases found")

// sweepTargets is the database list a pass sweeps: the configured databases, or
// the ones discover finds when none are configured, with the tracker rigs'
// databases left out. An empty discovery is errNoDatabases whether or not tracker
// rigs exist: an empty discovery is also what a Dolt outage looks like, so the
// pass must not read it as a clean sweep of the tracker rigs.
//
// The tracker rigs are logged as skipped (trackerd runs its own wisp reaper on
// its loop for them, gt-p85pw.112). A route table that cannot be read is an
// error, and the pass refuses rather than sweep a rig it could not tell apart.
func (d *Daemon) sweepTargets(configured []string, discover func() []string) ([]string, error) {
	databases := configured
	if len(databases) == 0 {
		databases = discover()
	}
	if len(databases) == 0 {
		return nil, errNoDatabases
	}
	rigs, err := reaper.TrackerRigs(d.config.TownRoot)
	if err != nil {
		return nil, fmt.Errorf("the route table cannot be read to find the tracker rigs: %w", err)
	}
	for _, rig := range rigs {
		d.logger.Printf("wisp_reaper: %s: skipped — on the tracker, trackerd runs its own wisp reaper", rig.Name)
	}
	return reaper.WithoutTrackerDatabases(databases, rigs), nil
}

// reaperWriter returns the bd writer for a live run against dbName, pinned to
// the beads dir whose metadata names that database, and nil for a dry run:
// the reaper selects with SQL but writes only through bd (gt-fcxe9.12).
// extraRoots are the directories searched after the town root for that beads
// dir (gt-33etw).
func (d *Daemon) reaperWriter(dbName string, dryRun bool, extraRoots ...string) (reaper.Writer, error) {
	if dryRun {
		return nil, nil
	}
	if d.reaperWriterForFn != nil {
		return d.reaperWriterForFn(d.config.TownRoot, dbName)
	}
	return reaper.WriterForDatabase(d.config.TownRoot, dbName, extraRoots...)
}

// openReaperDB opens the events retention step's connection to dbName.
func (d *Daemon) openReaperDB(dbName string, readTimeout, writeTimeout time.Duration) (*beadsql.DB, error) {
	if d.reaperOpenDBFn != nil {
		return d.reaperOpenDBFn(dbName, readTimeout, writeTimeout)
	}
	return reaper.OpenDB(d.doltUserInfo(), d.doltServerHost(), d.doltServerPort(), dbName, readTimeout, writeTimeout)
}

// reapWispsInline runs the reaper cycle in the daemon, recording each step on
// cycle. Delegates to the
// reaper package, which selects with SQL and writes through bd.
func (d *Daemon) reapWispsInline(config *WispReaperConfig, maxAge, deleteAge, staleIssueAge time.Duration, cycle *dogCycle) {
	host := d.doltServerHost()
	databases, err := d.sweepTargets(config.Databases, func() []string {
		return reaper.DiscoverDatabases(d.doltUserInfo(), host, d.doltServerPort())
	})
	if errors.Is(err, errNoDatabases) {
		d.logger.Printf("wisp_reaper: no databases to reap")
		cycle.failStep("scan", "no databases found")
		return
	}
	if err != nil {
		d.logger.Printf("wisp_reaper: refusing the sweep: %v", err)
		cycle.failStep("scan", "route table unreadable")
		return
	}
	if len(databases) == 0 {
		d.logger.Printf("wisp_reaper: every database belongs to a tracker rig; trackerd reaps them")
		cycle.closeStep("scan")
		return
	}

	// A set but unusable extra_beads_roots is refused: reading it as "search
	// the town only" would leave the databases it was added for unresolved,
	// and say nothing about why (gt-33etw).
	extraRoots, rootsErr := config.ParseExtraBeadsRoots()
	if rootsErr != nil {
		d.logger.Printf("wisp_reaper: refusing the sweep: %v", rootsErr)
		cycle.failStep("scan", "patrols.wisp_reaper.extra_beads_roots is not a list of absolute paths")
		return
	}
	d.logger.Printf("wisp_reaper: scanning %d databases", len(databases))
	cycle.closeStep("scan")

	dryRun := config.DryRun
	var totalReaped, totalMoleculeSteps, totalOpen, totalPurged, totalMailPurged, totalAutoClosed int
	// openDBs counts the databases totalOpen covers, so the alert can compare
	// this cycle's total to a baseline taken over the same set (gt-11kyy).
	var openDBs int

	// Step 2: Reap
	reapErrors := 0
	// skipped holds the databases no searched beads root names: the reap is the
	// first step to resolve each writer, so every later step drops them (below).
	skipped := map[string]bool{}
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := d.openReaperDB(dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			if errors.Is(err, beadsql.ErrNotBeads) {
				continue
			}
			d.logger.Printf("wisp_reaper: %s: connect error: %v", dbName, err)
			reapErrors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			d.logger.Printf("wisp_reaper: %s: skipped (no reaper schema)", dbName)
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun, extraRoots...)
		// With an extra root configured, a database no root names is skipped, not
		// failed: the root that holds it may not be mounted on this host (the pod
		// has no ~/.claude). With the town's roots alone it stays an error (gt-33etw).
		if errors.Is(err, doltserver.ErrNoBeadsDir) && len(extraRoots) > 0 {
			db.Close()
			skipped[dbName] = true
			d.logger.Printf("wisp_reaper: skipped database %s: no beads directory in this town's roots", dbName)
			continue
		}
		if err != nil {
			db.Close()
			d.logger.Printf("wisp_reaper: %s: %v", dbName, err)
			reapErrors++
			continue
		}
		result, err := reaper.Reap(db, w, dbName, maxAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: reap error: %v", dbName, err)
			reapErrors++
			continue
		}
		totalReaped += result.Reaped
		totalMoleculeSteps += result.MoleculeStepsClosed
		totalOpen += result.OpenRemain
		openDBs++
		if result.Reaped > 0 || result.MoleculeStepsClosed > 0 {
			reapSummary := fmt.Sprintf("wisp_reaper: %s: reaped %d stale wisps", dbName, result.Reaped)
			if result.MoleculeStepsClosed > 0 {
				reapSummary += fmt.Sprintf(", closed %d molecule steps", result.MoleculeStepsClosed)
			}
			d.logger.Printf("%s, %d open remain", reapSummary, result.OpenRemain)
		}
	}
	if reapErrors > 0 {
		cycle.failStep("reap", fmt.Sprintf("%d databases had reap errors", reapErrors))
	} else {
		cycle.closeStep("reap")
	}
	// A skipped database is out of every later step, so none opens or writes it.
	// databases may be the configured slice, so filter into a new one.
	if len(skipped) > 0 {
		var live []string
		for _, dbName := range databases {
			if !skipped[dbName] {
				live = append(live, dbName)
			}
		}
		databases = live
	}

	// Step 3: Purge
	purgeErrors := 0
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := d.openReaperDB(dbName, 30*time.Second, 30*time.Second)
		if err != nil {
			if errors.Is(err, beadsql.ErrNotBeads) {
				continue
			}
			purgeErrors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun, extraRoots...)
		if err != nil {
			db.Close()
			d.logger.Printf("wisp_reaper: %s: %v", dbName, err)
			purgeErrors++
			continue
		}
		result, err := reaper.Purge(db, w, dbName, deleteAge, defaultMailDeleteAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: purge error: %v", dbName, err)
			purgeErrors++
			continue
		}
		totalPurged += result.WispsPurged
		totalMailPurged += result.MailPurged
		for _, a := range result.Anomalies {
			d.logger.Printf("wisp_reaper: %s: ANOMALY: %s", dbName, a.Message)
		}
	}
	if purgeErrors > 0 {
		cycle.failStep("purge", fmt.Sprintf("%d databases had purge errors", purgeErrors))
	} else {
		cycle.closeStep("purge")
	}

	// Step 3b: Close plugin receipts (fast-track — 1h instead of 7d stale age)
	pluginReceiptAge := 1 * time.Hour
	var totalPluginClosed int
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := d.openReaperDB(dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			if errors.Is(err, beadsql.ErrNotBeads) {
				continue
			}
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun, extraRoots...)
		if err != nil {
			db.Close()
			d.logger.Printf("wisp_reaper: %s: %v", dbName, err)
			continue
		}
		result, err := reaper.ClosePluginReceipts(db, w, dbName, pluginReceiptAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: plugin receipt close error: %v", dbName, err)
			continue
		}
		totalPluginClosed += result.Closed
		if result.Closed > 0 {
			d.logger.Printf("wisp_reaper: %s: closed %d plugin receipts", dbName, result.Closed)
		}
	}

	// Step 3c: Close plugin dispatch mails (daemon→dog instruction beads left
	// open from before the dogs were retired)
	pluginDispatchAge := 1 * time.Hour
	var totalDispatchClosed int
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := d.openReaperDB(dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			if errors.Is(err, beadsql.ErrNotBeads) {
				continue
			}
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun, extraRoots...)
		if err != nil {
			db.Close()
			d.logger.Printf("wisp_reaper: %s: %v", dbName, err)
			continue
		}
		result, err := reaper.ClosePluginDispatches(db, w, dbName, pluginDispatchAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: plugin dispatch close error: %v", dbName, err)
			continue
		}
		totalDispatchClosed += result.Closed
		if result.Closed > 0 {
			d.logger.Printf("wisp_reaper: %s: closed %d plugin dispatches", dbName, result.Closed)
		}
	}

	// Step 3d: Events retention (opt-in)
	//
	// Off unless patrols.wisp_reaper.events_retention is set: deleting audit
	// rows is the operator's call (gt-hk21d). dry_run counts the eligible rows
	// and deletes nothing.
	if retain, err := config.EventsRetention(); err != nil {
		d.logger.Printf("wisp_reaper: events retention: %v", err)
		cycle.failStep("events-retention", "patrols.wisp_reaper.events_retention is not a positive age")
	} else if retain == 0 {
		d.logger.Printf("wisp_reaper: events retention skipped (set patrols.wisp_reaper.events_retention to allow)")
		cycle.skipStep("events-retention", "patrols.wisp_reaper.events_retention is not set")
	} else {
		retentionErrors := 0
		waitingForArchive := 0
		for _, dbName := range databases {
			if err := reaper.ValidateDBName(dbName); err != nil {
				continue
			}
			db, err := d.openReaperDB(dbName, 10*time.Second, 30*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
				d.logger.Printf("wisp_reaper: %s: events retention connect error: %v", dbName, err)
				retentionErrors++
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), eventsRetentionTimeout)
			result, purgeErr := reaper.PurgeEvents(ctx, db, reaper.EventsPurgeOptions{
				Now:        time.Now().UTC(),
				Retain:     retain,
				Batch:      defaultEventsPurgeBatch,
				MaxBatches: defaultEventsPurgeMaxBatches,
				DryRun:     dryRun,
				Database:   dbName,
				StatePath:  maintenanceGCStatePath(d.config.TownRoot),
			})
			cancel()
			db.Close()
			if purgeErr != nil {
				if result.Cutoff.IsZero() {
					d.logger.Printf("wisp_reaper: %s: events retention error: %v", dbName, purgeErr)
				} else {
					d.logger.Printf("wisp_reaper: %s: events retention error: %v (%s)", dbName, purgeErr, result.Summary())
				}
				retentionErrors++
				continue
			}
			if result.Waiting != "" {
				waitingForArchive++
			}
			d.logger.Printf("wisp_reaper: %s: events retention: %s", dbName, result.Summary())
		}
		// A database waiting for the events archive is skipped, not passed:
		// retention deletes only what the archive has written out (gt-2i850).
		switch {
		case retentionErrors > 0:
			cycle.failStep("events-retention", fmt.Sprintf("%d databases had events retention errors", retentionErrors))
		case waitingForArchive > 0:
			cycle.skipStep("events-retention", fmt.Sprintf("%d databases waiting for the events archive watermark", waitingForArchive))
		default:
			cycle.closeStep("events-retention")
		}
	}

	// Step 4: Auto-close
	//
	// Auto-close requires patrols.wisp_reaper.auto_close to be explicitly true
	// (see wispReaperAutoCloseEnabled) — `dry_run` is not the disarm, because
	// that knob also stops reap and purge.
	if !wispReaperAutoCloseEnabled(d.patrol()) {
		d.logger.Printf("wisp_reaper: auto-close skipped (set patrols.wisp_reaper.auto_close=true to allow)")
		cycle.skipStep("auto-close", "patrols.wisp_reaper.auto_close is not true")
	} else {
		autoCloseErrors := 0
		for _, dbName := range databases {
			if err := reaper.ValidateDBName(dbName); err != nil {
				continue
			}
			db, err := d.openReaperDB(dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				if errors.Is(err, beadsql.ErrNotBeads) {
					continue
				}
				autoCloseErrors++
				continue
			}
			// Auto-close operates on the issues table, not wisps, but if the database
			// has no beads schema at all we should skip it too.
			if ok, _ := reaper.HasReaperSchema(db); !ok {
				db.Close()
				continue
			}
			closed, err := d.autoCloseDB(db, dbName, staleIssueAge, dryRun, extraRoots...)
			db.Close()
			if err != nil {
				d.logger.Printf("wisp_reaper: %s: auto-close error: %v", dbName, err)
				autoCloseErrors++
				continue
			}
			totalAutoClosed += closed
		}
		if autoCloseErrors > 0 {
			cycle.failStep("auto-close", fmt.Sprintf("%d databases had auto-close errors", autoCloseErrors))
		} else {
			cycle.closeStep("auto-close")
		}
	}

	// Step 5: Report
	d.recordCycleOpenWisps(reapErrors, reaper.OpenWispSample{OpenWisps: totalOpen, Databases: openDBs, DryRun: dryRun})
	summary := fmt.Sprintf("wisp_reaper: cycle complete — reaped=%d", totalReaped)
	if totalMoleculeSteps > 0 {
		summary += fmt.Sprintf(" molecule_steps_closed=%d", totalMoleculeSteps)
	}
	// databases counts the databases the reap attempted, reaped or failed;
	// skipped counts those no searched beads root names, reported apart.
	summary += fmt.Sprintf(" purged=%d mail_purged=%d plugin_closed=%d dispatch_closed=%d auto_closed=%d open=%d databases=%d skipped=%d dryRun=%v",
		totalPurged, totalMailPurged, totalPluginClosed, totalDispatchClosed, totalAutoClosed, totalOpen, openDBs+reapErrors, len(skipped), dryRun)
	d.logger.Printf("%s", summary)
	cycle.closeStep("report")
}

// recordCycleOpenWisps reports one patrol cycle's open-wisp reading, unless the
// cycle lost databases to reap errors: that count covers fewer databases than
// the series is measured over, so recording it would anchor the series on an
// undercount and hide the growth the alert exists to catch (gt-11kyy). The
// failed step is already reported by the cycle itself.
func (d *Daemon) recordCycleOpenWisps(reapErrors int, sample reaper.OpenWispSample) {
	if reapErrors > 0 {
		d.logger.Printf("wisp_reaper: open-wisp alert skipped: %d database(s) failed to reap, so this cycle's count is incomplete", reapErrors)
		return
	}
	d.reportOpenWispAlert(sample)
}

// reportOpenWispAlert warns when this cycle's open-wisp count grew enough over
// the recorded baseline to mean accumulation rather than a working set, then
// records the cycle as the reading the next one is judged against. The record
// outlives the process — it is on disk — so a daemon restart loses no
// comparison (gt-11kyy).
func (d *Daemon) reportOpenWispAlert(sample reaper.OpenWispSample) {
	path := WispAlertBaselinePath(d.config.TownRoot)
	previous, err := LoadWispAlertState(path)
	if err != nil {
		d.logger.Printf("wisp_reaper: WARNING: cannot read the open-wisp baseline (%v) — "+
			"this cycle cannot be judged against the last one", err)
	}
	next, alert, detail := reaper.NextOpenWispAlertState(previous, sample)
	if alert {
		d.logger.Printf("wisp_reaper: WARNING: %s — investigate wisp lifecycle", detail)
	}
	if err := SaveWispAlertState(path, next); err != nil {
		d.logger.Printf("wisp_reaper: WARNING: cannot record the open-wisp baseline (%v) — "+
			"the next cycle will have nothing to compare against", err)
	}
}

// autoCloseDB runs the auto-close sweep against one open database and returns
// how many issues it closed. It previews before writing, because this path has
// no formula prose to order the two passes for it (gt-39bu).
//
// A below-floor stale-age is a soft refusal, not an error (gt-ecpj): AutoClose
// returns the set the mis-set threshold would take alongside ErrStaleAgeTooLow,
// so this logs the notice and reports zero closed. Failing the step instead
// would stop a patrol over a config value, which is the stop the soft refusal
// exists to remove. extraRoots are the directories searched after the town
// root when it resolves the writer (gt-33etw).
func (d *Daemon) autoCloseDB(db *beadsql.DB, dbName string, staleIssueAge time.Duration, dryRun bool, extraRoots ...string) (int, error) {
	preview, err := reaper.AutoClose(db, nil, dbName, reaper.AutoCloseOptions{
		StaleAge: staleIssueAge,
		DryRun:   true,
	})
	if err != nil {
		return 0, fmt.Errorf("preview: %w", err)
	}

	// The writer is resolved only when this run may write: a dry run, or a
	// preview with nothing to close, needs no bd.
	var w reaper.Writer
	if !dryRun && preview.Closed > 0 && !preview.Floored {
		if w, err = d.reaperWriter(dbName, dryRun, extraRoots...); err != nil {
			return 0, err
		}
	}
	result, err := reaper.AutoClose(db, w, dbName, reaper.AutoCloseOptions{
		StaleAge:    staleIssueAge,
		DryRun:      dryRun,
		PreviewHash: preview.PreviewHash,
	})
	if errors.Is(err, reaper.ErrStaleAgeTooLow) {
		candidates := 0
		if result != nil {
			candidates = len(result.ClosedEntries)
		}
		d.logger.Printf("wisp_reaper: %s: %s", dbName, reaper.FloorNotice(staleIssueAge, candidates))
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	// A dry-run cycle gets no error to carry the refusal, so the flag on the
	// result is what reports it here — and a refused sweep closed nothing, so
	// it counts as nothing closed rather than as the set it reported.
	if result.Floored {
		d.logger.Printf("wisp_reaper: %s: %s", dbName, reaper.FloorNotice(result.FlooredAt, len(result.ClosedEntries)))
		return 0, nil
	}
	if preview.Closed > 0 {
		d.logger.Printf("wisp_reaper: %s: auto-close preview: %d candidate(s)", dbName, preview.Closed)
	}
	return result.Closed, nil
}

// doltServerPort is the town's Dolt port (config.ResolveDoltEndpoint), or 0
// when the town names none: a connection then fails rather than reaching
// whatever listens on the default port (gt-y3pgh.9).
func (d *Daemon) doltServerPort() int {
	return agentconfig.ResolveDoltPort(d.config.TownRoot)
}

// doltUserInfo is the user[:password] the daemon connects to Dolt with.
func (d *Daemon) doltUserInfo() string {
	return doltserver.DefaultConfig(d.config.TownRoot).UserDSN()
}

// doltServerHost is the town's Dolt host, or the local machine.
func (d *Daemon) doltServerHost() string {
	if host := agentconfig.ResolveDoltHost(d.config.TownRoot); host != "" {
		return host
	}
	return "127.0.0.1"
}
