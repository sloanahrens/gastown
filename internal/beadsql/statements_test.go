package beadsql

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	gmssql "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
)

// TestDeclaredQueriesAreReadOnlyBdReads: every declared statement passes
// ReadOnly once its arguments are inlined, and reads a bd table.
func TestDeclaredQueriesAreReadOnlyBdReads(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]Query{
		"SchemaLevel":                 SchemaLevel(),
		"IssueCount":                  IssueCount(),
		"TableRowCount":               TableRowCount("hq", "wisps"),
		"EphemeralIssues":             EphemeralIssues(),
		"UnassignedInProgress":        UnassignedInProgress(),
		"InProgressByAge":             InProgressByAge(),
		"LabelPresent":                LabelPresent("gt-1", "gt:agent"),
		"CommentsOf":                  CommentsOf([]string{"gt-1", "o'brien"}),
		"AllWisps":                    AllWisps(),
		"WispsWithLabels":             WispsWithLabels([]string{"gt:merge-request", "it's"}),
		"PreloadedBeads":              PreloadedBeads([]string{"gt:agent"}, []string{"open", "hooked"}),
		"PreloadedBeadsStatuses":      PreloadedBeads(nil, []string{"in_progress"}),
		"PreloadedBeadsWispsOnly":     PreloadedBeads(nil, nil),
		"ExternalMoleculesAttachedTo": ExternalMoleculesAttachedTo("gt-a_b"),
		"JournalHead":                 JournalHead(),
		"JournalTail":                 JournalTail(6, 2),
		"JournalTailUnbounded":        JournalTail(0, 0),
		"RawDepsDown":                 RawDeps("gt-1", "down", "tracks"),
		"RawDepsUp":                   RawDeps("gt-1", "up", ""),
		"BackupIssues":                BackupIssues("hq", false, nil),
		"BackupIssuesScrubbed":        BackupIssues("hq", true, []string{"testdb_"}),
		"BackupTable":                 BackupTable("hq", "labels"),
		"BackupEvents":                BackupEvents("hq", ""),
		"BackupEventsSince":           BackupEvents("hq", "2026-10-31 16:53:07"),
	} {
		s, err := q.Inline()
		if err != nil {
			t.Errorf("%s: Inline: %v", name, err)
			continue
		}
		if !bdTableRead.MatchString(s) {
			t.Errorf("%s reads no bd table: %s", name, s)
		}
		args, err := q.BdArgs("--json")
		if err != nil || len(args) != 3 || args[0] != "sql" || args[1] != "--json" || args[2] != s {
			t.Errorf("%s: BdArgs = %q, %v", name, args, err)
		}
	}
	// TableProbe takes any table name; the probe of a missing one is how
	// callers learn it is missing.
	if s, err := TableProbe("nosuch").Inline(); err != nil || s != "SELECT 1 FROM `nosuch` LIMIT 1" {
		t.Errorf("TableProbe = %q, %v", s, err)
	}
}

func TestQueriesRefuseBadIdentifiers(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]Query{
		"TableProbe":       TableProbe("x`; DROP TABLE issues"),
		"TableRowCount":    TableRowCount("hq`", "issues"),
		"BackupIssues":     BackupIssues("a b", true, nil),
		"BackupTable":      BackupTable("hq", "labels;"),
		"BackupEvents":     BackupEvents("hq; DROP TABLE events", ""),
		"BackupEventBound": BackupEvents("hq", "2026-10-31' OR '1'='1"),
		// An empty list would build `IN ()`, which is not a statement, so it is
		// refused rather than sent.
		"CommentsOfNoIDs": CommentsOf(nil),
	} {
		if _, err := q.BdArgs(); err == nil {
			t.Errorf("%s: BdArgs accepted %s", name, q)
		}
		if _, err := (&DB{}).Query(context.Background(), q); err == nil {
			t.Errorf("%s: DB.Query accepted it", name)
		}
	}
}

// TestCommentsOfQuotesItsIDs: an issue id is a value, not SQL text, so one
// holding a quote or a fragment is quoted as a literal and the statement still
// passes ReadOnly. created_at keeps the whole timestamp, sub-second digits
// included, in the RFC3339 UTC form bd's own comments read reports, which is
// what the scorecard parses and compares.
func TestCommentsOfQuotesItsIDs(t *testing.T) {
	t.Parallel()
	s, err := CommentsOf([]string{"gt-1", "x') OR ('1'='1", `rig\id`}).Inline()
	if err != nil {
		t.Fatalf("Inline: %v", err)
	}
	for _, want := range []string{
		`FROM issues i LEFT JOIN comments c ON c.issue_id = i.id WHERE i.id IN ('gt-1', 'x'') OR (''1''=''1', 'rig\\id')`,
		`COALESCE(DATE_FORMAT(c.created_at, '%Y-%m-%dT%H:%i:%s.%fZ'), '') AS created_at`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CommentsOf lacks %q:\n%s", want, s)
		}
	}
}

// TestCommentsOfOrdersACommentsByCreatedAt runs CommentsOf's statement through
// an in-process MySQL-compatible engine, so the order is the engine's and not a
// reading of the SQL text. A bead's comments come back oldest first, so Label
// reads a stand-down or a refusal in the order it was written. The comments are
// written out of created_at order and interleaved across two beads: a read
// without the sort returns them in write order and fails.
func TestCommentsOfOrdersACommentsByCreatedAt(t *testing.T) {
	t.Parallel()
	ctx, engine := newCommentsTestEngine(t)
	mustExecComments(t, ctx, engine, "INSERT INTO issues (id) VALUES ('gt-1'), ('gt-2')")
	for _, c := range []struct{ id, issue, at, text string }{
		{"z9", "gt-1", "2026-11-07 12:03:00", "gt-1 second"},
		{"a1", "gt-2", "2026-11-07 12:04:00", "gt-2 second"},
		{"m5", "gt-1", "2026-11-07 12:01:00", "gt-1 first"},
		{"b2", "gt-2", "2026-11-07 12:02:00", "gt-2 first"},
	} {
		mustExecComments(t, ctx, engine, fmt.Sprintf(
			"INSERT INTO comments (id, issue_id, author, text, created_at) VALUES ('%s', '%s', 'furiosa', '%s', '%s')",
			c.id, c.issue, c.text, c.at))
	}
	s, err := CommentsOf([]string{"gt-1", "gt-2"}).Inline()
	if err != nil {
		t.Fatalf("Inline: %v", err)
	}
	_, iter, _, err := engine.Query(ctx, s)
	if err != nil {
		t.Fatalf("query %s: %v", s, err)
	}
	rows, err := gmssql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("draining: %v", err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, fmt.Sprintf("%v:%v", row[0], row[4]))
	}
	want := []string{
		"gt-1:2026-11-07T12:01:00.000000Z",
		"gt-1:2026-11-07T12:03:00.000000Z",
		"gt-2:2026-11-07T12:02:00.000000Z",
		"gt-2:2026-11-07T12:04:00.000000Z",
	}
	if !slices.Equal(got, want) {
		t.Errorf("comments by bead, oldest first:\n got %q\nwant %q", got, want)
	}
}

// newCommentsTestEngine builds an in-memory engine with the issues and comments
// tables CommentsOf reads.
func newCommentsTestEngine(t *testing.T) (*gmssql.Context, *sqle.Engine) {
	t.Helper()
	db := memory.NewDatabase("commentstest")
	pro := memory.NewDBProvider(db)
	session := memory.NewSession(gmssql.NewBaseSession(), pro)
	ctx := gmssql.NewContext(context.Background(), gmssql.WithSession(session))
	ctx.SetCurrentDatabase("commentstest")

	db.AddTable("issues", memory.NewTable(ctx, db, "issues", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "id", Type: types.Text, Nullable: false, Source: "issues"},
	}), db.GetForeignKeyCollection()))

	db.AddTable("comments", memory.NewTable(ctx, db, "comments", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "id", Type: types.Text, Nullable: false, Source: "comments"},
		{Name: "issue_id", Type: types.Text, Nullable: false, Source: "comments"},
		{Name: "author", Type: types.Text, Nullable: true, Source: "comments"},
		{Name: "text", Type: types.Text, Nullable: true, Source: "comments"},
		{Name: "created_at", Type: types.Datetime, Nullable: false, Source: "comments"},
	}), db.GetForeignKeyCollection()))

	engine := sqle.NewDefault(pro)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("closing test engine: %v", err)
		}
	})
	return ctx, engine
}

func mustExecComments(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if _, err := gmssql.RowIterToRows(ctx, iter); err != nil {
		t.Fatalf("draining %q: %v", query, err)
	}
}

func TestQuoteEscapesSQLLiterals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		values []string
		want   string
	}{
		{[]string{"mayor"}, `'mayor'`},
		{[]string{"o'brien"}, `'o''brien'`},
		{[]string{`rig\agent`}, `'rig\\agent'`},
		{[]string{`rig\`}, `'rig\\'`},
		{[]string{`rig/o\'malley`}, `'rig/o\\''malley'`},
		{[]string{"mayor/", `rig/o\'malley`}, `'mayor/', 'rig/o\\''malley'`},
	} {
		if got := quoteList(tt.values); got != tt.want {
			t.Errorf("quoteList(%q) = %s, want %s", tt.values, got, tt.want)
		}
	}
}

// The external-target match escapes _ with ! so it stays literal under LIKE.
func TestExternalTargetsMatchLiterally(t *testing.T) {
	t.Parallel()
	if s := ExternalMoleculesAttachedTo("gt-a_b").String(); !strings.Contains(s, "depends_on_external LIKE '%:gt-a!_b' ESCAPE '!'") {
		t.Errorf("ExternalMoleculesAttachedTo = %s", s)
	}
	if s := RawDeps("gt-a_b", "up", "").String(); !strings.Contains(s, "depends_on_external LIKE '%:gt-a!_b' ESCAPE '!'") {
		t.Errorf("RawDeps(up) = %s", s)
	}
}

func TestInlineBindsPlaceholdersOutsideQuotes(t *testing.T) {
	t.Parallel()
	q := Query{text: "SELECT id FROM issues WHERE title = '?' AND id = ? AND priority = ?", args: []any{"it's", 2}}
	if s, err := q.Inline(); err != nil || s != "SELECT id FROM issues WHERE title = '?' AND id = 'it''s' AND priority = 2" {
		t.Errorf("Inline = %q, %v", s, err)
	}
	for _, args := range [][]any{{"a"}, {"a", 1, "c"}, {"a", 1.5}} {
		if _, err := (Query{text: q.text, args: args}).Inline(); err == nil {
			t.Errorf("Inline with %v arguments succeeded", args)
		}
	}
	if _, err := (Query{text: "DELETE FROM issues"}).Inline(); !errors.Is(err, ErrNotReadOnly) {
		t.Errorf("Inline(DELETE) = %v, want ErrNotReadOnly", err)
	}
}

// TestPreloadReadCarriesItsDependencyArms covers gt-7dctf and gt-59p7e at the
// SQL layer: the one preload read is the wisps, their dependency rows, the
// issues and theirs, in a single statement. Each dependency arm tags itself
// dep_row = 1 and keys each row to its dependent, and it resolves targets the
// way bd does — the three typed target columns coalesced, against both the
// issues and wisps tables, so an external target this database does not hold
// drops out rather than reading as a blocker.
func TestPreloadReadCarriesItsDependencyArms(t *testing.T) {
	t.Parallel()
	s, err := PreloadedBeads([]string{"gt:agent"}, []string{"open"}).Inline()
	if err != nil {
		t.Fatalf("Inline: %v", err)
	}
	for _, want := range []string{
		// Both tables' rows are in the one statement, each tagged with the
		// table it came from: that tag is what tells them apart downstream.
		"FROM wisps w",
		quote(WispSrc) + " AS src",
		"FROM issues i",
		quote(IssueSrc) + " AS src",
		// A dependency arm asks for the dependencies of the rows the read
		// returned, not of its whole dependency table.
		"UNION ALL SELECT",
		"FROM wisp_dependencies d",
		"AND d.issue_id IN (SELECT id FROM wisps)",
		"FROM dependencies d",
		"AND d.issue_id IN (SELECT i2.id FROM issues i2 WHERE " +
			"i2.id IN (SELECT issue_id FROM labels WHERE label IN ('gt:agent')) OR i2.status IN ('open'))",
		// The issue rows keep `bd list`'s order, with the dependency rows
		// sorted after them by dep_row. A UNION takes one ORDER BY and it has
		// to be last, which is why this closes the read.
		"ORDER BY dep_row, priority, created_at, id",
		// The dependency rows are tagged and keyed to their dependent.
		"1 AS dep_row",
		"'' AS dep_issue_id",
		// Targets resolve the way bd resolves them, against both tables and
		// all three typed target columns, so a wisp target resolves and an
		// external target this database does not hold drops out.
		"COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)",
		"LEFT JOIN issues ti ON",
		"LEFT JOIN wisps tw ON",
		"COALESCE(tw.status, ti.status)",
		"COALESCE(tw.close_reason, ti.close_reason)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("PreloadedBeads lacks %q:\n%s", want, s)
		}
	}

	// The wisps arm must stay unfiltered by label and by status: bucketing the
	// wisp cache happens in Go, and ListAgentBeadsFromWisps runs type/ID
	// fallbacks over wisps a label join cannot see (gt-92zx). Labels and
	// statuses scope the issues arms alone, which is what lets one statement
	// answer for both tables.
	if wispArm := preloadArm(t, s, "FROM wisps w"); strings.Contains(wispArm, "WHERE") {
		t.Errorf("the wisps arm filters rows; only the issues arms may:\n%s", wispArm)
	}

	// A read with no dependency arms to line up with leaves the preload-only
	// columns out entirely, rather than carrying constants a caller could
	// mistake for "this row was read, and has no dependencies".
	if rowOnly, err := WispsWithLabels([]string{"gt:merge-request"}).Inline(); err != nil {
		t.Errorf("WispsWithLabels: Inline: %v", err)
	} else if strings.Contains(rowOnly, "dep_row") || strings.Contains(rowOnly, " AS src") {
		t.Errorf("WispsWithLabels carries preload-only columns:\n%s", rowOnly)
	}
}

// preloadArm is the UNION ALL arm of s that selects from table's rows, up to
// the next arm.
func preloadArm(t *testing.T, s, from string) string {
	t.Helper()
	start := strings.Index(s, from)
	if start < 0 {
		t.Fatalf("no arm selects %q:\n%s", from, s)
	}
	arm := s[start:]
	if end := strings.Index(arm, " UNION ALL "); end >= 0 {
		arm = arm[:end]
	}
	return arm
}

// TestPreloadUnionArmsAlignColumnForColumn: a UNION matches its arms by
// position, so a column a row arm selects and a dependency arm does not would
// shift every value after it — the row would decode as some other column, or
// the read would fail outright. Every arm is rendered from preloadColumns, and
// this is what says so out loud.
func TestPreloadUnionArmsAlignColumnForColumn(t *testing.T) {
	t.Parallel()
	rowHalf := strings.TrimPrefix(preloadRowSelect("i", "i.issue_type", IssueSrc, true), "SELECT ")
	depHalf := strings.TrimPrefix(preloadDependencyRows("dependencies", "SELECT id FROM issues", IssueSrc), "SELECT ")
	if got, want := selectFieldCount(t, depHalf), selectFieldCount(t, rowHalf); got != want {
		t.Errorf("dependency arm has %d columns, row arm has %d:\n%s\n%s", got, want, rowHalf, depHalf)
	}
	// The row arm carries its own value for every preload-only column the
	// dependency arm fills; without them the two would not line up at all.
	for _, c := range preloadColumns {
		if !c.preloadOnly {
			continue
		}
		expr := preloadSubstitute(c.row, "i", "i.issue_type", quote(IssueSrc))
		if !strings.Contains(rowHalf, expr) {
			t.Errorf("row arm lacks the preload-only column %q:\n%s", expr, rowHalf)
		}
	}
}

// TestPreloadFirstArmNamesEveryColumn: a UNION takes its output column names
// from its first arm — the wisps one — and internal/beads decodes the read by
// name. A column the first arm writes as a bare constant is therefore not just
// unnamed but actively misnamed: the issues' own column arrives under a name
// the decoder does not know and reads as zero. Every column of that arm is a
// table column, which keeps its name, or a constant carrying an alias.
func TestPreloadFirstArmNamesEveryColumn(t *testing.T) {
	t.Parallel()
	firstArm := strings.TrimPrefix(preloadRowSelect("w", "''", WispSrc, true), "SELECT ")
	for _, field := range selectFields(t, firstArm) {
		field = strings.TrimSpace(field)
		if strings.Contains(strings.ToUpper(field), " AS ") || strings.HasPrefix(field, "w.") {
			continue
		}
		t.Errorf("the first arm's column %q keeps no name the decoder knows; alias it:\n%s", field, firstArm)
	}
}

// selectFieldCount counts a SELECT list's fields.
func selectFieldCount(t *testing.T, selectList string) int {
	t.Helper()
	return len(selectFields(t, selectList))
}

// selectFields splits a SELECT list into its fields, ignoring the commas inside
// function calls and string literals.
func selectFields(t *testing.T, selectList string) []string {
	t.Helper()
	var fields []string
	depth, start := 0, 0
	for i := 0; i < len(selectList); i++ {
		switch selectList[i] {
		case '\'':
			for i++; i < len(selectList) && selectList[i] != '\''; i++ {
			}
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				fields = append(fields, selectList[start:i])
				start = i + 1
			}
		}
	}
	return append(fields, selectList[start:])
}

// TestBackupEventsReadsFromItsBoundary: the events export appends from the
// newest created_at it already holds, so the read is ordered created_at, id
// and bounded at that value; a bound that is not a timestamp is refused rather
// than sent as a literal Dolt would compare against nothing (gt-l7eeb).
func TestBackupEventsReadsFromItsBoundary(t *testing.T) {
	t.Parallel()
	text, err := BackupEvents("hq", "2026-10-31 16:53:07").Inline()
	if err != nil {
		t.Fatalf("Inline: %v", err)
	}
	for _, want := range []string{
		"FROM `hq`.events",
		"created_at >= '2026-10-31 16:53:07'",
		"ORDER BY created_at, id",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("BackupEvents lacks %q:\n%s", want, text)
		}
	}

	whole, err := BackupEvents("hq", "").Inline()
	if err != nil {
		t.Fatalf("Inline (whole table): %v", err)
	}
	if strings.Contains(whole, "WHERE") {
		t.Errorf("the whole-table read is filtered:\n%s", whole)
	}
	if !strings.Contains(whole, "ORDER BY created_at, id") {
		t.Errorf("the whole-table read is not in the append's order:\n%s", whole)
	}

	if _, err := BackupEvents("hq", "yesterday").Inline(); err == nil {
		t.Error("a bound that is not a timestamp was sent to Dolt")
	}
}
