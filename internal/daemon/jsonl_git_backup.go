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

// syncJsonlGitBackup exports issues from each database to JSONL, scrubs ephemeral data,
// and commits/pushes to a git repository.
// Non-fatal: errors are logged but don't stop the daemon.
func (d *Daemon) syncJsonlGitBackup() {
	if !d.isPatrolActive("jsonl_git_backup") {
		return
	}

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

	// Phase D: Spike detection — compare current counts to previous commit.
	threshold := spikeThreshold(config)
	spikes := d.verifyExportCounts(gitRepo, databases, counts, threshold)
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

	// Note: DO NOT remove the spike baseline here. The baseline file (.spike-counts.json)
	// stores the actual previous export count, not the git HEAD count. If we remove it
	// after every commit, the next run will fall back to git show HEAD:<path> which may
	// read stale/transient counts (bug gt-qo0e). Instead, keep the baseline until it's
	// verified trustworthy (when the count is stable vs the baseline within threshold).

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

// previousCommitLineCount returns the line count of a file in the previous git
// commit (HEAD). Returns 0, nil if the file doesn't exist in HEAD (first export).
//
// This function first checks for a .spike-counts.json baseline file (which stores
// the actual previous run's count) and uses that as the authoritative baseline.
// Only if no baseline exists does it fall back to reading from git show HEAD:<path>.
// This prevents false spike alarms caused by git show reading stale/transient counts.
func previousCommitLineCount(gitRepo, relPath string) (int, error) {
	// First, check for a spike baseline file - this is the authoritative source
	// for the previous count, as it records what was actually exported last time.
	spikeBase := loadSpikeBaseline(gitRepo)
	if spikeBase != nil {
		// The baseline file stores counts with database names as keys.
		// The relPath is like "db/issues.jsonl", so extract "db" as the key.
		db := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(filepath.Base(relPath)))
		// Try the db name as key (e.g., "db" for path "db/issues.jsonl")
		if baseCount, ok := spikeBase.Counts[db]; ok && baseCount > 0 {
			return baseCount, nil
		}
		// Also try the issues.jsonl filename as a key (fallback)
		if baseCount, ok := spikeBase.Counts[filepath.Base(relPath)]; ok && baseCount > 0 {
			return baseCount, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "show", "HEAD:"+filepath.ToSlash(relPath))
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// stderr intentionally not captured — "does not exist" is an expected case.

	if err := cmd.Run(); err != nil {
		// File doesn't exist in HEAD — first export, no baseline.
		return 0, nil
	}

	lines := 0
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		lines++
	}
	return lines, nil
}

// spikeInfo holds the result of a spike check for a single database file.
type spikeInfo struct {
	DB       string
	File     string
	Previous int
	Current  int
	Delta    float64 // absolute fractional change (0.0–1.0+)
}

// spikeBaseline records counts from a halted export so that subsequent runs
// can detect when the count has stabilized at a new level.
type spikeBaseline struct {
	Counts    map[string]int `json:"counts"`
	Timestamp string         `json:"timestamp"`
}

const spikeBaselineFile = ".spike-counts.json"

// loadSpikeBaseline reads the spike baseline file from the git repo directory.
// Returns nil if the file doesn't exist or can't be parsed.
func loadSpikeBaseline(gitRepo string) *spikeBaseline {
	path := filepath.Join(gitRepo, spikeBaselineFile)
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

// saveSpikeBaseline writes the current counts as a spike baseline file.
// Also ensures the file is git-ignored so it doesn't get committed.
func saveSpikeBaseline(gitRepo string, counts map[string]int) error {
	sb := spikeBaseline{
		Counts:    counts,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return err
	}
	// Ensure the spike baseline file is git-ignored.
	ensureGitIgnore(gitRepo, spikeBaselineFile)
	return os.WriteFile(filepath.Join(gitRepo, spikeBaselineFile), data, 0644)
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

// removeSpikeBaseline removes the spike baseline file when the baseline is
// no longer needed (e.g., when counts have stabilized and the git HEAD count
// is now trustworthy). This should only be called when the baseline has been
// verified as stale, not after every successful commit.
func removeSpikeBaseline(gitRepo string) {
	os.Remove(filepath.Join(gitRepo, spikeBaselineFile))
}

// verifyExportCounts compares current export line counts against the previous
// commit for each database. Returns a list of anomalies that exceed the spike
// threshold. On first export (no baseline), verification is skipped.
//
// Asymmetric thresholds: drops (possible data loss) use the configured threshold;
// increases (new issues filed) use 2x the threshold since growth is normal.
// Small absolute changes (<20 records) are always allowed to avoid false alarms
// on small databases.
//
// Recovery mechanism: when spike detection fires, a baseline file is saved with
// the current counts. On the next run, if the current count is stable relative
// to the spike baseline (within threshold), the spike is cleared and the export
// proceeds. This prevents permanent blocking after legitimate large changes
// (e.g., Reaper purges, filter updates).
func (d *Daemon) verifyExportCounts(gitRepo string, databases []string, counts map[string]int, threshold float64) []spikeInfo {
	const minAbsoluteDelta = 20 // ignore changes smaller than this many records

	var spikes []spikeInfo
	spikeBase := loadSpikeBaseline(gitRepo)

	for _, db := range databases {
		currentCount, ok := counts[db]
		if !ok {
			continue // database failed export, skip
		}

		relPath := filepath.Join(db, "issues.jsonl")
		prevCount, err := previousCommitLineCount(gitRepo, relPath)
		if err != nil {
			d.logger.Printf("jsonl_git_backup: verify: %s: error reading baseline: %v", db, err)
			continue
		}
		if prevCount == 0 {
			// First export — no baseline to compare against.
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
			// Check spike baseline: if the current count is stable relative
			// to a previously-halted count, this is a confirmed new level.
			if spikeBase != nil {
				if baseCount, ok := spikeBase.Counts[db]; ok && baseCount > 0 {
					baseDelta := math.Abs(float64(currentCount-baseCount)) / float64(baseCount)
					if baseDelta <= threshold {
						d.logger.Printf("jsonl_git_backup: %s: count stable vs spike baseline (%d → %d, %.1f%% vs baseline %d), accepting new level",
							db, prevCount, currentCount, fractionalDelta*100, baseCount)
						continue // Stable relative to spike baseline — not a new spike.
					}
				}
			}

			spike := spikeInfo{
				DB:       db,
				File:     relPath,
				Previous: prevCount,
				Current:  currentCount,
				Delta:    fractionalDelta,
			}
			spikes = append(spikes, spike)

			direction := "jump"
			if currentCount < prevCount {
				direction = "drop"
			}
			d.logger.Printf("jsonl_git_backup: SPIKE DETECTED: %s: %s from %d to %d (%.1f%% %s, threshold %.1f%%)",
				db, direction, prevCount, currentCount, fractionalDelta*100, direction, effectiveThreshold*100)
		}
	}

	// Save or clear spike baseline depending on results.
	if len(spikes) > 0 {
		if err := saveSpikeBaseline(gitRepo, counts); err != nil {
			d.logger.Printf("jsonl_git_backup: failed to save spike baseline: %v", err)
		}
	}

	return spikes
}

// formatSpikeReport creates a human-readable summary of spike anomalies for escalation.
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
