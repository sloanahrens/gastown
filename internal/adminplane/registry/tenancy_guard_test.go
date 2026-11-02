package registry

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// tenancyExempt names the tables the migrations create that carry no
// workspace_id column, and why. Round 1 of the tenancy decision is "workspace
// id in every record" (gt-411ng), so a table not listed here must have the
// column. An entry that names no table fails, so the list cannot rot.
var tenancyExempt = map[string]string{
	"workspaces": "keyed by id: a workspace id is the row's own identity, so a workspace_id column would name itself",
}

// createTable is one CREATE TABLE statement as the guard reads it. ParseErr is
// set when the statement creates a table the guard cannot check; reporting it
// as a violation keeps an unreadable statement from passing as clean.
type createTable struct {
	Name     string
	Columns  []string
	ParseErr string
}

// sqlToken is one lexical item of SQL: a word or quoted identifier (punct 0),
// or one of the punctuation bytes the scanner reads. Unquoted words are
// lower-cased because Postgres folds unquoted identifiers.
type sqlToken struct {
	text   string
	punct  byte
	quoted bool
}

// tenancyViolations reports every table the migrations create that has no
// workspace_id column and no written exemption, every exemption that names no
// table, and every exemption written without a reason.
func tenancyViolations(ms []Migration, exempt map[string]string) []string {
	var out []string
	present := make(map[string]bool)
	for _, m := range ms {
		file := fmt.Sprintf("%04d_%s.sql", m.Version, m.Name)
		for _, t := range scanCreateTables(m.SQL) {
			if t.Name != "" {
				present[t.Name] = true
			}
			if t.ParseErr != "" {
				out = append(out, fmt.Sprintf("%s: %s", file, t.ParseErr))
				continue
			}
			if slices.Contains(t.Columns, "workspace_id") {
				continue
			}
			if _, ok := exempt[t.Name]; ok {
				continue
			}
			out = append(out, fmt.Sprintf("%s: table %q has no workspace_id column and no written exemption (add the column, or name the table in tenancyExempt with the reason)", file, t.Name))
		}
	}
	for name, reason := range exempt {
		if !present[name] {
			out = append(out, fmt.Sprintf("tenancyExempt names table %q, which no migration creates; delete the entry or fix its name", name))
			continue
		}
		if strings.TrimSpace(reason) == "" {
			out = append(out, fmt.Sprintf("tenancyExempt entry for table %q has no written reason", name))
		}
	}
	sort.Strings(out)
	return out
}

// scanCreateTables returns every table sql creates. It reads only what it can
// check: a statement it cannot parse comes back with ParseErr set, never
// dropped, so a table cannot escape the guard by being written unusually.
func scanCreateTables(sql string) []createTable {
	toks := sqlTokens(sql)
	var out []createTable
	for i := 0; i < len(toks); i++ {
		if !isWord(toks[i], "create") {
			continue
		}
		j := i + 1
		// TEMP, TEMPORARY, UNLOGGED and GLOBAL/LOCAL may sit between CREATE and
		// TABLE; skipping them means an unlogged table is still checked.
		for j < len(toks) && (isWord(toks[j], "global") || isWord(toks[j], "local") ||
			isWord(toks[j], "temporary") || isWord(toks[j], "temp") || isWord(toks[j], "unlogged")) {
			j++
		}
		if j >= len(toks) || !isWord(toks[j], "table") {
			continue
		}
		j++
		if isWordSeq(toks, j, "if", "not", "exists") {
			j += 3
		}
		name, after, ok := qualifiedName(toks, j)
		if !ok {
			out = append(out, createTable{ParseErr: "a CREATE TABLE statement has no table name the guard can read"})
			i = j - 1
			continue
		}
		if after >= len(toks) || toks[after].punct != '(' {
			out = append(out, createTable{Name: name, ParseErr: fmt.Sprintf("table %q has no column list the guard can read; a CREATE TABLE ... AS statement needs explicit columns or a written exemption", name)})
			i = after - 1
			continue
		}
		cols, next, ok := columnList(toks, after)
		if !ok {
			out = append(out, createTable{Name: name, ParseErr: fmt.Sprintf("table %q has an unterminated column list", name)})
			i = len(toks)
			continue
		}
		out = append(out, createTable{Name: name, Columns: cols})
		i = next - 1
	}
	return out
}

// columnList reads the column names in the table body beginning at toks[open],
// a '('. A body item's first word is a column name unless it is a table
// constraint such as PRIMARY KEY, which names none.
func columnList(toks []sqlToken, open int) ([]string, int, bool) {
	var cols []string
	depth := 0
	expectName := false
	for i := open; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.punct == '(':
			depth++
			if depth == 1 {
				expectName = true
			}
		case t.punct == ')':
			depth--
			if depth == 0 {
				return cols, i + 1, true
			}
		case depth == 1 && t.punct == ',':
			expectName = true
		case depth == 1 && expectName:
			expectName = false
			if t.punct != 0 {
				continue
			}
			if isTableConstraint(t) {
				continue
			}
			cols = append(cols, t.text)
		}
	}
	return cols, len(toks), false
}

// qualifiedName reads a possibly schema-qualified table name, returning the
// bare table name and the index after it.
func qualifiedName(toks []sqlToken, i int) (string, int, bool) {
	name, ok := identName(toks, i)
	if !ok {
		return "", i, false
	}
	i++
	if i+1 < len(toks) && toks[i].punct == '.' {
		second, ok := identName(toks, i+1)
		if !ok {
			return "", i, false
		}
		name = second
		i += 2
	}
	return name, i, true
}

func identName(toks []sqlToken, i int) (string, bool) {
	if i >= len(toks) || toks[i].punct != 0 || toks[i].text == "" {
		return "", false
	}
	return toks[i].text, true
}

// sqlTokens splits sql into the identifiers and punctuation the scanner reads.
// Comments and string literals are dropped so a statement written inside one
// is not read as a table, and a '--' inside a string is not read as a comment.
func sqlTokens(sql string) []sqlToken {
	var toks []sqlToken
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			i += 2
			if i > len(sql) {
				i = len(sql)
			}
		case c == '\'':
			// A string literal is data, never a table or column name.
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '"':
			i++
			var b strings.Builder
			for i < len(sql) {
				if sql[i] == '"' {
					if i+1 < len(sql) && sql[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			toks = append(toks, sqlToken{text: b.String(), quoted: true})
		case c == '(' || c == ')' || c == ',' || c == '.' || c == ';':
			toks = append(toks, sqlToken{punct: c})
			i++
		case isWordByte(c):
			start := i
			for i < len(sql) && isWordByte(sql[i]) {
				i++
			}
			toks = append(toks, sqlToken{text: strings.ToLower(sql[start:i])})
		default:
			i++
		}
	}
	return toks
}

func isWord(t sqlToken, word string) bool {
	return t.punct == 0 && !t.quoted && t.text == word
}

func isWordSeq(toks []sqlToken, i int, words ...string) bool {
	if i+len(words) > len(toks) {
		return false
	}
	for k, w := range words {
		if !isWord(toks[i+k], w) {
			return false
		}
	}
	return true
}

func isTableConstraint(t sqlToken) bool {
	if t.punct != 0 || t.quoted {
		return false
	}
	switch t.text {
	case "primary", "unique", "check", "constraint", "foreign", "exclude", "like":
		return true
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' ||
		('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// TestTenancyGuardScanner drives the scanner on canned SQL: a table with the
// column, one without, quoted and schema-qualified names, a trailing comment,
// IF NOT EXISTS, and a statement it must not pass by dropping it.
func TestTenancyGuardScanner(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		sql     string
		want    map[string][]string
		wantErr bool
	}{
		"column present": {
			sql:  `CREATE TABLE adminplane.events (id text, workspace_id text NOT NULL);`,
			want: map[string][]string{"events": {"id", "workspace_id"}},
		},
		"column absent": {
			sql:  `CREATE TABLE adminplane.events (id text);`,
			want: map[string][]string{"events": {"id"}},
		},
		"quoted schema and table": {
			sql:  `CREATE TABLE "adminplane"."Events" ("id" text, "workspace_id" text);`,
			want: map[string][]string{"Events": {"id", "workspace_id"}},
		},
		"unquoted case folds": {
			sql:  `CREATE TABLE EVENTS (Workspace_ID text);`,
			want: map[string][]string{"events": {"workspace_id"}},
		},
		"trailing comment": {
			sql:  "CREATE TABLE events (workspace_id text); -- one row per workspace",
			want: map[string][]string{"events": {"workspace_id"}},
		},
		"comment names no table": {
			sql:  "-- CREATE TABLE ghost (id text);\nCREATE TABLE events (workspace_id text);",
			want: map[string][]string{"events": {"workspace_id"}},
		},
		"dash inside a string": {
			sql:  `CREATE TABLE events (workspace_id text DEFAULT '--', note text);`,
			want: map[string][]string{"events": {"workspace_id", "note"}},
		},
		"if not exists": {
			sql:  `CREATE TABLE IF NOT EXISTS adminplane.events (workspace_id text);`,
			want: map[string][]string{"events": {"workspace_id"}},
		},
		"table constraint": {
			sql:  `CREATE TABLE events (id text, PRIMARY KEY (id));`,
			want: map[string][]string{"events": {"id"}},
		},
		"create as select is unreadable": {
			sql:     `CREATE TABLE events AS SELECT 1;`,
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := scanCreateTables(tc.sql)
			tables := make(map[string][]string)
			errs := 0
			for _, ct := range got {
				if ct.ParseErr != "" {
					errs++
					continue
				}
				tables[ct.Name] = ct.Columns
			}
			if tc.wantErr {
				if errs == 0 {
					t.Errorf("scanCreateTables(%q) = %+v; want a statement it cannot read to be reported, not dropped", tc.sql, got)
				}
				return
			}
			if errs != 0 {
				t.Fatalf("scanCreateTables(%q) reported %d unreadable statement(s); want none", tc.sql, errs)
			}
			if len(tables) != len(tc.want) {
				t.Fatalf("scanCreateTables(%q) read tables %v; want %v", tc.sql, tables, tc.want)
			}
			for name, wantCols := range tc.want {
				if !slices.Equal(tables[name], wantCols) {
					t.Errorf("scanCreateTables(%q) read columns %v of table %q; want %v", tc.sql, tables[name], name, wantCols)
				}
			}
		})
	}
}

// TestTenancyGuardFlagsMissingColumn drives the guard's refusal branches on
// canned migrations: the missing column, the exemption that covers it, the
// stale entry, and the entry written without a reason.
func TestTenancyGuardFlagsMissingColumn(t *testing.T) {
	t.Parallel()
	missing := []Migration{{Version: 1, Name: "events", SQL: `CREATE TABLE adminplane.events (id text);`}}
	withColumn := []Migration{{Version: 1, Name: "events", SQL: `CREATE TABLE adminplane.events (id text, workspace_id text NOT NULL);`}}

	for name, tc := range map[string]struct {
		ms     []Migration
		exempt map[string]string
		want   []string
	}{
		"missing column names its table and file": {
			ms:   missing,
			want: []string{"0001_events.sql", `"events"`, "no workspace_id column"},
		},
		"the column passes": {
			ms: withColumn,
		},
		"an exemption with a reason passes": {
			ms:     missing,
			exempt: map[string]string{"events": "keyed by id"},
		},
		"an exemption naming no table is stale": {
			ms:     withColumn,
			exempt: map[string]string{"gone": "keyed by id"},
			want:   []string{`"gone"`, "no migration creates"},
		},
		"an exemption without a reason is refused": {
			ms:     missing,
			exempt: map[string]string{"events": "  "},
			want:   []string{`"events"`, "no written reason"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tenancyViolations(tc.ms, tc.exempt)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("tenancyViolations = %v; want no violation", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("tenancyViolations reported nothing; want a line naming %v", tc.want)
			}
			joined := strings.Join(got, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("tenancyViolations reported:\n%s\nwant a line naming %q", joined, w)
				}
			}
		})
	}
}

// TestTenancyGuardRealMigrations holds the embedded schema to the tenancy rule:
// every table has a workspace_id column or a written exemption, and every
// exemption names a table that exists. Removing the workspaces entry fails here
// because that table is keyed by id and carries no workspace_id column.
func TestTenancyGuardRealMigrations(t *testing.T) {
	t.Parallel()
	ms, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations() = %v; the guard needs the embedded schema", err)
	}
	if got := tenancyViolations(ms, tenancyExempt); len(got) > 0 {
		t.Errorf("tenancy guard found %d problem(s):\n%s", len(got), strings.Join(got, "\n"))
	}
}
