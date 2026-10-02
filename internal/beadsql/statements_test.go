package beadsql

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDeclaredQueriesAreReadOnlyBdReads: every declared statement passes
// ReadOnly once its arguments are inlined, and reads a bd table.
func TestDeclaredQueriesAreReadOnlyBdReads(t *testing.T) {
	t.Parallel()
	ids := []string{"mayor/", `rig/o\'malley`}
	for name, q := range map[string]Query{
		"SchemaLevel":             SchemaLevel(),
		"IssueCount":              IssueCount(),
		"TableRowCount":           TableRowCount("hq", "wisps"),
		"EphemeralIssues":         EphemeralIssues(),
		"UnassignedInProgress":    UnassignedInProgress(),
		"InProgressByAge":         InProgressByAge(),
		"LabelPresent":            LabelPresent("gt-1", "gt:agent"),
		"AllWisps":                AllWisps(),
		"WispsWithLabels":         WispsWithLabels([]string{"gt:merge-request", "it's"}),
		"PreloadedWisps":          PreloadedWisps(),
		"PreloadedIssues":         PreloadedIssues([]string{"gt:agent"}, []string{"open", "hooked"}),
		"PreloadedIssuesStatuses": PreloadedIssues(nil, []string{"in_progress"}),
		"MailWisps":               MailWisps(ids),
		"MailIssues":              MailIssues(ids),
		"MoleculesAttachedTo":     MoleculesAttachedTo("gt-a_b"),
		"RawDepsDown":             RawDeps("gt-1", "down", "tracks"),
		"RawDepsUp":               RawDeps("gt-1", "up", ""),
		"BackupIssues":            BackupIssues("hq", false, nil),
		"BackupIssuesScrubbed":    BackupIssues("hq", true, []string{"testdb_"}),
		"BackupTable":             BackupTable("hq", "labels"),
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
		"TableProbe":    TableProbe("x`; DROP TABLE issues"),
		"TableRowCount": TableRowCount("hq`", "issues"),
		"BackupIssues":  BackupIssues("a b", true, nil),
		"BackupTable":   BackupTable("hq", "labels;"),
		"no filter":     PreloadedIssues(nil, nil),
	} {
		if _, err := q.BdArgs(); err == nil {
			t.Errorf("%s: BdArgs accepted %s", name, q)
		}
		if _, err := (&DB{}).Query(context.Background(), q); err == nil {
			t.Errorf("%s: DB.Query accepted it", name)
		}
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
		{[]string{"mayor/", `rig/o\'malley`}, `'mayor/','rig/o\\''malley'`},
	} {
		if got := quoteList(tt.values, ","); got != tt.want {
			t.Errorf("quoteList(%q) = %s, want %s", tt.values, got, tt.want)
		}
	}
}

func TestMailQueriesMatchEveryIdentityAndItsCC(t *testing.T) {
	t.Parallel()
	s := MailWisps([]string{"mayor/", "mayor", `rig/o\'malley`}).String()
	for _, want := range []string{`'mayor/'`, `'mayor'`, `'cc:mayor/'`, `'cc:mayor'`, `'rig/o\\''malley'`, `'cc:rig/o\\''malley'`} {
		if !strings.Contains(s, want) {
			t.Errorf("MailWisps lacks %s:\n%s", want, s)
		}
	}
}

// The external-target match escapes _ with ! so it stays literal under LIKE.
func TestExternalTargetsMatchLiterally(t *testing.T) {
	t.Parallel()
	if s := MoleculesAttachedTo("gt-a_b").String(); !strings.Contains(s, "depends_on_external LIKE '%:gt-a!_b' ESCAPE '!'") {
		t.Errorf("MoleculesAttachedTo = %s", s)
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

// TestPreloadReadsCarryTheirDependencyHalf covers gt-7dctf at the SQL layer:
// each preload read is the rows UNION ALL their dependency rows, the
// dependency half tags itself dep_row = 1 and keys each row to its dependent,
// and it resolves targets the way bd does — the three typed target columns
// coalesced, against both the issues and wisps tables, so an external target
// this database does not hold drops out rather than reading as a blocker.
func TestPreloadReadsCarryTheirDependencyHalf(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		query Query
		want  []string
	}{
		"PreloadedWisps": {
			query: PreloadedWisps(),
			want: []string{
				"UNION ALL SELECT",
				"FROM wisp_dependencies d",
				// The dependency half asks for the dependencies of the rows
				// the read returned, not of the whole table.
				"AND d.issue_id IN (SELECT id FROM wisps)",
			},
		},
		"PreloadedIssues": {
			query: PreloadedIssues([]string{"gt:agent"}, []string{"open"}),
			want: []string{
				"UNION ALL SELECT",
				"FROM dependencies d",
				"AND d.issue_id IN (SELECT i2.id FROM issues i2 WHERE " +
					"i2.id IN (SELECT issue_id FROM labels WHERE label IN ('gt:agent')) OR i2.status IN ('open'))",
				// The row half keeps `bd list`'s order, with the dependency
				// rows sorted after it by dep_row. A UNION takes one ORDER BY
				// and it has to be last, which is why this closes the read.
				"ORDER BY dep_row, priority, created_at, id",
			},
		},
	} {
		s, err := tt.query.Inline()
		if err != nil {
			t.Errorf("%s: Inline: %v", name, err)
			continue
		}
		for _, want := range append(tt.want,
			// The dependency rows are tagged and keyed to their dependent.
			"1 AS dep_row",
			"'' AS dep_issue_id",
			// Targets resolve the way bd resolves them, against both tables
			// and all three typed target columns, so a wisp target resolves
			// and an external target this database does not hold drops out.
			"COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)",
			"LEFT JOIN issues ti ON",
			"LEFT JOIN wisps tw ON",
			"COALESCE(tw.status, ti.status)",
			"COALESCE(tw.close_reason, ti.close_reason)",
		) {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, s)
			}
		}
	}
	// A read with no dependency half to line up with leaves the dep_* columns
	// out entirely, rather than carrying constants a caller could mistake for
	// "this row was read, and has no dependencies".
	if rowOnly, err := WispsWithLabels([]string{"gt:merge-request"}).Inline(); err != nil {
		t.Errorf("WispsWithLabels: Inline: %v", err)
	} else if strings.Contains(rowOnly, "dep_row") {
		t.Errorf("WispsWithLabels carries dependency columns:\n%s", rowOnly)
	}
}

// TestPreloadUnionHalvesAlignColumnForColumn: a UNION matches its halves by
// position, so a column the row half selects and the dependency half does not
// would shift every value after it. Both halves are rendered from
// preloadColumns, and this is what says so out loud.
func TestPreloadUnionHalvesAlignColumnForColumn(t *testing.T) {
	t.Parallel()
	rowHalf := strings.TrimPrefix(preloadRowSelect("i", "i.issue_type", true), "SELECT ")
	depHalf := strings.TrimPrefix(preloadDependencyRows("dependencies", "SELECT id FROM issues"), "SELECT ")
	if got, want := selectFieldCount(t, depHalf), selectFieldCount(t, rowHalf); got != want {
		t.Errorf("dependency half has %d columns, row half has %d:\n%s\n%s", got, want, rowHalf, depHalf)
	}
	// The row half's dep_* columns are the constants that tie a row to no
	// dependency; without them the two halves would not line up at all.
	if got := strings.Count(rowHalf, " AS dep_"); got != selectFieldCount(t, depHalf)-13 {
		t.Errorf("row half carries %d dep columns, want one per dependency column:\n%s", got, rowHalf)
	}
}

// selectFieldCount counts a SELECT list's fields, ignoring the commas inside
// function calls and string literals.
func selectFieldCount(t *testing.T, selectList string) int {
	t.Helper()
	depth, count := 0, 1
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
				count++
			}
		}
	}
	return count
}
