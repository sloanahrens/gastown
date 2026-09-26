// Package reaper provides wisp and issue cleanup operations for Dolt databases.
//
// These functions are the "callable helper functions" for the Dog-driven
// mol-dog-reaper formula. They execute SQL operations but do not make
// eligibility decisions — the Dog (or daemon orchestrator) decides what
// to reap, purge, and auto-close based on the formula.
package reaper

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// validDBName matches safe database names (alphanumeric, underscore, hyphen).
var validDBName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// DefaultDatabases is the static fallback list of known production databases.
// Used only when SHOW DATABASES fails (server unreachable).
// GH#2385: Removed legacy "gt" and "bd" names — modern towns use "hq" (town
// beads) and rig-specific names. Those databases no longer exist in most
// installations and their presence in the fallback caused phantom DB errors.
var DefaultDatabases = []string{"hq"}

// testPollutionPrefixes are database name prefixes created by tests.
var testPollutionPrefixes = []string{"testdb_", "beads_t", "beads_pt", "doctest_", "dolt_remotes_check_"}

// isNothingToCommit returns true if the error is a Dolt "nothing to commit" error.
func isNothingToCommit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "nothing to commit")
}

// isTableNotFound returns true if the error indicates a missing table.
// This happens when beads stores its data on a separate Dolt instance from
// the gt Dolt server, so tables like issues/labels/dependencies don't exist
// on the server the reaper connects to.
func isTableNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "table not found") || strings.Contains(msg, "doesn't exist")
}

// DiscoverDatabases queries SHOW DATABASES on the Dolt server and returns
// all production databases, filtering out system databases and test pollution.
// Falls back to DefaultDatabases on any error.
func DiscoverDatabases(host string, port int) []string {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/?parseTime=true&timeout=5s", host, port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return DefaultDatabases
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return DefaultDatabases
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if name == "information_schema" || name == "mysql" {
			continue
		}
		lower := strings.ToLower(name)
		skip := false
		for _, prefix := range testPollutionPrefixes {
			if strings.HasPrefix(lower, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		databases = append(databases, name)
	}

	if len(databases) == 0 {
		return DefaultDatabases
	}
	return databases
}

// ScanResult holds the results of scanning a database for reaper candidates.
type ScanResult struct {
	Database               string `json:"database"`
	ReapCandidates         int    `json:"reap_candidates"`
	MoleculeStepCandidates int    `json:"molecule_step_candidates,omitempty"`
	PurgeCandidates        int    `json:"purge_candidates"`
	MailCandidates         int    `json:"mail_candidates"`
	StaleCandidates        int    `json:"stale_candidates"`
	// Floored reports that the stale-age asked for sat below MinStaleIssueAge,
	// so StaleCandidates is the count at that threshold — the set auto-close's
	// refusal names for the same flag — rather than at the floor.
	Floored bool `json:"floored,omitempty"`
	// FlooredAt is the stale-age that was asked for when Floored is set.
	FlooredAt              time.Duration `json:"floored_at,omitempty"`
	AbsentParentCandidates int           `json:"absent_parent_candidates,omitempty"`
	OpenWisps              int           `json:"open_wisps"`
	Anomalies              []Anomaly     `json:"anomalies,omitempty"`
}

// ReapResult holds the results of a reap operation.
type ReapResult struct {
	Database            string    `json:"database"`
	Reaped              int       `json:"reaped"`
	MoleculeStepsClosed int       `json:"molecule_steps_closed,omitempty"`
	AbsentParentClosed  int       `json:"absent_parent_closed,omitempty"`
	OpenRemain          int       `json:"open_remain"`
	DryRun              bool      `json:"dry_run,omitempty"`
	Anomalies           []Anomaly `json:"anomalies,omitempty"`
}

// PurgeResult holds the results of a purge operation.
type PurgeResult struct {
	Database    string    `json:"database"`
	WispsPurged int       `json:"wisps_purged"`
	MailPurged  int       `json:"mail_purged"`
	DryRun      bool      `json:"dry_run,omitempty"`
	Anomalies   []Anomaly `json:"anomalies,omitempty"`
}

// ClosedEntry records an individual issue closure with details for logging.
type ClosedEntry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	AgeDays  int    `json:"age_days"`
	Database string `json:"database"`
}

// AutoCloseResult holds the results of an auto-close operation.
type AutoCloseResult struct {
	Database      string        `json:"database"`
	Closed        int           `json:"closed"`
	ClosedEntries []ClosedEntry `json:"closed_entries,omitempty"`
	DryRun        bool          `json:"dry_run,omitempty"`
	// MaxCloses is the per-run cap that applied to this operation.
	MaxCloses int `json:"max_closes,omitempty"`
	// PreviewHash fingerprints the candidate set this operation selected. A dry
	// run returns it so the caller can hand it back to the live run
	// (AutoCloseOptions.PreviewHash) — the preview authorizes the write, and
	// this is the token that carries the authorization (gt-39bu).
	PreviewHash string `json:"preview_hash,omitempty"`
	// Floored reports that the caller asked for a stale-age below
	// MinStaleIssueAge: the sweep refused, so Closed is 0 and ClosedEntries
	// holds the set that threshold would take rather than the floor's.
	Floored bool `json:"floored,omitempty"`
	// FlooredAt is the stale-age that was asked for when Floored is set.
	FlooredAt time.Duration `json:"floored_at,omitempty"`
	Anomalies []Anomaly     `json:"anomalies,omitempty"`
}

// AutoCloseOptions parameterizes an AutoClose sweep.
type AutoCloseOptions struct {
	// StaleAge is how long an issue may sit untouched before it is stale.
	// Values below MinStaleIssueAge are refused as a soft error — AutoClose
	// reports the set this threshold would take, closes nothing, and returns
	// ErrStaleAgeTooLow — unless Force is set.
	StaleAge time.Duration
	// DryRun reports candidates without writing.
	DryRun bool
	// MaxCloses caps how many issues one call may close. Zero means
	// DefaultAutoCloseMaxPerRun. Exceeding it refuses the whole run unless
	// Force is set.
	MaxCloses int
	// PreviewHash authorizes a live run: it is the AutoCloseResult.PreviewHash a
	// preceding dry run returned for this exact candidate set. A live run
	// without it, or with a hash for a different set, refuses and closes
	// nothing (gt-39bu). Force lifts it.
	PreviewHash string
	// Force lifts the StaleAge floor, the MaxCloses cap, and the PreviewHash
	// requirement. Reserved for a human operator who has looked at the
	// candidate list.
	Force bool
}

// Anomaly represents an unexpected condition found during reaper operations.
type Anomaly struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

const (
	// DefaultQueryTimeout is the timeout for individual reaper SQL queries.
	DefaultQueryTimeout = 30 * time.Second
	// DefaultBatchSize is the number of rows per batch DELETE operation.
	DefaultBatchSize = 100
	// DefaultAlertThreshold is the open-wisp count above which callers should
	// surface a warning. This must fire on genuine runaway accumulation, NOT on
	// normal operation. The open-wisp count is dominated by healthy, recent
	// wisps (observed steady-state ~1966 open in a busy town); actionable wisps
	// are limited to stale open-parent-free wisps past max-age plus closed
	// molecule-step wisps (typically ~15-25). The previous value of 800 sat below
	// the healthy open count, so it false-alarmed HIGH every scan despite nothing
	// being wrong. Raised to 3000 so the alert tracks runaway growth rather than
	// the normal working set. See hq-57jr8.
	DefaultAlertThreshold = 3000

	// DefaultMRProtectionTTL caps how long a cleanup wisp stays protected from
	// age-based reaping by its state:merge-requested label, measured from when
	// the state was last set (wisp_events label_set row, else created_at).
	// A merge the refinery handles lands in minutes, so 24h of silence is "MR
	// genuinely lost". Callers pass 0 to mrProtectedJoin for unbounded
	// protection (gt-apam).
	DefaultMRProtectionTTL = 24 * time.Hour

	// MinStaleIssueAge is the shortest stale-age AutoClose will act on without
	// Force. The 2026-09-16 reaper incident ran at 7d, which was short enough to
	// sweep eight-day-old agent beads; anything below a week cannot distinguish
	// "abandoned" from "waiting its turn" in a town that moves this fast.
	// A below-floor request is refused instead, and the refusal reports the set
	// the threshold asked for would take (gt-ecpj): a shorter age reaches
	// further back, so the floor's own set is the smaller one, and reporting it
	// would hide the blast radius an operator has to see before passing Force.
	// Both results carry that refusal in a Floored flag rather than a
	// FlooredAt > 0 test, which would read a requested threshold of zero as
	// "not floored". See gt-2qzr.
	MinStaleIssueAge = 7 * 24 * time.Hour

	// DefaultAutoCloseMaxPerRun caps how many issues a single AutoClose call
	// may close. A run that exceeds it refuses outright rather than closing a
	// prefix of the list. The incident run closed 102 beads across four
	// databases in one cycle; a cap this side of that turns a mis-set threshold
	// into a refusal an operator can read. See gt-2qzr.
	DefaultAutoCloseMaxPerRun = 20
)

var (
	// ErrStaleAgeTooLow is a soft error: AutoClose is asked below
	// MinStaleIssueAge without Force, so it refuses the write while reporting
	// the set the threshold would take in the result it returns alongside this
	// error (gt-ecpj). Refusal and candidates ride along in the same call, so a
	// caller reports them and exits 0 rather than stopping the cycle. Force
	// lifts the floor.
	ErrStaleAgeTooLow = errors.New("stale-age below minimum")
	// ErrTooManyCloses is returned when a sweep would exceed its per-run cap
	// without Force. No issues are closed when this is returned.
	ErrTooManyCloses = errors.New("auto-close candidate count exceeds cap")
	// ErrPreviewRequired is returned when a live sweep arrives with no preview
	// hash, so the set it would close was never counted. No issues are closed
	// when this is returned.
	ErrPreviewRequired = errors.New("auto-close live run without a preview")
	// ErrPreviewMismatch is returned when the preview hash names a different
	// candidate set than the live run's: the sweep would close something the
	// dry run never showed. No issues are closed when this is returned.
	ErrPreviewMismatch = errors.New("auto-close preview does not match the candidate set")
)

// FloorNotice is the operator-facing sentence for a below-floor stale-age
// refusal: the threshold that was asked for, the floor it sat under, and how
// many candidates that threshold would take (gt-ecpj). Callers prefix it with
// the database and print it wherever they report the refusal.
//
// candidates is the asked-for threshold's count, not the floor's: the floor
// takes the smaller set, so its count would understate what --force authorizes.
func FloorNotice(askedFor time.Duration, candidates int) string {
	return fmt.Sprintf("stale-age %s is below the %s floor (pass --force to override); nothing closed — reporting the %d candidate(s) that threshold would take",
		askedFor, MinStaleIssueAge, candidates)
}

// PreviewHash fingerprints the candidate set a sweep is about to close: the
// database, the stale-age that selected it, and the issue ids, sorted so query
// order cannot change the hash.
//
// A hash the dry run produces and the live run demands is what makes "count
// first, then close" structural instead of an instruction order the caller can
// invert (gt-39bu).
func PreviewHash(dbName string, staleAge time.Duration, ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)

	h := sha256.New()
	// NUL separators keep the fields apart, so no two different (db, age, ids)
	// triples can hash alike — "a"+"b" never encodes the same as "ab"+"".
	fmt.Fprintf(h, "db=%s\x00stale_age=%s\x00ids=%s", dbName, staleAge, strings.Join(sorted, "\x00"))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ValidateDBName returns an error if the database name is unsafe.
func ValidateDBName(dbName string) error {
	if !validDBName.MatchString(dbName) {
		return fmt.Errorf("invalid database name: %q", dbName)
	}
	return nil
}

// OpenDB opens a connection to the Dolt server for a given database.
func OpenDB(host string, port int, dbName string, readTimeout, writeTimeout time.Duration) (*sql.DB, error) {
	if err := ValidateDBName(dbName); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=%s&writeTimeout=%s",
		host, port, dbName,
		fmt.Sprintf("%ds", int(readTimeout.Seconds())),
		fmt.Sprintf("%ds", int(writeTimeout.Seconds())))
	return sql.Open("mysql", dsn)
}

// parentExcludeJoin returns a LEFT JOIN clause and WHERE condition that restricts
// results to wisps whose parent molecule is closed, missing, or nonexistent.
//
// This replaces the previous parentCheckWhere() which used 3 correlated EXISTS
// subqueries per row, causing O(n*m) query cost on large wisp tables (gt-jd1z).
// The LEFT JOIN approach runs the subquery once and hash-joins: O(n+m).
//
// Semantics (unchanged from parentCheckWhere):
//   - No parent-child dependency → eligible (orphan wisps)
//   - Parent status is 'closed' → eligible (parent already reaped)
//   - Parent row missing (dangling ref) → eligible (parent already purged)
//
// The inverse is simpler: exclude wisps that have an OPEN parent.
//
// Usage:
//
//	join, where := parentExcludeJoin(dbName)
//	query := fmt.Sprintf("SELECT ... FROM wisps w %s WHERE ... AND %s", dbName, join, where)
func parentExcludeJoin(dbName string) (joinClause, whereCondition string) {
	joinClause = `LEFT JOIN (
		SELECT DISTINCT wd.issue_id
		FROM wisp_dependencies wd
		LEFT JOIN wisps pw ON pw.id = wd.depends_on_wisp_id LEFT JOIN issues pi ON pi.id = wd.depends_on_issue_id
		WHERE wd.type = 'parent-child'
		AND (pw.status IN ('open', 'hooked', 'in_progress') OR pi.status IN ('open', 'hooked', 'in_progress') OR wd.depends_on_external IS NOT NULL)
	) open_parent ON open_parent.issue_id = w.id`
	whereCondition = "open_parent.issue_id IS NULL"
	return
}

const openWispStatusWhere = "w.status IN ('open', 'hooked', 'in_progress')"

// cleanupWispProtectedJoin is the half of mrProtectedJoin that protects
// cleanup wisps tracking an outstanding MR: labels cleanup +
// state:merge-requested. The protection lapses maxProtection after the state
// was last set — wisp_events row (event_type='label_set') when one exists,
// the wisp's created_at otherwise — so a wisp whose MERGED signal never
// arrived becomes reapable instead of immortal (gt-apam). The caller supplies
// the cutoff; the clause renders it as a literal so the TTL is one bind per
// query, not one per protected wisp.
//
// Like parentExcludeJoin, the two return values are a LEFT JOIN clause and a
// WHERE-clause fragment: the caller MUST introduce the WHERE keyword itself
// before appending whereCondition. Appending it straight onto the last JOIN's
// ON clause instead is a silent no-op — a LEFT JOIN never drops a left-side
// row over its own ON predicate — which let every wisp through as
// "protected" and disabled all age-based reaping (gt-apam rejection).
func cleanupWispProtectedJoin(maxProtection time.Duration, cutoff time.Time) (joinClause, whereCondition string) {
	joinClause = `LEFT JOIN (
		SELECT issue_id FROM wisp_labels WHERE label = 'cleanup'
	) wisp_cleanup ON wisp_cleanup.issue_id = w.id
	LEFT JOIN (
		SELECT issue_id, MAX(created_at) AS last_requested_at
		FROM wisp_events
		WHERE event_type = 'label_set' AND old_value = 'state:merge-requested'
		GROUP BY issue_id
	) mr_requested ON mr_requested.issue_id = w.id
	LEFT JOIN (
		SELECT issue_id FROM wisp_labels WHERE label = 'state:merge-requested'
	) mr_label ON mr_label.issue_id = w.id`

	whereCondition = "wisp_cleanup.issue_id IS NOT NULL AND mr_label.issue_id IS NOT NULL"
	if maxProtection > 0 {
		whereCondition += fmt.Sprintf(
			" AND COALESCE(mr_requested.last_requested_at, w.created_at) >= '%s'",
			cutoff.Format("2006-01-02 15:04:05"))
	}
	return joinClause, whereCondition
}

// mrProtectedJoin returns a LEFT JOIN clause and WHERE condition that excludes
// wisps actively participating in the merge queue from age-based reaping:
//   - MR wisps (label gt:merge-request): beads status doesn't distinguish
//     ready/claimed/mid-batch phases, so any open MR wisp must be protected
//     regardless of age.
//   - cleanup wisps tracking an outstanding MR (labels cleanup +
//     state:merge-requested).
//
// A dormant town (e.g. Dolt down for days) can leave a genuinely live MR
// older than max-age, so age alone must never be sufficient to close these
// classes (gt-4okk: reaper age-sweep closed a live MR wisp during a 5-day
// Dolt outage, and the witness gc purged it irrecoverably two minutes later).
//
// The cleanup-wisp branch lapses maxProtection after the state was last set
// (gt-apam). The TTL cutoff renders as a SQL literal, not a bind arg, so the
// caller passes cutoff = now - maxProtection and keeps binding only the age
// cutoff and the live-reference IDs.
func mrProtectedJoin(maxProtection time.Duration, cutoff time.Time) (joinClause, whereCondition string) {
	cleanupJoin, cleanupWhere := cleanupWispProtectedJoin(maxProtection, cutoff)
	joinClause = `LEFT JOIN (
		SELECT DISTINCT issue_id FROM wisp_labels WHERE label = 'gt:merge-request'
		UNION
		SELECT w.id FROM wisps w
		` + cleanupJoin + `
		WHERE ` + cleanupWhere + `
		AND w.status IN ('open', 'hooked', 'in_progress')
	) mr_protected ON mr_protected.issue_id = w.id`
	whereCondition = "mr_protected.issue_id IS NULL"
	return
}

var activeMRFieldPattern = regexp.MustCompile(`(?m)^active_mr:\s*(\S+)\s*$`)
var hookBeadFieldPattern = regexp.MustCompile(`(?m)^hook_bead:\s*(\S+)\s*$`)
var agentStateFieldPattern = regexp.MustCompile(`(?m)^agent_state:\s*(\S+)\s*$`)

// agentBeadWispQuery and agentBeadIssueQuery locate agent beads in the two stores
// they can occupy. Identity is the 'gt:agent' label: agent beads are created as
// issue_type='task' (internal/beads/beads_agent.go), so a type of 'agent' matches
// none of them, and their durable home is the issues table — wisps holds copies
// the migration in internal/doltserver/wisps_migrate.go made. Reading the type
// from wisps alone matched zero rows in every rig database, leaving the
// reference protection below disarmed (gt-l6y9).
const (
	agentBeadWispQuery  = `SELECT description FROM wisps WHERE issue_type = 'agent' OR id IN (SELECT issue_id FROM wisp_labels WHERE label = 'gt:agent')`
	agentBeadIssueQuery = `SELECT description FROM issues WHERE issue_type = 'agent' OR id IN (SELECT issue_id FROM labels WHERE label = 'gt:agent')`
)

// liveAgentReferencedWispIDs returns the set of wisp IDs a live (non-nuked) agent
// bead still names as its active_mr or hook_bead. Both are durable pointers into
// the wisp tables, so a sweep that removes the target leaves the reference
// dangling and unreconcilable — the polecat then waits on work that cannot be
// looked up (gt-gyb6, gt-4okk). The protection lapses when the pointer does: on
// the agent's next MR, on hook release, or on nuke. Both agent-bead stores are
// read (agentBeadWispQuery, agentBeadIssueQuery).
func liveAgentReferencedWispIDs(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	protected := make(map[string]bool)

	if err := collectAgentReferences(ctx, db, agentBeadWispQuery, protected); err != nil {
		return nil, err
	}

	// A database whose issues table is absent is tolerated here the way Scan
	// tolerates it for its mail count: beads may keep that table on a separate
	// Dolt instance.
	if err := collectAgentReferences(ctx, db, agentBeadIssueQuery, protected); err != nil && !isTableNotFound(err) {
		return nil, err
	}
	return protected, nil
}

// collectAgentReferences adds to protected every wisp ID named as active_mr or
// hook_bead by an agent bead in the rows query returns.
func collectAgentReferences(ctx context.Context, db *sql.DB, query string, protected map[string]bool) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query agent beads for references: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var description string
		if err := rows.Scan(&description); err != nil {
			return fmt.Errorf("scan agent bead description: %w", err)
		}
		// A nuked agent is gone; its pointers stop holding wisps open.
		if m := agentStateFieldPattern.FindStringSubmatch(description); m != nil && m[1] == "nuked" {
			continue
		}
		for _, pattern := range []*regexp.Regexp{activeMRFieldPattern, hookBeadFieldPattern} {
			if m := pattern.FindStringSubmatch(description); m != nil && m[1] != "null" {
				protected[m[1]] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read agent bead descriptions: %w", err)
	}
	return nil
}

// wispExcludeClause renders a parameterized "AND w.id NOT IN (...)" clause
// for the wisp IDs in ids, plus its bind args in the same order. Returns ("", nil)
// when ids is empty so callers can append the result unconditionally.
func wispExcludeClause(ids map[string]bool) (clause string, args []interface{}) {
	if len(ids) == 0 {
		return "", nil
	}
	placeholders := make([]string, 0, len(ids))
	args = make([]interface{}, 0, len(ids))
	for id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	clause = fmt.Sprintf(" AND w.id NOT IN (%s)", strings.Join(placeholders, ","))
	return clause, args
}

// closedMoleculeStepSubquery selects step-wisps whose parent molecule has already closed.
// wisp_dependencies.issue_id is the child; depends_on_wisp_id is the parent molecule.
const closedMoleculeStepSubquery = `
	SELECT DISTINCT wd.issue_id
	FROM wisp_dependencies wd
	INNER JOIN wisps pm ON pm.id = wd.depends_on_wisp_id
	WHERE wd.type = 'parent-child'
	AND pm.issue_type = 'molecule'
	AND pm.status = 'closed'
	AND NOT EXISTS (
		SELECT 1 FROM wisp_dependencies open_dep
		LEFT JOIN wisps open_pw ON open_pw.id = open_dep.depends_on_wisp_id
		LEFT JOIN issues open_pi ON open_pi.id = open_dep.depends_on_issue_id
		WHERE open_dep.issue_id = wd.issue_id
		AND open_dep.type = 'parent-child'
		AND (open_pw.status IN ('open', 'hooked', 'in_progress') OR open_pi.status IN ('open', 'hooked', 'in_progress') OR open_dep.depends_on_external IS NOT NULL)
	)`

func closedMoleculeStepJoin(alias string) string {
	return fmt.Sprintf("INNER JOIN (%s) %s ON %s.issue_id = w.id", closedMoleculeStepSubquery, alias, alias)
}

func closedMoleculeStepExcludeJoin(alias string) string {
	return fmt.Sprintf("LEFT JOIN (%s) %s ON %s.issue_id = w.id", closedMoleculeStepSubquery, alias, alias)
}

type sqlRunner interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

// HasReaperSchema checks whether the database has the tables required for reaper
// operations (wisps and issues). Returns false (no error) when tables are missing
// — callers use this to skip databases that have incomplete beads schema (e.g.
// partially initialized databases on the central Dolt server).
func HasReaperSchema(db *sql.DB) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('wisps', 'issues', 'wisp_dependencies') AND table_schema = DATABASE()").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check reaper schema: %w", err)
	}
	if count < 3 {
		return false, nil
	}

	hasWispDependencyColumns, err := hasColumns(ctx, db, "wisp_dependencies", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external")
	if err != nil || !hasWispDependencyColumns {
		return hasWispDependencyColumns, err
	}
	dependenciesExists, err := tableExists(ctx, db, "dependencies")
	if err != nil || !dependenciesExists {
		return !dependenciesExists, err
	}
	return hasColumns(ctx, db, "dependencies", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external")
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_name = ? AND table_schema = DATABASE()", table).Scan(&count)
	return count > 0, err
}

func hasColumns(ctx context.Context, db *sql.DB, table string, columns ...string) (bool, error) {
	if len(columns) == 0 {
		return true, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(columns)), ",")
	args := make([]interface{}, 0, len(columns)+1)
	args = append(args, table)
	for _, column := range columns {
		args = append(args, column)
	}
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name IN (%s)", placeholders)
	err := db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count == len(columns), err
}

// Scan counts reaper candidates in a database without modifying anything.
func Scan(db *sql.DB, dbName string, maxAge, purgeAge, mailDeleteAge, staleIssueAge time.Duration) (*ScanResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	result := &ScanResult{Database: dbName}
	now := time.Now().UTC()
	parentJoin, parentWhere := parentExcludeJoin(dbName)
	moleculeStepJoin := closedMoleculeStepJoin("closed_molecule_step")
	moleculeStepExcludeJoin := closedMoleculeStepExcludeJoin("closed_molecule_step")
	mrJoin, mrWhere := mrProtectedJoin(DefaultMRProtectionTTL, now.Add(-DefaultMRProtectionTTL))

	moleculeStepQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM wisps w %s WHERE %s AND w.issue_type != 'agent'",
		moleculeStepJoin, openWispStatusWhere)
	if err := db.QueryRowContext(ctx, moleculeStepQuery).Scan(&result.MoleculeStepCandidates); err != nil {
		return nil, fmt.Errorf("count molecule step candidates: %w", err)
	}

	// Count reap candidates: open wisps past max_age with eligible parent status.
	// Must match Reap() eligibility semantics exactly, including the exclusion of
	// agent beads and live merge-queue wisps, otherwise scan can report candidates
	// that reap will never close.
	// Uses LEFT JOIN anti-pattern instead of correlated EXISTS to avoid O(n*m) cost (gt-jd1z).
	// Closed-molecule steps are counted separately above and excluded here so counts stay disjoint.
	referencedIDs, err := liveAgentReferencedWispIDs(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("compute live agent referenced ids: %w", err)
	}
	referencedClause, referencedArgs := wispExcludeClause(referencedIDs)
	reapQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM wisps w %s %s %s WHERE %s AND w.created_at < ? AND w.issue_type != 'agent' AND %s AND closed_molecule_step.issue_id IS NULL AND %s%s",
		parentJoin, moleculeStepExcludeJoin, mrJoin, openWispStatusWhere, parentWhere, mrWhere, referencedClause)
	reapArgs := []interface{}{now.Add(-maxAge)}
	reapArgs = append(reapArgs, referencedArgs...)
	if err := db.QueryRowContext(ctx, reapQuery, reapArgs...).Scan(&result.ReapCandidates); err != nil {
		return nil, fmt.Errorf("count reap candidates: %w", err)
	}

	// Count purge candidates: closed wisps past purge_age.
	// No parent check needed — closed wisps past the delete age are purgeable except
	// for the ones a live agent bead still references, which Purge() also spares.
	// The parent check (correlated subqueries on wisp_dependencies) was causing O(n*m) query
	// cost with 1800+ closed wisps, leading to CPU spikes and connection timeouts (gt-wvd2).
	purgeQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?%s",
		referencedClause)
	purgeArgs := append([]interface{}{now.Add(-purgeAge)}, referencedArgs...)
	if err := db.QueryRowContext(ctx, purgeQuery, purgeArgs...).Scan(&result.PurgeCandidates); err != nil {
		return nil, fmt.Errorf("count purge candidates: %w", err)
	}

	// Count mail candidates.
	// The issues/labels tables may not exist on the gt Dolt server if beads
	// stores its data on a separate Dolt instance. Skip gracefully.
	mailQuery := "SELECT COUNT(*) FROM issues WHERE status = 'closed' AND closed_at < ? AND id IN (SELECT issue_id FROM labels WHERE label = 'gt:message')"
	if err := db.QueryRowContext(ctx, mailQuery, now.Add(-mailDeleteAge)).Scan(&result.MailCandidates); err != nil {
		if !isTableNotFound(err) {
			return nil, fmt.Errorf("count mail candidates: %w", err)
		}
		// issues/labels table not on this server — skip mail count
	}

	// Count stale issue candidates using the SAME eligibility clause as
	// AutoClose (gt-2qzr). Scan is the dog's preview of the sweep; if the two
	// diverge the count cannot be used to gate the write.
	// Same caveat: issues/dependencies tables may live on a separate Dolt instance.
	if staleIssueAge < MinStaleIssueAge {
		// Same threshold AutoClose refuses on, and the same choice of set:
		// the count stays at the threshold asked for so it can be read against
		// the refusal for the same flag.
		result.Floored = true
		result.FlooredAt = staleIssueAge
	}
	staleQuery := "SELECT COUNT(*) FROM issues i WHERE " + staleIssueEligibilityClause("")
	if err := db.QueryRowContext(ctx, staleQuery, now.Add(-staleIssueAge)).Scan(&result.StaleCandidates); err != nil {
		if !isTableNotFound(err) {
			return nil, fmt.Errorf("count stale candidates: %w", err)
		}
		// issues/dependencies table not on this server — skip stale count
	}

	// Total open wisps.
	openQuery := "SELECT COUNT(*) FROM wisps WHERE status IN ('open', 'hooked', 'in_progress')"
	if err := db.QueryRowContext(ctx, openQuery).Scan(&result.OpenWisps); err != nil {
		return nil, fmt.Errorf("count open wisps: %w", err)
	}

	// Anomaly detection: dangling parent references.
	danglingQuery := `
		SELECT COUNT(*) FROM wisp_dependencies wd
		LEFT JOIN wisps pw ON pw.id = wd.depends_on_wisp_id LEFT JOIN issues pi ON pi.id = wd.depends_on_issue_id
		WHERE wd.type = 'parent-child' AND wd.depends_on_external IS NULL AND (wd.depends_on_wisp_id IS NOT NULL OR wd.depends_on_issue_id IS NOT NULL) AND pw.id IS NULL AND pi.id IS NULL`
	var danglingCount int
	if err := db.QueryRowContext(ctx, danglingQuery).Scan(&danglingCount); err == nil && danglingCount > 0 {
		result.Anomalies = append(result.Anomalies, Anomaly{
			Type:    "dangling_parent_ref",
			Message: fmt.Sprintf("%d wisp(s) have parent dependency records pointing to purged/missing parents", danglingCount),
			Count:   danglingCount,
		})
		// Also count these as absent-parent candidates for the reaper's preview.
		// The Reap function will close these wisps.
		result.AbsentParentCandidates = danglingCount
	}

	return result, nil
}

// Reap closes stale wisps in a database whose parent molecule is already closed.
// UPDATEs are batched to avoid holding a write lock for extended periods on large tables.
func Reap(db *sql.DB, dbName string, maxAge time.Duration, dryRun bool) (*ReapResult, error) {
	// Use a longer timeout to accommodate batched processing across large tables.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	now := time.Now().UTC()
	cutoff := now.Add(-maxAge)
	parentJoin, parentWhere := parentExcludeJoin(dbName)
	moleculeStepJoin := closedMoleculeStepJoin("closed_molecule_step")
	moleculeStepExcludeJoin := closedMoleculeStepExcludeJoin("closed_molecule_step")
	mrJoin, mrWhere := mrProtectedJoin(DefaultMRProtectionTTL, now.Add(-DefaultMRProtectionTTL))
	referencedIDs, err := liveAgentReferencedWispIDs(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("compute live agent referenced ids: %w", err)
	}
	referencedClause, referencedArgs := wispExcludeClause(referencedIDs)
	// Exclude agent beads (issue_type='agent') from reaping — they have persistent
	// identity and should not be closed by the wisp reaper regardless of age.
	// Exclude live merge-queue wisps (mrWhere) and any wisp a live agent still
	// references as active_mr or hook_bead (referencedClause) — age is never
	// sufficient on its own to close these, since a dormant town can make a live MR
	// look stale (gt-4okk).
	// Closed-molecule steps are closed immediately through a separate path, so stale
	// max-age counts exclude them to keep dry-run and scan counts disjoint.
	whereClause := fmt.Sprintf(
		"%s AND w.created_at < ? AND w.issue_type != 'agent' AND %s AND closed_molecule_step.issue_id IS NULL AND %s%s",
		openWispStatusWhere, parentWhere, mrWhere, referencedClause)
	whereArgs := []interface{}{cutoff}
	whereArgs = append(whereArgs, referencedArgs...)

	// absentParentJoin and absentParentWhere are used for both dry-run counting
	// and real execution. Define them once here.
	absentParentJoin := `LEFT JOIN wisp_dependencies wd ON wd.issue_id = w.id
		LEFT JOIN wisps pm ON pm.id = wd.depends_on_wisp_id
		LEFT JOIN issues pi ON pi.id = wd.depends_on_issue_id`
	absentParentWhere := fmt.Sprintf("wd.type = 'parent-child' AND wd.depends_on_external IS NULL AND pm.id IS NULL AND pi.id IS NULL AND NOT EXISTS ("+
		"  SELECT 1 FROM wisp_dependencies open_dep"+
		"  LEFT JOIN wisps open_pw ON open_pw.id = open_dep.depends_on_wisp_id"+
		"  LEFT JOIN issues open_pi ON open_pi.id = open_dep.depends_on_issue_id"+
		"  WHERE open_dep.issue_id = wd.issue_id"+
		"  AND open_dep.type = 'parent-child'"+
		"  AND (open_pw.status IN ('open', 'hooked', 'in_progress') OR open_pi.status IN ('open', 'hooked', 'in_progress') OR open_dep.depends_on_external IS NOT NULL)"+
		") AND w.issue_type != 'agent' AND %s", openWispStatusWhere)

	result := &ReapResult{Database: dbName, DryRun: dryRun}

	if dryRun {
		moleculeStepCountQuery := fmt.Sprintf(
			"SELECT COUNT(*) FROM wisps w %s WHERE %s AND w.issue_type != 'agent'",
			moleculeStepJoin, openWispStatusWhere)
		if err := db.QueryRowContext(ctx, moleculeStepCountQuery).Scan(&result.MoleculeStepsClosed); err != nil {
			return nil, fmt.Errorf("dry-run molecule step count: %w", err)
		}

		// Count absent-parent candidates: open wisps whose parent molecule record is purged.
		absentParentCountQuery := fmt.Sprintf(
			"SELECT COUNT(*) FROM wisps w %s WHERE %s AND w.issue_type != 'agent'",
			absentParentJoin, absentParentWhere)
		if err := db.QueryRowContext(ctx, absentParentCountQuery).Scan(&result.AbsentParentClosed); err != nil {
			return nil, fmt.Errorf("dry-run absent-parent count: %w", err)
		}

		countQuery := fmt.Sprintf("SELECT COUNT(*) FROM wisps w %s %s %s WHERE %s", parentJoin, moleculeStepExcludeJoin, mrJoin, whereClause)
		if err := db.QueryRowContext(ctx, countQuery, whereArgs...).Scan(&result.Reaped); err != nil {
			return nil, fmt.Errorf("dry-run count: %w", err)
		}
		openQuery := "SELECT COUNT(*) FROM wisps WHERE status IN ('open', 'hooked', 'in_progress')"
		if err := db.QueryRowContext(ctx, openQuery).Scan(&result.OpenRemain); err != nil {
			return nil, fmt.Errorf("count open: %w", err)
		}
		return result, nil
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("pin connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return nil, fmt.Errorf("disable autocommit: %w", err)
	}
	sqlCommitted := false
	defer func() {
		if !sqlCommitted {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_, _ = conn.ExecContext(context.Background(), "SET @@autocommit = 1")
	}()

	moleculeStepIDQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w %s WHERE %s AND w.issue_type != 'agent' LIMIT %d",
		moleculeStepJoin, openWispStatusWhere, DefaultBatchSize)
	moleculeStepsClosed, err := closeWispsInBatches(ctx, conn, moleculeStepIDQuery, nil, "closed molecule steps")
	if err != nil {
		return nil, err
	}
	result.MoleculeStepsClosed = moleculeStepsClosed

	// Close wisps whose parent molecule record was purged (absent parent).
	// These are molecule step-wisps that the reaper should reap but the closedMoleculeStep
	// subquery misses because INNER JOIN wisps pm requires the parent row to exist.
	// Uses the same pattern as Scan's danglingQuery: both wisp and issue parents must be absent.
	absentParentIDQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w %s WHERE %s LIMIT %d",
		absentParentJoin, absentParentWhere, DefaultBatchSize)
	absentParentClosed, err := closeWispsInBatches(ctx, conn, absentParentIDQuery, nil, "absent-parent molecule steps")
	if err != nil {
		return nil, err
	}

	// Batch UPDATE: select IDs in chunks, update each chunk.
	// This avoids holding a write lock on the entire table for minutes.
	// Uses LEFT JOIN anti-pattern instead of correlated EXISTS to avoid O(n*m) cost (gt-jd1z).
	idQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w %s %s %s WHERE %s LIMIT %d",
		parentJoin, moleculeStepExcludeJoin, mrJoin, whereClause, DefaultBatchSize)

	totalReaped, err := closeWispsInBatches(ctx, conn, idQuery, whereArgs, "stale wisps")
	if err != nil {
		return nil, err
	}

	result.Reaped = totalReaped
	totalClosed := totalReaped + moleculeStepsClosed + absentParentClosed

	if totalClosed > 0 {
		// Flush the SQL transaction to the Dolt working set before DOLT_COMMIT.
		// With autocommit=0, UPDATE changes are in the SQL transaction buffer,
		// not the Dolt working set. DOLT_COMMIT operates on the working set,
		// so without this COMMIT it sees "nothing to commit".
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return result, fmt.Errorf("sql commit: %w", err)
		}
		sqlCommitted = true
		commitMsg := fmt.Sprintf("reaper: close %d wisps in %s", totalClosed, dbName)
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			// "nothing to commit" is expected when the reaper reverts dirty working
			// set changes back to match HEAD. The wisps were set to "open" in the
			// server's in-memory working set without being committed; closing them
			// makes the working set match HEAD again, so DOLT_COMMIT sees no diff.
			if !isNothingToCommit(err) {
				return result, fmt.Errorf("dolt commit: %w", err)
			}
		}
	}

	openQuery := "SELECT COUNT(*) FROM wisps WHERE status IN ('open', 'hooked', 'in_progress')"
	if err := conn.QueryRowContext(ctx, openQuery).Scan(&result.OpenRemain); err != nil {
		return result, fmt.Errorf("count open: %w", err)
	}

	return result, nil
}

func closeWispsInBatches(ctx context.Context, runner sqlRunner, idQuery string, queryArgs []interface{}, description string) (int, error) {
	total := 0
	for {
		rows, err := runner.QueryContext(ctx, idQuery, queryArgs...)
		if err != nil {
			return total, fmt.Errorf("select %s batch: %w", description, err)
		}

		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, fmt.Errorf("scan %s id: %w", description, err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("read %s ids: %w", description, err)
		}
		rows.Close()

		if len(ids) == 0 {
			return total, nil
		}

		placeholders := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")

		updateQuery := fmt.Sprintf(
			"UPDATE wisps SET status='closed', closed_at=NOW() WHERE id IN (%s) AND status IN ('open', 'hooked', 'in_progress') AND issue_type != 'agent'",
			inClause)
		sqlResult, err := runner.ExecContext(ctx, updateQuery, args...)
		if err != nil {
			return total, fmt.Errorf("close %s batch: %w", description, err)
		}

		affected, _ := sqlResult.RowsAffected()
		total += int(affected)
	}
}

// Purge deletes old closed wisps and mail from a database.
func Purge(db *sql.DB, dbName string, purgeAge, mailDeleteAge time.Duration, dryRun bool) (*PurgeResult, error) {
	result := &PurgeResult{Database: dbName, DryRun: dryRun}

	// Purge closed wisps.
	purged, anomalies, err := purgeClosedWisps(db, dbName, purgeAge, dryRun)
	if err != nil {
		return nil, fmt.Errorf("purge wisps: %w", err)
	}
	result.WispsPurged = purged
	result.Anomalies = append(result.Anomalies, anomalies...)

	// Purge old mail.
	mailPurged, err := purgeOldMail(db, dbName, mailDeleteAge, dryRun)
	if err != nil {
		return result, fmt.Errorf("purge mail: %w", err)
	}
	result.MailPurged = mailPurged

	return result, nil
}

func purgeClosedWisps(db *sql.DB, dbName string, purgeAge time.Duration, dryRun bool) (int, []Anomaly, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	deleteCutoff := time.Now().UTC().Add(-purgeAge)
	var anomalies []Anomaly

	// A wisp a live agent bead still points at is spared: deleting it strands the
	// pointer where no lookup can resolve it, which is the wedge — not the size of
	// the table — that parks the polecat (gt-gyb6). The close path already spares
	// these, so without this the same sweep deletes what the reap just protected.
	referencedIDs, err := liveAgentReferencedWispIDs(ctx, db)
	if err != nil {
		return 0, nil, fmt.Errorf("compute live agent referenced ids: %w", err)
	}
	referencedClause, referencedArgs := wispExcludeClause(referencedIDs)

	// Digest: count by wisp_type.
	// No parent check — closed wisps past the delete age are purgeable.
	// The parent check (correlated subqueries on wisp_dependencies) was causing O(n*m)
	// query cost with 1800+ closed wisps, leading to CPU spikes and timeouts (gt-wvd2).
	digestQuery := fmt.Sprintf(
		"SELECT COALESCE(w.wisp_type, 'unknown') AS wtype, COUNT(*) AS cnt FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?%s GROUP BY wtype",
		referencedClause) //nolint:gosec // G201: referencedClause is a parameterized NOT IN clause, never a value
	digestArgs := append([]interface{}{deleteCutoff}, referencedArgs...)
	rows, err := db.QueryContext(ctx, digestQuery, digestArgs...)
	if err != nil {
		return 0, nil, fmt.Errorf("digest query: %w", err)
	}
	digestTotal := 0
	for rows.Next() {
		var wtype string
		var cnt int
		if err := rows.Scan(&wtype, &cnt); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("digest scan: %w", err)
		}
		digestTotal += cnt
	}
	rows.Close()

	if digestTotal == 0 {
		return 0, anomalies, nil
	}

	if dryRun {
		return digestTotal, anomalies, nil
	}

	if _, err := db.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return 0, nil, fmt.Errorf("disable autocommit: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "SET @@autocommit = 1")
	}()

	// Batch delete — status+age filter, plus the live-reference exclusion above.
	idQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?%s LIMIT %d",
		referencedClause, DefaultBatchSize)
	auxTables := []string{"wisp_labels", "wisp_comments", "wisp_events", "wisp_dependencies"}

	totalDeleted, err := batchDeleteRows(ctx, db, idQuery, deleteCutoff, "wisps", auxTables, referencedArgs...)
	if err != nil {
		return totalDeleted, anomalies, err
	}

	if totalDeleted > 0 {
		// Flush SQL transaction to working set before DOLT_COMMIT.
		if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
			anomalies = append(anomalies, Anomaly{
				Type:    "sql_commit_failed",
				Message: fmt.Sprintf("sql commit after purge failed: %v", err),
			})
			return totalDeleted, anomalies, nil
		}
		commitMsg := fmt.Sprintf("reaper: purge %d closed wisps from %s", totalDeleted, dbName)
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('--allow-empty', '-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			// Non-fatal — log but continue.
			anomalies = append(anomalies, Anomaly{
				Type:    "dolt_commit_failed",
				Message: fmt.Sprintf("dolt commit after purge failed: %v", err),
			})
		}
	}

	return totalDeleted, anomalies, nil
}

func purgeOldMail(db *sql.DB, dbName string, mailDeleteAge time.Duration, dryRun bool) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	mailCutoff := time.Now().UTC().Add(-mailDeleteAge)

	countQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s`.issues WHERE status = 'closed' AND closed_at < ? AND id IN (SELECT issue_id FROM `%s`.labels WHERE label = 'gt:message')",
		dbName, dbName)
	var count int
	if err := db.QueryRowContext(ctx, countQuery, mailCutoff).Scan(&count); err != nil {
		if isTableNotFound(err) {
			return 0, nil // issues/labels not on this server
		}
		return 0, fmt.Errorf("count mail: %w", err)
	}
	if count == 0 {
		return 0, nil
	}

	if dryRun {
		return count, nil
	}

	if _, err := db.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return 0, fmt.Errorf("disable autocommit: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "SET @@autocommit = 1")
	}()

	idQuery := fmt.Sprintf(
		"SELECT i.id FROM `%s`.issues i INNER JOIN `%s`.labels l ON i.id = l.issue_id WHERE i.status = 'closed' AND i.closed_at < ? AND l.label = 'gt:message' LIMIT %d",
		dbName, dbName, DefaultBatchSize)
	auxTables := []string{"labels", "comments", "events", "dependencies"}

	totalDeleted, err := batchDeleteRows(ctx, db, idQuery, mailCutoff, "issues", auxTables)
	if err != nil {
		return totalDeleted, err
	}

	if totalDeleted > 0 {
		// Flush SQL transaction to working set before DOLT_COMMIT.
		if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
			return totalDeleted, fmt.Errorf("sql commit: %w", err)
		}
		commitMsg := fmt.Sprintf("reaper: purge %d old mail from %s", totalDeleted, dbName)
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('--allow-empty', '-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			// Non-fatal.
		}
	}

	return totalDeleted, nil
}

// staleIssueEligibilityClause is the single definition of which open issues
// are eligible for staleness auto-close, shared by Scan (candidate counting)
// and AutoClose (the write). Scan is the dog's preview of the sweep, so the
// two MUST agree; when they diverged the dog could not gate the write on the
// count it had just produced (gt-2qzr).
//
// dbQualifier is "" when the connection is already scoped to one database
// (Scan) or the backtick-quoted database name plus a dot when the statement
// resolves tables explicitly (AutoClose).
//
// Exclusions, and the incident each one answers:
//   - P0/P1 (priority > 1): incidents and criticals are never "stale".
//   - Infrastructure issue types (gt-2qzr): molecule beads ARE the patrol and
//     work molecules — closing one detaches live agent lifecycle. rig/agent are
//     standing identities, event/convoy/epic are lifecycle containers whose
//     retirement is status-driven, never staleness-driven (hq-jnap for convoys).
//   - Protection and runtime labels (gt-2qzr): gt:agent marks every mayor,
//     deacon, dog, witness, refinery, crew, and polecat bead. They are idle by
//     design, so staleness is meaningless for them, and closing one breaks
//     `gt agents resolve` for that role. The rest of the list mirrors
//     beads.ProtectedIssueLabel plus beads.InternalIssueLabel, and covers
//     plugin receipts (type:plugin-run).
//   - Agent id patterns (gt-2qzr): defense in depth for an agent bead that
//     somehow lost its label. `gt agents list` resolves these roles by id.
//   - Active dependency edges: an issue that is blocked by, or blocks, an open
//     issue is still in play.
func staleIssueEligibilityClause(dbQualifier string) string {
	table := func(name string) string { return dbQualifier + name }
	return fmt.Sprintf(`
		i.status IN ('open', 'in_progress')
		AND i.updated_at < ?
		AND i.priority > 1
		AND i.issue_type NOT IN ('epic', 'convoy', 'molecule', 'rig', 'agent', 'event')
		AND i.id NOT IN (
			SELECT DISTINCT l.issue_id FROM %s l
			WHERE l.label IN (
				'gt:standing-orders', 'gt:keep', 'gt:role', 'gt:rig', 'gt:agent',
				'gt:wisp', 'gt:message', 'gt:handoff', 'gt:merge-request',
				'gt:queue', 'gt:convoy', 'gt:formula', 'type:plugin-run'
			)
		)
		AND i.id NOT LIKE '%%-witness' AND i.id NOT LIKE '%%-refinery'
		AND i.id NOT LIKE '%%-mayor' AND i.id NOT LIKE '%%-deacon'
		AND i.id NOT LIKE '%%-crew-%%' AND i.id NOT LIKE '%%-polecat-%%'
		AND i.id NOT LIKE '%%-dog-%%' AND i.id NOT LIKE '%%-dogs'
		AND i.id NOT IN (
			SELECT DISTINCT d.issue_id FROM %s d
			INNER JOIN %s dep ON d.depends_on_issue_id = dep.id
			WHERE dep.status IN ('open', 'in_progress')
		)
		AND i.id NOT IN (
			SELECT DISTINCT d.depends_on_issue_id FROM %s d
			INNER JOIN %s blocker ON d.issue_id = blocker.id
			WHERE d.depends_on_issue_id IS NOT NULL
			AND blocker.status IN ('open', 'in_progress')
		)`,
		table("labels"), table("dependencies"), table("issues"),
		table("dependencies"), table("issues"))
}

// AutoClose closes issues that have been open with no updates past
// opts.StaleAge. Eligibility is defined by staleIssueEligibilityClause.
//
// Three brakes guard against a mis-parameterized sweep (gt-2qzr, where a
// hardcoded 7d threshold closed 102 beads, including every agent bead in the
// town): a live run may not close more than opts.MaxCloses issues and must
// carry the preview hash of the set a dry run produced, both hard refusals —
// nothing is closed when they trip. A below-floor stale-age is a soft refusal
// (gt-ecpj): nothing is closed either, but the call reports the set the
// threshold would take alongside ErrStaleAgeTooLow, so the caller gets the
// operator notice without the cycle halting on exit status. All three are
// lifted by opts.Force.
func AutoClose(db *sql.DB, dbName string, opts AutoCloseOptions) (*AutoCloseResult, error) {
	// A below-floor threshold refuses, and the refusal is the whole output:
	// nothing is closed, so what the caller has to act on is how much the
	// threshold would have taken. opts.StaleAge is left as asked for — the
	// query runs at it — rather than pulled up to the floor, whose smaller set
	// would hide exactly the blast radius the refusal is for.
	maxCloses := opts.MaxCloses
	if maxCloses <= 0 {
		maxCloses = DefaultAutoCloseMaxPerRun
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	staleCutoff := time.Now().UTC().Add(-opts.StaleAge)
	result := &AutoCloseResult{Database: dbName, DryRun: opts.DryRun, MaxCloses: maxCloses}
	result.Floored = opts.StaleAge < MinStaleIssueAge && !opts.Force
	if result.Floored {
		// Recorded before the dry-run return below, so a preview of a
		// below-floor threshold is flagged for its caller too.
		result.FlooredAt = opts.StaleAge
	}

	whereClause := staleIssueEligibilityClause("`" + dbName + "`.")

	// Two-step SELECT-then-UPDATE to avoid self-referencing subquery in UPDATE,
	// which is not valid MySQL (Error 1093) and fragile in Dolt (dolthub/dolt#10600).
	selectQuery := fmt.Sprintf("SELECT i.id, i.title, i.updated_at FROM issues i WHERE %s", whereClause)
	rows, err := db.QueryContext(ctx, selectQuery, staleCutoff)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil // issues/dependencies not on this server
		}
		return nil, fmt.Errorf("select stale: %w", err)
	}
	type candidate struct {
		id        string
		title     string
		updatedAt time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.title, &c.updatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stale id: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	// Build per-issue closure log entries.
	now := time.Now().UTC()
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.id
		result.ClosedEntries = append(result.ClosedEntries, ClosedEntry{
			ID:       c.id,
			Title:    c.title,
			AgeDays:  int(now.Sub(c.updatedAt).Hours() / 24),
			Database: dbName,
		})
	}

	result.PreviewHash = PreviewHash(dbName, opts.StaleAge, ids)

	if opts.DryRun {
		result.Closed = len(ids)
		return result, nil
	}

	if result.Floored {
		// Ahead of the empty check: the refusal is about the threshold, not
		// about what is stale today. Ahead of the preview guard too — nothing
		// is closed, so there is no write for a preview to authorize.
		return result, fmt.Errorf("%w: %s", ErrStaleAgeTooLow, FloorNotice(result.FlooredAt, len(ids)))
	}

	if len(ids) == 0 {
		// Nothing to close, so there is no write for a preview to authorize.
		return result, nil
	}

	// Counting has to precede closing however the caller ordered its commands:
	// a set the caller cannot show a preview for is not authorized, and neither
	// is one that moved since the preview (gt-39bu).
	if !opts.Force && opts.PreviewHash != result.PreviewHash {
		if opts.PreviewHash == "" {
			return result, fmt.Errorf(
				"%w: dry-run first, then pass its preview hash (--preview=%s) to close exactly this set",
				ErrPreviewRequired, result.PreviewHash)
		}
		return result, fmt.Errorf(
			"%w: preview %s, current set %s (%d candidates); re-run the dry run",
			ErrPreviewMismatch, opts.PreviewHash, result.PreviewHash, len(ids))
	}

	// Refuse-and-report rather than close-then-apologize: a sweep this large
	// means the threshold is wrong or a filter regressed, and the stop is the
	// designed signal (see the formula's Scope Boundary). Nothing is closed,
	// but the candidate list rides along so the operator can inspect the
	// refusal instead of re-running to see what it would have taken.
	if len(ids) > maxCloses && !opts.Force {
		return result, fmt.Errorf("%w: %d candidates in %s exceeds the per-run cap of %d (pass --force to override)",
			ErrTooManyCloses, len(ids), dbName, maxCloses)
	}

	if _, err := db.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return nil, fmt.Errorf("disable autocommit: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "SET @@autocommit = 1")
	}()

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	updateQuery := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', closed_at = NOW(), close_reason = 'stale:auto-closed by reaper' WHERE id IN (%s)",
		dbName, strings.Join(placeholders, ","))
	if _, err := db.ExecContext(ctx, updateQuery, args...); err != nil {
		return nil, fmt.Errorf("auto-close: %w", err)
	}

	result.Closed = len(ids)

	if len(ids) > 0 {
		// Flush SQL transaction to working set before DOLT_COMMIT.
		if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
			result.Anomalies = append(result.Anomalies, Anomaly{
				Type:    "sql_commit_failed",
				Message: fmt.Sprintf("sql commit after auto-close failed: %v", err),
			})
			return result, nil
		}
		commitMsg := fmt.Sprintf("reaper: auto-close %d stale issues in %s", len(ids), dbName)
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			// "nothing to commit" is expected when the updated tables are dolt_ignored.
			if !isNothingToCommit(err) {
				result.Anomalies = append(result.Anomalies, Anomaly{
					Type:    "dolt_commit_failed",
					Message: fmt.Sprintf("dolt commit after auto-close failed: %v", err),
				})
			}
		}
	}

	return result, nil
}

// batchDeleteRows deletes rows from a primary table and its auxiliary tables in batches.
// extraArgs bind the query's remaining placeholders after the age cutoff.
func batchDeleteRows(ctx context.Context, db *sql.DB, idQuery string, cutoffArg time.Time, primaryTable string, auxTables []string, extraArgs ...interface{}) (int, error) {
	queryArgs := append([]interface{}{cutoffArg}, extraArgs...)
	totalDeleted := 0
	for {
		idRows, err := db.QueryContext(ctx, idQuery, queryArgs...)
		if err != nil {
			return totalDeleted, fmt.Errorf("select batch: %w", err)
		}

		var ids []string
		for idRows.Next() {
			var id string
			if err := idRows.Scan(&id); err != nil {
				idRows.Close()
				return totalDeleted, fmt.Errorf("scan id: %w", err)
			}
			ids = append(ids, id)
		}
		idRows.Close()

		if len(ids) == 0 {
			break
		}

		placeholders := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := "(" + strings.Join(placeholders, ",") + ")"

		for _, tbl := range auxTables {
			delAux := fmt.Sprintf("DELETE FROM `%s` WHERE issue_id IN %s", tbl, inClause) //nolint:gosec // G201: tbl is internal
			if _, err := db.ExecContext(ctx, delAux, args...); err != nil {
				// Non-fatal: log and continue.
			}
		}

		// Clean up typed reverse dependency references to prevent dangling parent refs.
		var reverseDeletes []string
		switch primaryTable {
		case "wisps":
			reverseDeletes = []string{
				fmt.Sprintf("DELETE FROM wisp_dependencies WHERE depends_on_wisp_id IN %s", inClause),
				fmt.Sprintf("DELETE FROM dependencies WHERE depends_on_wisp_id IN %s", inClause),
			}
		case "issues":
			reverseDeletes = []string{
				fmt.Sprintf("DELETE FROM wisp_dependencies WHERE depends_on_issue_id IN %s", inClause),
				fmt.Sprintf("DELETE FROM dependencies WHERE depends_on_issue_id IN %s", inClause),
			}
		}
		for _, delReverse := range reverseDeletes {
			if _, err := db.ExecContext(ctx, delReverse, args...); err != nil {
				// Non-fatal.
			}
		}

		delPrimary := fmt.Sprintf("DELETE FROM `%s` WHERE id IN %s", primaryTable, inClause) //nolint:gosec // G201: primaryTable is internal
		sqlResult, err := db.ExecContext(ctx, delPrimary, args...)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete %s batch: %w", primaryTable, err)
		}
		affected, _ := sqlResult.RowsAffected()
		totalDeleted += int(affected)
	}

	return totalDeleted, nil
}

// ClosePluginReceiptResult holds the results of closing plugin run receipts.
type ClosePluginReceiptResult struct {
	Database  string    `json:"database"`
	Closed    int       `json:"closed"`
	DryRun    bool      `json:"dry_run,omitempty"`
	Anomalies []Anomaly `json:"anomalies,omitempty"`
}

// ClosePluginReceipts closes open issues labeled "type:plugin-run" that are
// older than maxAge. These are transient run receipts created by deacon dog
// plugins; they should be closed shortly after creation since they exist only
// for audit/cooldown-gate purposes. The standard AutoClose path requires 7 days
// of staleness, which lets plugin receipts accumulate into the hundreds.
func ClosePluginReceipts(db *sql.DB, dbName string, maxAge time.Duration, dryRun bool) (*ClosePluginReceiptResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-maxAge)
	result := &ClosePluginReceiptResult{Database: dbName, DryRun: dryRun}

	// Find open issues with the "type:plugin-run" label older than maxAge.
	selectQuery := fmt.Sprintf(`
		SELECT i.id FROM `+"`%s`"+`.issues i
		INNER JOIN `+"`%s`"+`.labels l ON i.id = l.issue_id
		WHERE i.status IN ('open', 'in_progress')
		AND l.label = 'type:plugin-run'
		AND i.created_at < ?`, dbName, dbName)

	rows, err := db.QueryContext(ctx, selectQuery, cutoff)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil
		}
		return nil, fmt.Errorf("select plugin receipts: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan plugin receipt id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	result.Closed = len(ids)
	if len(ids) == 0 || dryRun {
		return result, nil
	}

	if _, err := db.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return nil, fmt.Errorf("disable autocommit: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "SET @@autocommit = 1")
	}()

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	updateQuery := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', closed_at = NOW() WHERE id IN (%s)",
		dbName, strings.Join(placeholders, ","))
	if _, err := db.ExecContext(ctx, updateQuery, args...); err != nil {
		return nil, fmt.Errorf("close plugin receipts: %w", err)
	}

	// Flush and commit.
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		result.Anomalies = append(result.Anomalies, Anomaly{
			Type:    "sql_commit_failed",
			Message: fmt.Sprintf("sql commit after plugin receipt close failed: %v", err),
		})
		return result, nil
	}
	commitMsg := fmt.Sprintf("reaper: close %d plugin receipts in %s", len(ids), dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
		if !isNothingToCommit(err) {
			result.Anomalies = append(result.Anomalies, Anomaly{
				Type:    "dolt_commit_failed",
				Message: fmt.Sprintf("dolt commit after plugin receipt close failed: %v", err),
			})
		}
	}

	return result, nil
}

// ClosePluginDispatches closes open dispatch mail beads created by the daemon
// when sending plugin instructions to dogs. These beads are labeled "gt:message"
// + "from:daemon" with a title prefix "Plugin:" and are never closed after the
// dog completes. Without this, they accumulate at ~288/day (one per 5-minute
// stuck-agent-dog run) and are only caught by AutoClose after 7 days.
func ClosePluginDispatches(db *sql.DB, dbName string, maxAge time.Duration, dryRun bool) (*ClosePluginReceiptResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-maxAge)
	result := &ClosePluginReceiptResult{Database: dbName, DryRun: dryRun}

	// Find open issues with both "gt:message" and "from:daemon" labels whose
	// title starts with "Plugin:", older than maxAge.
	selectQuery := fmt.Sprintf(`
		SELECT i.id FROM `+"`%s`"+`.issues i
		INNER JOIN `+"`%s`"+`.labels l1 ON i.id = l1.issue_id
		INNER JOIN `+"`%s`"+`.labels l2 ON i.id = l2.issue_id
		WHERE i.status IN ('open', 'in_progress')
		AND l1.label = 'gt:message'
		AND l2.label = 'from:daemon'
		AND i.title LIKE 'Plugin:%%'
		AND i.created_at < ?`, dbName, dbName, dbName)

	rows, err := db.QueryContext(ctx, selectQuery, cutoff)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil
		}
		return nil, fmt.Errorf("select plugin dispatches: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan plugin dispatch id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	result.Closed = len(ids)
	if len(ids) == 0 || dryRun {
		return result, nil
	}

	if _, err := db.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return nil, fmt.Errorf("disable autocommit: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "SET @@autocommit = 1")
	}()

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	updateQuery := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', closed_at = NOW() WHERE id IN (%s)",
		dbName, strings.Join(placeholders, ","))
	if _, err := db.ExecContext(ctx, updateQuery, args...); err != nil {
		return nil, fmt.Errorf("close plugin dispatches: %w", err)
	}

	// Flush and commit.
	if _, err := db.ExecContext(ctx, "COMMIT"); err != nil {
		result.Anomalies = append(result.Anomalies, Anomaly{
			Type:    "sql_commit_failed",
			Message: fmt.Sprintf("sql commit after plugin dispatch close failed: %v", err),
		})
		return result, nil
	}
	commitMsg := fmt.Sprintf("reaper: close %d plugin dispatches in %s", len(ids), dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
		if !isNothingToCommit(err) {
			result.Anomalies = append(result.Anomalies, Anomaly{
				Type:    "dolt_commit_failed",
				Message: fmt.Sprintf("dolt commit after plugin dispatch close failed: %v", err),
			})
		}
	}

	return result, nil
}

// FormatJSON marshals any value to indented JSON.
func FormatJSON(v interface{}) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return string(data)
}
