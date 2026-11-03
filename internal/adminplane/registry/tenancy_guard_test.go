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
// table, every exemption written without a reason, and every migration the
// tokenizer cannot read whole.
func tenancyViolations(ms []Migration, exempt map[string]string) []string {
	var out []string
	present := make(map[string]bool)
	// created is every table the migrations create, with the columns later ALTER
	// TABLE actions add, drop or rename recorded as the migrations run, so a
	// column added after the CREATE counts and one dropped after it does not.
	type createdTable struct {
		file     string
		name     string
		columns  []string
		parseErr string
	}
	var created []createdTable
	at := make(map[string]int) // table name -> index in created
	for _, m := range ms {
		file := fmt.Sprintf("%04d_%s.sql", m.Version, m.Name)
		toks, err := sqlTokens(m.SQL)
		if err != nil {
			// An unreadable file is reported, never scanned around: whatever it
			// hides after the point the tokenizer stopped would otherwise go
			// unchecked.
			out = append(out, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		for _, t := range scanCreateTables(toks) {
			if t.Name != "" {
				present[t.Name] = true
				at[t.Name] = len(created)
			}
			created = append(created, createdTable{file: file, name: t.Name, columns: t.Columns, parseErr: t.ParseErr})
		}
		for _, ch := range scanAlterColumns(toks) {
			i, ok := at[ch.table]
			if !ok {
				continue
			}
			if ch.err != "" {
				if created[i].parseErr == "" {
					created[i].parseErr = fmt.Sprintf("table %q has an ALTER TABLE action the guard cannot read: %s", ch.table, ch.err)
				}
				continue
			}
			created[i].columns = ch.apply(created[i].columns)
		}
	}
	for _, t := range created {
		if t.parseErr != "" {
			out = append(out, fmt.Sprintf("%s: %s", t.file, t.parseErr))
			continue
		}
		if slices.Contains(t.columns, "workspace_id") {
			continue
		}
		if _, ok := exempt[t.name]; ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s: table %q has no workspace_id column and no written exemption (add the column, or name the table in tenancyExempt with the reason)", t.file, t.name))
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

// scanCreateTables returns every table the tokens create. It reads only what it
// can check: a statement it cannot parse comes back with ParseErr set, never
// dropped, so a table cannot escape the guard by being written unusually.
func scanCreateTables(toks []sqlToken) []createTable {
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

// columnChange is one ALTER TABLE action that changes a table's column list:
// ADD appends add, DROP removes drop, and RENAME removes drop and appends add.
// err is set when the action names the table but not the column it changes; the
// table is then reported, because reading the action as "changes nothing" could
// let a dropped workspace_id pass unseen.
type columnChange struct {
	table string
	drop  string
	add   string
	err   string
}

// apply returns cols with this change applied: the dropped (or renamed-from)
// name removed, then the added (or renamed-to) name appended.
func (c columnChange) apply(cols []string) []string {
	if c.drop != "" {
		cols = slices.DeleteFunc(cols, func(s string) bool { return s == c.drop })
	}
	if c.add != "" {
		cols = append(cols, c.add)
	}
	return cols
}

// scanAlterColumns returns the column changes the tokens' ALTER TABLE actions
// make, in the order they run. It reads ADD [COLUMN], DROP [COLUMN] and
// RENAME [COLUMN] a TO b; DROP CONSTRAINT, RENAME CONSTRAINT and RENAME TO name
// no column and are skipped. An unreadable ADD adds nothing, so the table stays
// flagged unless its CREATE lists the column; an unreadable DROP or RENAME
// returns a change with err set. An ALTER of a table no migration creates is
// not reported here; the guard checks only the tables the migrations create.
func scanAlterColumns(toks []sqlToken) []columnChange {
	var out []columnChange
	for i := 0; i < len(toks); i++ {
		if !isWord(toks[i], "alter") {
			continue
		}
		j := i + 1
		if j >= len(toks) || !isWord(toks[j], "table") {
			continue
		}
		j++
		if isWordSeq(toks, j, "if", "exists") {
			j += 2
		}
		if j < len(toks) && isWord(toks[j], "only") {
			j++
		}
		name, at, ok := qualifiedName(toks, j)
		if !ok {
			continue
		}
		// The actions run to the end of the statement. A comma at the top level
		// starts the next action; a comma inside parentheses (numeric(10,2))
		// belongs to the action it sits in.
		depth := 0
		actionStart := true
		for ; at < len(toks); at++ {
			t := toks[at]
			switch {
			case t.punct == '(':
				depth++
			case t.punct == ')':
				depth--
			case depth == 0 && t.punct == ';':
				at = len(toks)
			case depth == 0 && t.punct == ',':
				actionStart = true
				continue
			}
			if depth != 0 || !actionStart {
				continue
			}
			actionStart = false
			switch {
			case isWord(t, "add"):
				k := at + 1
				if k < len(toks) && isWord(toks[k], "column") {
					k++
				}
				if isWordSeq(toks, k, "if", "not", "exists") {
					k += 3
				}
				if col, ok := identName(toks, k); ok {
					out = append(out, columnChange{table: name, add: col})
				}
			case isWord(t, "drop"):
				// DROP CONSTRAINT names no column; only a DROP COLUMN (or a bare
				// DROP <column>) changes the column list.
				if isWordSeq(toks, at+1, "constraint") {
					continue
				}
				if col, ok := alterDropColumn(toks, at+1); ok {
					out = append(out, columnChange{table: name, drop: col})
				} else {
					out = append(out, columnChange{table: name, err: "a DROP action names no column the guard can read"})
				}
			case isWord(t, "rename"):
				if col, to, ok := alterRenameColumn(toks, at+1); ok {
					out = append(out, columnChange{table: name, drop: col, add: to})
				} else if !alterRenamesOther(toks, at+1) {
					out = append(out, columnChange{table: name, err: "a RENAME action names no column the guard can read"})
				}
			}
		}
	}
	return out
}

// alterDropColumn reads a DROP action's column name: [COLUMN] [IF EXISTS] <col>.
func alterDropColumn(toks []sqlToken, k int) (string, bool) {
	if k < len(toks) && isWord(toks[k], "column") {
		k++
	}
	if isWordSeq(toks, k, "if", "exists") {
		k += 2
	}
	return identName(toks, k)
}

// alterRenameColumn reads a RENAME action's old and new column names:
// [COLUMN] <old> TO <new>.
func alterRenameColumn(toks []sqlToken, k int) (string, string, bool) {
	if k < len(toks) && isWord(toks[k], "column") {
		k++
	}
	from, ok := identName(toks, k)
	if !ok || k+1 >= len(toks) || !isWord(toks[k+1], "to") {
		return "", "", false
	}
	to, ok := identName(toks, k+2)
	if !ok {
		return "", "", false
	}
	return from, to, true
}

// alterRenamesOther reports whether the RENAME action at k renames the table
// (RENAME TO <name>) or one of its constraints (RENAME CONSTRAINT ...), neither
// of which changes the column list.
func alterRenamesOther(toks []sqlToken, k int) bool {
	return isWordSeq(toks, k, "to") || isWordSeq(toks, k, "constraint")
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
// Comments, string literals and dollar-quoted bodies are dropped so a statement
// written inside one is not read as a table, and a '--' inside a string is not
// read as a comment. A dollar-quoted body that is never closed is an error, not
// a body running to the end of the file: the statements it would swallow must
// not pass unchecked.
func sqlTokens(sql string) ([]sqlToken, error) {
	var toks []sqlToken
	for i := 0; i < len(sql); {
		c := sql[i]
		if c == '$' {
			end, opened, closed := dollarQuotedEnd(sql, i)
			if opened {
				if !closed {
					return nil, fmt.Errorf("a dollar-quoted string opened at byte %d is never closed; the guard cannot read the rest of the file", i)
				}
				i = end
				continue
			}
		}
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
	return toks, nil
}

// dollarQuotedEnd reads the dollar-quoted string that begins at sql[i], a '$'.
// A dollar-quoted body is data, so it is skipped whole: otherwise a CREATE
// TABLE inside a function body would be read as a table. The tag is empty or
// follows identifier rules (a letter or underscore first), so a '$1' placeholder
// does not open a quote. opened reports that a delimiter starts here, closed
// that the matching delimiter was found, and end is the index after the body;
// an unterminated body returns opened and not closed so the caller can refuse
// the file rather than skip what follows it.
func dollarQuotedEnd(sql string, i int) (end int, opened, closed bool) {
	j := i + 1
	for j < len(sql) && isWordByte(sql[j]) && sql[j] != '$' {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return 0, false, false
	}
	if j > i+1 && !isTagStart(sql[i+1]) {
		return 0, false, false
	}
	delim := sql[i : j+1]
	body := j + 1
	if k := strings.Index(sql[body:], delim); k >= 0 {
		return body + k + len(delim), true, true
	}
	return 0, true, false
}

func isTagStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
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
// IF NOT EXISTS, a statement it must not pass by dropping it, and a
// dollar-quoted body it must refuse rather than skip.
func TestTenancyGuardScanner(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		sql        string
		want       map[string][]string
		wantErr    bool
		wantTokErr bool
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
		"dollar quoted body names no table": {
			sql:  `CREATE FUNCTION f() RETURNS void AS $$ BEGIN CREATE TABLE ghost (id text); END $$ LANGUAGE plpgsql;`,
			want: map[string][]string{},
		},
		"tagged dollar quoted body": {
			sql:  `DO $body$ BEGIN CREATE TABLE ghost (id text); END $body$; CREATE TABLE events (workspace_id text);`,
			want: map[string][]string{"events": {"workspace_id"}},
		},
		"unterminated dollar quote is refused": {
			sql:        `$$ CREATE TABLE ghost (id text);`,
			wantTokErr: true,
		},
		"a dollar placeholder is not a quoted body": {
			sql:  `CREATE TABLE events (workspace_id text, note text DEFAULT $1);`,
			want: map[string][]string{"events": {"workspace_id", "note"}},
		},
		"a digit tag does not open a quoted body": {
			sql:  `SELECT $1$ CREATE TABLE ghost (id text); $1$;`,
			want: map[string][]string{"ghost": {"id"}},
		},
		"create as select is unreadable": {
			sql:     `CREATE TABLE events AS SELECT 1;`,
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			toks, err := sqlTokens(tc.sql)
			if tc.wantTokErr {
				if err == nil {
					t.Fatalf("sqlTokens(%q) read the file; want an error naming the unterminated body", tc.sql)
				}
				return
			}
			if err != nil {
				t.Fatalf("sqlTokens(%q) = %v; want no error", tc.sql, err)
			}
			got := scanCreateTables(toks)
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

// TestTenancyGuardConsidersAlterAddColumn drives the guard on canned migrations
// whose table gets the column from ALTER TABLE ... ADD COLUMN rather than its
// CREATE: a later migration adds it, the creating file adds it after the CREATE,
// and an alter that names another column, another table, or runs before the
// create leaves the table flagged.
func TestTenancyGuardConsidersAlterAddColumn(t *testing.T) {
	t.Parallel()
	create := Migration{Version: 1, Name: "events", SQL: `CREATE TABLE adminplane.events (id text);`}
	for name, tc := range map[string]struct {
		ms      []Migration
		wantErr bool
	}{
		"a later migration adds the column": {
			ms: []Migration{create, {Version: 2, Name: "events_workspace", SQL: `ALTER TABLE adminplane.events ADD COLUMN workspace_id text NOT NULL;`}},
		},
		"the creating file adds the column after the create": {
			ms: []Migration{{Version: 1, Name: "events", SQL: `CREATE TABLE adminplane.events (id text); ALTER TABLE adminplane.events ADD workspace_id text;`}},
		},
		"if not exists and a qualified name": {
			ms: []Migration{create, {Version: 2, Name: "events_workspace", SQL: `ALTER TABLE IF EXISTS "adminplane"."events" ADD COLUMN IF NOT EXISTS workspace_id text;`}},
		},
		"one of several actions adds the column": {
			ms: []Migration{create, {Version: 2, Name: "events_workspace", SQL: `ALTER TABLE events ADD COLUMN owner_id text, ADD COLUMN workspace_id text;`}},
		},
		"a different column does not satisfy the guard": {
			ms:      []Migration{create, {Version: 2, Name: "events_owner", SQL: `ALTER TABLE events ADD COLUMN owner_id text;`}},
			wantErr: true,
		},
		"a constraint named workspace_id does not satisfy the guard": {
			ms:      []Migration{create, {Version: 2, Name: "events_check", SQL: `ALTER TABLE events ADD CONSTRAINT workspace_id CHECK (id <> '');`}},
			wantErr: true,
		},
		"another table's column does not satisfy the guard": {
			ms:      []Migration{create, {Version: 2, Name: "other", SQL: `CREATE TABLE other (id text); ALTER TABLE other ADD COLUMN workspace_id text;`}},
			wantErr: true,
		},
		"an alter before the create does not count": {
			ms:      []Migration{{Version: 1, Name: "pre", SQL: `ALTER TABLE events ADD COLUMN workspace_id text;`}, {Version: 2, Name: "events", SQL: `CREATE TABLE events (id text);`}},
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tenancyViolations(tc.ms, nil)
			if tc.wantErr {
				if len(got) == 0 {
					t.Fatalf("tenancyViolations reported nothing; want the table with no workspace_id flagged")
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("tenancyViolations = %v; want no violation", got)
			}
		})
	}
}

// TestTenancyGuardConsidersAlterDropAndRename drives the guard on canned
// migrations whose table loses the column to ALTER TABLE ... DROP COLUMN or
// RENAME COLUMN: the drop of workspace_id, of another column, the rename away
// from it and to it, and the actions that name no column.
func TestTenancyGuardConsidersAlterDropAndRename(t *testing.T) {
	t.Parallel()
	create := Migration{Version: 1, Name: "events", SQL: `CREATE TABLE adminplane.events (id text);`}
	withColumn := Migration{Version: 1, Name: "events", SQL: `CREATE TABLE adminplane.events (id text, workspace_id text NOT NULL);`}
	for name, tc := range map[string]struct {
		ms      []Migration
		wantErr bool
		want    string
	}{
		"a later migration drops the column": {
			ms:      []Migration{withColumn, {Version: 2, Name: "drop_workspace", SQL: `ALTER TABLE adminplane.events DROP COLUMN workspace_id;`}},
			wantErr: true,
		},
		"a drop without the column keyword": {
			ms:      []Migration{withColumn, {Version: 2, Name: "drop_workspace", SQL: `ALTER TABLE events DROP workspace_id;`}},
			wantErr: true,
		},
		"an add then a drop in one statement leaves the column absent": {
			ms:      []Migration{create, {Version: 2, Name: "add_then_drop", SQL: `ALTER TABLE events ADD COLUMN workspace_id text, DROP COLUMN workspace_id;`}},
			wantErr: true,
		},
		"a drop of another column keeps the guard satisfied": {
			ms: []Migration{withColumn, {Version: 2, Name: "drop_owner", SQL: `ALTER TABLE events DROP COLUMN id;`}},
		},
		"a dropped constraint is not a dropped column": {
			ms: []Migration{withColumn, {Version: 2, Name: "drop_constraint", SQL: `ALTER TABLE events DROP CONSTRAINT events_workspace_check;`}},
		},
		"a rename away from the column": {
			ms:      []Migration{withColumn, {Version: 2, Name: "rename_workspace", SQL: `ALTER TABLE events RENAME COLUMN workspace_id TO tenant_id;`}},
			wantErr: true,
		},
		"a rename without the column keyword": {
			ms:      []Migration{withColumn, {Version: 2, Name: "rename_workspace", SQL: `ALTER TABLE events RENAME workspace_id TO tenant_id;`}},
			wantErr: true,
		},
		"a rename to the column satisfies the guard": {
			ms: []Migration{create, {Version: 2, Name: "rename_tenant", SQL: `ALTER TABLE events RENAME COLUMN id TO workspace_id;`}},
		},
		"an add then a rename in one statement": {
			ms: []Migration{create, {Version: 2, Name: "add_then_rename", SQL: `ALTER TABLE events ADD COLUMN tenant_id text, RENAME COLUMN tenant_id TO workspace_id;`}},
		},
		"a renamed table is not a renamed column": {
			ms: []Migration{withColumn, {Version: 2, Name: "rename_table", SQL: `ALTER TABLE events RENAME TO events_v2;`}},
		},
		"a renamed constraint is not a renamed column": {
			ms: []Migration{withColumn, {Version: 2, Name: "rename_constraint", SQL: `ALTER TABLE events RENAME CONSTRAINT events_pkey TO events_pk;`}},
		},
		"a drop the scanner cannot read is refused": {
			ms:      []Migration{withColumn, {Version: 2, Name: "bad_drop", SQL: `ALTER TABLE events DROP COLUMN IF EXISTS;`}},
			wantErr: true,
			want:    "cannot read",
		},
		"a rename the scanner cannot read is refused": {
			ms:      []Migration{withColumn, {Version: 2, Name: "bad_rename", SQL: `ALTER TABLE events RENAME COLUMN;`}},
			wantErr: true,
			want:    "cannot read",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tenancyViolations(tc.ms, nil)
			if tc.wantErr {
				if len(got) == 0 {
					t.Fatalf("tenancyViolations reported nothing; want the table with no workspace_id flagged")
				}
				if tc.want != "" && !strings.Contains(strings.Join(got, "\n"), tc.want) {
					t.Fatalf("tenancyViolations = %v; want a line naming %q", got, tc.want)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("tenancyViolations = %v; want no violation", got)
			}
		})
	}
}

// TestTenancyGuardRefusesUnterminatedDollarQuote drives the guard on a migration
// whose dollar-quoted string is never closed: the body would otherwise hide
// every later statement, so the guard must report the file rather than read it
// as clean.
func TestTenancyGuardRefusesUnterminatedDollarQuote(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ms      []Migration
		wantErr bool
	}{
		"an unterminated body hides a table": {
			ms:      []Migration{{Version: 1, Name: "broken", SQL: `$$ CREATE TABLE ghost (id text);`}},
			wantErr: true,
		},
		"an unterminated body hides the tables after a clean statement": {
			ms:      []Migration{{Version: 1, Name: "broken", SQL: `CREATE TABLE events (workspace_id text); $$ CREATE TABLE ghost (id text);`}},
			wantErr: true,
		},
		"a body closed before the tables is read": {
			ms: []Migration{{Version: 1, Name: "fn", SQL: `DO $body$ BEGIN CREATE TABLE ghost (id text); END $body$; CREATE TABLE events (workspace_id text);`}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tenancyViolations(tc.ms, nil)
			if !tc.wantErr {
				if len(got) != 0 {
					t.Fatalf("tenancyViolations = %v; want no violation", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("tenancyViolations reported nothing; want the unterminated body reported")
			}
			if joined := strings.Join(got, "\n"); !strings.Contains(joined, "never closed") {
				t.Fatalf("tenancyViolations = %v; want a line naming the unterminated body", got)
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
