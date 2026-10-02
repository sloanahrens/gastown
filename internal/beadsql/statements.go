package beadsql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Query is one of gastown's declared reads of bd's tables. Its text is built
// only in this package, so every bd-table statement gastown sends, through a
// DB, through `bd sql` or through `dolt sql`, is written against
// SchemaVersion here and passes ReadOnly before it leaves (gt-7iwy0.5).
// A constructor that was handed an unusable identifier carries the error,
// which surfaces where the query is run.
type Query struct {
	text string
	args []any
	err  error
}

// String is the statement with its arguments inlined, for logs and fakes.
func (q Query) String() string {
	s, err := q.Inline()
	if err != nil {
		return q.text
	}
	return s
}

// Inline returns the statement with its arguments inlined as SQL literals,
// for transports that take a statement and no arguments (`bd sql`,
// `dolt sql -q`). It refuses a statement ReadOnly refuses.
func (q Query) Inline() (string, error) {
	if q.err != nil {
		return "", q.err
	}
	s, err := inline(q.text, q.args)
	if err != nil {
		return "", err
	}
	if err := ReadOnly(s); err != nil {
		return "", err
	}
	return s, nil
}

// BdArgs returns the argv of `bd sql <flags...> <statement>`.
func (q Query) BdArgs(flags ...string) ([]string, error) {
	s, err := q.Inline()
	if err != nil {
		return nil, err
	}
	args := append([]string{"sql"}, flags...)
	return append(args, s), nil
}

// Query runs q on the database with its arguments bound by the driver.
func (d *DB) Query(ctx context.Context, q Query) (*sql.Rows, error) {
	if q.err != nil {
		return nil, q.err
	}
	return d.QueryContext(ctx, q.text, q.args...)
}

// inline replaces each ? outside quotes with the next argument as a literal.
func inline(text string, args []any) (string, error) {
	if len(args) == 0 {
		return text, nil
	}
	bare := stripQuoted(text)
	var b strings.Builder
	n := 0
	for i := 0; i < len(text); i++ {
		if bare[i] != '?' {
			b.WriteByte(text[i])
			continue
		}
		if n == len(args) {
			return "", fmt.Errorf("beadsql: %d arguments for more placeholders: %s", len(args), firstWords(text))
		}
		switch v := args[n].(type) {
		case string:
			b.WriteString(quote(v))
		case int:
			b.WriteString(strconv.Itoa(v))
		default:
			return "", fmt.Errorf("beadsql: cannot inline %T", v)
		}
		n++
	}
	if n != len(args) {
		return "", fmt.Errorf("beadsql: %d arguments for %d placeholders: %s", len(args), n, firstWords(text))
	}
	return b.String(), nil
}

// quote is s as a single-quoted SQL string literal.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// quoteList is values as a comma-separated list of SQL string literals.
func quoteList(values []string, sep string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = quote(v)
	}
	return strings.Join(quoted, sep)
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ident checks a database or table name before it is spliced in backticks.
func ident(name string) error {
	if !identifier.MatchString(name) {
		return fmt.Errorf("beadsql: invalid identifier %q", name)
	}
	return nil
}

func declared(text string) Query { return Query{text: text} }

// SchemaLevel reads the database's bd schema level (the startup handshake's
// read through `bd sql`).
func SchemaLevel() Query { return declared(SchemaLevelQuery) }

// IssueCount counts the issues table: closed issues included, wisps (their
// own table) not. `bd count` hides infrastructure types, so it does not
// answer this.
func IssueCount() Query { return declared("SELECT COUNT(*) as cnt FROM issues") }

// TableProbe returns a row when table exists and is readable.
func TableProbe(table string) Query {
	if err := ident(table); err != nil {
		return Query{err: err}
	}
	return declared("SELECT 1 FROM `" + table + "` LIMIT 1")
}

// TableRowCount counts table in database (the daemon's served-data spot
// check over `dolt sql`).
func TableRowCount(database, table string) Query {
	for _, name := range []string{database, table} {
		if err := ident(name); err != nil {
			return Query{err: err}
		}
	}
	return declared("SELECT COUNT(*) AS cnt FROM `" + database + "`.`" + table + "`")
}

// Doctor checks' reads. `bd list` has no ephemeral filter and folds the
// wisps tier in with --include-infra, so these stay SQL over issues alone.

// EphemeralIssues are issues-table rows flagged ephemeral: wisps stored in
// the wrong table (misclassified-wisp check).
func EphemeralIssues() Query {
	return declared(`SELECT id, title FROM issues WHERE ephemeral = 1`)
}

// UnassignedInProgress are in-progress issues with no assignee, oldest
// update first (null-assignee check).
func UnassignedInProgress() Query {
	return declared(`SELECT id, title, updated_at FROM issues WHERE status = 'in_progress' AND (assignee IS NULL OR assignee = '') ORDER BY updated_at ASC`)
}

// InProgressByAge are in-progress issues, oldest update first (stuck-patrol
// check).
func InProgressByAge() Query {
	return declared(`SELECT id, title, status, updated_at FROM issues WHERE status = 'in_progress' ORDER BY updated_at ASC`)
}

// LabelPresent returns a row when id carries label in this database.
func LabelPresent(id, label string) Query {
	return declared("SELECT 1 AS present FROM labels WHERE issue_id = " + quote(id) + " AND label = " + quote(label) + " LIMIT 1")
}

// wispRowSelect is the SELECT list the row-only wisps reads share, derived
// from the preload column list so the two stay in step; internal/beads'
// bdSQLIssueRow decodes it. The LEFT JOIN on wisp_labels populates
// labels_csv; a read that filters on a label adds its own INNER JOIN, before
// wispRowGroupBy.
func wispRowSelect() string {
	return preloadRowSelect("w", "''", false) + preloadRowTableJoin("w", "wisps", "wisp_labels")
}

func wispRowGroupBy() string { return preloadRowGroupBy("w") }

// AllWisps reads every wisp with its labels.
func AllWisps() Query { return declared(wispRowSelect() + wispRowGroupBy()) }

// WispsWithLabels reads every wisp carrying any of labels, with all its
// labels. The INNER JOIN hides a wisp without a label row.
func WispsWithLabels(labels []string) Query {
	return declared(wispRowSelect() +
		" JOIN wisp_labels l ON w.id = l.issue_id" +
		" WHERE l.label IN (" + quoteList(labels, ", ") + ")" +
		wispRowGroupBy())
}

// The two preload reads — PreloadedWisps behind PreloadLabeledWisps, and
// PreloadedIssues behind PreloadIssues — return their rows and, in the same
// round trip, the dependency rows of every row they return (gt-7dctf). That
// is what lets ListMergeRequests hydrate its merge requests without the
// separate `bd show --json <ids>` it used to pay per rig: the dependency and
// blocker data a hydration needs is already read.
//
// Each read is one UNION ALL. Its row half is the read's own rows, tagged
// dep_row = 0; its dependency half is one row per dependency of those rows,
// tagged dep_row = 1 and keyed to the row it belongs to by dep_issue_id. Both
// halves are rendered from preloadColumns, so they line up column for column
// by construction rather than by two hand-kept lists staying in step.

// depTargetExpr is bd's own resolution of a dependency row's target: the
// first non-null of the three typed target columns (issueops.DepTargetExpr).
const depTargetExpr = "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)"

// preloadColumn is one column shared by the reads in this file: the row
// expression ({t} is the row table's alias, {issue_type} the one column the
// tables source differently) and what a dependency row puts in the same
// position. A row-only read — one with no dependency half to line up with —
// drops the dependency columns (depOnly).
type preloadColumn struct {
	row     string
	dep     string
	depOnly bool
}

// preloadColumns is that shared list: every column the issues and wisps reads
// select, plus close_reason (`bd list` reports it; the wisps reads did not
// select it) and the dep_* columns. One list serves every read, so a column
// added here reaches all of them; a read with no use for one still decodes it
// and ignores it.
var preloadColumns = []preloadColumn{
	{row: "{t}.id", dep: "d.issue_id"},
	{row: "{t}.title", dep: "''"},
	{row: "{t}.description", dep: "''"},
	{row: "{t}.status", dep: "''"},
	{row: "{t}.priority", dep: "0"},
	{row: "{issue_type}", dep: "''"},
	{row: "{t}.assignee", dep: "''"},
	// The two datetime columns are NULL, not '', on a dependency row: a UNION
	// takes its column types from its first half, and the reads' ORDER BY then
	// sorts these as datetimes, which Dolt refuses to read '' as.
	{row: "{t}.created_at", dep: "NULL"},
	{row: "{t}.updated_at", dep: "NULL"},
	{row: "{t}.created_by", dep: "''"},
	{row: "{t}.ephemeral", dep: "0"},
	{row: "{t}.close_reason", dep: "''"},
	{row: "GROUP_CONCAT(al.label) as labels_csv", dep: "''"},
	{row: "0 AS dep_row", dep: "1 AS dep_row", depOnly: true},
	{row: "'' AS dep_issue_id", dep: "d.issue_id", depOnly: true},
	{row: "'' AS dep_type", dep: "d.type", depOnly: true},
	{row: "'' AS dep_id", dep: "COALESCE(tw.id, ti.id)", depOnly: true},
	{row: "'' AS dep_status", dep: "COALESCE(tw.status, ti.status)", depOnly: true},
	{row: "'' AS dep_close_reason", dep: "COALESCE(tw.close_reason, ti.close_reason)", depOnly: true},
	{row: "'' AS dep_title", dep: "COALESCE(tw.title, ti.title)", depOnly: true},
	{row: "0 AS dep_priority", dep: "COALESCE(tw.priority, ti.priority)", depOnly: true},
	{row: "'' AS dep_issue_type", dep: "COALESCE(tw.issue_type, ti.issue_type)", depOnly: true},
}

// preloadRowSelect renders the row half for alias, with the dependency columns
// included when the read has a dependency half for them to line up with.
// issueType stands in for {issue_type}: a wisp's Type stays "" because
// ListAgentBeadsFromWisps reads that field to classify agent beads, and the
// wisps table's own issue_type column ('task', 'molecule', …) is not the same
// thing, so the wisps reads pass a constant there.
func preloadRowSelect(alias, issueType string, withDeps bool) string {
	var exprs []string
	for _, c := range preloadColumns {
		if c.depOnly && !withDeps {
			continue
		}
		exprs = append(exprs, preloadSubstitute(c.row, alias, issueType))
	}
	return "SELECT " + strings.Join(exprs, ", ")
}

// preloadRowGroupBy collapses the one-row-per-label join into one row per row
// of the row table, with labels_csv carrying the labels. Grouping by that
// table's id is enough: every other column selected from it is functionally
// dependent on its primary key, and labels_csv is the only aggregate. A
// constant cannot be listed here at all — Dolt reads one as a column ordinal —
// which is what the wisps read's {issue_type} renders to.
func preloadRowGroupBy(alias string) string { return " GROUP BY " + alias + ".id" }

// preloadRowTableJoin is the row half's FROM clause for a read over table,
// whose labels live in labelsTable.
func preloadRowTableJoin(alias, table, labelsTable string) string {
	return " FROM " + table + " " + alias + " LEFT JOIN " + labelsTable + " al ON " + alias + ".id = al.issue_id"
}

// preloadDependencyRows is the dependency half: one row per dependency of a
// row in scope, carrying the relation type and the target's own status, close
// reason, title, priority and type, under the dep_* names. scope is the
// subquery naming the depending ids, which are the rows the read returned.
//
// The target is resolved the way bd resolves it — depTargetExpr, then looked
// up in the issues and wisps tables — so a dependency whose target this
// database does not hold (an external id from another rig) drops out here
// exactly as bd drops it from `bd show`, and a target that is itself a wisp
// resolves. Dependencies of wisps carry their targets in wisps, so both
// tables have to be joined for either read.
func preloadDependencyRows(depTable, scope string) string {
	exprs := make([]string, len(preloadColumns))
	for i, c := range preloadColumns {
		exprs[i] = c.dep
	}
	return "SELECT " + strings.Join(exprs, ", ") +
		" FROM " + depTable + " d" +
		" LEFT JOIN issues ti ON ti.id = " + depTargetExpr +
		" LEFT JOIN wisps tw ON tw.id = " + depTargetExpr +
		" WHERE (ti.id IS NOT NULL OR tw.id IS NOT NULL)" +
		" AND d.issue_id IN (" + scope + ")"
}

func preloadSubstitute(expr, alias, issueType string) string {
	expr = strings.ReplaceAll(expr, "{t}", alias)
	return strings.ReplaceAll(expr, "{issue_type}", issueType)
}

// PreloadedWisps is the wisps read behind PreloadLabeledWisps: every wisp in
// the rig with its labels — unfiltered by label, because the agent side of the
// preload runs type and ID fallbacks over wisps the label join cannot see
// (gt-92zx) — plus the dependency rows of every wisp it returns.
func PreloadedWisps() Query {
	return declared(preloadRowSelect("w", "''", true) +
		preloadRowTableJoin("w", "wisps", "wisp_labels") +
		wispRowGroupBy() +
		" UNION ALL " +
		preloadDependencyRows("wisp_dependencies", "SELECT id FROM wisps"))
}

// PreloadedIssues is the issues read behind PreloadIssues: every issue
// carrying any of labels or in any of statuses, with its labels and, in the
// same round trip, the dependency rows of every issue it returns. The row
// half is ordered as `bd list` orders (priority, then age); dep_row first
// keeps the dependency rows after it, so the order of the issue rows — which
// the snapshot preserves — is unchanged by the union. Both lists empty is a
// caller error.
func PreloadedIssues(labels, statuses []string) Query {
	clauses := issueScopeClauses("i", labels, statuses)
	if len(clauses) == 0 {
		return Query{err: fmt.Errorf("beadsql: PreloadedIssues needs a label or a status")}
	}
	scope := strings.Join(issueScopeClauses("i2", labels, statuses), " OR ")
	return declared(preloadRowSelect("i", "i.issue_type", true) +
		preloadRowTableJoin("i", "issues", "labels") +
		" WHERE " + strings.Join(clauses, " OR ") +
		preloadRowGroupBy("i") +
		" UNION ALL " +
		preloadDependencyRows("dependencies", "SELECT i2.id FROM issues i2 WHERE "+scope) +
		" ORDER BY " + preloadOrderBy)
}

// preloadOrderBy is PreloadedIssues' ORDER BY, at the end of the whole UNION
// because a UNION takes only one and it has to come last. dep_row first keeps
// the dependency rows after the issue rows, so the issue rows keep the order
// `bd list` gives them — priority, then age — which the snapshot preserves and
// the readers rely on. These are the output column names of the union's first
// half, which is what an ORDER BY over a UNION sees.
const preloadOrderBy = "dep_row, priority, created_at, id"

// issueScopeClauses are the WHERE terms PreloadedIssues selects rows on,
// written against alias so that the dependency half's scope subquery asks the
// same question of the same rows.
func issueScopeClauses(alias string, labels, statuses []string) []string {
	var clauses []string
	if len(labels) > 0 {
		clauses = append(clauses, alias+".id IN (SELECT issue_id FROM labels WHERE label IN ("+quoteList(labels, ", ")+"))")
	}
	if len(statuses) > 0 {
		clauses = append(clauses, alias+".status IN ("+quoteList(statuses, ", ")+")")
	}
	return clauses
}

// MailWisps reads the open or hooked gt:message wisps assigned to, or
// cc'ing, any of identities, with their labels and which of the two matched.
func MailWisps(identities []string) Query {
	ids, ccs := mailLists(identities)
	return declared("SELECT w.id, w.title, w.description, w.status, w.priority, w.assignee, w.created_at, w.updated_at, " +
		"GROUP_CONCAT(DISTINCT al.label) as labels_csv, " +
		"MAX(CASE WHEN w.assignee IN (" + ids + ") THEN 1 ELSE 0 END) as assignee_match, " +
		"MAX(CASE WHEN cc.label IS NOT NULL THEN 1 ELSE 0 END) as cc_match " +
		"FROM wisps w " +
		"JOIN wisp_labels msg_label ON w.id = msg_label.issue_id AND msg_label.label = 'gt:message' " +
		"JOIN wisp_labels al ON w.id = al.issue_id " +
		"LEFT JOIN wisp_labels cc ON w.id = cc.issue_id AND cc.label IN (" + ccs + ") " +
		"WHERE w.status IN ('open', 'hooked') AND (w.assignee IN (" + ids + ") OR cc.label IS NOT NULL) " +
		"GROUP BY w.id, w.title, w.description, w.status, w.priority, w.assignee, w.created_at, w.updated_at")
}

// MailIssues reads the open or hooked gt:message issues assigned to, or
// cc'ing, any of identities, with the matching cc labels and whether the
// message carries the read label.
func MailIssues(identities []string) Query {
	ids, ccs := mailLists(identities)
	return declared("SELECT i.id, i.title, i.status, i.assignee, " +
		"GROUP_CONCAT(DISTINCT cc.label) AS cc_labels_csv, " +
		"MAX(CASE WHEN rd.label IS NOT NULL THEN 1 ELSE 0 END) AS is_read " +
		"FROM issues i " +
		"JOIN labels msg_label ON i.id = msg_label.issue_id AND msg_label.label = 'gt:message' " +
		"LEFT JOIN labels cc ON i.id = cc.issue_id AND cc.label IN (" + ccs + ") " +
		"LEFT JOIN labels rd ON i.id = rd.issue_id AND rd.label = 'read' " +
		"WHERE i.status IN ('open', 'hooked') AND (i.assignee IN (" + ids + ") OR cc.label IS NOT NULL) " +
		"GROUP BY i.id, i.title, i.status, i.assignee")
}

// mailLists are identities and their cc: labels as SQL literal lists.
func mailLists(identities []string) (ids, ccs string) {
	cc := make([]string, len(identities))
	for i, id := range identities {
		cc[i] = "cc:" + id
	}
	return quoteList(identities, ","), quoteList(cc, ",")
}

// MoleculesAttachedTo reads the open molecule wisps that block, or are
// parented under, beadID by any of the typed dependency target columns.
func MoleculesAttachedTo(beadID string) Query {
	id := quote(beadID)
	return declared(`SELECT DISTINCT wisp_dependencies.issue_id FROM wisp_dependencies JOIN wisps ON wisps.id = wisp_dependencies.issue_id WHERE wisps.issue_type = 'molecule' AND wisps.status NOT IN ('closed', 'tombstone') AND wisp_dependencies.type IN ('blocks', 'conditional-blocks', 'parent-child') AND (wisp_dependencies.depends_on_issue_id = ` + id + ` OR wisp_dependencies.depends_on_wisp_id = ` + id + ` OR wisp_dependencies.depends_on_external = ` + id + ` OR depends_on_external LIKE ` + quote(externalSuffixPattern(beadID)) + ` ESCAPE '!')`)
}

// externalSuffixPattern matches an external:<prefix>:<id> target naming id,
// escaping _ with ! (not valid in a bead ID) so it stays literal.
func externalSuffixPattern(id string) string {
	return "%:" + strings.ReplaceAll(id, "_", "!_")
}

// RawDeps reads dependency rows of issueID straight from the dependencies
// table, without bd's join to issues, so targets in other databases still
// show. direction "up" reads the dependents (column issue_id); anything else
// reads the targets (column depends_on_id). depType "" means every type.
func RawDeps(issueID, direction, depType string) Query {
	var q Query
	if direction == "up" {
		q = Query{
			text: "SELECT issue_id FROM dependencies WHERE (depends_on_issue_id = ? OR depends_on_wisp_id = ? OR depends_on_external LIKE ? ESCAPE '!')",
			args: []any{issueID, issueID, externalSuffixPattern(issueID)},
		}
	} else {
		q = Query{
			text: "SELECT COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) AS depends_on_id FROM dependencies WHERE issue_id = ?",
			args: []any{issueID},
		}
	}
	if depType != "" {
		q.text += " AND type = ?"
		q.args = append(q.args, depType)
	}
	return q
}

// backupScrub keeps the durable work product (bugs, features, tasks, epics,
// chores) of an issues export and drops ephemeral and infrastructure rows.
const backupScrub = ` WHERE (ephemeral IS NULL OR ephemeral != 1)` +
	` AND status != 'tombstone'` +
	` AND issue_type NOT IN ('message', 'event', 'agent', 'convoy', 'molecule', 'role', 'merge-request', 'rig')` +
	` AND id NOT LIKE '%-wisp-%'` +
	` AND id NOT LIKE '%-cv-%'` +
	` AND id NOT LIKE '%-wf-%'` +
	` AND id NOT LIKE 'test%'`

// backupScrubTail follows the per-prefix exclusions.
const backupScrubTail = ` AND id NOT LIKE 'offlinebrew-%'` +
	` AND title NOT LIKE '--%'` +
	` AND title NOT LIKE 'Usage: %'`

// BackupIssues reads every column of database's issues for the JSONL
// backup, ordered by id. With scrub it keeps only durable work product,
// and drops ids starting with any of excludeIDPrefixes (the test-database
// prefixes, matched literally).
func BackupIssues(database string, scrub bool, excludeIDPrefixes []string) Query {
	if err := ident(database); err != nil {
		return Query{err: err}
	}
	text := "SELECT * FROM `" + database + "`.issues"
	if scrub {
		text += backupScrub
		for _, p := range excludeIDPrefixes {
			text += ` AND id NOT LIKE ` + quote(strings.ReplaceAll(p, "_", `\_`)+"%")
		}
		text += backupScrubTail
	}
	return declared(text + " ORDER BY id")
}

// BackupTable reads every column of database's table for the JSONL backup,
// ordered by its first column.
func BackupTable(database, table string) Query {
	for _, name := range []string{database, table} {
		if err := ident(name); err != nil {
			return Query{err: err}
		}
	}
	return declared(fmt.Sprintf("SELECT * FROM `%s`.`%s` ORDER BY 1", database, table))
}
