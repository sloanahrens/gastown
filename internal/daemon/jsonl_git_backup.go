package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/events"
	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/testdb"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultJsonlGitBackupInterval = 15 * time.Minute
	jsonlExportTimeout            = 60 * time.Second
	gitPushTimeout                = 120 * time.Second
	gitCmdTimeout                 = 30 * time.Second
	maxConsecutivePushFailures    = 3

	// gitLocalPhaseTimeout bounds the local file-staging half of one backup tick
	// (add, diff --cached, commit) as a whole, not per command. gitCmdTimeout is
	// a network budget, and a loaded host blows through it while staging the
	// export tree; the `git add` it killed is what orphaned .git/index.lock and
	// poisoned every later tick (gt-1aj2). One shared deadline still bounds a
	// wedged tick to well under the 15-minute interval, where three stacked
	// per-command budgets would not.
	gitLocalPhaseTimeout = 5 * time.Minute

	// gitIndexLockGracePeriod is how long .git/index.lock must sit untouched
	// before the daemon treats it as abandoned rather than held. git records no
	// owner in the lock, so age is the only signal available; two minutes is far
	// below the backup interval, so a lock left by a dead tick is always
	// recognizable, while a lock a live git process just created is not.
	gitIndexLockGracePeriod = 2 * time.Minute

	// jsonlSupplementalExportTimeout bounds one database's whole supplemental
	// export — the six non-issues tables together, not each table on its own:
	// per table, a wedged dolt held the backup cycle and its doltMaintMu read
	// lock for half an hour, past the 15-minute tick (gt-4nqej). Five minutes
	// gives events, the largest table, the room the 60 s issues bound did not
	// (gt-e38w6).
	jsonlSupplementalExportTimeout = 5 * time.Minute

	defaultSpikeThreshold = 0.50 // 50% delta triggers halt (was 20%, too sensitive for bulk ops)

	// defaultEscalationTimeout is the per-attempt timeout for the gt escalate
	// child process.  Kept at 60 s so that, even under slot-starvation or
	// Dolt contention (the conditions that produced the gt-tlwv drops on
	// 2026-09-16), the child has enough wall-clock to acquire the slot and
	// complete the Dolt write.  The caller retries up to 3 attempts with
	// exponential backoff, giving ~120 s total budget.
	defaultEscalationTimeout = 60 * time.Second

	// maxEscalationRetries is the maximum number of attempts before giving up
	// and logging the escalation to the feed as a last-resort fallback.
	maxEscalationRetries = 3

	// gitPostBufferBytes raises http.postBuffer above git's 1 MB default on the
	// offsite backup repo. JSONL exports accumulate across many unpushed
	// snapshot commits between successful pushes, so the pack GitHub receives
	// can exceed 1 MB; over that, GitHub's git-receive-pack rejects the chunked
	// POST with "HTTP 400" (curl 22) and pushes fail until someone notices and
	// sets this by hand (gt-kxa1).
	gitPostBufferBytes = "524288000" // 500 MB
)

// testPollutionPatterns matches issue IDs or titles that indicate test data leaked
// into production exports. These records are filtered out before writing JSONL.
//
// The bar for a title pattern is that the sweep itself generates the form, never
// that a title merely contains a word: a pattern that fires on real work drops
// real beads from the offsite backup every cycle, and verifyNoPollution cannot
// see the loss because it shares the definition (gt-q6ljl). So the generated
// forms are matched exactly — an underscore test slug ("test_something", the
// form the suites mint) or the "test issue <n>" titles the sweep creates — and
// ordinary prose such as "Test coverage for X" or "test the pager" is kept.
var testPollutionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^test[_\s]issue\b`),     // title: "Test issue 42", "test issue for validation"
	regexp.MustCompile(`(?i)^test_`),                // title: "test_something" (underscore slug)
	regexp.MustCompile(`^bd-[0-9]{1,2}$`),           // id: bd-1, bd-99 (suspiciously short IDs)
	regexp.MustCompile(`^bd-[a-z]{3,5}[0-9]{1,2}$`), // id: bd-abc12 (test-style IDs)
	testDatabaseIDPattern(),                         // id prefixes from test databases
	regexp.MustCompile(`(?i)^--help`),               // title: "--help" CLI artifacts
	regexp.MustCompile(`(?i)^Usage:\s`),             // title: "Usage: ..." CLI help output
	regexp.MustCompile(`^offlinebrew-`),             // id: offlinebrew-* test prefixes
	regexp.MustCompile(`-wisp-`),                    // id: wisp-pattern IDs leaked into issues table
}

// testDatabaseIDPattern matches an issue ID that starts with a test-database
// prefix (internal/testdb, the one list).
func testDatabaseIDPattern() *regexp.Regexp {
	quoted := testdb.Prefixes()
	for i, p := range quoted {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`^(` + strings.Join(quoted, "|") + `)`)
}

// validDBName matches safe database names (alphanumeric, underscore, hyphen).
var validDBName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// backupIssuesQuery reads db's issues for the backup; with scrub, only the
// durable work product, without the test-database prefixes (internal/testdb,
// the one list).
func backupIssuesQuery(db string, scrub bool) beadsql.Query {
	return beadsql.BackupIssues(db, scrub, testdb.Prefixes())
}

// jsonlGitBackupInterval returns the configured interval, or the default (15m).
func jsonlGitBackupInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.JsonlGitBackup != nil {
		if config.Patrols.JsonlGitBackup.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.JsonlGitBackup.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultJsonlGitBackupInterval
}

// triggerJsonlGitBackup runs a jsonl_git_backup cycle when the patrol is due,
// on its own goroutine.
//
// The ticker that drives this call is a check cadence, not a run cadence: an
// in-process ticker resets its countdown on every daemon restart, so due-ness
// is instead decided from the persisted last-run time in
// daemon/patrol_last_run.json, which survives a restart (gt-ima2, gt-gxpwc).
//
// Dispatched onto its own goroutine, like the gt-ima2 fix for compactor_dog:
// a cycle can hold for several minutes (git add/commit/push), and running it
// inline would stall every other tick behind it.
func (d *Daemon) triggerJsonlGitBackup() {
	if !d.isPatrolActive("jsonl_git_backup") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "jsonl_git_backup", d.jsonlGitBackupRun.last(), time.Now(), jsonlGitBackupInterval(d.patrolConfig))
	if !dec.due {
		d.logger.Printf("jsonl_git_backup: not due — %s", dec.note)
		return
	}

	if !d.jsonlGitBackupRunning.CompareAndSwap(false, true) {
		d.logger.Printf("jsonl_git_backup: previous cycle still running — skipping this check")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("jsonl_git_backup: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("jsonl_git_backup: due — %s", dec.note)
	}

	go func() {
		defer d.jsonlGitBackupRunning.Store(false)
		d.syncJsonlGitBackup()
	}()
}

// syncJsonlGitBackup exports issues from each database to JSONL, scrubs ephemeral data,
// and commits/pushes to a git repository.
// Non-fatal: errors are logged but don't stop the daemon.
func (d *Daemon) syncJsonlGitBackup() {
	if !d.isPatrolActive("jsonl_git_backup") {
		return
	}
	release, ok := d.tryDoltTask("jsonl_git_backup")
	if !ok {
		return
	}
	defer release()

	// Record that a cycle was attempted, regardless of outcome below — the
	// same "attempted" semantics as the ticker firing before gt-ima2/gt-gxpwc.
	// An unwritable daemon directory only means the next restart or tick
	// re-checks; it does not fail the cycle itself.
	defer func() {
		d.recordPatrolRun(&d.jsonlGitBackupRun, "jsonl_git_backup", time.Now())
	}()

	cycle := d.startDogCycle("jsonl_git_backup")
	defer cycle.close()

	config := d.patrolConfig.Patrols.JsonlGitBackup

	// Resolve git repo path. Default lives under TownRoot (matches vitals.go's
	// display path and where JSONL exports actually land) — NOT $HOME, which
	// silently diverges from TownRoot whenever the town isn't rooted at
	// $HOME/gt directly, leaving this patrol permanently unable to find its
	// own target directory (gt-kme).
	gitRepo := config.GitRepo
	if gitRepo == "" {
		gitRepo = filepath.Join(d.config.TownRoot, ".dolt-archive", "git")
	}

	// Ensure the backup repo exists, initializing it on first run instead of
	// silently no-op'ing forever. A missing repo used to mean this patrol
	// never ran a single cycle (0 successes across every dispatch) with no
	// signal anywhere that offsite backup was dead (gt-kme).
	if err := d.ensureGitRepoInitialized(gitRepo); err != nil {
		d.logger.Printf("jsonl_git_backup: cannot initialize git repo %s: %v", gitRepo, err)
		cycle.failStep("export", "git repo init failed: "+err.Error())
		d.escalateAlert(alertKeyJSONLInit, "jsonl_git_backup", fmt.Sprintf("cannot initialize offsite backup repo %s: %v", gitRepo, err))
		return
	}
	d.clearAlerts("backup repo initialized", alertKeyJSONLInit)

	// Determine whether to scrub (default true).
	scrub := true
	if config.Scrub != nil {
		scrub = *config.Scrub
	}

	// Resolve Dolt data dir for auto-discovery of running server.
	var dataDir string
	if d.doltServer != nil && d.doltServer.IsEnabled() && d.doltServer.config.DataDir != "" {
		dataDir = d.doltServer.config.DataDir
	} else {
		dataDir = filepath.Join(d.config.TownRoot, ".dolt-data")
	}
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		d.logger.Printf("jsonl_git_backup: data dir %s does not exist, skipping", dataDir)
		return
	}

	// Get database list. The shipped default config has no explicit Databases
	// list (see the "daemon" section of settings/config.json), so falling through to a bare skip here
	// meant this patrol did nothing on every single tick regardless of the
	// git-repo fix above — the documented "auto-discovers from dolt server"
	// behavior (types.go) was never implemented (gt-kme).
	databases := config.Databases
	if len(databases) == 0 {
		databases = discoverJsonlBackupDatabases(dataDir)
	}
	if len(databases) == 0 {
		d.logger.Printf("jsonl_git_backup: no databases found (configured or auto-discovered) under %s, skipping", dataDir)
		cycle.failStep("export", "no databases found")
		d.escalateAlert(alertKeyJSONLNoDBs, "jsonl_git_backup", fmt.Sprintf("no databases found under %s — offsite backup has nothing to export", dataDir))
		return
	}
	d.clearAlerts("databases discovered", alertKeyJSONLNoDBs)

	d.logger.Printf("jsonl_git_backup: exporting %d database(s) to %s (scrub=%v)", len(databases), gitRepo, scrub)

	// export is the dolt-backed per-database export, unless a test injected one
	// (jsonlExportFn) to drive this cycle's bookkeeping without running dolt.
	export := d.exportDatabaseToJsonl
	if d.jsonlExportFn != nil {
		export = d.jsonlExportFn
	}

	exported := 0
	var failed []string
	var failures []string
	var exportedDBs []string
	counts := make(map[string]int)
	for _, db := range databases {
		n, err := export(db, gitRepo, dataDir, scrub)
		if err != nil {
			d.logger.Printf("jsonl_git_backup: %s: export failed: %v", db, err)
			failed = append(failed, db)
			failures = append(failures, fmt.Sprintf("%s: %v", db, err))
		} else {
			counts[db] = n
			exported++
			exportedDBs = append(exportedDBs, db)
		}
	}

	if exported == 0 {
		// The all-failed case escalates like the init and no-databases paths: a
		// cycle that exports nothing is a backup an operator must hear about
		// (gt-fxm2i).
		d.logger.Printf("jsonl_git_backup: no databases exported successfully")
		cycle.failStep("export", "no databases exported successfully")
		// Forget the detail a partial cycle recorded: this raise replaces that
		// alert's text, so the next partial cycle raises rather than matching a
		// text that no longer describes what is failing (gt-ga9hx).
		d.jsonlExportAlert = ""
		d.escalateAlert(alertKeyJSONLExport, "jsonl_git_backup", fmt.Sprintf(
			"every database export failed — the offsite backup exported nothing this cycle:\n%s",
			strings.Join(failures, "\n")))
		return
	}
	if len(failed) > 0 {
		// A partial failure keeps the alert raised: the failed database's
		// offsite copy is the stale file already in the repo, and the commit
		// subject that names it exists only when a commit happens for another
		// reason. Only a tick where every database exported clears it.
		d.logger.Printf("jsonl_git_backup: %d/%d database export(s) failed: %s", len(failed), len(databases), strings.Join(failed, ", "))
		d.raiseExportAlert(fmt.Sprintf(
			"%d of %d database export(s) failed — the offsite copy of %s is stale:\n%s",
			len(failed), len(databases), strings.Join(failed, ", "), strings.Join(failures, "\n")))
	} else {
		d.clearAlerts("every database export succeeded", alertKeyJSONLExport)
		// The failure is over, and the detail that stood for it with it: the
		// same failure returning later is a new alert, not a repeat of this
		// one (gt-ga9hx).
		d.jsonlExportAlert = ""
	}

	cycle.closeStep("export")

	// Phase D: Pollution firewall — filter test data from exports.
	removed := d.applyPollutionFilter(gitRepo, databases)
	if total := sumRemoved(removed); total > 0 {
		d.logger.Printf("jsonl_git_backup: filtered %d total test-pollution record(s)", total)
		// Take the filtered records out of the counts so spike detection
		// uses accurate numbers. A count we could not adjust leaves the
		// pre-filter totals, which the spike guard must not compare
		// against — halt rather than commit a count we know is wrong
		// (gt-q6ljl).
		if err := recountAfterFilter(counts, removed); err != nil {
			d.logger.Printf("jsonl_git_backup: HALTING — %v", err)
			d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", fmt.Sprintf("cannot recount exports after the pollution filter: %v — refusing to commit counts that were not verified", err))
			cycle.failStep("push", err.Error())
			return
		}
	}

	// Post-scrub verification: re-scan output for any remaining pollution.
	// Only the databases that exported this cycle are checked, since they are
	// the ones whose files the commit below carries: a failed database keeps the
	// copy already in the repo, which the commit phase unstages, and the export
	// alert already names it (gt-fhcwe).
	remaining, err := d.verifyNoPollution(gitRepo, exportedDBs)
	if err != nil {
		// An export this check could not read is an export nothing looked at.
		// Committing it would push whatever the unread shards hold and clear the
		// alert on a result that was never reached (gt-fhcwe).
		d.logger.Printf("jsonl_git_backup: HALTING — post-scrub verification failed: %v", err)
		d.escalateAlert(alertKeyJSONLScrub, "jsonl_git_backup", fmt.Sprintf("post-scrub verification could not read the export: %v — refusing to push a backup that was not checked for test pollution", err))
		cycle.failStep("verify", err.Error())
		return
	}
	if remaining > 0 {
		d.logger.Printf("jsonl_git_backup: WARNING: %d suspicious record(s) survived scrub+filter", remaining)
		d.escalateAlert(alertKeyJSONLScrub, "jsonl_git_backup", fmt.Sprintf("post-scrub verification found %d suspicious records — review JSONL exports", remaining))
	} else {
		d.clearAlerts("no suspicious records survived scrub", alertKeyJSONLScrub)
	}

	cycle.closeStep("verify")

	// Phase D: Spike detection — compare current counts against the rolling
	// baseline derived from recent backup commit history.
	threshold := spikeThreshold(config)
	spikes, baselineErr := d.verifyExportCounts(gitRepo, databases, counts, threshold)
	if baselineErr != nil {
		// History exists but yields no baseline. Fail loud (gt-tj-he):
		// committing would anchor an unchecked baseline, and silently
		// skipping would mask the hole forever. (A repo with no commits at all
		// is the bootstrap case, which verifyExportCounts returns as no error.)
		d.logger.Printf("jsonl_git_backup: HALTING — %v", baselineErr)
		msg := fmt.Sprintf("cannot compute spike baseline: %v — refusing to commit an unchecked export", baselineErr)
		if errors.Is(baselineErr, errNoSpikeBaseline) {
			// No automatic path out: the state is ambiguous by construction, so
			// name the manual recovery rather than leave the patrol halted with
			// no documented way back (gt-tj-he).
			msg += fmt.Sprintf("\n\n%s has commits, but none record per-database counts ('backup <ts> m%d: <db>=<N>'), so there is no previous level to compare against.\n\n%s\n\nOr start the history over by removing %s/.git — the next run then bootstraps normally.",
				gitRepo, countMetricVersion, spikeAcceptHint(gitRepo, databases, counts), gitRepo)
		}
		d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", msg)
		cycle.failStep("push", baselineErr.Error())
		return // Do NOT commit — no baseline to verify against.
	}
	if len(spikes) > 0 {
		report := formatSpikeReport(spikes)
		d.logger.Printf("jsonl_git_backup: HALTING — spike detected:\n%s", report)
		d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", report+"\n\n"+spikeAcceptHint(gitRepo, databases, counts))
		cycle.failStep("push", "spike detected")
		return // Do NOT commit — spike detected.
	}
	d.clearAlerts("no export spike", alertKeyJSONLSpike)

	// Commit and push if anything changed.
	// Include failed databases in commit message so staleness is visible.
	pushStatus := "ok"
	if err := d.commitAndPushJsonlBackup(gitRepo, databases, counts, failed); err != nil {
		d.logger.Printf("jsonl_git_backup: git operations failed: %v", err)
		pushStatus = "failed"
		cycle.failStep("push", err.Error())
		d.jsonlPushFailures++
		if d.jsonlPushFailures >= maxConsecutivePushFailures {
			d.logger.Printf("jsonl_git_backup: ESCALATION: %d consecutive push failures", d.jsonlPushFailures)
			msg := fmt.Sprintf("git push failed %d consecutive times: %v", d.jsonlPushFailures, err)
			if isPostBufferPushError(err.Error()) {
				msg += "\n\n" + postBufferHint(d.gitPackSizeSummary(gitRepo))
			}
			d.escalateAlert(alertKeyJSONLPush, "jsonl_git_backup", msg)
			// Reset to avoid flooding escalations every tick.
			d.jsonlPushFailures = 0
		}
	} else {
		d.jsonlPushFailures = 0
		cycle.closeStep("push")
		d.clearAlerts("backup push succeeded", alertKeyJSONLPush)
	}

	d.logger.Printf("jsonl_git_backup: exported %d/%d database(s), push=%s", exported, len(databases), pushStatus)
	cycle.closeStep("report")
}

// raiseExportAlert raises the backup's export alert for a partial export
// failure, unless this process already raised it for the same failure: gt
// escalate records a repeat on the open escalation bead, so a database that
// stays broken would spend a Dolt write and a subprocess every 15-minute tick
// (gt-vwry, the write-per-tick maintenance_gc_stale.go and patrol_scan both
// skip). A failure that changed — another database, another error — is raised
// onto that same bead, which is what refreshes its text, and a cycle where
// every database exported clears the alert and forgets the detail (gt-ga9hx).
//
// The detail is remembered only once the escalation landed, so an alert gt
// escalate dropped is raised again next cycle rather than recorded as
// delivered (gt-kox0).
func (d *Daemon) raiseExportAlert(detail string) {
	if d.jsonlExportAlert == detail {
		d.logger.Printf("jsonl_git_backup: the export alert is already raised for this failure — not raising it again")
		return
	}
	if err := d.escalateAlertErr(alertKeyJSONLExport, "jsonl_git_backup", detail); err != nil {
		d.logger.Printf("jsonl_git_backup: the export alert did not reach the town; raising it again next cycle: %v", err)
		return
	}
	d.jsonlExportAlert = detail
}

// supplementalTables lists non-issues tables to include in JSONL backup.
// These contain structural data (dependencies, labels, config) that would be
// lost if we only backed up the issues table. Wisp tables are excluded — they
// contain high-volume ephemeral data handled by the Reaper Dog.
var supplementalTables = []string{
	"comments",
	"config",
	"dependencies",
	"events",
	"labels",
	"metadata",
}

// exportDatabaseToJsonl exports the issues table (with optional scrub) and all
// supplemental tables to JSONL files in {gitRepo}/{db}/ directory.
//
// Issues go to {db}/issues.jsonl (scrubbed). Other tables go to {db}/{table}.jsonl.
// A table too large for one file continues in numbered shards (see
// jsonlShardMaxBytes); the returned count is the rows across all of them.
// Also writes a legacy {db}.jsonl (symlink to {db}/issues.jsonl) for backward compat.
//
// Returns the total number of records exported across all tables. When a
// supplemental table fails the others are still exported, but the call returns
// an error: the database counts as failed for the cycle.
func (d *Daemon) exportDatabaseToJsonl(db, gitRepo, dataDir string, scrub bool) (int, error) {
	if !validDBName.MatchString(db) {
		return 0, fmt.Errorf("invalid database name: %q", db)
	}

	// Create per-database subdirectory.
	dbDir := filepath.Join(gitRepo, db)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return 0, fmt.Errorf("creating dir %s: %w", dbDir, err)
	}

	total := 0

	exportTable := d.exportTableToJsonl
	if d.jsonlTableExportFn != nil {
		exportTable = d.jsonlTableExportFn
	}

	// 1. Export issues table (with scrub filter).
	n, err := exportTable("issues", backupIssuesQuery(db, scrub), dbDir, dataDir, jsonlExportTimeout)
	if err != nil {
		return 0, fmt.Errorf("issues: %w", err)
	}
	total += n

	// 2. Export supplemental tables (no scrub, full export). A failing table
	// does not stop the others, but its rows are missing from total, which must
	// not pass for a full export. The six share one budget per database: each
	// gets what is left of it, and once it is spent the rest are not attempted,
	// so a dolt that stops answering costs the cycle one bound (gt-4nqej).
	// Each table that is still to come keeps a floor of that budget back, so a
	// table that is slow but succeeds cannot leave the later ones unattempted
	// every cycle.
	var tableErrs []string
	suppBudget := d.jsonlSupplementalExportBudget()
	deadline := d.clk().Now().Add(suppBudget)
	floor := suppBudget / time.Duration(2*len(supplementalTables))
	for i, table := range supplementalTables {
		remaining := deadline.Sub(d.clk().Now())
		if remaining <= 0 {
			d.logger.Printf("jsonl_git_backup: %s/%s: not attempted — the %s supplemental export budget for the database is spent",
				db, table, suppBudget)
			tableErrs = append(tableErrs, fmt.Sprintf("%s: not attempted, the supplemental export budget was spent", table))
			continue
		}
		budget := remaining - floor*time.Duration(len(supplementalTables)-i-1)
		if budget < floor {
			budget = min(floor, remaining)
		}
		tn, err := exportTable(table, beadsql.BackupTable(db, table), dbDir, dataDir, budget)
		if err != nil {
			d.logger.Printf("jsonl_git_backup: %s/%s: export failed: %v", db, table, err)
			tableErrs = append(tableErrs, fmt.Sprintf("%s: %v", table, err))
			continue
		}
		total += tn
	}
	if len(tableErrs) > 0 {
		return 0, fmt.Errorf("supplemental table export failed (stale file kept): %s", strings.Join(tableErrs, "; "))
	}

	d.logger.Printf("jsonl_git_backup: %s: exported %d records across %d tables", db, total, 1+len(supplementalTables))
	return total, nil
}

// jsonlExportCommand builds the `dolt sql` command for one JSONL export.
//
// The password travels in DOLT_CLI_PASSWORD, never in argv: argv is world
// readable in `ps` on the shared host, so -p leaked the Dolt password to every
// local user (gt-q6ljl). This is the path internal/doltserver (doltserver.go)
// and the daemon's own dolt checks already use. An inherited DOLT_CLI_PASSWORD
// is stripped first so the one canonical value wins, as doltCommand does
// (dolt.go); duplicate keys would otherwise let a stale credential through.
func jsonlExportCommand(ctx context.Context, useServer bool, host string, port int, user, password, query, dataDir string) *exec.Cmd {
	var cmd *exec.Cmd
	if useServer {
		cmd = exec.CommandContext(ctx, "dolt",
			"--host", host,
			"--port", strconv.Itoa(port),
			"--no-tls",
			"-u", user,
			"sql", "-r", "json", "-q", query)
		cmd.Env = append(filterEnvKey(cmd.Environ(), "DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD="+password)
	} else {
		cmd = exec.CommandContext(ctx, "dolt", "sql", "-r", "json", "-q", query)
	}
	// Always set cmd.Dir to prevent stray .doltcfg/ creation (GH#2537).
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)
	return cmd
}

// jsonlSupplementalExportBudget is how long one database's supplemental tables
// may take together: jsonlSupplementalExportTimeout, or the bound a test set
// (jsonlSupplementalTimeout).
func (d *Daemon) jsonlSupplementalExportBudget() time.Duration {
	if d.jsonlSupplementalTimeout != 0 {
		return d.jsonlSupplementalTimeout
	}
	return jsonlSupplementalExportTimeout
}

// runJSONLExport runs cmd, which was built on ctx, and returns its stdout. A run
// ctx's deadline killed names the table and the timeout, because the bare
// "signal: killed" the process reports says neither.
func runJSONLExport(ctx context.Context, cmd *exec.Cmd, table string, timeout time.Duration) ([]byte, error) {
	return runJSONLExportWith(ctx, (*exec.Cmd).Run, cmd, table, timeout)
}

// runJSONLExportWith is runJSONLExport with the process run injected, so a test
// can fail a run without starting dolt.
func runJSONLExportWith(ctx context.Context, run func(*exec.Cmd) error, cmd *exec.Cmd, table string, timeout time.Duration) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := run(cmd); err != nil {
		return nil, jsonlExportFailure(ctx, err, stderr.String(), table, timeout)
	}
	return stdout.Bytes(), nil
}

// jsonlExportFailure shapes a failed export's error: the table and timeout when
// ctx's deadline killed it, and the process's stderr whenever it wrote any.
func jsonlExportFailure(ctx context.Context, err error, stderr, table string, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%s export killed after the %s timeout: %w", table, timeout, err)
	}
	if errMsg := strings.TrimSpace(stderr); errMsg != "" {
		return fmt.Errorf("%w: %s", err, errMsg)
	}
	return err
}

// exportTableToJsonl runs a query and writes the result as JSONL to {dir}/{table}.jsonl,
// continuing in {table}.2.jsonl, {table}.3.jsonl, ... when it outgrows one shard.
// Connects to the running Dolt server via --host/--port to get current committed data,
// falling back to embedded mode (cmd.Dir=dataDir) if no server config is available.
// budget is the whole time this table's export may run; the caller hands the
// issues table jsonlExportTimeout and each supplemental table what is left of
// its database's shared bound. Returns the number of records exported.
func (d *Daemon) exportTableToJsonl(table string, q beadsql.Query, dir, dataDir string, budget time.Duration) (int, error) {
	query, err := q.Inline()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()

	// Prefer querying the running server (accurate, up-to-date data) over embedded
	// mode (reads on-disk state which may lag behind server commits).
	// A managed server always has the town's endpoint (New drops a manager
	// for a town that names none).
	host := "127.0.0.1"
	port := 0
	user := "root"
	password := ""
	useServer := false
	if d.doltServer != nil && d.doltServer.IsEnabled() {
		if d.doltServer.config.Host != "" {
			host = d.doltServer.config.Host
		}
		port = d.doltServer.config.Port
		if d.doltServer.config.User != "" {
			user = d.doltServer.config.User
		}
		password = d.doltServer.config.Password
		useServer = true
	}

	cmd := jsonlExportCommand(ctx, useServer, host, port, user, password, query, dataDir)

	out, err := runJSONLExport(ctx, cmd, table, budget)
	if err != nil {
		return 0, err
	}

	var result struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return 0, fmt.Errorf("parsing dolt output: %w", err)
	}

	n, err := writeTableShards(dir, table, result.Rows, jsonlShardMaxBytes)
	if err != nil {
		return 0, err
	}
	d.logger.Printf("jsonl_git_backup: %s: exported %d rows in %s (timeout %s)", table, n, time.Since(start).Round(time.Millisecond), budget)
	return n, nil
}

// jsonlShardMaxBytes is where a table's export moves on to its next shard.
// GitHub refuses a push holding any file over 100 MB, and the 8 KB value cap on
// events rows bounds a row, not a table: hq/events.jsonl reached 100.08 MB on
// 2026-10-16 with every value capped, and the offsite backup stopped again. A
// table now exports as numbered shards cut here, so no file grows with the row
// count.
const jsonlShardMaxBytes = 50 * 1024 * 1024

// jsonlShardHardLimit is the size no exported file may reach. It sits under
// GitHub's 100 MB (read as 100 MiB or 100 000 000 bytes) with room for the
// one row that crosses jsonlShardMaxBytes.
const jsonlShardHardLimit = 90 * 1024 * 1024

// jsonlPushFileLimitBytes is the per-file size the backup's remote refuses a
// push for. GitHub checks it against the objects the push SENDS — the whole
// unpushed range — not against the tree being committed, which is why a
// sharded export can still be unpushable (gt-q30zx). A test lowers the bound
// through Daemon.jsonlPushFileLimit rather than writing a real 100 MB export.
const jsonlPushFileLimitBytes = 100 * 1024 * 1024

// jsonlBackupAuthor authors every backup commit, the replacing one included
// (see recoverOversizeBackupPush).
const jsonlBackupAuthor = "Gas Town Daemon <daemon@gastown.local>"

// jsonlShardName names shard n (counting from 1) of a table's export. The first
// shard keeps the unsharded name, so a table that fits one file reads exactly as
// it did before sharding.
func jsonlShardName(table string, n int) string {
	if n <= 1 {
		return table + ".jsonl"
	}
	return fmt.Sprintf("%s.%d.jsonl", table, n)
}

// jsonlShardPaths lists the files of a table's export in shard order:
// {table}.jsonl, then {table}.2.jsonl, {table}.3.jsonl, ... A table with no
// export in dir has no paths; a dir that cannot be read is an error.
func jsonlShardPaths(dir, table string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byShard := make(map[int]string)
	for _, e := range entries {
		name := e.Name()
		if name == jsonlShardName(table, 1) {
			byShard[1] = filepath.Join(dir, name)
			continue
		}
		rest, ok := strings.CutPrefix(name, table+".")
		if !ok {
			continue
		}
		num, ok := strings.CutSuffix(rest, ".jsonl")
		if !ok {
			continue
		}
		// Round-trip through the shard name so "events.02.jsonl" and
		// "events.x.jsonl" are not taken for shards.
		if n, err := strconv.Atoi(num); err == nil && n >= 2 && name == jsonlShardName(table, n) {
			byShard[n] = filepath.Join(dir, name)
		}
	}
	nums := make([]int, 0, len(byShard))
	for n := range byShard {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	paths := make([]string, 0, len(nums))
	for _, n := range nums {
		paths = append(paths, byShard[n])
	}
	return paths, nil
}

// jsonlShardOps is the filesystem work a shard swap does. Production passes
// the os package; a test that drives the rollback of a failing call passes its
// own, since neither a rename nor a remove that fails part way through the swap
// can be arranged on a directory that accepts all of them (gt-fhcwe).
type jsonlShardOps struct {
	rename func(oldpath, newpath string) error
	remove func(path string) error
	// removeAll takes a temp whatever shape it is in. Remove refuses a
	// directory, and a write that failed on a path a directory sits at is one
	// of the ways a temp survives (gt-1911b).
	removeAll func(path string) error
	verify    func(dir, table string, wantRows int, maxFileBytes int64) error
}

// osShardOps is the swap's real filesystem work.
func osShardOps() jsonlShardOps {
	return jsonlShardOps{rename: os.Rename, remove: os.Remove, removeAll: os.RemoveAll, verify: verifyJsonlShards}
}

// writeTableShards writes rows as the table's JSONL export in dir, cut into
// shards of about maxBytes (a single row larger than that gets a shard of its
// own), and returns the number of rows written.
//
// The export in dir is replaced whole or not at all: the shards already there
// are moved aside, the new ones are published, and a failure at any point puts
// the old export back, so the directory never holds shards from both
// (gt-fhcwe). A shard over jsonlShardHardLimit is refused while the shards are
// still in memory, and the export on disk is verified, before either happens.
func writeTableShards(dir, table string, rows []json.RawMessage, maxBytes int) (int, error) {
	return writeTableShardsWith(osShardOps(), dir, table, rows, maxBytes)
}

func writeTableShardsWith(ops jsonlShardOps, dir, table string, rows []json.RawMessage, maxBytes int) (int, error) {
	var shards []*bytes.Buffer
	cur := &bytes.Buffer{}
	for _, row := range rows {
		if table == "events" {
			capped, err := capEventRowValues(row, eventValueBackupCap)
			if err != nil {
				return 0, fmt.Errorf("capping events row: %w", err)
			}
			row = capped
		}
		var line bytes.Buffer
		if err := json.Compact(&line, row); err != nil {
			return 0, fmt.Errorf("compacting JSON row: %w", err)
		}
		line.WriteByte('\n')
		if cur.Len() > 0 && cur.Len()+line.Len() > maxBytes {
			shards = append(shards, cur)
			cur = &bytes.Buffer{}
		}
		cur.Write(line.Bytes())
	}
	// An empty table still writes its (empty) first shard, as it always has.
	shards = append(shards, cur)

	// Screen the sizes while the shards are still only in memory: written first
	// and rejected after, an oversized shard is committed with the rest of the
	// cycle's work.
	if err := checkShardSizes(table, shards, jsonlShardHardLimit); err != nil {
		return 0, err
	}

	tmpPaths := make([]string, len(shards))
	// Best effort: a temp that will not go is a .tmp nothing else reads, and
	// the failure being reported is the one to report.
	removeTmps := func() {
		for _, p := range tmpPaths {
			if p != "" {
				_ = ops.removeAll(p)
			}
		}
	}
	for i, shard := range shards {
		tmpPath := filepath.Join(dir, jsonlShardName(table, i+1)+".tmp")
		// Registered before the write, so a write that fails part way leaves
		// the temp it made to removeTmps.
		tmpPaths[i] = tmpPath
		if err := os.WriteFile(tmpPath, shard.Bytes(), 0644); err != nil {
			removeTmps()
			return 0, fmt.Errorf("writing %s: %w", tmpPath, err)
		}
	}

	// The export is rewritten whole every cycle, so a .bak here outlived the
	// swap it belongs to — a crash cut it short — and holds nothing to restore.
	if err := removeShardBackups(dir, table); err != nil {
		removeTmps()
		return 0, fmt.Errorf("clearing %s shard backups: %w", table, err)
	}
	existing, err := jsonlShardPaths(dir, table)
	if err != nil {
		removeTmps()
		return 0, fmt.Errorf("listing %s shards: %w", table, err)
	}
	setAside := make([]string, len(existing)) // setAside[i] is where shard i went
	restore := func() error {
		var firstErr error
		for i, path := range existing {
			if setAside[i] == "" {
				continue
			}
			if err := ops.rename(setAside[i], path); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("restoring %s: %w", filepath.Base(path), err)
			}
		}
		return firstErr
	}
	// withdraw takes the shards it just published back out and then puts the
	// export that was here back, so a half-done swap leaves one generation of
	// the export rather than a mix of two. A shard that was never published is
	// not an error to withdraw.
	withdraw := func() error {
		var firstErr error
		for i := range tmpPaths {
			path := filepath.Join(dir, jsonlShardName(table, i+1))
			if err := ops.remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
				firstErr = fmt.Errorf("withdrawing %s: %w", filepath.Base(path), err)
			}
		}
		return errors.Join(firstErr, restore())
	}
	for i, path := range existing {
		setAside[i] = path + ".bak"
		if err := ops.rename(path, setAside[i]); err != nil {
			setAside[i] = ""
			// The rollback can fail too, and then the export is left with a
			// gap in it, so both failures are reported.
			err = errors.Join(err, restore())
			removeTmps()
			return 0, fmt.Errorf("setting %s aside: %w", filepath.Base(path), err)
		}
	}

	for i, tmpPath := range tmpPaths {
		if err := ops.rename(tmpPath, filepath.Join(dir, jsonlShardName(table, i+1))); err != nil {
			err = errors.Join(err, withdraw())
			removeTmps()
			return 0, fmt.Errorf("renaming %s: %w", tmpPath, err)
		}
		tmpPaths[i] = ""
	}

	if err := ops.verify(dir, table, len(rows), jsonlShardHardLimit); err != nil {
		// The export on disk is not the one that was asked for, so it does not
		// replace the last one that verified.
		return 0, errors.Join(err, withdraw())
	}

	// A temp an earlier cycle left is named after a shard this export may no
	// longer write, so the swap stepped over it. It is the same stale tail as
	// the set-aside shards below, and `git add -A` over the directory would
	// commit it, so a temp that will not go withdraws the new export rather
	// than leave it beside a file the commit carries (gt-1911b).
	//
	// The sweep runs before the set-aside removal: the withdrawal puts those
	// files back, so they have to still be there.
	if err := removeShardTemps(ops.removeAll, dir, table); err != nil {
		return 0, errors.Join(fmt.Errorf("clearing %s temp files: %w", table, err), withdraw())
	}

	// The set-aside files are the stale tail of an earlier, longer export, and
	// nothing else now that the new export has verified. Until they are gone
	// the directory holds two generations of the export, so one that will not go
	// withdraws the new export instead of leaving the pair to be committed
	// (gt-fhcwe).
	var residue []error
	for _, bak := range setAside {
		if err := ops.remove(bak); err != nil {
			residue = append(residue, err)
		}
	}
	if len(residue) > 0 {
		errs := append([]error{fmt.Errorf("removing the stale %s shards", table)}, residue...)
		return 0, errors.Join(append(errs, withdraw())...)
	}
	return len(rows), nil
}

// checkShardSizes returns an error naming the first shard over hardLimit, so a
// shard too big to push is refused while the export is still only in memory
// (gt-fhcwe).
func checkShardSizes(table string, shards []*bytes.Buffer, hardLimit int64) error {
	for i, shard := range shards {
		if int64(shard.Len()) > hardLimit {
			return fmt.Errorf("writing %s: shard %d is %d bytes, over the %d byte limit GitHub would refuse", table, i+1, shard.Len(), hardLimit)
		}
	}
	return nil
}

// removeShardBackups deletes the {table}.jsonl.bak and {table}.N.jsonl.bak
// files in dir. They are what a shard swap leaves behind when a crash cuts it
// short, and the next cycle rewrites the whole export, so they are stale
// duplicates that would otherwise ride into the commit.
func removeShardBackups(dir, table string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl.bak") || !strings.HasPrefix(name, table+".") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// removeShardTemps deletes the {table}.jsonl.tmp and {table}.N.jsonl.tmp files
// in dir. A write that failed and a cycle killed part way through one both
// leave them, and jsonlShardPaths does not read a .tmp as a shard, so the next
// export's swap steps over them and `git add -A` commits them with it
// (gt-1911b). The path goes whatever sits at it, since nothing else writes
// under a temp's name.
//
// remove is the swap's own removal (jsonlShardOps.removeAll), so a test can
// drive the withdrawal a temp that will not go triggers.
func removeShardTemps(remove func(path string) error, dir, table string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl.tmp") || !strings.HasPrefix(name, table+".") {
			continue
		}
		if err := remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// verifyJsonlShards checks a table's export on disk: the shards hold wantRows
// lines between them and none is over maxFileBytes.
func verifyJsonlShards(dir, table string, wantRows int, maxFileBytes int64) error {
	paths, err := jsonlShardPaths(dir, table)
	if err != nil {
		return fmt.Errorf("verifying %s export: %w", table, err)
	}
	got := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("verifying %s export: %w", table, err)
		}
		if info.Size() > maxFileBytes {
			return fmt.Errorf("verifying %s export: %s is %d bytes, over the %d byte limit GitHub would refuse", table, filepath.Base(path), info.Size(), maxFileBytes)
		}
		n, err := countFileLines(path)
		if err != nil {
			return fmt.Errorf("verifying %s export: %w", table, err)
		}
		got += n
	}
	if got != wantRows {
		return fmt.Errorf("verifying %s export: %d shard(s) hold %d rows, exported %d", table, len(paths), got, wantRows)
	}
	return nil
}

// eventValueBackupCap bounds the old_value and new_value of each events row in
// the offsite backup. An "updated" event stores the whole prior issue and the
// whole changed field, so every edit of a bead with long notes writes two copies
// of those notes: on 2026-09-23 two such beads put 102 MB of hq/events.jsonl
// into the backup and GitHub refused the push (100 MB per-file limit), which
// stopped the offsite backup for every database. The issues themselves are
// exported whole in issues.jsonl; only this edit history is shortened, and
// Dolt keeps the full values.
const eventValueBackupCap = 8 * 1024

// capEventRowValues returns row with old_value and new_value cut to at most
// limit bytes plus a marker naming how much was dropped. Other fields, and
// values already within the limit, pass through unchanged.
func capEventRowValues(row json.RawMessage, limit int) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil {
		return nil, err
	}
	changed := false
	for _, key := range []string{"old_value", "new_value"} {
		raw, ok := fields[key]
		if !ok || len(raw) <= limit {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || len(value) <= limit {
			continue // not a string, or its escaping alone crossed the limit
		}
		cut := limit
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = value[:cut] + fmt.Sprintf("...[truncated %d bytes in backup; full value in Dolt]", len(value)-cut)
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
		changed = true
	}
	if !changed {
		return row, nil
	}
	return json.Marshal(fields)
}

// commitAndPushJsonlBackup stages, commits, and pushes JSONL files. The commit
// message includes counts for successful exports AND names of failed databases,
// so partial failures are visible in git history.
//
// A tick with nothing to commit still pushes (see pushJsonlBackup), so an
// earlier tick's commit that failed to reach origin is retried rather than
// reported as success (gt-fxm2i).
func (d *Daemon) commitAndPushJsonlBackup(gitRepo string, databases []string, counts map[string]int, failed []string) error {
	// The local commands below share one deadline, so a single slow one can use
	// the whole phase without the rest stacking past a tick.
	localDeadline := time.Now().Add(d.jsonlLocalPhaseBudget())
	remaining := func() time.Duration { return time.Until(localDeadline) }

	// Stage all JSONL files (flat legacy files + subdirectory structure).
	// Use "." instead of "*/" to correctly handle initially-untracked subdirectories.
	if err := d.stageGit(gitRepo, "git add -A .", remaining, func(g daemonGit) error {
		return g.Add("-A", ".")
	}); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// A failed database never reached the spike guard, and verifyExportCounts
	// skips a database without a count, so its freshly written issues.jsonl
	// (the issues table is written before a supplemental table fails) is
	// unchecked and must not be committed. Unstage the whole database: its
	// committed files stay as the last verified copy.
	if len(failed) > 0 {
		var paths []string
		for _, db := range failed {
			paths = append(paths, db, db+".jsonl")
		}
		if err := d.stageGit(gitRepo, "git reset (failed databases)", remaining, func(g daemonGit) error {
			return g.ResetFiles(paths...)
		}); err != nil {
			return fmt.Errorf("unstaging failed databases: %w", err)
		}
	}

	// Check if there are staged changes. A failed check is not "no
	// changes": the commit below decides.
	var staged []gtgit.StagedChange
	hasStaged := true
	if err := d.stageGit(gitRepo, "git diff --cached", remaining, func(g daemonGit) error {
		var err error
		staged, err = g.StagedChanges()
		return err
	}); err == nil && len(staged) == 0 {
		hasStaged = false
	}

	// Build commit message with counts in deterministic order.
	timestamp := time.Now().Format("2006-01-02 15:04")
	msg := jsonlBackupSubject(timestamp, jsonlCountParts(databases, counts))
	if len(failed) > 0 {
		sort.Strings(failed)
		msg += fmt.Sprintf(" [FAILED: %s]", strings.Join(failed, ", "))
	}

	if hasStaged {
		// Commit.
		if err := d.stageGit(gitRepo, "git commit", remaining, func(g daemonGit) error {
			return g.CommitWithAuthor(msg, jsonlBackupAuthor)
		}); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
	} else {
		d.logger.Printf("jsonl_git_backup: no changes to commit")
	}

	// The rolling spike baseline is re-derived from commit history (the subject
	// lines recorded above) on every run, so nothing to clean up here. The
	// in-repo cache (.spike-baseline-cache.json) is git-ignored and left in
	// place: recomputeSpikeBaseline consults it only when history carries no
	// counts at all, so it can never outrank a committed level (gt-tj-he).

	return d.pushJsonlBackup(gitRepo, hasStaged, msg)
}

// pushJsonlBackup pushes the backup repository's current branch to origin,
// returning nil once origin carries HEAD.
//
// It runs whether or not this tick committed: a push-less tick still pushes an
// earlier tick's unpushed commit, so a failed push is retried rather than
// reported as success (gt-fxm2i).
func (d *Daemon) pushJsonlBackup(gitRepo string, committed bool, msg string) error {
	// Push requires a remote. This is the OFFSITE layer — a repo with commits
	// but no remote is data sitting on the same disk it started on, which is
	// exactly the failure this patrol exists to prevent. Treat it as a hard
	// error (not a graceful skip) so it surfaces through the same
	// consecutive-failure escalation as a real push failure, instead of
	// silently "succeeding" forever (gt-kme).
	local := d.backupGitAt(gitRepo, gitCmdTimeout)
	if _, err := local.RemoteURL("origin"); err != nil {
		return fmt.Errorf("no 'origin' remote configured — offsite backup cannot push")
	}

	// Detect current branch name for push (master vs main).
	branch, err := local.CurrentBranch()
	if err != nil || branch == "" {
		branch = "main" // fallback
	}

	// With no commit this tick, push only if origin does not already carry HEAD.
	if !committed {
		if !d.jsonlBackupAheadOf(gitRepo, "origin", branch) {
			d.logger.Printf("jsonl_git_backup: no changes to commit and origin/%s already has HEAD", branch)
			return nil
		}
		d.logger.Printf("jsonl_git_backup: no new changes — retrying push of an unpushed commit to origin/%s", branch)
	}

	if err := d.backupGitAt(gitRepo, gitCmdTimeout).PushWithTimeout("origin", branch, false, gitPushTimeout); err != nil {
		if !isOversizeFilePushError(err.Error()) {
			return fmt.Errorf("git push: %w", err)
		}
		// The remote weighed what the push sends and refused it for a file
		// over its limit. Re-exporting cannot clear that: the file is in the
		// unpushed history, not in the tree (gt-q30zx).
		if rerr := d.recoverOversizeBackupPush(gitRepo, branch, msg); rerr != nil {
			return fmt.Errorf("git push: %w; replacing the unpushed history: %v", err, rerr)
		}
		return nil
	}
	if committed {
		d.logger.Printf("jsonl_git_backup: committed and pushed: %s", msg)
	} else {
		d.logger.Printf("jsonl_git_backup: pushed previously unpushed commit(s) to origin/%s", branch)
	}
	return nil
}

// isOversizeFilePushError reports whether a git push error is the remote
// refusing the push for a file over its per-file limit. A string classifier
// (no exec), cheap enough for the error path. Each alternative is a size
// refusal in full: a lone fragment of one must not arm recovery, which drops
// commits.
func isOversizeFilePushError(errMsg string) bool {
	return strings.Contains(errMsg, "exceeds GitHub's file size limit") ||
		(strings.Contains(errMsg, "GH001") && strings.Contains(errMsg, "Large files detected"))
}

// pushFileLimitBytes is the per-file size this daemon's backup remote refuses a
// push for — a file of exactly this many bytes is one it takes. GitHub's
// shipped limit, or the smaller one a test set (jsonlPushFileLimit).
func (d *Daemon) pushFileLimitBytes() int64 {
	if d.jsonlPushFileLimit > 0 {
		return d.jsonlPushFileLimit
	}
	return jsonlPushFileLimitBytes
}

// recoverOversizeBackupPush replaces the backup repository's unpushed range
// with one commit parented on origin's tip and pushes it, so a push refused
// for a blob an unpushed commit still carries can succeed (gt-q30zx). It
// declines rather than drop a range this repository cannot account for,
// leaving HEAD and the remote untouched for the caller to report.
func (d *Daemon) recoverOversizeBackupPush(gitRepo, branch, msg string) error {
	limit := d.pushFileLimitBytes()
	g := d.backupGitAt(gitRepo, gitCmdTimeout)

	// A remote weighs the objects a push SENDS, not the tree being committed,
	// so a blob only an unpushed commit carries refuses every later push. The
	// range is replaced, not rewritten: origin's tip becomes the new commit's
	// parent, so the push that follows is a fast-forward and every commit
	// origin already has stays untouched.

	// The index is what the replacing commit will carry, and commitAndPush has
	// just committed it (or found nothing staged), so it is HEAD's tree.
	sizes, err := g.TreeFileSizes("HEAD")
	if err != nil {
		return fmt.Errorf("weighing HEAD's tree: %w", err)
	}
	if over := overLimitFiles(sizes, limit); len(over) > 0 {
		return fmt.Errorf("the export itself still holds a file over the %d-byte limit (%s) — shard the table (gt-4btp4); the unpushed history is not the problem", limit, strings.Join(over, ", "))
	}

	originTip, err := g.Rev("refs/remotes/origin/" + branch)
	if err != nil {
		return fmt.Errorf("origin/%s's tip is not known locally (%v) — cannot tell which commits are unpushed", branch, err)
	}
	head, err := g.Rev("HEAD")
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	if head == originTip {
		return fmt.Errorf("origin/%s already carries HEAD; this refusal is not about the unpushed range", branch)
	}
	contained, err := g.IsAncestor(originTip, "HEAD")
	if err != nil {
		return fmt.Errorf("checking whether origin/%s's tip is an ancestor of HEAD: %w", branch, err)
	}
	if !contained {
		return fmt.Errorf("origin/%s holds commits HEAD does not (gt-63sz) — refusing to replace a range this repository cannot account for", branch)
	}

	var staged int
	if err := d.runGitIndexOp(gitRepo, "git reset --soft (oversize recovery)", func() error {
		if err := g.ResetSoft(originTip); err != nil {
			return err
		}
		changes, err := g.StagedChanges()
		if err != nil {
			return err
		}
		staged = len(changes)
		if staged == 0 {
			// The unpushed commits held nothing the export does not: origin
			// already carries HEAD's content, so there is nothing to push.
			return nil
		}
		// The replacing commit reuses this tick's subject, counts and metric
		// version included: the spike baseline is read back out of those lines
		// (gt-26qqh).
		return g.CommitWithAuthor(msg, jsonlBackupAuthor)
	}); err != nil {
		return fmt.Errorf("replacing the unpushed range with a commit on %s: %w", originTip, err)
	}
	if staged == 0 {
		d.logger.Printf("jsonl_git_backup: dropped unpushed commit(s) over an over-limit file; origin/%s already carries the export", branch)
		return nil
	}

	if err := g.PushWithTimeout("origin", branch, false, gitPushTimeout); err != nil {
		return fmt.Errorf("git push after replacing the unpushed range: %w", err)
	}
	d.logger.Printf("jsonl_git_backup: replaced the unpushed commits with one on origin/%s's tip — an over-limit file survived in their history", branch)
	return nil
}

// overLimitFiles lists the files of a tree whose blob is over limit bytes,
// each with its size, in path order so a tick's diagnosis names the same files
// the tick before it did. The boundary is the remote's own: a file of exactly
// limit bytes is one a push carries.
func overLimitFiles(sizes map[string]int64, limit int64) []string {
	paths := make([]string, 0, len(sizes))
	for path, size := range sizes {
		if size > limit {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	over := make([]string, 0, len(paths))
	for _, path := range paths {
		over = append(over, fmt.Sprintf("%s (%d bytes)", path, sizes[path]))
	}
	return over
}

// jsonlBackupAheadOf reports whether the backup repository's HEAD holds commits
// origin/<branch> does not — i.e. a push is needed. It reads the local
// remote-tracking ref, so it needs no network.
//
// Any ref it cannot resolve (never fetched, no tracking ref, or a failed read)
// counts as ahead: attempting a push is cheaper than skipping a commit that may
// never have reached the remote, and a push that cannot succeed reports its own
// error.
func (d *Daemon) jsonlBackupAheadOf(gitRepo, remote, branch string) bool {
	g := d.backupGitAt(gitRepo, gitCmdTimeout)
	head, err := g.Rev("HEAD")
	if err != nil {
		return true
	}
	tip, err := g.Rev("refs/remotes/" + remote + "/" + branch)
	if err != nil {
		return true
	}
	if tip == head {
		return false
	}
	// HEAD is already on origin when it is an ancestor of origin's tip.
	contained, err := g.IsAncestor(head, tip)
	if err != nil {
		return true
	}
	return !contained
}

// ensureGitRepoInitialized makes sure gitRepo exists and is a git repository,
// initializing it (mkdir + git init) on first use instead of silently
// skipping forever when the directory doesn't exist yet. A remote is NOT
// configured here — that still requires an explicit `git remote add origin`
// — but at least the local half stops being permanently inert (gt-kme).
func (d *Daemon) ensureGitRepoInitialized(gitRepo string) error {
	g := d.backupGitAt(gitRepo, gitCmdTimeout)
	if _, err := os.Stat(filepath.Join(gitRepo, ".git")); err != nil {
		if err := os.MkdirAll(gitRepo, 0755); err != nil {
			return fmt.Errorf("creating %s: %w", gitRepo, err)
		}
		if err := g.InitRepo("main"); err != nil {
			return fmt.Errorf("git init: %w", err)
		}
	}

	// Set unconditionally (idempotent) rather than only on fresh init, so a
	// pre-existing repo created before this fix — or one where the config was
	// lost — also gets covered on the next daemon start (gt-kxa1).
	if err := g.ConfigSet("http.postBuffer", gitPostBufferBytes); err != nil {
		return fmt.Errorf("git config http.postBuffer: %w", err)
	}
	return nil
}

// isPostBufferPushError reports whether a git push error looks like GitHub
// rejecting an oversized pack sent as a chunked HTTP POST — the classic
// "error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400"
// failure that occurs when http.postBuffer is left at git's 1 MB default
// (gt-kxa1). It's a plain string classifier (no exec) so it stays cheap to
// call from a hot error path and easy to table-test.
func isPostBufferPushError(errMsg string) bool {
	return strings.Contains(errMsg, "HTTP 400") || strings.Contains(errMsg, "curl 22")
}

// postBufferHint explains the postBuffer failure and names the local pack
// size (when known) so an escalation reads as a diagnosis, not just a
// symptom. packSize is typically the output of gitPackSizeSummary, which
// returns "" when it couldn't be determined — the hint still reads fine
// without it.
func postBufferHint(packSize string) string {
	hint := fmt.Sprintf("cause: pack exceeds git's http.postBuffer default (1 MB); "+
		"http.postBuffer is now set to %s bytes at repo init (gt-kxa1) — if this repo "+
		"predates that fix, run: git config http.postBuffer %s", gitPostBufferBytes, gitPostBufferBytes)
	if packSize != "" {
		hint += fmt.Sprintf(" (local pack size: %s)", packSize)
	}
	return hint
}

// gitPackSizeSummary returns the repo's local pack size as reported by
// `git count-objects -v` (its "size-pack" line, in KiB), for inclusion in
// postBuffer escalation hints. Returns "" on any failure — the hint is still
// useful without this detail, so callers don't need to handle an error.
func (d *Daemon) gitPackSizeSummary(gitRepo string) string {
	size, err := d.backupGitAt(gitRepo, gitCmdTimeout).PackSize()
	if err != nil || size == "" {
		return ""
	}
	return size + " KiB"
}

// discoverJsonlBackupDatabases lists production database directories under
// dataDir, for use when JsonlGitBackupConfig.Databases is empty. It skips
// test/scratch DB prefixes (internal/testdb) and requires a real `.dolt` subdirectory so stray non-database
// entries in dataDir (config files, dropped-database housekeeping dirs) are
// never mistaken for a production database.
func discoverJsonlBackupDatabases(dataDir string) []string {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if testdb.IsTestDatabaseName(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dataDir, name, ".dolt")); err != nil {
			continue // not a dolt database directory
		}
		databases = append(databases, name)
	}
	sort.Strings(databases)
	return databases
}

// gitIdentityEnv returns the HOME/USER/LOGNAME entries missing from the
// daemon's environment, recovered from the user database. Daemon-launched git
// falls back to getpwuid(uid) for committer/author identity if $USER and
// $LOGNAME are absent; on macOS that lookup can fail with "No user exists
// for uid N" once the long-lived daemon's connection to opendirectoryd is no
// longer reachable. Adding these to the backup's git calls lets git use them
// directly and skip the system passwd lookup. See gh#zt1w.
func gitIdentityEnv() []string {
	return gitIdentityEnvFrom(os.Environ(), user.Current)
}

// gitIdentityEnvFrom is gitIdentityEnv over env, recovering missing identity
// from current. It adds nothing when env has all three, or when current
// fails (git then errors as it would have).
func gitIdentityEnvFrom(env []string, current func() (*user.User, error)) []string {
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			have[kv[:eq]] = true
		}
	}
	if have["HOME"] && have["USER"] && have["LOGNAME"] {
		return nil
	}
	u, err := current()
	if err != nil {
		return nil
	}
	var add []string
	if !have["HOME"] && u.HomeDir != "" {
		add = append(add, "HOME="+u.HomeDir)
	}
	if !have["USER"] && u.Username != "" {
		add = append(add, "USER="+u.Username)
	}
	if !have["LOGNAME"] && u.Username != "" {
		add = append(add, "LOGNAME="+u.Username)
	}
	return add
}

// backupGitAt opens the backup repository at dir: openGitFn's when a test
// set one, else a *git.Git whose every call carries the recovered identity
// (gitIdentityEnv) and is killed after timeout.
func (d *Daemon) backupGitAt(dir string, timeout time.Duration) daemonGit {
	if d.openGitFn != nil {
		return d.openGitFn(dir)
	}
	return gtgit.NewGit(dir).WithEnv(gitIdentityEnv()).WithTimeout(timeout)
}

// jsonlLocalPhaseBudget is how long one backup tick's local staging half may
// take: gitLocalPhaseTimeout, or the test's override.
func (d *Daemon) jsonlLocalPhaseBudget() time.Duration {
	if d.jsonlLocalPhaseTimeout != 0 {
		return d.jsonlLocalPhaseTimeout
	}
	return gitLocalPhaseTimeout
}

// stageGit runs one local command of the backup phase, bounding it by the time
// remaining before the phase deadline and refusing it once that is spent: the
// git runner sets a context timeout only above zero (internal/git/exec.go), so
// a spent budget would run the command with no deadline at all, and a `git
// commit` that hangs holds jsonlGitBackupRunning — and every later trigger with
// it (gt-ttqq8). remaining and the deadline it counts down to are read for each
// attempt, so the retry after an index-lock race gets the time left then rather
// than the time the first attempt started with.
func (d *Daemon) stageGit(gitRepo, desc string, remaining func() time.Duration, op func(g daemonGit) error) error {
	return d.runGitIndexOp(gitRepo, desc, func() error {
		budget := remaining()
		if budget <= 0 {
			return fmt.Errorf("%s: the local phase budget is spent; refusing a git command that would run with no deadline", desc)
		}
		return op(d.backupGitAt(gitRepo, budget))
	})
}

// runGitIndexOp runs op, a git command that reads or writes gitRepo's index,
// steering around the index lock a previous tick may have left behind. A tick
// whose `git add` is killed on its deadline cannot release that lock, and
// without this every later tick fails "index.lock: File exists" until a human
// removes it (gt-1aj2). desc names the command in the log.
func (d *Daemon) runGitIndexOp(gitRepo, desc string, op func() error) error {
	// Two attempts at most: the first may find a lock orphaned by an earlier
	// tick, the second runs only after this clears that lock.
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			d.logger.Printf("jsonl_git_backup: cleared stale %s before %s", gitIndexLockPath(gitRepo), desc)
		}

		started := time.Now()
		err = op()
		if err == nil {
			return nil
		}

		// A killed command leaves its own lock behind; clear it now so one slow
		// tick cannot fail the next two and escalate.
		if errors.Is(err, gtgit.ErrTimedOut) {
			if d.clearIndexLockModifiedSince(gitRepo, started) {
				d.logger.Printf("jsonl_git_backup: cleared %s orphaned by timed-out %s", gitIndexLockPath(gitRepo), desc)
			}
			return err
		}

		// The lock may have appeared between the check above and git's attempt
		// at it. Retry once: the next pass re-reads the lock's age, so a lock
		// with a live owner is left alone and this error stands.
		if attempt == 0 && isIndexLockExistsError(err) {
			continue
		}
		return err
	}
	return err
}

// gitIndexLockPath returns the path of gitRepo's index lock. The backup repo is
// always a plain repository — ensureGitRepoInitialized creates it — so .git is a
// directory and never the indirection file a worktree or submodule uses.
func gitIndexLockPath(gitRepo string) string {
	return filepath.Join(gitRepo, ".git", "index.lock")
}

// clearStaleIndexLock removes the index lock when it has sat untouched for at
// least grace, reporting whether it removed one. A younger lock is assumed to
// have a live owner and is left in place.
func (d *Daemon) clearStaleIndexLock(gitRepo string, grace time.Duration) bool {
	lockPath := gitIndexLockPath(gitRepo)
	info, err := os.Stat(lockPath)
	if err != nil || time.Since(info.ModTime()) < grace {
		return false
	}
	return d.removeIndexLock(lockPath)
}

// clearIndexLockModifiedSince removes the index lock when it was written at or
// after since — the shape of a lock a git process we just killed left behind.
// The one-second slack absorbs filesystems whose mtime is coarser than Go's.
func (d *Daemon) clearIndexLockModifiedSince(gitRepo string, since time.Time) bool {
	lockPath := gitIndexLockPath(gitRepo)
	info, err := os.Stat(lockPath)
	if err != nil || info.ModTime().Before(since.Add(-time.Second)) {
		return false
	}
	return d.removeIndexLock(lockPath)
}

// removeIndexLock deletes lockPath, logging an unremovable lock rather than
// letting the failure read as "no lock was there".
func (d *Daemon) removeIndexLock(lockPath string) bool {
	if err := os.Remove(lockPath); err != nil {
		d.logger.Printf("jsonl_git_backup: could not remove %s: %v", lockPath, err)
		return false
	}
	return true
}

// isIndexLockExistsError reports whether git refused to run because the index
// lock exists.
func isIndexLockExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "index.lock") {
		return false
	}
	return strings.Contains(msg, "File exists") || strings.Contains(msg, "Another git process")
}

// maxEscalationTitleLen bounds the escalation title's length. bd 1.0.3+
// rejects newline-containing flag values (see beads_escalation.go), so a
// multiline message (e.g. full go-test output) must never reach --title
// verbatim — only its first line, truncated, is used for the title. The
// full message is still delivered via --stdin/--reason.
const maxEscalationTitleLen = 200

// escalationTitle builds a single-line "source: summary" title safe to pass
// as `bd create --title=...`, collapsing a possibly-multiline message down
// to its first line. A line ends at the first "\r" or "\n", the same
// characters notify.Escalation.Validate refuses in a description.
func escalationTitle(source, message string) string {
	summary := message
	if idx := strings.IndexAny(summary, "\r\n"); idx >= 0 {
		summary = summary[:idx]
	}
	summary = strings.TrimSpace(summary)
	title := fmt.Sprintf("%s: %s", source, summary)
	if runes := []rune(title); len(runes) > maxEscalationTitleLen {
		title = string(runes[:maxEscalationTitleLen-1]) + "…"
	}
	return title
}

// Alert keys for the daemon's patrol readers.
//
// Each is the stable identity of one recurring condition, not of one firing.
// `gt escalate` records a repeat of a key onto the bead that already represents
// it, and clearAlerts closes the key when the condition goes away, so a
// condition that persists across many patrol cycles leaves exactly one open
// escalation behind (gt-vwry) instead of one per cycle.
const (
	alertKeyJSONLInit   = "jsonl_git_backup:init"
	alertKeyJSONLNoDBs  = "jsonl_git_backup:no-databases"
	alertKeyJSONLExport = "jsonl_git_backup:export"
	alertKeyJSONLScrub  = "jsonl_git_backup:scrub-suspicious"
	alertKeyJSONLSpike  = "jsonl_git_backup:spike"
	alertKeyJSONLPush   = "jsonl_git_backup:push"
)

// escalate raises an alert whose key is derived from its own title, for
// producers whose subject is already carried in the message (and for the
// escalation sinks other parts of the daemon inject as function values).
// Producers that know a stable class for their condition call escalateAlert
// with an explicit key instead.
func (d *Daemon) escalate(source, message string) {
	d.escalateAlert(escalationTitle(source, message), source, message)
}

// escalateAlert files an escalation under the given alert key via gt escalate.
// message may be multiline (e.g. full go-test output); it
// is passed as the escalation reason via --stdin rather than embedded in the
// title, since `gt escalate` forwards the title straight to
// `bd create --title=...`, which rejects newlines and would otherwise drop the
// alert silently.
//
// The key travels as --fingerprint so the alert's identity is explicit at the
// call site rather than inferred from prose that may embed varying detail.
//
// Under load (slot-starvation, Dolt contention) gt escalate can take >10 s.
// We raise the per-attempt timeout to 60 s, retry up to maxEscalationRetries
// times with exponential backoff, and on final drop log the full message to
// the feed as a last-resort fallback (gt-tlwv).
func (d *Daemon) escalateAlert(key, source, message string) {
	// The callers that do not branch on delivery have no next action to take on a
	// drop: escalateAlertErr's own escalation_dropped feed line is their record.
	_ = d.escalateAlertErr(key, source, message)
}

// escalateAlertErr is escalateAlert with the delivery outcome exposed, for the
// callers whose next action depends on whether the alert actually landed: an
// alarm that reports itself as raised when it never arrived is worse than no
// alarm, because it closes the streak that would have retried.
func (d *Daemon) escalateAlertErr(key, source, message string) error {
	return d.escalateAlertSeverity("HIGH", key, source, message)
}

// escalateAlertSeverity is escalateAlertErr at a chosen severity, for an alert
// that informs rather than pages (a slow landing is "low", gt-lcu5p).
func (d *Daemon) escalateAlertSeverity(severity, key, source, message string) error {
	title := escalationTitle(source, message)

	var lastErr error
	for attempt := 0; attempt < maxEscalationRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), defaultEscalationTimeout)
		err := d.notify().Escalate(ctx, notify.Escalation{
			Severity:    severity,
			Description: title,
			Reason:      message,
			Fingerprint: key,
		})
		lastErr = err

		// A timed-out attempt is retried like any other failure, but logged
		// as a timeout (gt-tlwv).
		if errors.Is(err, context.DeadlineExceeded) {
			d.logger.Printf("escalate(%s): attempt %d/%d timed out after %s — %s",
				source, attempt+1, maxEscalationRetries, defaultEscalationTimeout, message)
			cancel()

			// If this was the last attempt, log to feed as fallback.
			if attempt == maxEscalationRetries-1 {
				_ = events.LogFeedTo(d.config.TownRoot, events.TypeEscalationDropped, "daemon", map[string]interface{}{
					"source":  source,
					"error":   "context deadline exceeded",
					"title":   title,
					"message": message, // include the full message on final drop
				})
			}

			// Back-off before the next retry (exponential: 1 s, 2 s).
			if attempt < maxEscalationRetries-1 {
				backoff := time.Duration(attempt+1) * time.Second
				d.logger.Printf("escalate(%s): retrying in %s …", source, backoff)
				d.clk().Sleep(backoff)
			}
			continue
		}
		cancel()

		if err == nil {
			return nil // success
		}

		// The error carries gt's output, or "<no output>" when a killed
		// process flushed nothing.
		errMsg := err.Error()

		d.logger.Printf("escalate(%s): attempt %d/%d failed: %s — dropped message: %s",
			source, attempt+1, maxEscalationRetries, errMsg, message)

		// If this was the last attempt, log to feed as fallback.
		if attempt == maxEscalationRetries-1 {
			_ = events.LogFeedTo(d.config.TownRoot, events.TypeEscalationDropped, "daemon", map[string]interface{}{
				"source":  source,
				"error":   err.Error(),
				"title":   title,
				"message": message, // include the full message on final drop
			})
		}

		// Back-off before the next retry (exponential: 1 s, 2 s).
		if attempt < maxEscalationRetries-1 {
			backoff := time.Duration(attempt+1) * time.Second
			d.logger.Printf("escalate(%s): retrying in %s …", source, backoff)
			d.clk().Sleep(backoff)
		}
	}

	if lastErr == nil {
		// Unreachable in practice: every path that does not return nil above
		// recorded an error. A caller that branches on delivery must still get an
		// answer rather than a bare nil door out.
		lastErr = fmt.Errorf("gt escalate %s did not report success", key)
	}
	return lastErr
}

// clearAlerts auto-closes the escalations for the given keys, because the
// conditions they describe no longer hold (gt-vwry).
//
// Unlike escalateAlert this does not retry: an escalation that fails to send
// loses an alert nobody has seen yet, while a clear that fails leaves an alert
// open for one more cycle, and the next cycle clears it again. A patrol reader
// clears on every healthy pass, so the ordinary case is "nothing to clear" —
// `gt escalate clear` treats that as success rather than an error.
//
// Callers clear the key for the condition they just re-checked, and only that
// key. A blanket "cycle was fine, close everything" would close alerts whose
// conditions were never actually re-checked on this pass.
func (d *Daemon) clearAlerts(reason string, keys ...string) {
	_ = d.clearAlertsErr(reason, keys...)
}

// clearAlertsErr is clearAlerts for a caller that skips its next clear once
// this one lands, and so must learn when it did not.
func (d *Daemon) clearAlertsErr(reason string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultEscalationTimeout)
	defer cancel()
	if err := d.notify().ClearEscalations(ctx, reason, keys...); err != nil {
		d.logger.Printf("clearAlerts(%s): %v", strings.Join(keys, ","), err)
		return err
	}
	d.logger.Printf("clearAlerts(%s): %s", strings.Join(keys, ","), reason)
	return nil
}

// spikeThreshold returns the configured spike threshold or the default (20%).
func spikeThreshold(config *JsonlGitBackupConfig) float64 {
	if config != nil && config.SpikeThreshold != nil {
		t := *config.SpikeThreshold
		if t > 0 && t <= 1.0 {
			return t
		}
	}
	return defaultSpikeThreshold
}

// isTestPollution checks if a JSONL record looks like test data that leaked into
// production. Checks both "id" and "title" fields against known test patterns.
func isTestPollution(record map[string]interface{}) bool {
	for _, field := range []string{"id", "title"} {
		val, ok := record[field]
		if !ok {
			continue
		}
		s, ok := val.(string)
		if !ok {
			continue
		}
		for _, pat := range testPollutionPatterns {
			if pat.MatchString(s) {
				return true
			}
		}
	}
	return false
}

// filterTestPollutionStream removes test-data records from a JSONL stream.
//
// It reads whole lines with no length cap — bufio.Scanner's is fixed, and a
// line over it ends the scan, so every later record is silently dropped and,
// with at least one pollution record removed, the truncated buffer is written
// back over the export and pushed offsite (gt-q6ljl). A read error is returned,
// never mistaken for end of input. "issues export writes description, design
// and notes whole", so multi-megabyte lines are ordinary data.
func filterTestPollutionStream(r io.Reader) ([]byte, int, error) {
	reader := bufio.NewReader(r)
	var out bytes.Buffer
	removed := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			body := trimLineEnding(line)
			if len(body) > 0 {
				var record map[string]interface{}
				if jsonErr := json.Unmarshal(body, &record); jsonErr != nil {
					// Can't parse — keep it (don't silently drop unknown data).
					writeLine(&out, body)
				} else if isTestPollution(record) {
					removed++
				} else {
					writeLine(&out, body)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out.Bytes(), removed, nil
			}
			return nil, 0, err
		}
	}
}

// trimLineEnding strips one trailing newline and any carriage return before it,
// matching bufio.ScanLines, so re-emitted lines are byte-identical to the input.
func trimLineEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// writeLine appends body and a newline to out. A line that had no trailing
// newline in the input gains one, so the export is always newline-terminated.
func writeLine(out *bytes.Buffer, body []byte) {
	out.Write(body)
	out.WriteByte('\n')
}

// errNoSpikeBaseline is returned when history exists but yields no baseline:
// the backup repo has commits, none of them carry per-database counts, and no
// cache is available to bridge. Callers must surface it as a hard failure,
// never silently substitute a stale or zero value (gt-tj-he).
var errNoSpikeBaseline = errors.New("no spike baseline available")

// errSpikeBaselineBootstrap is returned when the backup repo has no commits at
// all — a freshly initialized repo (ensureGitRepoInitialized runs `git init`
// with no seed commit) and no cache. There is nothing to compare against and no
// spike is possible without a previous level, so callers must PROCEED: the
// commit this run makes is what seeds the history the next run derives from.
// Treating this as errNoSpikeBaseline would deadlock the patrol permanently —
// no baseline, so no commit, so never a baseline (gt-tj-he).
var errSpikeBaselineBootstrap = errors.New("no backup commit history yet")

// recomputeSpikeBaseline derives the rolling spike baseline from the backup
// repo's commit history (gt-tj-he). Each backup commit records its per-database
// counts in the subject line ("backup <ts> m<V>: db1=N1 db2=N2"), so the last
// defaultSpikeBaselineWindow backup commits supply the window. This replaces
// the old git show HEAD:<relPath> read, which misjudged spikes whenever the
// committed file count diverged from what the detector expected.
//
// Only commits counted under countMetricVersion enter the window. A database
// whose counts all come from another version is listed in the baseline's
// Rebaselined set instead, so the caller rebaselines that database alone and
// still checks every database that has same-version history (gt-26qqh).
//
// Commit history is the sole authority for the window. The in-repo cache
// (saveSpikeBaselineHistory) is consulted only when history yields no counts at
// all — a repo whose history was reset, or one whose commits predate the
// count-bearing subject format — where it supplies the last known levels rather
// than a hard halt. A committed level therefore always outranks a cached one,
// and the two can never both contribute an entry for the same database. (An
// earlier revision seeded the cache and then appended history entries beside
// it, so in steady state a stale age-0 cache entry shadowed the fresh committed
// one and the baseline was perpetually one cycle behind — gt-tj-he.)
//
// Returns errSpikeBaselineBootstrap for a repo with no commits (first run),
// errNoSpikeBaseline when commits exist but none carry counts and no cache can
// bridge them, or a wrapped error when git itself fails.
func (d *Daemon) recomputeSpikeBaseline(gitRepo string) (*spikeBaseline, error) {
	window := make(map[string][]spikeCommit) // db → newest-first
	order := []string{}

	// Derive the window from commit history. Each backup commit's subject line
	// carries its per-database counts; the most recent defaultSpikeBaselineWindow
	// such commits form the rolling window, newest first.
	//
	// Ordering matters: commit dates have second resolution, so two commits in
	// the same second tie on date and a date sort is ambiguous. --topo-order
	// instead orders by actual ancestry (commit graph), which is the genuine
	// newest-first backup sequence git log already shows. We fetch a generous
	// pool (not a tight --max-count, which truncates by commit-graph order and
	// can drop newer commits in favor of older side-branches) and keep the
	// topological order.
	fetch := 64 // generous pool; well under any realistic backup repo's commits
	log, err := d.backupGitAt(gitRepo, gitCmdTimeout).LogAll(fetch)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	type commitSubject struct {
		date   string
		counts map[string]int
	}
	var commits []commitSubject
	// otherVersion maps a database to the newest version it was counted under
	// when that is not the current metric; only same-version commits enter the
	// window, since a count measured under another definition is not comparable
	// (gt-26qqh).
	otherVersion := map[string]int{}
	total := 0
	seen := map[string]bool{}
	for _, entry := range log {
		// Count every commit, not just the count-bearing ones: a repo with
		// zero commits is the bootstrap case (proceed), while commits that
		// carry no counts are unreadable history (halt) — see the tail below.
		total++
		counts := parseCommitCounts(entry.Subject)
		if len(counts) == 0 || seen[entry.Hash] {
			continue
		}
		seen[entry.Hash] = true
		if v := parseCommitVersion(entry.Subject); v != countMetricVersion {
			for db := range counts {
				if otherVersion[db] == 0 {
					otherVersion[db] = v
				}
			}
			continue
		}
		date := ""
		if !entry.Time.IsZero() {
			date = entry.Time.Format(time.RFC3339)
		}
		commits = append(commits, commitSubject{date: date, counts: counts})
	}
	// git log --topo-order is already newest-first by ancestry; trim to the
	// window. (We sort topologically rather than by date: commit dates are
	// second-resolution, so same-second commits tie and a date sort is
	// ambiguous, whereas ancestry order is exact.)
	if len(commits) > defaultSpikeBaselineWindow {
		commits = commits[:defaultSpikeBaselineWindow]
	}

	// Layer commits into each database's window. `commits` is newest-first, so
	// a lower age is a more recent backup commit. We collect (not prepend) and
	// sort each window by age ascending afterward: relying on append order
	// would be fragile to Go's (non-deterministic) map iteration order inside
	// the per-commit count loop, whereas an explicit age sort makes the
	// newest-first invariant exact. Ages are commit indices, so the entries of
	// one database's window all have distinct ages — the sort has no ties to
	// resolve, and nothing else ever adds an entry to this map.
	for i, c := range commits {
		for db, n := range c.counts {
			if window[db] == nil {
				order = append(order, db)
			}
			window[db] = append(window[db], spikeCommit{Db: db, N: n, Age: i, Date: c.date, Source: spikeSourceCommit, Version: countMetricVersion})
		}
	}
	for db := range window {
		sort.Slice(window[db], func(i, j int) bool { return window[db][i].Age < window[db][j].Age })
	}

	// A database counted only under another metric has no comparable level: it
	// is a new baseline, not a spike. Databases with same-version history keep
	// their spike check.
	rebaselined := map[string]int{}
	for db, v := range otherVersion {
		if window[db] == nil {
			rebaselined[db] = v
		}
	}
	if len(window) == 0 && len(rebaselined) > 0 {
		// Counts exist, all under another metric.
		return &spikeBaseline{
			Window:      defaultSpikeBaselineWindow,
			Computed:    time.Now().Format(time.RFC3339),
			Counts:      map[string][]spikeCommit{},
			Rebaselined: rebaselined,
		}, nil
	}
	if len(window) == 0 {
		// No count-bearing history. The cache is the only remaining source of
		// levels, and it may only be trusted here, where no committed level
		// exists to outrank it.
		if sb := cachedSpikeBaseline(gitRepo); sb != nil {
			return withoutStaleLevels(sb), nil
		}
		if total == 0 {
			// Fresh repo: nothing to compare against, no spike possible.
			return nil, errSpikeBaselineBootstrap
		}
		// Commits exist but carry no counts this detector can read: fail loud
		// (gt-tj-he) — a silent zero would read as a first export.
		return nil, errNoSpikeBaseline
	}

	sb := &spikeBaseline{
		Window:   defaultSpikeBaselineWindow,
		Computed: time.Now().Format(time.RFC3339),
		Counts:   map[string][]spikeCommit{},
	}
	for _, db := range order {
		entries := window[db]
		if len(entries) > sb.Window {
			entries = entries[:sb.Window]
		}
		if len(entries) > 0 {
			sb.Counts[db] = entries
		}
	}
	// Persist the window so a later run whose history is wiped still has the
	// last known levels. Non-fatal on failure: the window is already computed
	// in memory, and the cache is a fallback, never the source of truth.
	_ = saveSpikeBaselineHistory(gitRepo, sb)
	sb.Rebaselined = rebaselined
	return sb, nil
}

// cachedSpikeBaseline rebuilds a window from the in-repo cache, or returns nil
// when there is no usable cache. Entries are re-labeled as cache-sourced and
// re-based so the newest cached level is age 0 (ages are relative to the run
// that wrote them), de-duplicated by age, and trimmed to the window depth.
func cachedSpikeBaseline(gitRepo string) *spikeBaseline {
	cached := loadSpikeBaselineHistory(gitRepo)
	if cached == nil {
		return nil
	}
	depth := defaultSpikeBaselineWindow
	if cached.Window > 0 {
		depth = cached.Window
	}
	out := &spikeBaseline{
		Window:   depth,
		Computed: time.Now().Format(time.RFC3339),
		Counts:   map[string][]spikeCommit{},
	}
	for db, entries := range cached.Counts {
		if len(entries) == 0 {
			continue
		}
		fresh := make([]spikeCommit, 0, len(entries))
		for _, e := range entries {
			e.Source = spikeSourceCache
			fresh = append(fresh, e)
		}
		// Stable sort: ages may repeat in a corrupt cache, and which duplicate
		// survives dedupe must not depend on sort internals (gt-tj-he).
		sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Age < fresh[j].Age })
		fresh = dedupeSpikeAges(fresh)
		// Re-base: ages are relative to whatever run wrote the cache, and the
		// newest level it recorded is the most recent level known to anyone.
		base := fresh[0].Age
		for i := range fresh {
			fresh[i].Age -= base
		}
		if len(fresh) > depth {
			fresh = fresh[:depth]
		}
		out.Counts[db] = fresh
	}
	if len(out.Counts) == 0 {
		return nil
	}
	return out
}

// dedupeSpikeAges drops entries repeating an age, keeping the first. Ages index
// the newest-first backup sequence, so a repeated age means the source was
// inconsistent (a truncated or hand-edited cache); collapsing it makes "the
// newest level" well-defined instead of dependent on sort internals (gt-tj-he).
func dedupeSpikeAges(entries []spikeCommit) []spikeCommit {
	seen := make(map[int]bool, len(entries))
	out := entries[:0]
	for _, e := range entries {
		if seen[e.Age] {
			continue
		}
		seen[e.Age] = true
		out = append(out, e)
	}
	return out
}

// spikeBaselineCount returns the baseline count for a database from a rolling
// baseline: the newest entry in its window. ok is false when the database is
// absent from the window (first export) or the window is empty.
func spikeBaselineCount(sb *spikeBaseline, db string) (n int, ok bool) {
	if sb == nil {
		return 0, false
	}
	entries := sb.Counts[db]
	if len(entries) == 0 {
		return 0, false
	}
	return entries[0].N, true
}

// spikeHistoryDetail renders the baseline window for one database as a
// source-annotated string, e.g. "hq: 1271 (HEAD), 1266 (HEAD~1), 1250 (HEAD~2)".
// Cache-sourced entries are labeled "cached" rather than given a HEAD offset:
// they carry no position in the current commit graph (gt-tj-he).
func spikeHistoryDetail(sb *spikeBaseline, db string) string {
	if sb == nil {
		return ""
	}
	entries := sb.Counts[db]
	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, len(entries))
	for i, e := range entries {
		label := "HEAD"
		switch {
		case e.Source == spikeSourceCache:
			label = "cached"
		case i > 0:
			label = fmt.Sprintf("HEAD~%d", i)
		}
		parts[i] = fmt.Sprintf("%d (%s)", e.N, label)
	}
	return db + ": " + strings.Join(parts, ", ")
}

// spikeInfo holds the result of a spike check for a single database file.
type spikeInfo struct {
	DB       string
	File     string
	Previous int
	Current  int
	Delta    float64 // absolute fractional change (0.0–1.0+)
	// Baseline is the previous count's provenance, i.e.
	// spikeHistoryDetail for this database — which levels the window holds and
	// whether each came from a backup commit or the cache. Carried on the spike
	// so the escalation report names both sides' sources, not just the log
	// (gt-tj-he).
	Baseline string
}

// spikeCommit is one database's count record from a single backup commit:
// parsed from the "db=NNN" pairs in the commit's subject line.
//
// Two correctness constraints drive the shape:
//   - Fields are exported with explicit json tags. This host's Go toolchains
//     run the encoding/json v2 experiment, which (like v1) drops unexported
//     fields during marshal — a "cached window" of unexported-tagged fields
//     silently serializes to {} and the cache never rehydrates. go vet
//     rejects tagged unexported fields outright, so exporting is the only
//     clean form.
//   - Age is the ONLY order the code may rely on. The per-commit count loop
//     iterates a map, whose order Go does not guarantee, so any window built
//     by append/prepend discipline is fragile; recomputeSpikeBaseline sorts
//     every window by Age ascending to make newest-first exact. Never
//     depend on a slice's element order here.
//
// The in-repo cache (saveSpikeBaselineHistory) marshals these records, so a
// window survives into a run whose commit history no longer carries it.
type spikeCommit struct {
	Db   string `json:"db"`   // database name
	N    int    `json:"n"`    // record count at this commit
	Age  int    `json:"age"`  // backup commits before the most recent one (0 = latest)
	Date string `json:"date"` // commit date (RFC3339); "" if unavailable
	// Source names where this level came from — "commit" (a backup commit
	// subject in this repo's history) or "cache" (the in-repo baseline cache,
	// used only when history carries no counts). Reported so a spike's
	// "previous" value can be traced to its origin (gt-tj-he).
	Source string `json:"source"`
	// Version is the count metric the level was measured under (see
	// countMetricVersion). A cache written before versioning omits it, which
	// reads as version 1, like a subject without the token.
	Version int `json:"version,omitempty"`
}

const (
	// spikeSourceCommit marks a window entry parsed from a backup commit
	// subject; spikeSourceCache marks one read from the in-repo cache.
	spikeSourceCommit = "commit"
	spikeSourceCache  = "cache"
)

// spikeBaseline is the rolling baseline: each database's count across the last
// defaultSpikeBaselineWindow backup commits, newest first. Unlike the
// single-snapshot .spike-counts.json (which goes stale the moment a spike halt
// blocks the commit it was derived from — gt-tj-he), this window is re-derived
// from commit history on every run and tracks committed levels.
type spikeBaseline struct {
	Window   int                      `json:"window"`
	Computed string                   `json:"computed"`
	Counts   map[string][]spikeCommit `json:"counts"`
	// Rebaselined maps a database to the count-metric version of the history it
	// was last counted under, for each database whose only levels were measured
	// under a version other than countMetricVersion. Such a database has no
	// comparable level, so the spike check treats its next count as a new
	// baseline. Derived on every run, never cached (gt-26qqh).
	Rebaselined map[string]int `json:"-"`
}

const (
	// defaultSpikeBaselineWindow is how many recent backup commits the rolling
	// baseline spans (gt-tj-he). Eight commits at the default 15-minute interval
	// is two hours of history.
	defaultSpikeBaselineWindow = 8
	// spikeBaselineCacheFile is the in-repo cache of the last computed rolling
	// baseline. It is NOT the source of truth: every run whose history carries
	// counts overwrites it, and recomputeSpikeBaseline reads it back only when
	// history carries none — a reset history, or commits predating the
	// count-bearing subject format. Without it those cases would halt with no
	// baseline even though the last known levels are still on disk (gt-tj-he).
	spikeBaselineCacheFile = ".spike-baseline-cache.json"
)

// countMetricVersion names the count the backup commits per database: the line
// total across the issue file and every supplemental table file, minus the
// records the pollution filter removed. The commit subject carries it as an
// "m<N>" token ("backup <ts> m2: hq=37562"), and the spike check only compares
// counts of the same version. A change to what the count measures MUST bump this
// number: a count measured under another definition reads as a spike against
// the old history, which would halt the backup for good, since a halt commits
// nothing and so never advances that history (gt-26qqh).
//
// Version 1 is the issue-file line count, and is also what a subject without the
// token means, so history from before the token parses unchanged.
const countMetricVersion = 2

// legacyCountMetricVersion is the version of a subject (or cache entry) that
// carries no token.
const legacyCountMetricVersion = 1

// parseCommitVersion reads the count-metric version from a backup commit
// subject ("backup <ts> m2: db=N"). The token is the "m<N>:" field ahead of the
// first count pair; a subject without one is version 1.
func parseCommitVersion(subject string) int {
	for _, field := range strings.Fields(subject) {
		if strings.ContainsRune(field, '=') {
			break
		}
		digits, ok := strings.CutPrefix(field, "m")
		if !ok {
			continue
		}
		digits, ok = strings.CutSuffix(digits, ":")
		if !ok {
			continue
		}
		if v, err := strconv.Atoi(digits); err == nil && v > 0 {
			return v
		}
	}
	return legacyCountMetricVersion
}

// withoutStaleLevels drops, per database, the cached levels measured under a
// count metric other than countMetricVersion and lists the databases left with
// none in Rebaselined. A cache entry without a version reads as version 1, like
// a subject without the token. A database with a current-version level keeps its
// check even when another database's cache entries are stale (gt-26qqh).
func withoutStaleLevels(sb *spikeBaseline) *spikeBaseline {
	sb.Rebaselined = map[string]int{}
	for db, entries := range sb.Counts {
		kept := entries[:0]
		stale := 0
		for _, e := range entries {
			v := e.Version
			if v == 0 {
				v = legacyCountMetricVersion
			}
			switch {
			case v == countMetricVersion:
				kept = append(kept, e)
			case v > stale:
				stale = v
			}
		}
		if len(kept) == 0 {
			delete(sb.Counts, db)
			sb.Rebaselined[db] = stale
			continue
		}
		sb.Counts[db] = kept
	}
	return sb
}

// parseCommitCounts extracts the "db=NNN" count pairs from a git commit's
// subject line. Backup commits are always titled
// "backup <timestamp> m<V>: db1=N1 db2=N2 [FAILED: ...]" (see commitAndPushJsonlBackup),
// so the subject is the only count record surviving an unpushed or halted
// cycle. Returns nil when no pairs are present.
// jsonlBackupSubject is the subject a backup commit carries and the spike
// baseline parses: "backup <ts> m<version>: <db>=<N> ...". The commit and the
// accept hint both build it here, so neither can drift from what
// parseCommitCounts and parseCommitVersion read.
func jsonlBackupSubject(timestamp string, parts []string) string {
	return fmt.Sprintf("backup %s m%d: %s", timestamp, countMetricVersion, strings.Join(parts, " "))
}

// jsonlCountParts renders a subject's count fields in database order.
func jsonlCountParts(databases []string, counts map[string]int) []string {
	parts := make([]string, 0, len(databases))
	for _, db := range databases {
		if n, ok := counts[db]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", db, n))
		}
	}
	return parts
}

func parseCommitCounts(subject string) map[string]int {
	var out map[string]int
	for _, field := range strings.Fields(subject) {
		if i := strings.IndexByte(field, '='); i > 0 {
			if n, err := strconv.Atoi(field[i+1:]); err == nil && n >= 0 &&
				validDBName.MatchString(field[:i]) {
				if out == nil {
					out = make(map[string]int)
				}
				out[field[:i]] = n
			}
		}
	}
	return out
}

// loadSpikeBaselineHistory reads the rolling baseline cache from the git repo
// directory. Returns nil if the file doesn't exist or can't be parsed.
func loadSpikeBaselineHistory(gitRepo string) *spikeBaseline {
	path := filepath.Join(gitRepo, spikeBaselineCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sb spikeBaseline
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil
	}
	return &sb
}

// saveSpikeBaselineHistory writes the rolling baseline cache. The file is
// git-ignored so it never lands in the backup repo.
func saveSpikeBaselineHistory(gitRepo string, sb *spikeBaseline) error {
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return err
	}
	ensureGitIgnore(gitRepo, spikeBaselineCacheFile)
	return os.WriteFile(filepath.Join(gitRepo, spikeBaselineCacheFile), data, 0644)
}

// ensureGitIgnore adds an entry to .gitignore if not already present.
func ensureGitIgnore(gitRepo, entry string) {
	ignorePath := filepath.Join(gitRepo, ".gitignore")
	data, _ := os.ReadFile(ignorePath)
	content := string(data)
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == entry {
			return // Already present.
		}
	}
	// Append the entry.
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += entry + "\n"
	if err := os.WriteFile(ignorePath, []byte(content), 0644); err != nil {
		// Non-fatal: spike-baseline writes still work without the ignore entry.
		return
	}
}

// verifyExportCounts compares current export counts against the rolling
// baseline derived from recent backup commit history. Returns a list of
// anomalies that exceed the spike threshold, (nil, nil) when there is no
// baseline to compare against yet (first run on a fresh repo), or (nil, err)
// when history exists that yields no baseline — callers must surface that as a
// hard failure rather than silently treating it as a first export (gt-tj-he).
//
// Asymmetric thresholds: drops (possible data loss) use the configured
// threshold; increases (new issues filed) use 2x the threshold since growth is
// normal. Small absolute changes (<20 records) are always allowed to avoid
// false alarms on small databases.
//
// A database absent from the baseline window is a first export and is skipped;
// there is no level to compare against. A baseline of zero is checked against
// the small-change floor, since a ratio against zero measures nothing
// (gt-ttqq8).
func (d *Daemon) verifyExportCounts(gitRepo string, databases []string, counts map[string]int, threshold float64) ([]spikeInfo, error) {
	const minAbsoluteDelta = 20 // ignore changes smaller than this many records

	var spikes []spikeInfo
	spikeBase, err := d.recomputeSpikeBaseline(gitRepo)
	if errors.Is(err, errSpikeBaselineBootstrap) {
		// The backup repo has no commits yet, so there is no previous level to
		// compare against and no spike is possible. Proceed with the export:
		// the commit this run makes is what seeds the history the next run
		// derives its baseline from. Halting here would deadlock the patrol —
		// no baseline means no commit means never a baseline (gt-tj-he).
		d.logger.Printf("jsonl_git_backup: verify: no backup history yet (first run) — nothing to compare against, skipping spike check")
		return nil, nil
	}
	if err != nil {
		// Commits exist but carry no counts this detector can read. Fail loud
		// (gt-tj-he): a silent skip would read as a first export and anchor an
		// unchecked baseline.
		d.logger.Printf("jsonl_git_backup: verify: cannot compute spike baseline: %v", err)
		return nil, fmt.Errorf("no spike baseline: %w", err)
	}

	for _, db := range databases {
		currentCount, ok := counts[db]
		if !ok {
			continue // database failed export, skip
		}

		prevCount, ok := spikeBaselineCount(spikeBase, db)
		if from, changed := spikeBase.Rebaselined[db]; !ok && changed {
			// The count metric changed since this database was last committed,
			// so the history holds no count comparable to this run's. Committing
			// this run's count is what ends the mismatch; halting would commit
			// nothing and stay mismatched for ever (gt-26qqh).
			d.logger.Printf("jsonl_git_backup: verify: %s: count metric changed from m%d to m%d — rebaselining: this run's count (%d) becomes the new baseline, spike check skipped",
				db, from, countMetricVersion, currentCount)
			continue
		}
		if !ok {
			// First export of this database — no baseline to compare against.
			d.logger.Printf("jsonl_git_backup: verify: %s: first export (%d records), skipping spike check", db, currentCount)
			continue
		}
		absDelta := currentCount - prevCount
		if absDelta < 0 {
			absDelta = -absDelta
		}
		// Small absolute changes are always fine — avoids false alarms on
		// small databases where a few issues cause large percentage swings.
		if absDelta < minAbsoluteDelta {
			continue
		}

		// A zero baseline leaves the ratio without a denominator: |c-0|/0 is
		// +Inf, which beats every threshold, and a halted tick commits nothing,
		// so the level stayed zero and the database could never be accepted at
		// all (gt-ttqq8). Measuring the growth against the small-change floor
		// keeps the ratio finite and keeps a bulk load into an empty database
		// halting for an operator to accept.
		denominator := prevCount
		if denominator == 0 {
			denominator = minAbsoluteDelta
		}
		fractionalDelta := math.Abs(float64(currentCount-prevCount)) / float64(denominator)

		// Asymmetric: increases are less suspicious than drops.
		// New issues being filed is normal growth; losing issues suggests data loss.
		effectiveThreshold := threshold
		if currentCount > prevCount {
			effectiveThreshold = threshold * 2 // 2x tolerance for growth
		}

		if fractionalDelta > effectiveThreshold {
			spike := spikeInfo{
				DB:       db,
				File:     filepath.Join(db, "issues.jsonl"),
				Previous: prevCount,
				Current:  currentCount,
				Delta:    fractionalDelta,
				Baseline: spikeHistoryDetail(spikeBase, db),
			}
			spikes = append(spikes, spike)

			direction := "jump"
			if currentCount < prevCount {
				direction = "drop"
			}
			d.logger.Printf("jsonl_git_backup: SPIKE DETECTED: %s: %s from %d to %d (%.1f%% %s, threshold %.1f%%) — baseline window: %s",
				db, direction, prevCount, currentCount, fractionalDelta*100, direction, effectiveThreshold*100,
				spikeHistoryDetail(spikeBase, db))
		}
	}

	return spikes, nil
}

// formatSpikeReport creates a human-readable summary of spike anomalies for
// escalation. Each spike names both counts' sources — the baseline window entry
// the previous count came from (commit or cache) and the export file the
// current count was read from — so a reviewer can tell a real data change from
// a mis-derived baseline without re-deriving it by hand (gt-tj-he).
func formatSpikeReport(spikes []spikeInfo) string {
	var b strings.Builder
	b.WriteString("JSONL export spike detection triggered:\n")
	for _, s := range spikes {
		direction := "JUMP (possible pollution)"
		if s.Current < s.Previous {
			direction = "DROP (possible data loss)"
		}
		fmt.Fprintf(&b, "  %s: %d → %d (%.1f%% change) — %s\n",
			s.DB, s.Previous, s.Current, s.Delta*100, direction)
		fmt.Fprintf(&b, "      previous %d from %s\n", s.Previous, s.Baseline)
		fmt.Fprintf(&b, "      current  %d from %s\n", s.Current, s.File)
	}
	b.WriteString("Export halted. Manual review required.")
	return b.String()
}

// spikeAcceptHint is how a halt ends: the baseline is the per-database counts in
// the newest backup commit subjects, and a halted tick commits nothing, so
// every tick halts on the same level until a newer count-bearing commit exists
// (gt-ttqq8). The subject is built by jsonlBackupSubject, the writer's own
// formatter, so the command given to an operator is one the parser reads back.
func spikeAcceptHint(gitRepo string, databases []string, counts map[string]int) string {
	parts := jsonlCountParts(databases, counts)
	if len(parts) == 0 {
		parts = []string{"<db>=<N>"}
	}
	subject := jsonlBackupSubject(time.Now().Format("2006-01-02 15:04"), parts)
	return fmt.Sprintf("To accept the export and resume the backup, land its counts as the new baseline:\n  git -C %q commit --allow-empty -m %q",
		gitRepo, subject)
}

// countFileLines counts the number of non-empty lines in a file. It reads
// whole lines with no length cap and reports a read error rather than treating
// it as end of input, so a long line never shortens the count (gt-q6ljl).
func countFileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	count := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(trimLineEnding(line)) > 0 {
			count++
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return count, nil
			}
			return 0, readErr
		}
	}
}

// recountAfterFilter takes the issue lines the pollution filter removed out of
// each filtered database's count.
//
// counts[db] is the exportDatabaseToJsonl total: issues plus the supplemental
// tables, the same metric the spike baseline is built from. Recounting
// issues.jsonl alone switched metrics on exactly the ticks the filter fired,
// so the spike guard read a 76% drop (or, after a polluted first commit, a
// 3x spike) and halted every later tick (gt-tx0ia).
//
// A database the filter left alone keeps its count. A removed count above the
// database's total cannot come from one export, so it is returned rather than
// clamped: the caller must not commit counts it could not verify (gt-q6ljl).
//
// The recounts are validated as a set before any of them is applied. Adjusting
// each database as it is reached left counts partly recounted when a later one
// was rejected, and map order — not the data — decided which databases those
// were. The caller halts on the error either way, but a partly adjusted map is
// indistinguishable from a verified one to anything that reads counts next
// (gt-83c3w).
func recountAfterFilter(counts, removed map[string]int) error {
	type adjustment struct {
		db        string
		remaining int
	}
	var adjustments []adjustment
	for db, n := range removed {
		total, ok := counts[db]
		if !ok || n <= 0 {
			continue
		}
		if n > total {
			return fmt.Errorf("recount %s: filter removed %d records from an export of %d", db, n, total)
		}
		adjustments = append(adjustments, adjustment{db: db, remaining: total - n})
	}
	for _, a := range adjustments {
		counts[a.db] = a.remaining
	}
	return nil
}

// sumRemoved totals a per-database removed-record map.
func sumRemoved(removed map[string]int) int {
	total := 0
	for _, n := range removed {
		total += n
	}
	return total
}

// applyPollutionFilter reads each shard of each database's issues export,
// filters out test pollution records, and rewrites the shard. Returns the
// records removed per database; a database it removed nothing from has no
// entry.
//
// The file is rewritten only when the read and the filter both succeed. A read
// error means the buffer is a partial read, and writing it back would truncate
// the export — the failure this filter exists to prevent (gt-q6ljl).
func (d *Daemon) applyPollutionFilter(gitRepo string, databases []string) map[string]int {
	removedByDB := make(map[string]int)
	for _, db := range databases {
		// Every shard of issues is filtered; a database whose export failed has
		// no issues export to filter. That is absence, not a read failure, and
		// export already reported it. Anything else is a real read failure and
		// is named.
		paths, err := jsonlShardPaths(filepath.Join(gitRepo, db), "issues")
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				d.logger.Printf("jsonl_git_backup: %s: pollution filter skipped: %v", db, err)
			}
			continue
		}
		removed := 0
		for _, issuesPath := range paths {
			n, err := filterIssuesFile(issuesPath)
			if err != nil {
				d.logger.Printf("jsonl_git_backup: %s: pollution filter skipped: %v", db, err)
				continue
			}
			removed += n
		}
		if removed > 0 {
			d.logger.Printf("jsonl_git_backup: %s: filtered %d test-pollution record(s)", db, removed)
			removedByDB[db] = removed
		}
	}
	return removedByDB
}

// filterIssuesFile filters one issues shard (issues.jsonl, issues.2.jsonl, ...)
// in place and returns the number of records removed. It writes the file only
// when the whole read succeeded, so a read error leaves it byte-identical.
func filterIssuesFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	filtered, removed, err := filterTestPollutionStream(f)
	f.Close()
	if err != nil {
		return 0, fmt.Errorf("read failed, %s left untouched: %w", filepath.Base(path), err)
	}
	if removed == 0 {
		return 0, nil
	}
	if err := os.WriteFile(path, filtered, 0644); err != nil {
		return 0, fmt.Errorf("writing filtered export: %w", err)
	}
	return removed, nil
}

// verifyNoPollution re-scans all exported issues shards for any remaining
// suspicious records that survived both the SQL scrub and the regex filter.
// Returns the total number of suspicious records found across all databases.
//
// A shard it cannot read, and a database whose export it cannot find at all,
// are errors rather than a clean result: this is the check that decides whether
// a polluted export gets pushed, so anything it did not read leaves the export
// unverified and halts the cycle (gt-fhcwe). Every database it is given
// exported this cycle, and an export writes at least one issues shard, so
// nothing to read means the export is not there.
//
// The scan uses the same uncapped reader as the filter: a long line must not
// hide the records after it from the check that shares the filter's definition
// (gt-q6ljl).
func (d *Daemon) verifyNoPollution(gitRepo string, databases []string) (int, error) {
	total := 0
	for _, db := range databases {
		paths, err := jsonlShardPaths(filepath.Join(gitRepo, db), "issues")
		if err != nil {
			return total, fmt.Errorf("%s: %w", db, err)
		}
		if len(paths) == 0 {
			return total, fmt.Errorf("%s: no issues export to check", db)
		}
		for _, issuesPath := range paths {
			data, err := os.ReadFile(issuesPath)
			if err != nil {
				return total, fmt.Errorf("%s: %w", filepath.Base(issuesPath), err)
			}
			reader := bufio.NewReader(bytes.NewReader(data))
			for {
				line, readErr := reader.ReadBytes('\n')
				body := trimLineEnding(line)
				if len(body) > 0 {
					var record map[string]interface{}
					if json.Unmarshal(body, &record) == nil && isTestPollution(record) {
						id, _ := record["id"].(string)
						title, _ := record["title"].(string)
						d.logger.Printf("jsonl_git_backup: VERIFY FAIL: %s: suspicious record id=%q title=%q", db, id, title)
						total++
					}
				}
				if readErr != nil {
					break
				}
			}
		}
	}
	return total, nil
}

// parseLineCount parses a line count from `wc -l` style output or plain integer.
func parseLineCount(s string) (int, error) {
	s = strings.TrimSpace(s)
	// wc -l output format: "  42 filename" or just "42"
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty input")
	}
	return strconv.Atoi(fields[0])
}
