package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
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

	defaultSpikeThreshold         = 0.50 // 50% delta triggers halt (was 20%, too sensitive for bulk ops)

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

// errGitCmdTimeout marks a git command killed because its context deadline
// expired. The kill signal gives git no chance to release .git/index.lock, so
// callers of index-mutating commands clear one up on this error (gt-1aj2).
var errGitCmdTimeout = errors.New("command timed out")

// testPollutionPatterns matches issue IDs or titles that indicate test data leaked
// into production exports. These records are filtered out before writing JSONL.
var testPollutionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^Test Issue`),                      // title: "Test Issue ..."
	regexp.MustCompile(`(?i)^test[_\s]`),                       // title: "test_something" or "test something"
	regexp.MustCompile(`^bd-[0-9]{1,2}$`),                      // id: bd-1, bd-99 (suspiciously short IDs)
	regexp.MustCompile(`^bd-[a-z]{3,5}[0-9]{1,2}$`),            // id: bd-abc12 (test-style IDs)
	regexp.MustCompile(`^(testdb_|beads_t|beads_pt|doctest_)`), // id prefixes from test databases
	regexp.MustCompile(`(?i)^--help`),                          // title: "--help" CLI artifacts
	regexp.MustCompile(`(?i)^Usage:\s`),                        // title: "Usage: ..." CLI help output
	regexp.MustCompile(`^offlinebrew-`),                        // id: offlinebrew-* test prefixes
	regexp.MustCompile(`-wisp-`),                               // id: wisp-pattern IDs leaked into issues table
}

// validDBName matches safe database names (alphanumeric, underscore, hyphen).
var validDBName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// scrubQuery is the WHERE clause for filtering ephemeral data.
// Kept separate from Sprintf to avoid %% confusion.
// The query selects only durable work product (bugs, features, tasks, epics, chores).
const scrubWhereClause = ` WHERE (ephemeral IS NULL OR ephemeral != 1)` +
	` AND status != 'tombstone'` +
	` AND issue_type NOT IN ('message', 'event', 'agent', 'convoy', 'molecule', 'role', 'merge-request', 'rig')` +
	` AND id NOT LIKE '%-wisp-%'` +
	` AND id NOT LIKE '%-cv-%'` +
	` AND id NOT LIKE '%-wf-%'` +
	` AND id NOT LIKE 'test%'` +
	` AND id NOT LIKE 'beads\_t%'` +
	` AND id NOT LIKE 'beads\_pt%'` +
	` AND id NOT LIKE 'doctest\_%'` +
	` AND id NOT LIKE 'offlinebrew-%'` +
	` AND title NOT LIKE '--%'` +
	` AND title NOT LIKE 'Usage: %'` +
	` ORDER BY id`

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

	dec := evaluatePatrolDue(d.config.TownRoot, "jsonl_git_backup", time.Time{}, time.Now(), jsonlGitBackupInterval(d.patrolConfig))
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
		if err := savePatrolLastRun(d.config.TownRoot, "jsonl_git_backup", time.Now()); err != nil {
			d.logger.Printf("jsonl_git_backup: WARNING: cannot persist last-run time (%v) — "+
				"the next check may re-run sooner than expected", err)
		}
	}()

	// Pour molecule for observability (nil-safe — all methods are no-ops on nil).
	mol := d.pourDogMolecule(constants.MolDogJSONL, nil)
	defer mol.close()

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
	if err := ensureGitRepoInitialized(gitRepo); err != nil {
		d.logger.Printf("jsonl_git_backup: cannot initialize git repo %s: %v", gitRepo, err)
		mol.failStep("export", "git repo init failed: "+err.Error())
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
	// list (see mayor/daemon.json), so falling through to a bare skip here
	// meant this patrol did nothing on every single tick regardless of the
	// git-repo fix above — the documented "auto-discovers from dolt server"
	// behavior (types.go) was never implemented (gt-kme).
	databases := config.Databases
	if len(databases) == 0 {
		databases = discoverJsonlBackupDatabases(dataDir)
	}
	if len(databases) == 0 {
		d.logger.Printf("jsonl_git_backup: no databases found (configured or auto-discovered) under %s, skipping", dataDir)
		mol.failStep("export", "no databases found")
		d.escalateAlert(alertKeyJSONLNoDBs, "jsonl_git_backup", fmt.Sprintf("no databases found under %s — offsite backup has nothing to export", dataDir))
		return
	}
	d.clearAlerts("databases discovered", alertKeyJSONLNoDBs)

	d.logger.Printf("jsonl_git_backup: exporting %d database(s) to %s (scrub=%v)", len(databases), gitRepo, scrub)

	exported := 0
	var failed []string
	counts := make(map[string]int)
	for _, db := range databases {
		n, err := d.exportDatabaseToJsonl(db, gitRepo, dataDir, scrub)
		if err != nil {
			d.logger.Printf("jsonl_git_backup: %s: export failed: %v", db, err)
			failed = append(failed, db)
		} else {
			counts[db] = n
			exported++
		}
	}

	if exported == 0 {
		d.logger.Printf("jsonl_git_backup: no databases exported successfully")
		mol.failStep("export", "no databases exported successfully")
		return
	}

	mol.closeStep("export")

	// Phase D: Pollution firewall — filter test data from exports.
	removed := d.applyPollutionFilter(gitRepo, databases)
	if removed > 0 {
		d.logger.Printf("jsonl_git_backup: filtered %d total test-pollution record(s)", removed)
		// Recount after filtering so spike detection uses accurate numbers.
		recountAfterFilter(gitRepo, databases, counts)
	}

	// Post-scrub verification: re-scan output for any remaining pollution.
	if remaining := d.verifyNoPollution(gitRepo, databases); remaining > 0 {
		d.logger.Printf("jsonl_git_backup: WARNING: %d suspicious record(s) survived scrub+filter", remaining)
		d.escalateAlert(alertKeyJSONLScrub, "jsonl_git_backup", fmt.Sprintf("post-scrub verification found %d suspicious records — review JSONL exports", remaining))
	} else {
		d.clearAlerts("no suspicious records survived scrub", alertKeyJSONLScrub)
	}

	mol.closeStep("verify")

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
			msg += fmt.Sprintf("\n\n%s has commits, but none record per-database counts ('backup <ts>: <db>=<N>'), so there is no previous level to compare against. To recover, land one count-bearing commit by hand (git -C %s add -A && git -C %s commit -m 'backup <ts>: <db>=<N>'), or start the history over by removing %s/.git — the next run then bootstraps normally.",
				gitRepo, gitRepo, gitRepo, gitRepo)
		}
		d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", msg)
		mol.failStep("push", baselineErr.Error())
		return // Do NOT commit — no baseline to verify against.
	}
	if len(spikes) > 0 {
		report := formatSpikeReport(spikes)
		d.logger.Printf("jsonl_git_backup: HALTING — spike detected:\n%s", report)
		d.escalateAlert(alertKeyJSONLSpike, "jsonl_git_backup", report)
		mol.failStep("push", "spike detected")
		return // Do NOT commit — spike detected.
	}
	d.clearAlerts("no export spike", alertKeyJSONLSpike)

	// Commit and push if anything changed.
	// Include failed databases in commit message so staleness is visible.
	pushStatus := "ok"
	if err := d.commitAndPushJsonlBackup(gitRepo, databases, counts, failed); err != nil {
		d.logger.Printf("jsonl_git_backup: git operations failed: %v", err)
		pushStatus = "failed"
		mol.failStep("push", err.Error())
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
		mol.closeStep("push")
		d.clearAlerts("backup push succeeded", alertKeyJSONLPush)
	}

	d.logger.Printf("jsonl_git_backup: exported %d/%d database(s), push=%s", exported, len(databases), pushStatus)
	mol.closeStep("report")
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
// Also writes a legacy {db}.jsonl (symlink to {db}/issues.jsonl) for backward compat.
//
// Returns the total number of records exported across all tables.
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

	// 1. Export issues table (with scrub filter).
	var query string
	if scrub {
		query = "SELECT * FROM `" + db + "`.issues" + scrubWhereClause
	} else {
		query = "SELECT * FROM `" + db + "`.issues ORDER BY id"
	}
	n, err := d.exportTableToJsonl("issues", query, dbDir, dataDir)
	if err != nil {
		return 0, fmt.Errorf("issues: %w", err)
	}
	total += n

	// 2. Export supplemental tables (no scrub, full export).
	for _, table := range supplementalTables {
		tQuery := fmt.Sprintf("SELECT * FROM `%s`.`%s` ORDER BY 1", db, table)
		tn, err := d.exportTableToJsonl(table, tQuery, dbDir, dataDir)
		if err != nil {
			// Non-fatal for supplemental tables — log and continue.
			d.logger.Printf("jsonl_git_backup: %s/%s: export failed (non-fatal): %v", db, table, err)
			continue
		}
		total += tn
	}

	d.logger.Printf("jsonl_git_backup: %s: exported %d records across %d tables", db, total, 1+len(supplementalTables))
	return total, nil
}

// exportTableToJsonl runs a query and writes the result as JSONL to {dir}/{table}.jsonl.
// Connects to the running Dolt server via --host/--port to get current committed data,
// falling back to embedded mode (cmd.Dir=dataDir) if no server config is available.
// Returns the number of records exported.
func (d *Daemon) exportTableToJsonl(table, query, dir, dataDir string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), jsonlExportTimeout)
	defer cancel()

	// Prefer querying the running server (accurate, up-to-date data) over embedded
	// mode (reads on-disk state which may lag behind server commits).
	host := "127.0.0.1"
	port := 3307
	user := "root"
	password := ""
	useServer := false
	if d.doltServer != nil && d.doltServer.IsEnabled() {
		if d.doltServer.config.Host != "" {
			host = d.doltServer.config.Host
		}
		if d.doltServer.config.Port != 0 {
			port = d.doltServer.config.Port
		}
		if d.doltServer.config.User != "" {
			user = d.doltServer.config.User
		}
		password = d.doltServer.config.Password
		useServer = true
	}

	var cmd *exec.Cmd
	if useServer {
		cmd = exec.CommandContext(ctx, "dolt",
			"--host", host,
			"--port", strconv.Itoa(port),
			"--no-tls",
			"-u", user,
			"-p", password,
			"sql", "-r", "json", "-q", query)
	} else {
		cmd = exec.CommandContext(ctx, "dolt", "sql", "-r", "json", "-q", query)
	}
	// Always set cmd.Dir to prevent stray .doltcfg/ creation (GH#2537).
	cmd.Dir = dataDir
	util.SetDetachedProcessGroup(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return 0, fmt.Errorf("%s: %s", err, errMsg)
		}
		return 0, err
	}

	var result struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return 0, fmt.Errorf("parsing dolt output: %w", err)
	}

	outPath := filepath.Join(dir, table+".jsonl")
	tmpPath := outPath + ".tmp"

	var buf bytes.Buffer
	for _, row := range result.Rows {
		if table == "events" {
			capped, err := capEventRowValues(row, eventValueBackupCap)
			if err != nil {
				return 0, fmt.Errorf("capping events row: %w", err)
			}
			row = capped
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, row); err != nil {
			return 0, fmt.Errorf("compacting JSON row: %w", err)
		}
		buf.Write(compact.Bytes())
		buf.WriteByte('\n')
	}

	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
		return 0, fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("renaming %s: %w", tmpPath, err)
	}

	return len(result.Rows), nil
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

// commitAndPushJsonlBackup stages, commits, and pushes JSONL files if changed.
// The commit message includes counts for successful exports AND names of failed
// databases, so partial failures are visible in git history.
func (d *Daemon) commitAndPushJsonlBackup(gitRepo string, databases []string, counts map[string]int, failed []string) error {
	// The three local commands below share one deadline, so a single slow one
	// can use the whole phase without three of them stacking past a tick.
	localDeadline := time.Now().Add(gitLocalPhaseTimeout)
	localBudget := func() time.Duration { return time.Until(localDeadline) }

	// Stage all JSONL files (flat legacy files + subdirectory structure).
	// Use "." instead of "*/" to correctly handle initially-untracked subdirectories.
	if err := d.runGitIndexCmd(gitRepo, localBudget(), "add", "-A", "."); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Check if there are staged changes.
	if err := d.runGitIndexCmd(gitRepo, localBudget(), "diff", "--cached", "--quiet"); err == nil {
		d.logger.Printf("jsonl_git_backup: no changes to commit")
		return nil
	}

	// Build commit message with counts in deterministic order.
	timestamp := time.Now().Format("2006-01-02 15:04")
	var parts []string
	for _, db := range databases {
		if n, ok := counts[db]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", db, n))
		}
	}
	msg := fmt.Sprintf("backup %s: %s", timestamp, strings.Join(parts, " "))
	if len(failed) > 0 {
		sort.Strings(failed)
		msg += fmt.Sprintf(" [FAILED: %s]", strings.Join(failed, ", "))
	}

	// Commit.
	if err := d.runGitIndexCmd(gitRepo, localBudget(), "commit", "-m", msg,
		"--author=Gas Town Daemon <daemon@gastown.local>"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// The rolling spike baseline is re-derived from commit history (the subject
	// lines recorded above) on every run, so nothing to clean up here. The
	// in-repo cache (.spike-baseline-cache.json) is git-ignored and left in
	// place: recomputeSpikeBaseline consults it only when history carries no
	// counts at all, so it can never outrank a committed level (gt-tj-he).

	// Push requires a remote. This is the OFFSITE layer — a repo with commits
	// but no remote is data sitting on the same disk it started on, which is
	// exactly the failure this patrol exists to prevent. Treat it as a hard
	// error (not a graceful skip) so it surfaces through the same
	// consecutive-failure escalation as a real push failure, instead of
	// silently "succeeding" forever (gt-kme).
	if !d.hasGitRemote(gitRepo, "origin") {
		return fmt.Errorf("committed locally but no 'origin' remote configured — backup is not offsite")
	}

	// Detect current branch name for push (master vs main).
	branch := d.currentGitBranch(gitRepo)
	if branch == "" {
		branch = "main" // fallback
	}
	if err := d.runGitCmd(gitRepo, gitPushTimeout, "push", "origin", branch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	d.logger.Printf("jsonl_git_backup: committed and pushed: %s", msg)
	return nil
}

// ensureGitRepoInitialized makes sure gitRepo exists and is a git repository,
// initializing it (mkdir + git init) on first use instead of silently
// skipping forever when the directory doesn't exist yet. A remote is NOT
// configured here — that still requires an explicit `git remote add origin`
// — but at least the local half stops being permanently inert (gt-kme).
func ensureGitRepoInitialized(gitRepo string) error {
	if _, err := os.Stat(filepath.Join(gitRepo, ".git")); err != nil {
		if err := os.MkdirAll(gitRepo, 0755); err != nil {
			return fmt.Errorf("creating %s: %w", gitRepo, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "init", "-b", "main")
		cmd.Env = gitChildEnv()
		util.SetDetachedProcessGroup(cmd)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			errMsg := strings.TrimSpace(stderr.String())
			if errMsg != "" {
				return fmt.Errorf("git init: %s", errMsg)
			}
			return fmt.Errorf("git init: %w", err)
		}
	}

	// Set unconditionally (idempotent) rather than only on fresh init, so a
	// pre-existing repo created before this fix — or one where the config was
	// lost — also gets covered on the next daemon start (gt-kxa1).
	if err := setGitPostBuffer(gitRepo); err != nil {
		return fmt.Errorf("git config http.postBuffer: %w", err)
	}
	return nil
}

// setGitPostBuffer sets http.postBuffer on gitRepo to gitPostBufferBytes.
func setGitPostBuffer(gitRepo string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "config", "http.postBuffer", gitPostBufferBytes)
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
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
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "count-objects", "-v")
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if kb, ok := strings.CutPrefix(line, "size-pack:"); ok {
			return strings.TrimSpace(kb) + " KiB"
		}
	}
	return ""
}

// discoverJsonlBackupDatabases lists production database directories under
// dataDir, for use when JsonlGitBackupConfig.Databases is empty. Mirrors
// dolt-archive/run.sh's auto-discovery exclusions (test/scratch DB prefixes)
// and additionally requires a real `.dolt` subdirectory so stray non-database
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
		if strings.HasPrefix(name, "testdb_") || strings.HasPrefix(name, "beads_t") ||
			strings.HasPrefix(name, "beads_pt") || strings.HasPrefix(name, "doctest_") ||
			strings.HasPrefix(name, "dolt_remotes_check_") {
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

// gitChildEnv returns os.Environ() augmented with HOME/USER/LOGNAME/SSH_AUTH_SOCK
// when missing. Daemon-launched git falls back to getpwuid(uid) for committer/author
// identity if $USER and $LOGNAME are absent; on macOS that lookup can fail with
// "No user exists for uid N" once the long-lived daemon's connection to
// opendirectoryd is no longer reachable. Forwarding these vars lets git use them
// directly and skip the system passwd lookup. See gh#zt1w.
func gitChildEnv() []string {
	env := os.Environ()
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			have[kv[:eq]] = true
		}
	}

	if have["HOME"] && have["USER"] && have["LOGNAME"] {
		return env
	}

	// Recover identity vars from os/user. user.Current() consults $USER/$HOME
	// before falling back to getpwuid; if all three are missing it may itself
	// fail, in which case we return env unchanged and let git error normally.
	u, err := user.Current()
	if err != nil {
		return env
	}
	if !have["HOME"] && u.HomeDir != "" {
		env = append(env, "HOME="+u.HomeDir)
	}
	if !have["USER"] && u.Username != "" {
		env = append(env, "USER="+u.Username)
	}
	if !have["LOGNAME"] && u.Username != "" {
		env = append(env, "LOGNAME="+u.Username)
	}
	return env
}

// hasGitRemote checks if the named remote exists in the git repo.
func (d *Daemon) hasGitRemote(gitRepo, name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "remote", "get-url", name)
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	return cmd.Run() == nil
}

// currentGitBranch returns the current branch name, or empty string on error.
func (d *Daemon) currentGitBranch(gitRepo string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

// runGitCmd runs a git command in the specified directory with the given timeout.
func (d *Daemon) runGitCmd(dir string, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			if errMsg := strings.TrimSpace(stderr.String()); errMsg != "" {
				return fmt.Errorf("%w after %s: %s", errGitCmdTimeout, timeout, errMsg)
			}
			return fmt.Errorf("%w after %s", errGitCmdTimeout, timeout)
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
	}
	return nil
}

// runGitIndexCmd runs a git command that reads or writes gitRepo's index,
// steering around the index lock a previous tick may have left behind. A tick
// whose `git add` is killed on its deadline cannot release that lock, and
// without this every later tick fails "index.lock: File exists" until a human
// removes it (gt-1aj2).
func (d *Daemon) runGitIndexCmd(gitRepo string, timeout time.Duration, args ...string) error {
	desc := "git " + strings.Join(args, " ")

	// Two attempts at most: the first may find a lock orphaned by an earlier
	// tick, the second runs only after this clears that lock.
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if d.clearStaleIndexLock(gitRepo, gitIndexLockGracePeriod) {
			d.logger.Printf("jsonl_git_backup: cleared stale %s before %s", gitIndexLockPath(gitRepo), desc)
		}

		started := time.Now()
		err = d.runGitCmd(gitRepo, timeout, args...)
		if err == nil {
			return nil
		}

		// A killed command leaves its own lock behind; clear it now so one slow
		// tick cannot fail the next two and escalate.
		if errors.Is(err, errGitCmdTimeout) {
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
// to its first line.
func escalationTitle(source, message string) string {
	summary := message
	if idx := strings.IndexByte(summary, '\n'); idx >= 0 {
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
	alertKeyMainBranchTest = "main_branch_test:failures"
	alertKeyJSONLInit      = "jsonl_git_backup:init"
	alertKeyJSONLNoDBs     = "jsonl_git_backup:no-databases"
	alertKeyJSONLScrub     = "jsonl_git_backup:scrub-suspicious"
	alertKeyJSONLSpike     = "jsonl_git_backup:spike"
	alertKeyJSONLPush      = "jsonl_git_backup:push"
)

// escalate raises an alert whose key is derived from its own title, for
// producers whose subject is already carried in the message (and for the
// escalation sinks other parts of the daemon inject as function values).
// Producers that know a stable class for their condition call escalateAlert
// with an explicit key instead.
func (d *Daemon) escalate(source, message string) {
	d.escalateAlert(escalationTitle(source, message), source, message)
}

// escalateAlert sends an escalation message to the mayor via gt escalate under
// the given alert key. message may be multiline (e.g. full go-test output); it
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
	title := escalationTitle(source, message)

	var lastErr error
	for attempt := 0; attempt < maxEscalationRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), defaultEscalationTimeout)
		cmd := exec.CommandContext(ctx, "gt", "escalate", "-s", "HIGH", "--fingerprint", key, "--stdin", title)
		cmd.Stdin = strings.NewReader(message)
		cmd.Dir = d.config.TownRoot
		cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
		util.SetDetachedProcessGroup(cmd)

		output, err := cmd.CombinedOutput()
		lastErr = err

		// Check for context deadline exceeded before cmd.Wait() may mask the
		// cause; cmd.Process is nil once the process exits, so we must check
		// the context first (gt-tlwv).
		if ctxErr := ctx.Err(); ctxErr == context.DeadlineExceeded {
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
				time.Sleep(backoff)
			}
			continue
		}
		cancel()

		if err == nil {
			return nil // success
		}

		// When a process is killed by signal (SIGKILL from CommandContext),
		// CombinedOutput() returns nil output, so the old "%v (%s)" format
		// printed "signal: killed ()" with empty parens.  Capture stderr
		// separately so signal-killed processes still show diagnostics.
		stderr := strings.TrimSpace(string(output))
		if stderr == "" {
			// CombinedOutput captures both stdout+stderr together; if it's
			// empty the process likely died before flushing anything.
			stderr = "<no output>"
		}
		errMsg := fmt.Sprintf("%s (%s)", err.Error(), stderr)

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
			time.Sleep(backoff)
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
	if len(keys) == 0 {
		return
	}

	args := []string{"escalate", "clear", "--reason", reason}
	for _, key := range keys {
		args = append(args, "--fingerprint", key)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultEscalationTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gt", args...)
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)

	if out, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("clearAlerts(%s): %v — %s", strings.Join(keys, ","), err, strings.TrimSpace(string(out)))
		return
	}
	d.logger.Printf("clearAlerts(%s): %s", strings.Join(keys, ","), reason)
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

// filterTestPollution removes test-data records from a JSONL byte buffer.
// Returns the filtered buffer and the number of records removed.
func filterTestPollution(data []byte) ([]byte, int) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Increase buffer for large JSONL lines.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var out bytes.Buffer
	removed := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record map[string]interface{}
		if err := json.Unmarshal(line, &record); err != nil {
			// Can't parse — keep it (don't silently drop unknown data).
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		if isTestPollution(record) {
			removed++
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), removed
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
// counts in the subject line ("backup <ts>: db1=N1 db2=N2"), so the last
// defaultSpikeBaselineWindow backup commits supply the window. This replaces
// the old git show HEAD:<relPath> read, which misjudged spikes whenever the
// committed file count diverged from what the detector expected.
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
func recomputeSpikeBaseline(gitRepo string) (*spikeBaseline, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	fetch := 64 // generous pool; well under any realistic backup repo's commits
	const sep = "\x1f"
	args := []string{"-C", gitRepo, "log", "--all", "--topo-order", "--max-count",
		strconv.Itoa(fetch), "--format=%H" + sep + "%cI" + sep + "%s"}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	type commitSubject struct {
		date   string
		counts map[string]int
	}
	var commits []commitSubject
	total := 0
	seen := map[string]bool{}
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, sep, 3)
		if len(fields) != 3 {
			continue
		}
		hash, ts, subject := fields[0], fields[1], fields[2]
		// Count every commit, not just the count-bearing ones: a repo with
		// zero commits is the bootstrap case (proceed), while commits that
		// carry no counts are unreadable history (halt) — see the tail below.
		total++
		counts := parseCommitCounts(subject)
		if len(counts) == 0 || seen[hash] {
			continue
		}
		seen[hash] = true
		date := ""
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			date = parsed.Format(time.RFC3339)
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
			window[db] = append(window[db], spikeCommit{Db: db, N: n, Age: i, Date: c.date, Source: spikeSourceCommit})
		}
	}
	for db := range window {
		sort.Slice(window[db], func(i, j int) bool { return window[db][i].Age < window[db][j].Age })
	}

	if len(window) == 0 {
		// No count-bearing history. The cache is the only remaining source of
		// levels, and it may only be trusted here, where no committed level
		// exists to outrank it.
		if sb := cachedSpikeBaseline(gitRepo); sb != nil {
			return sb, nil
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
	Window   int                 `json:"window"`
	Computed string              `json:"computed"`
	Counts   map[string][]spikeCommit `json:"counts"`
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

// parseCommitCounts extracts the "db=NNN" count pairs from a git commit's
// subject line. Backup commits are always titled
// "backup <timestamp>: db1=N1 db2=N2 [FAILED: ...]" (see commitAndPushJsonlBackup),
// so the subject is the only count record surviving an unpushed or halted
// cycle. Returns nil when no pairs are present.
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
// A database absent from the baseline window is a first export and is skipped.
func (d *Daemon) verifyExportCounts(gitRepo string, databases []string, counts map[string]int, threshold float64) ([]spikeInfo, error) {
	const minAbsoluteDelta = 20 // ignore changes smaller than this many records

	var spikes []spikeInfo
	spikeBase, err := recomputeSpikeBaseline(gitRepo)
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

		fractionalDelta := math.Abs(float64(currentCount-prevCount)) / float64(prevCount)

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

// countFileLines counts the number of non-empty lines in a file.
func countFileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			count++
		}
	}
	return count, scanner.Err()
}

// recountAfterFilter re-reads the issues.jsonl file for each database to get
// accurate post-filter line counts. This is needed because counts from
// exportDatabaseToJsonl reflect pre-filter totals.
func recountAfterFilter(gitRepo string, databases []string, counts map[string]int) {
	for _, db := range databases {
		if _, ok := counts[db]; !ok {
			continue
		}
		issuesPath := filepath.Join(gitRepo, db, "issues.jsonl")
		n, err := countFileLines(issuesPath)
		if err != nil {
			continue
		}
		counts[db] = n
	}
}

// applyPollutionFilter reads each database's issues.jsonl, filters out test
// pollution records, and rewrites the file. Returns total records removed.
func (d *Daemon) applyPollutionFilter(gitRepo string, databases []string) int {
	totalRemoved := 0
	for _, db := range databases {
		issuesPath := filepath.Join(gitRepo, db, "issues.jsonl")
		data, err := os.ReadFile(issuesPath)
		if err != nil {
			continue
		}
		filtered, removed := filterTestPollution(data)
		if removed > 0 {
			d.logger.Printf("jsonl_git_backup: %s: filtered %d test-pollution record(s)", db, removed)
			if err := os.WriteFile(issuesPath, filtered, 0644); err != nil {
				d.logger.Printf("jsonl_git_backup: %s: error writing filtered file: %v", db, err)
				continue
			}
			totalRemoved += removed
		}
	}
	return totalRemoved
}

// verifyNoPollution re-scans all exported issues.jsonl files for any remaining
// suspicious records that survived both the SQL scrub and the regex filter.
// Returns the total number of suspicious records found across all databases.
func (d *Daemon) verifyNoPollution(gitRepo string, databases []string) int {
	total := 0
	for _, db := range databases {
		issuesPath := filepath.Join(gitRepo, db, "issues.jsonl")
		data, err := os.ReadFile(issuesPath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var record map[string]interface{}
			if err := json.Unmarshal(line, &record); err != nil {
				continue
			}
			if isTestPollution(record) {
				id, _ := record["id"].(string)
				title, _ := record["title"].(string)
				d.logger.Printf("jsonl_git_backup: VERIFY FAIL: %s: suspicious record id=%q title=%q", db, id, title)
				total++
			}
		}
	}
	return total
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
