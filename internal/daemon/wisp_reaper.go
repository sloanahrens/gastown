package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	// defaultWispReaperInterval is the patrol interval. Set to 1h since reaping
	// is cleanup work, not latency-sensitive. Was 30m before Dog-driven refactor.
	defaultWispReaperInterval = 1 * time.Hour
	// Wisps older than this are reaped (closed). Configurable via formula var max_age.
	defaultWispMaxAge = 24 * time.Hour
	// Closed wisps older than this are permanently deleted. Formula var: purge_age.
	defaultWispDeleteAge = 7 * 24 * time.Hour
	// Alert threshold: if open wisp count exceeds this, the Dog should escalate.
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

// WispReaperConfig holds configuration for the wisp_reaper patrol.
type WispReaperConfig struct {
	Enabled      bool     `json:"enabled"`
	DryRun       bool     `json:"dry_run,omitempty"`
	IntervalStr  string   `json:"interval,omitempty"`
	MaxAgeStr    string   `json:"max_age,omitempty"`
	DeleteAgeStr string   `json:"delete_age,omitempty"`
	Databases    []string `json:"databases,omitempty"`

	// StaleIssueAgeStr overrides how long an issue may sit untouched before
	// auto-close (e.g. "720h" or "30d"). Empty means defaultStaleIssueAge.
	StaleIssueAgeStr string `json:"stale_issue_age,omitempty"`

	// AutoClose disarms ONLY the stale-issue auto-close step, leaving reap and
	// purge running — the point of a dedicated knob (gt-2qzr), since dry_run
	// pauses the whole patrol. nil means unset: the dog path keeps its default
	// behavior while the inline fallback stays disarmed.
	AutoClose *bool `json:"auto_close,omitempty"`
}

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
// This is read by `gt reaper auto-close` as well, so the setting disarms the
// Dog-driven path too — not just the daemon's own inline fallback. A nil knob
// is "unset", not "disarmed".
func WispReaperAutoCloseDisarmed(config *DaemonPatrolConfig) bool {
	if config == nil || config.Patrols == nil || config.Patrols.WispReaper == nil {
		return false
	}
	knob := config.Patrols.WispReaper.AutoClose
	return knob != nil && !*knob
}

// wispReaperAutoCloseEnabled reports whether a given execution path may
// auto-close. The Dog-driven path is the designed home for the sweep, so an
// unset knob leaves it enabled. The inline fallback is an error path: it runs
// because Dog dispatch FAILED, and a dispatch failure must never be upgraded
// into a destructive sweep, so it needs an explicit opt-in (gt-2qzr).
func wispReaperAutoCloseEnabled(config *DaemonPatrolConfig, inlineFallback bool) bool {
	if WispReaperAutoCloseDisarmed(config) {
		return false
	}
	if !inlineFallback {
		return true
	}
	knob := (*bool)(nil)
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		knob = config.Patrols.WispReaper.AutoClose
	}
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
// Dispatched onto its own goroutine, like the gt-ima2 fix for compactor_dog:
// dispatchReaperDog shells out to `gt sling`, and running that inline would
// stall every other tick behind it.
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

// reapWisps is the thin orchestrator for the wisp_reaper patrol.
// It pours a mol-dog-reaper molecule, then dispatches a Dog to execute it.
// The Dog reads the formula steps and calls `gt reaper` CLI helpers.
// Falls back to inline execution if Dog dispatch fails.
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
		// Belt and braces: the formula skips the step, and `gt reaper auto-close`
		// independently refuses, so a Dog that ignores the instruction still
		// cannot sweep.
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

	// Try dispatching to a Dog for formula-driven execution.
	if err := d.dispatchReaperDog(vars); err != nil {
		// The cause matters more than the fact: gt-2qzr was diagnosed late
		// because the log recorded only "exit status 1", which said nothing
		// about why the sweep had silently moved to the inline fallback.
		d.logger.Printf("wisp_reaper: Dog dispatch failed (%v), running inline fallback", err)
		d.reapWispsInline(config, maxAge, deleteAge, staleIssueAge, mol)
		return
	}

	d.logger.Printf("wisp_reaper: dispatched to Dog for formula-driven execution")
}

// dispatchReaperDog dispatches the mol-dog-reaper formula to a Dog via gt sling.
func (d *Daemon) dispatchReaperDog(vars map[string]string) error {
	args := []string{"sling", constants.MolDogReaper, "deacon/dogs"}
	for k, v := range vars {
		args = append(args, "--var", fmt.Sprintf("%s=%s", k, v))
	}

	cmd := exec.Command(d.gtPath, args...) //nolint:gosec // G204: d.gtPath resolved at daemon init via LookPath
	cmd.Dir = d.config.TownRoot
	// gt sling performs writes, so use mutation routing env: it preserves PATH
	// while stripping stale bd target selectors and derived Beads endpoint aliases.
	cmd.Env = bdMutationRoutingEnv(d.config.TownRoot)
	util.SetDetachedProcessGroup(cmd)
	// Capture output so a failure reports WHY it failed, not just that it did.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gt sling: %w: %s", err, summarizeCommandOutput(out))
	}
	return nil
}

// summarizeCommandOutput renders subprocess output for a single log line.
// Commands here are chatty (sling prints progress), so the tail is what
// carries the failure; the cap keeps one bad cycle from filling the log.
func summarizeCommandOutput(out []byte) string {
	const maxLen = 2000
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "(no output)"
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		s = s[len(s)-maxLen:]
	}
	return s
}

// reapWispsInline is the fallback that runs the reaper cycle inline when
// Dog dispatch is unavailable. Delegates to the reaper package for SQL execution.
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
	d.logger.Printf("wisp_reaper: scanning %d databases (inline fallback)", len(databases))
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
		result, err := reaper.Reap(db, dbName, maxAge, dryRun)
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
		result, err := reaper.Purge(db, dbName, deleteAge, defaultMailDeleteAge, dryRun)
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
		result, err := reaper.ClosePluginReceipts(db, dbName, pluginReceiptAge, dryRun)
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

	// Step 3c: Close plugin dispatch mails (daemon→dog instruction beads that are never closed)
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
		result, err := reaper.ClosePluginDispatches(db, dbName, pluginDispatchAge, dryRun)
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
	// The inline path runs because Dog dispatch FAILED. Auto-close writes to
	// durable issues, unlike reap/purge which only retire ephemeral wisps, so a
	// dispatch error must not be upgraded into a sweep of the town's issue
	// tracker (gt-2qzr). It therefore requires patrols.wisp_reaper.auto_close to
	// be explicitly true — `dry_run` is not the disarm, because that knob also
	// stops reap and purge.
	if !wispReaperAutoCloseEnabled(d.patrolConfig, true) {
		d.logger.Printf("wisp_reaper: auto-close skipped in inline fallback (set patrols.wisp_reaper.auto_close=true to allow)")
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
	preview, err := reaper.AutoClose(db, dbName, reaper.AutoCloseOptions{
		StaleAge: staleIssueAge,
		DryRun:   true,
	})
	if err != nil {
		return 0, fmt.Errorf("preview: %w", err)
	}

	result, err := reaper.AutoClose(db, dbName, reaper.AutoCloseOptions{
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
