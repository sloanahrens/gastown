package cloudledger_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/cloudledger/ledgertest"
)

// goldenPath is the golden DDL the queries are pinned to. go test runs a
// package's tests in its directory, so the path is relative to it.
const goldenPath = "testdata/ledger.golden.sql"

var (
	goldenCreate   = regexp.MustCompile(`^CREATE TABLE IF NOT EXISTS (\w+) \($`)
	sqlLiteral     = regexp.MustCompile(`'[^']*'`)
	sqlPlaceholder = regexp.MustCompile(`\$\d+`)
	sqlTableRef    = regexp.MustCompile(`(?i)\b(?:FROM|INTO|UPDATE|JOIN)\s+(\w+)`)
	sqlWord        = regexp.MustCompile(`[A-Za-z_]\w*`)
)

// sqlKeywords are the words the package's queries use that are neither a table
// nor a column. A new keyword fails the test naming it, so it is added here on
// purpose.
var sqlKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "order": true, "by": true,
	"desc": true, "asc": true, "limit": true, "and": true, "or": true,
	"distinct": true, "null": true, "true": true, "false": true,
	"current_user": true, "insert": true, "into": true, "values": true,
	"update": true, "set": true, "delete": true, "on": true, "conflict": true,
	"do": true, "nothing": true, "returning": true,
}

// readGolden returns each table the golden DDL creates, with its column names.
func readGolden(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading the golden DDL: %v", err)
	}
	tables, err := parseGolden(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

// parseGolden returns each table the DDL in text creates, with its column names
// in the order it creates them. It reads names only: a column's type and its
// constraints are not compared, so a type that drifts from the migration is
// not caught here. A table whose body never closes, or that names no column,
// is an error, so a golden that lost its columns fails rather than matching
// nothing.
func parseGolden(text string) (map[string][]string, error) {
	tables := map[string][]string{}
	table := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case table == "":
			if m := goldenCreate.FindStringSubmatch(line); m != nil {
				table = m[1]
				tables[table] = []string{}
			}
		case line == ");":
			table = ""
		case line == "" || strings.HasPrefix(line, "--"):
		default:
			// A table constraint (PRIMARY KEY, UNIQUE, CONSTRAINT) names no column.
			word := sqlWord.FindString(line)
			if w := strings.ToUpper(word); w == "PRIMARY" || w == "UNIQUE" || w == "CONSTRAINT" {
				break
			}
			tables[table] = append(tables[table], word)
		}
	}
	if table != "" {
		return nil, fmt.Errorf("the golden DDL's table %s has no closing );", table)
	}
	for name, cols := range tables {
		if len(cols) == 0 {
			return nil, fmt.Errorf("the golden DDL creates no columns for %s", name)
		}
	}
	return tables, nil
}

// unnamed returns what stmt names that the golden DDL does not create: a table
// it reads or writes, or a column the tables it names lack. String literals and
// $n placeholders are values, not names, and a word followed by "(" is a
// function.
func unnamed(golden map[string][]string, stmt string) []string {
	text := sqlPlaceholder.ReplaceAllString(sqlLiteral.ReplaceAllString(stmt, " "), " ")
	var out []string
	known := map[string]bool{}
	for _, m := range sqlTableRef.FindAllStringSubmatch(text, -1) {
		cols, ok := golden[m[1]]
		if !ok {
			out = append(out, "table "+m[1])
		}
		known[m[1]] = true
		for _, c := range cols {
			known[c] = true
		}
	}
	for _, loc := range sqlWord.FindAllStringIndex(text, -1) {
		word := text[loc[0]:loc[1]]
		if sqlKeywords[strings.ToLower(word)] || known[word] {
			continue
		}
		if loc[1] < len(text) && text[loc[1]] == '(' {
			continue
		}
		out = append(out, "column "+word)
	}
	return out
}

// TestCloudLedgerQueriesMatchTheGoldenSchema fails when a query the Postgres
// source sends names a table or a column the golden DDL does not create. The
// statements come from running each read against the fake ledger, so the test
// checks the SQL the package sends, not a copy of it. A column's type is not
// compared (see readGolden).
func TestCloudLedgerQueriesMatchTheGoldenSchema(t *testing.T) {
	t.Parallel()

	golden := readGolden(t)
	tables := []string{cloudledger.PatrolReportTable, cloudledger.PatrolHeartbeatTable, cloudledger.ReleaseTable}
	for _, table := range tables {
		if len(golden[table]) == 0 {
			t.Fatalf("the golden DDL creates no columns for %s", table)
		}
	}

	ctx := context.Background()
	at := ledgerNow
	l := &ledgertest.Ledger{
		Report:    emptyReport(),
		Heartbeat: &at,
		Releases:  map[string][]ledgertest.ReleaseRow{"alpha": {{At: ledgerNow, Line: []byte(`{}`)}}},
	}
	src := pgSource(t, l)
	if _, err := src.LatestPatrolReport(ctx); err != nil {
		t.Fatalf("report: %v", err)
	}
	if _, err := src.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := src.ReleasesSince(ctx, "alpha", time.Time{}); err != nil {
		t.Fatalf("releases since: %v", err)
	}
	if _, err := src.LastRelease(ctx, "alpha"); err != nil {
		t.Fatalf("last release: %v", err)
	}
	if _, err := src.Apps(ctx); err != nil {
		t.Fatalf("apps: %v", err)
	}

	stmts := l.Statements()
	for _, table := range tables {
		if !slices.ContainsFunc(stmts, func(s string) bool { return strings.Contains(s, "FROM "+table) }) {
			t.Errorf("no read reached %s, so its queries are not checked", table)
		}
	}
	for _, stmt := range stmts {
		for _, name := range unnamed(golden, stmt) {
			t.Errorf("%s: the query names %s, which the golden DDL does not create", stmt, name)
		}
	}

	t.Run("a column the golden lacks is named", func(t *testing.T) {
		t.Parallel()
		got := unnamed(golden, "SELECT line, ghost_column FROM ledger_release WHERE app = $1")
		if !slices.Contains(got, "column ghost_column") {
			t.Errorf("unnamed = %v, want the column ghost_column named", got)
		}
	})
	t.Run("a column of another table is named", func(t *testing.T) {
		t.Parallel()
		got := unnamed(golden, "SELECT finished_at FROM ledger_release")
		if !slices.Contains(got, "column finished_at") {
			t.Errorf("unnamed = %v, want finished_at named: it is a column of ledger_patrol_report only", got)
		}
	})
	t.Run("a table the golden lacks is named", func(t *testing.T) {
		t.Parallel()
		got := unnamed(golden, "SELECT line FROM ledger_ghost")
		if !slices.Contains(got, "table ledger_ghost") {
			t.Errorf("unnamed = %v, want the table ledger_ghost named", got)
		}
	})
	t.Run("literals and placeholders are not names", func(t *testing.T) {
		t.Parallel()
		if got := unnamed(golden, "SELECT has_table_privilege(current_user, $1, 'INSERT')"); len(got) != 0 {
			t.Errorf("unnamed = %v, want none", got)
		}
	})
	t.Run("a golden table that never closes is refused", func(t *testing.T) {
		t.Parallel()
		_, err := parseGolden("CREATE TABLE IF NOT EXISTS ledger_release (\n    app text NOT NULL,\n")
		if err == nil || !strings.Contains(err.Error(), "ledger_release") {
			t.Errorf("err = %v, want the unclosed table named", err)
		}
	})
	t.Run("a golden table with no columns is refused", func(t *testing.T) {
		t.Parallel()
		_, err := parseGolden("CREATE TABLE IF NOT EXISTS ledger_release (\n);\n")
		if err == nil || !strings.Contains(err.Error(), "ledger_release") {
			t.Errorf("err = %v, want the empty table named", err)
		}
	})
	t.Run("a table constraint names no column", func(t *testing.T) {
		t.Parallel()
		got, err := parseGolden("CREATE TABLE IF NOT EXISTS t (\n    a text NOT NULL,\n    PRIMARY KEY (a)\n);\n")
		if err != nil {
			t.Fatalf("parseGolden: %v", err)
		}
		if want := []string{"a"}; !slices.Equal(got["t"], want) {
			t.Errorf("columns = %v, want %v", got["t"], want)
		}
	})
}
