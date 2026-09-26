package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var historyJSON bool

var historyCmd = &cobra.Command{
	Use:     "history <bead-id>",
	GroupID: GroupDiag,
	Short:   "Report how far back a bead's recorded history goes, and whether it is truncated",
	Long: `Report how far back a bead's recorded history actually goes.

bd history <id> prints one snapshot of the issue per retained Dolt commit, and
Dolt commit history is truncated whenever a database is flattened (gt maintain,
gt dolt flatten, compactor-dog). bd history does not disclose that: a bead whose
snapshots begin at the flatten reads exactly like a bead whose snapshots begin
at its creation, so a field showing one value looks like a field that never
changed. Absence before the floor is not evidence of absence.

gt history names the floor and gives the verdict:

  - where the bead's commit-snapshot history starts (the dolt_log floor, the
    oldest commit after the repository root that a flatten leaves behind),
  - whether that is after the bead was created, i.e. truncated,
  - whether the bead's row is in a table Dolt ignores (a wisp), in which case no
    commit snapshots it, so neither verdict applies and the floor is silent,
  - how many rows survive in the events table, which a flatten preserves, and
    whether they reach back past the floor,
  - the command that reads them.

Examples:
  gt history gt-abc123          # Human-readable verdict
  gt history gt-abc123 --json   # Machine-readable report`,
	Args: cobra.ExactArgs(1),
	RunE: runHistory,
}

func init() {
	historyCmd.Flags().BoolVar(&historyJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(historyCmd)
}

// historyReport is the machine-readable form of the verdict.
type historyReport struct {
	BeadID   string `json:"bead_id"`
	Database string `json:"database"`

	BeadCreated string `json:"bead_created_at,omitempty"`
	BeadTable   string `json:"bead_table,omitempty"`

	// Versioned is false when the bead's row lives in a dolt_ignored table. No
	// commit then carries a snapshot of it, so Truncated stays false for the
	// opposite reason to a complete history: there was never a snapshot to lose.
	Versioned bool `json:"versioned"`

	FloorAt      string `json:"dolt_log_floor_at,omitempty"`
	FloorCommit  string `json:"dolt_log_floor_commit,omitempty"`
	FloorMessage string `json:"dolt_log_floor_message,omitempty"`
	Commits      int64  `json:"dolt_log_commits"`

	// Truncated is true when the commit snapshots start after the bead was
	// created, so bd history silently omits part of the bead's life.
	Truncated   bool   `json:"truncated"`
	MissingFrom string `json:"missing_snapshots_since,omitempty"`
	MissingSpan string `json:"missing_snapshots_span,omitempty"`

	Events            int64  `json:"events"`
	EventsEarliest    string `json:"events_earliest_at,omitempty"`
	EventsLatest      string `json:"events_latest_at,omitempty"`
	EventsUnavailable bool   `json:"events_unavailable,omitempty"`
	// EventsReachFloor is true when the events table still holds rows from
	// before the floor, i.e. the span the commit snapshots lost.
	EventsReachFloor bool `json:"events_reach_past_floor"`

	RecoveryCommand string `json:"recovery_command"`
}

// historySnapshot is a bead's stored creation time and the table it was read
// from: issues for ordinary beads, wisps for the ephemeral ones.
type historySnapshot struct {
	Created time.Time
	Table   string
	Found   bool
}

// historyEventSpan summarizes a bead's rows in the events table.
type historyEventSpan struct {
	Count    int64
	Earliest time.Time
	Latest   time.Time
	Found    bool
}

// historyFloor is the oldest retained commit that can carry a bead snapshot.
// Beads created before it have no commit snapshot of their early life; see
// floorFrom for why it is not simply the oldest commit in dolt_log.
type historyFloor struct {
	At      time.Time
	Commit  string
	Message string
	Found   bool
}

// historyQuerier is the read surface the verdict needs. Narrowing it to these
// five facts (rather than passing *sql.DB) keeps the report testable without a
// Dolt server and keeps the SQL that follows the schema in one place.
type historyQuerier interface {
	floor(ctx context.Context, dbName string) (historyFloor, error)
	commitCount(ctx context.Context, dbName string) (int64, error)
	beadCreated(ctx context.Context, dbName, beadID string) (historySnapshot, error)
	eventSpan(ctx context.Context, dbName, beadID string) (historyEventSpan, error)
	tableVersioned(ctx context.Context, dbName, table string) (bool, error)
}

// doltQuerier reads history facts from a live Dolt server.
type doltQuerier struct{ db *sql.DB }

// snapshotSlack is how much later than a row's own timestamp its commit may be
// dated before the gap counts as a missing snapshot rather than the same write.
//
// A commit is dated after the rows it writes, so a bead created in a still-
// retained commit has created_at <= floor date within one write. Without this
// slack every bead created in the oldest retained commit would read as
// truncated, which is the failure mode in the other direction: a confident
// claim of loss where the snapshot is right there. A flatten removes whole days,
// so nothing of the size that matters here hides inside the slack.
const snapshotSlack = 5 * time.Minute

// dbNameRe guards the one place a database name is interpolated into SQL. Every
// name reaching it comes from metadata.json rather than from user input, but a
// name that is not a plain identifier would break the query rather than be
// harmless, so it is rejected explicitly.
var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// historyCommit is one row of dolt_log, oldest first as the query returns them.
type historyCommit struct {
	Hash    string
	At      time.Time
	Message string
}

// floorFrom picks the truncation floor out of dolt_log ordered oldest-first.
// The first entry is the repository ROOT, which every flatten preserves: a
// flatten soft-resets to the root and then recommits, so the root stays the
// oldest commit forever and is the one commit that is never truncated. It also
// predates the schema, so no bead row exists in it. Everything a flatten
// discarded sat between the root and the next commit, which is why that next
// commit is the floor a caller comparing against a bead's creation date needs.
//
// Returning the root instead reports "history is complete" on exactly the
// databases that have been flattened — the failure this command exists to fix.
// A database whose only commit is the root has no floor: nothing beyond the
// empty root is retained, and Found stays false.
func floorFrom(commits []historyCommit) historyFloor {
	if len(commits) < 2 {
		return historyFloor{}
	}
	second := commits[1]
	return historyFloor{
		At:      second.At,
		Commit:  second.Hash,
		Message: second.Message,
		Found:   true,
	}
}

func (q doltQuerier) floor(ctx context.Context, dbName string) (historyFloor, error) {
	// The two oldest commits are all the floor needs; dolt_log itself can hold
	// thousands, and the rest are never read.
	query := fmt.Sprintf(
		"SELECT commit_hash, date, message FROM `%s`.dolt_log ORDER BY date ASC, commit_hash ASC LIMIT 2", dbName)
	rows, err := q.db.QueryContext(ctx, query)
	if err != nil {
		return historyFloor{}, fmt.Errorf("reading dolt_log floor: %w", err)
	}
	defer rows.Close()

	var commits []historyCommit
	for rows.Next() {
		var (
			commit  string
			date    time.Time
			message sql.NullString
		)
		if err := rows.Scan(&commit, &date, &message); err != nil {
			return historyFloor{}, fmt.Errorf("reading dolt_log floor: %w", err)
		}
		commits = append(commits, historyCommit{Hash: commit, At: date, Message: message.String})
	}
	if err := rows.Err(); err != nil {
		return historyFloor{}, fmt.Errorf("reading dolt_log floor: %w", err)
	}
	return floorFrom(commits), nil
}

func (q doltQuerier) commitCount(ctx context.Context, dbName string) (int64, error) {
	var count int64
	query := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)
	if err := q.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting dolt_log commits: %w", err)
	}
	return count, nil
}

// beadCreated reads the bead's creation time and the table it came from.
// Ordinary beads live in issues and wisps in wisps, so both are consulted: a
// wisp is a molecule step or a patrol bead, exactly the kind of row whose
// history gets asked about, and reading only issues would report it as missing
// from the database. The table is reported back because the two are versioned
// differently, and the verdict turns on that.
func (q doltQuerier) beadCreated(ctx context.Context, dbName, beadID string) (historySnapshot, error) {
	var readErr error
	for _, table := range []string{"issues", "wisps"} {
		query := fmt.Sprintf("SELECT created_at FROM `%s`.%s WHERE id = ?", dbName, table)
		var created sql.NullTime
		err := q.db.QueryRowContext(ctx, query, beadID).Scan(&created)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			// A missing table is not a missing bead: keep looking, but keep the
			// failure so a bead that turns up nowhere is reported as unreadable
			// rather than absent.
			if readErr == nil {
				readErr = fmt.Errorf("reading %s row: %w", table, err)
			}
			continue
		}
		if !created.Valid {
			continue
		}
		return historySnapshot{Created: created.Time, Table: table, Found: true}, nil
	}
	if readErr != nil {
		return historySnapshot{}, readErr
	}
	return historySnapshot{}, nil
}

// tableVersioned reports whether Dolt versions the table a bead's row lives in.
// A table matched by dolt_ignore is in no commit at all, so dolt_log holds no
// snapshot of its rows — kept or discarded — and the floor about to be compared
// against cannot say anything about them (gt-ivlh).
func (q doltQuerier) tableVersioned(ctx context.Context, dbName, table string) (bool, error) {
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s`.dolt_ignore WHERE ignored = 1 AND ? LIKE pattern", dbName)
	var matches int
	if err := q.db.QueryRowContext(ctx, query, table).Scan(&matches); err != nil {
		return false, fmt.Errorf("reading dolt_ignore for %s: %w", table, err)
	}
	return matches == 0, nil
}

// eventSpan counts a bead's rows in the events table. Wisps are recorded in
// wisp_events instead, so both are consulted and summed; a database that never
// created wisp_events is not an error, it simply has none.
func (q doltQuerier) eventSpan(ctx context.Context, dbName, beadID string) (historyEventSpan, error) {
	var span historyEventSpan
	var tableErr error
	for _, table := range []string{"events", "wisp_events"} {
		query := fmt.Sprintf(
			"SELECT COUNT(*), MIN(created_at), MAX(created_at) FROM `%s`.%s WHERE issue_id = ?",
			dbName, table)
		var (
			count            int64
			earliest, latest sql.NullTime
		)
		err := q.db.QueryRowContext(ctx, query, beadID).Scan(&count, &earliest, &latest)
		if err != nil {
			// Remember the first failure but keep going: half the audit trail
			// still beats none, and the caller only sees an error if no table
			// answered at all.
			if tableErr == nil {
				tableErr = fmt.Errorf("reading %s: %w", table, err)
			}
			continue
		}
		if count == 0 {
			continue
		}
		span.Count += count
		if earliest.Valid && (!span.Found || earliest.Time.Before(span.Earliest)) {
			span.Earliest = earliest.Time
		}
		if latest.Valid && latest.Time.After(span.Latest) {
			span.Latest = latest.Time
		}
		span.Found = true
	}
	if !span.Found && tableErr != nil {
		return span, tableErr
	}
	return span, nil
}

func runHistory(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead ID required\n\nUsage: gt history <bead-id>")
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	dbName, err := historyDatabaseForBead(townRoot, beadID)
	if err != nil {
		return err
	}
	if !dbNameRe.MatchString(dbName) {
		return fmt.Errorf("database name %q is not a plain identifier — refusing to query it", dbName)
	}

	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking the Dolt server: %w", err)
	}
	if !running {
		return fmt.Errorf("Dolt server is not running — start with 'gt dolt start'")
	}

	config := doltserver.DefaultConfig(townRoot)
	dsn := buildDoltDSNFromConfig(config, dbName, dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "30s",
		WriteTimeout: "30s",
	})
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connecting to database %s: %w", dbName, err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var dummy int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&dummy); err != nil {
		return fmt.Errorf("database %q not reachable: %w", dbName, err)
	}

	report, err := buildHistoryReport(ctx, doltQuerier{db: db}, dbName, beadID)
	if err != nil {
		return err
	}

	if historyJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	return renderHistoryReport(os.Stdout, report)
}

// historyDatabaseForBead maps a bead ID to the Dolt database that holds it, via
// the ID's prefix and the town's routes. An unrouted prefix is an error rather
// than a fallback to the town database: querying the wrong database would
// report a confident verdict about a bead that is not in it.
func historyDatabaseForBead(townRoot, beadID string) (string, error) {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return "", fmt.Errorf("cannot determine a prefix for bead %q", beadID)
	}

	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return "", fmt.Errorf("loading routes: %w", err)
	}
	routePath := ""
	found := false
	for _, route := range routes {
		if route.Prefix == prefix {
			routePath = route.Path
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("prefix %q of bead %s is not in routes.jsonl", prefix, beadID)
	}

	beadsDir := filepath.Join(townRoot, ".beads")
	if routePath != "." {
		rigName := strings.SplitN(routePath, "/", 2)[0]
		beadsDir = doltserver.FindRigBeadsDir(townRoot, rigName)
	}

	dbName := doltserver.DatabaseForBeadsDir(beadsDir)
	if dbName == "" {
		return "", fmt.Errorf("no dolt_database in %s/metadata.json — cannot locate the database for %s",
			beadsDir, beadID)
	}
	return dbName, nil
}

// buildHistoryReport gathers the facts and turns them into the verdict. It
// touches no Dolt connection directly: everything arrives through the querier.
func buildHistoryReport(ctx context.Context, q historyQuerier, dbName, beadID string) (*historyReport, error) {
	report := &historyReport{
		BeadID:          beadID,
		Database:        dbName,
		RecoveryCommand: fmt.Sprintf("bd history %s --events", beadID),
	}

	snapshot, err := q.beadCreated(ctx, dbName, beadID)
	if err != nil {
		return nil, err
	}
	if !snapshot.Found {
		return nil, fmt.Errorf("%s is not in database %s — nothing to report", beadID, dbName)
	}
	report.BeadCreated = formatHistoryTime(snapshot.Created)
	report.BeadTable = snapshot.Table

	// A premise of the verdict, so an unreadable dolt_ignore fails the command
	// rather than falling back to the issues answer for a row that may not be in
	// issues.
	report.Versioned, err = q.tableVersioned(ctx, dbName, snapshot.Table)
	if err != nil {
		return nil, err
	}

	count, err := q.commitCount(ctx, dbName)
	if err != nil {
		return nil, err
	}
	report.Commits = count

	floor, err := q.floor(ctx, dbName)
	if err != nil {
		return nil, err
	}
	if floor.Found {
		report.FloorAt = formatHistoryTime(floor.At)
		report.FloorCommit = floor.Commit
		report.FloorMessage = strings.TrimSpace(floor.Message)
	}

	// An unreadable events table must not sink the floor disclosure, which is
	// the part that says the snapshots are truncated at all.
	span, err := q.eventSpan(ctx, dbName, beadID)
	if err != nil {
		report.EventsUnavailable = true
		// Drop whatever the failed read left behind rather than let a partial
		// span be mistaken for the whole one below.
		span = historyEventSpan{}
	} else {
		report.Events = span.Count
		if span.Found {
			report.EventsEarliest = formatHistoryTime(span.Earliest)
			report.EventsLatest = formatHistoryTime(span.Latest)
		}
	}

	// The snapshots are truncated exactly when the oldest retained commit that
	// can carry a snapshot is newer than the bead itself: everything the bead
	// did before the floor has no snapshot left. Neither claim reaches a
	// dolt_ignored table, whose rows no commit holds: a wisp has no snapshot of
	// its life to begin with, so a floor date says nothing about it either way.
	if !report.Versioned {
		return report, nil
	}
	if floor.Found && floor.At.After(snapshot.Created.Add(snapshotSlack)) {
		report.Truncated = true
		report.MissingFrom = formatHistoryTime(floor.At)
		report.MissingSpan = formatHistoryGap(floor.At.Sub(snapshot.Created))
		// A flatten preserves rows, so the events table should still hold the
		// span the snapshots lost. Claim it only when the rows reach back.
		report.EventsReachFloor = span.Found && span.Earliest.Before(floor.At)
	}
	return report, nil
}

func formatHistoryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// formatHistoryGap renders a duration at day granularity, the scale these
// floors move on.
func formatHistoryGap(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

func renderHistoryReport(out io.Writer, r *historyReport) error {
	fmt.Fprintf(out, "%s %s  %s\n\n",
		style.Bold.Render("●"), style.Bold.Render(r.BeadID),
		style.Dim.Render("database "+r.Database))

	fmt.Fprintf(out, "  Bead created      %s\n", r.BeadCreated)
	if r.FloorAt != "" {
		floor := r.FloorAt
		if r.FloorMessage != "" {
			floor += fmt.Sprintf("  %q", r.FloorMessage)
		}
		fmt.Fprintf(out, "  dolt_log floor    %s  %s\n", floor,
			style.Dim.Render(fmt.Sprintf("(%d commits retained)", r.Commits)))
	} else {
		fmt.Fprintf(out, "  dolt_log floor    %s\n", style.Dim.Render("(no commits found)"))
	}

	switch {
	case r.EventsUnavailable:
		fmt.Fprintf(out, "  Events table      %s\n",
			style.Dim.Render("unreadable — retained history not checked"))
	case r.Events > 0:
		fmt.Fprintf(out, "  Events table      %d rows, %s → %s\n", r.Events, r.EventsEarliest, r.EventsLatest)
	default:
		fmt.Fprintf(out, "  Events table      %s\n", style.Dim.Render("none for this bead"))
	}

	fmt.Fprintln(out)
	if !r.Versioned {
		fmt.Fprintf(out, "  %s this bead's row lives in the %s table, which Dolt is\n",
			style.Warning.Render("⚠ NOT VERSIONED —"), r.BeadTable)
		fmt.Fprintf(out, "    configured to ignore, so no commit carries a snapshot of it. The floor\n")
		fmt.Fprintf(out, "    above has none to truncate, and %s has no\n",
			style.Dim.Render("bd history "+r.BeadID))
		fmt.Fprintf(out, "    snapshots of this bead to report — its events are the whole record:\n\n")
		fmt.Fprintf(out, "    %s\n", style.Info.Render(r.RecoveryCommand))
		return nil
	}
	if !r.Truncated {
		fmt.Fprintf(out, "  %s the commit snapshots begin at or before this bead was\n",
			style.Success.Render("✓ COMPLETE —"))
		fmt.Fprintf(out, "    created, so %s covers its whole life.\n", style.Dim.Render("bd history "+r.BeadID))
		return nil
	}

	fmt.Fprintf(out, "  %s the commit snapshots begin after this bead was created, so\n",
		style.Warning.Render("⚠ TRUNCATED —"))
	fmt.Fprintf(out, "    %s silently omits its first %s (everything before %s).\n",
		style.Dim.Render("bd history "+r.BeadID), r.MissingSpan, r.MissingFrom)
	fmt.Fprintf(out, "    A field that shows one value may have changed before that floor:\n")
	fmt.Fprintf(out, "    absence inside the window is not evidence of absence.\n\n")

	switch {
	case r.EventsReachFloor:
		fmt.Fprintf(out, "  The events table survives flattening and still holds this bead's span\n")
		fmt.Fprintf(out, "  from before the floor — the history is not destroyed. Read it with:\n\n")
	case r.EventsUnavailable:
		fmt.Fprintf(out, "  Whether the surviving events reach past the floor is unknown: the events\n")
		fmt.Fprintf(out, "  table could not be read. Try:\n\n")
	case r.Events > 0:
		fmt.Fprintf(out, "  The events table survives flattening, but its oldest row for this bead is\n")
		fmt.Fprintf(out, "  %s, newer than the floor — it does not cover the missing span.\n",
			r.EventsEarliest)
		fmt.Fprintf(out, "  Read what it does hold with:\n\n")
	default:
		fmt.Fprintf(out, "  No events rows survive for this bead either; the pre-floor history is gone.\n\n")
	}
	fmt.Fprintf(out, "    %s\n", style.Info.Render(r.RecoveryCommand))
	return nil
}
