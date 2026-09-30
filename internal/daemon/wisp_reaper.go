package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
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
	// Alert threshold: if open wisp count exceeds this, the reaper warns.
	// Shared with `gt reaper run` warning. See reaper.DefaultAlertThreshold.
	wispAlertThreshold = reaper.DefaultAlertThreshold
	// Closed mail older than this is permanently deleted. Formula var: mail_delete_age.
	defaultMailDeleteAge = 7 * 24 * time.Hour
	// Issues stale longer than this are auto-closed. Formula var: stale_issue_age.
	//
	// This MUST track the mol-dog-reaper formula default (720h). It was 7d until
	// gt-2qzr: the daemon injected the shorter value on the dog path, overriding
	// the formula, and the inline fallback used it directly. Agent beads are idle
	// by design and were eight days old, so a 7d threshold swept 102 durable
	// beads — every agent, dog, and patrol-molecule bead in the town. Auto-close
	// is for work that was abandoned, and abandonment does not look like a week.
	defaultStaleIssueAge = 30 * 24 * time.Hour
)

// wispReaperInterval returns the configured interval, or the default (1h).
func wispReaperInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.IntervalStr != "" {
			if d, err := ParseAgeDuration(config.Patrols.WispReaper.IntervalStr); err == nil && d > 0 {
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
			if d, err := ParseAgeDuration(config.Patrols.WispReaper.MaxAgeStr); err == nil && d > 0 {
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
			if d, err := ParseAgeDuration(config.Patrols.WispReaper.DeleteAgeStr); err == nil && d > 0 {
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
			if d, err := ParseAgeDuration(config.Patrols.WispReaper.StaleIssueAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultStaleIssueAge
}

// WispReaperAutoCloseDisarmed reports whether daemon.json explicitly disarms
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

// ParseAgeDuration parses an age string, accepting the day suffix that the
// mol-dog-reaper formula emits for its 30d default. time.ParseDuration rejects
// "30d" as "unknown unit d", which is how the incident run ended up with a
// locally-invented shorter value instead of the formula's intent (gt-2qzr).
func ParseAgeDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(days))
		if err != nil {
			return 0, fmt.Errorf("invalid day suffix in %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
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

	dec := evaluatePatrolDue(d.config.TownRoot, "wisp_reaper", time.Time{}, time.Now(), wispReaperInterval(d.patrolConfig))
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

// reapWisps is the thin orchestrator for the wisp_reaper patrol. It pours a
// mol-dog-reaper molecule as the cycle's receipt and runs the sweep inline,
// closing the molecule's steps as it goes.
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
		if err := savePatrolLastRun(d.config.TownRoot, "wisp_reaper", time.Now()); err != nil {
			d.logger.Printf("wisp_reaper: WARNING: cannot persist last-run time (%v) — "+
				"the next check may re-run sooner than expected", err)
		}
	}()

	config := d.patrolConfig.Patrols.WispReaper
	maxAge := wispReaperMaxAge(d.patrolConfig)
	deleteAge := wispDeleteAge(d.patrolConfig)
	staleIssueAge := wispReaperStaleIssueAge(d.patrolConfig)

	vars := map[string]string{
		"max_age":         maxAge.String(),
		"purge_age":       deleteAge.String(),
		"stale_issue_age": staleIssueAge.String(),
		"mail_delete_age": defaultMailDeleteAge.String(),
		"alert_threshold": fmt.Sprintf("%d", wispAlertThreshold),
	}

	if config.DryRun {
		vars["dry_run"] = "true"
	}
	if WispReaperAutoCloseDisarmed(d.patrolConfig) {
		// Recorded on the receipt, so the molecule says why auto-close did
		// nothing.
		vars["auto_close"] = "false"
	}
	if len(config.Databases) > 0 {
		vars["databases"] = strings.Join(config.Databases, ",")
	}

	// Pour the molecule for observability tracking.
	mol := d.pourDogMolecule(constants.MolDogReaper, vars)
	defer mol.close()

	if config.DryRun {
		d.logger.Printf("wisp_reaper: DRY RUN — reporting only, no changes will be made")
	}

	d.reapWispsInline(config, maxAge, deleteAge, staleIssueAge, mol)
}

// reaperWriter returns the bd writer for a live run against dbName, pinned to
// the beads dir whose metadata names that database, and nil for a dry run:
// the reaper selects with SQL but writes only through bd (gt-fcxe9.12).
func (d *Daemon) reaperWriter(dbName string, dryRun bool) (reaper.Writer, error) {
	if dryRun {
		return nil, nil
	}
	if d.reaperWriterForFn != nil {
		return d.reaperWriterForFn(d.config.TownRoot, dbName)
	}
	return reaper.WriterForDatabase(d.config.TownRoot, dbName)
}

// reapWispsInline runs the reaper cycle in the daemon. Delegates to the
// reaper package, which selects with SQL and writes through bd.
func (d *Daemon) reapWispsInline(config *WispReaperConfig, maxAge, deleteAge, staleIssueAge time.Duration, mol *dogMol) {
	databases := config.Databases
	host := d.doltServerHost()
	if len(databases) == 0 {
		databases = reaper.DiscoverDatabases(host, d.doltServerPort())
	}
	if len(databases) == 0 {
		d.logger.Printf("wisp_reaper: no databases to reap")
		mol.failStep("scan", "no databases found")
		return
	}
	d.logger.Printf("wisp_reaper: scanning %d databases", len(databases))
	mol.closeStep("scan")

	port := d.doltServerPort()
	dryRun := config.DryRun
	var totalReaped, totalMoleculeSteps, totalOpen, totalPurged, totalMailPurged, totalAutoClosed int

	// Step 2: Reap
	reapErrors := 0
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: connect error: %v", dbName, err)
			reapErrors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			d.logger.Printf("wisp_reaper: %s: skipped (no reaper schema)", dbName)
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun)
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
		if result.Reaped > 0 || result.MoleculeStepsClosed > 0 {
			reapSummary := fmt.Sprintf("wisp_reaper: %s: reaped %d stale wisps", dbName, result.Reaped)
			if result.MoleculeStepsClosed > 0 {
				reapSummary += fmt.Sprintf(", closed %d molecule steps", result.MoleculeStepsClosed)
			}
			d.logger.Printf("%s, %d open remain", reapSummary, result.OpenRemain)
		}
	}
	if reapErrors > 0 {
		mol.failStep("reap", fmt.Sprintf("%d databases had reap errors", reapErrors))
	} else {
		mol.closeStep("reap")
	}

	// Step 3: Purge
	purgeErrors := 0
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 30*time.Second, 30*time.Second)
		if err != nil {
			purgeErrors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun)
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
		mol.failStep("purge", fmt.Sprintf("%d databases had purge errors", purgeErrors))
	} else {
		mol.closeStep("purge")
	}

	// Step 3b: Close plugin receipts (fast-track — 1h instead of 7d stale age)
	pluginReceiptAge := 1 * time.Hour
	var totalPluginClosed int
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun)
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
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		w, err := d.reaperWriter(dbName, dryRun)
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

	// Step 4: Auto-close
	//
	// Auto-close requires patrols.wisp_reaper.auto_close to be explicitly true
	// (see wispReaperAutoCloseEnabled) — `dry_run` is not the disarm, because
	// that knob also stops reap and purge.
	if !wispReaperAutoCloseEnabled(d.patrolConfig) {
		d.logger.Printf("wisp_reaper: auto-close skipped (set patrols.wisp_reaper.auto_close=true to allow)")
		mol.closeStep("auto-close")
	} else {
		autoCloseErrors := 0
		for _, dbName := range databases {
			if err := reaper.ValidateDBName(dbName); err != nil {
				continue
			}
			db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
			if err != nil {
				autoCloseErrors++
				continue
			}
			// Auto-close operates on the issues table, not wisps, but if the database
			// has no beads schema at all we should skip it too.
			if ok, _ := reaper.HasReaperSchema(db); !ok {
				db.Close()
				continue
			}
			closed, err := d.autoCloseDB(db, dbName, staleIssueAge, dryRun)
			db.Close()
			if err != nil {
				d.logger.Printf("wisp_reaper: %s: auto-close error: %v", dbName, err)
				autoCloseErrors++
				continue
			}
			totalAutoClosed += closed
		}
		if autoCloseErrors > 0 {
			mol.failStep("auto-close", fmt.Sprintf("%d databases had auto-close errors", autoCloseErrors))
		} else {
			mol.closeStep("auto-close")
		}
	}

	// Step 5: Report
	if totalOpen > wispAlertThreshold {
		d.logger.Printf("wisp_reaper: WARNING: %d open wisps exceed threshold %d — investigate wisp lifecycle",
			totalOpen, wispAlertThreshold)
	}
	summary := fmt.Sprintf("wisp_reaper: cycle complete — reaped=%d", totalReaped)
	if totalMoleculeSteps > 0 {
		summary += fmt.Sprintf(" molecule_steps_closed=%d", totalMoleculeSteps)
	}
	summary += fmt.Sprintf(" purged=%d mail_purged=%d plugin_closed=%d dispatch_closed=%d auto_closed=%d open=%d databases=%d dryRun=%v",
		totalPurged, totalMailPurged, totalPluginClosed, totalDispatchClosed, totalAutoClosed, totalOpen, len(databases), dryRun)
	d.logger.Printf("%s", summary)
	mol.closeStep("report")
}

// autoCloseDB runs the auto-close sweep against one open database and returns
// how many issues it closed. It previews before writing, because this path has
// no formula prose to order the two passes for it (gt-39bu).
//
// A below-floor stale-age is a soft refusal, not an error (gt-ecpj): AutoClose
// returns the set the mis-set threshold would take alongside ErrStaleAgeTooLow,
// so this logs the notice and reports zero closed. Failing the step instead
// would stop a patrol over a config value, which is the stop the soft refusal
// exists to remove.
func (d *Daemon) autoCloseDB(db *sql.DB, dbName string, staleIssueAge time.Duration, dryRun bool) (int, error) {
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
		if w, err = d.reaperWriter(dbName, dryRun); err != nil {
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

// doltServerPort returns the configured Dolt server port.
func (d *Daemon) doltServerPort() int {
	if d.doltServer != nil {
		return d.doltServer.config.Port
	}
	if port := agentconfig.ResolveDoltPort(d.config.TownRoot); port > 0 {
		return port
	}
	return doltserver.DefaultPort
}

func (d *Daemon) doltServerHost() string {
	if d.doltServer != nil && d.doltServer.config.Host != "" {
		return d.doltServer.config.Host
	}
	if host := agentconfig.ResolveDoltHost(d.config.TownRoot); host != "" {
		return host
	}
	if cfg := doltserver.DefaultConfig(d.config.TownRoot); cfg.Host != "" {
		return cfg.Host
	}
	return "127.0.0.1"
}
