package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// issueTextColumn matches one text column of a schema's column list: a name, then
// the type text, as tracker.issues declares them.
var issueTextColumn = regexp.MustCompile(`(?m)^\s+(\w+)\s+text\b`)

// issuesTable is the body of the CREATE TABLE tracker.issues statement: from its
// opening line to the closing ");" that ends it.
var issuesTable = regexp.MustCompile(`(?s)CREATE TABLE tracker\.issues \((.*?)\n\);`)

// issueAlter is one ALTER TABLE statement on tracker.issues: from its opening
// words to the ";" that ends it.
var issueAlter = regexp.MustCompile(`(?is)ALTER TABLE (?:ONLY )?tracker\.issues\s(.*?);`)

// addedTextColumn matches one ADD COLUMN clause that adds a text column.
var addedTextColumn = regexp.MustCompile(`(?is)\bADD\s+COLUMN\s+(?:IF NOT EXISTS\s+)?(\w+)\s+text\b`)

// issueTextColumns returns the text columns of tracker.issues that one schema
// file gives it: the columns its CREATE TABLE declares, then the ones an ALTER
// TABLE adds.
func issueTextColumns(schema string) []string {
	var columns []string
	for _, table := range issuesTable.FindAllStringSubmatch(schema, -1) {
		for _, col := range issueTextColumn.FindAllStringSubmatch(table[1], -1) {
			columns = append(columns, col[1])
		}
	}
	for _, alter := range issueAlter.FindAllStringSubmatch(schema, -1) {
		for _, col := range addedTextColumn.FindAllStringSubmatch(alter[1], -1) {
			columns = append(columns, col[1])
		}
	}
	return columns
}

// TestSizesSQLCountsEveryIssueTextColumn reads the tracker schema and fails when
// a text column of tracker.issues is missing from sizesSQL, so the report cannot
// drift from the table it sizes. A column added by a later migration is a text
// column the report must count too.
func TestSizesSQLCountsEveryIssueTextColumn(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "store", "schema", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("finding the tracker schema: %v (%d files)", err, len(files))
	}
	var columns []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		columns = append(columns, issueTextColumns(string(raw))...)
	}
	if len(columns) == 0 {
		t.Fatal("found no text columns for tracker.issues in the schema")
	}
	for _, col := range columns {
		if want := "octet_length(" + col + ")"; !strings.Contains(sizesSQL, want) {
			t.Errorf("sizesSQL does not count tracker.issues.%s: it must contain %q", col, want)
		}
	}
}

// TestIssueTextColumnsReadsAlterMigrations fails when a text column a later
// migration adds to tracker.issues is not among the columns the schema gives it,
// or when a column an ALTER adds to another table is taken for one of them.
func TestIssueTextColumnsReadsAlterMigrations(t *testing.T) {
	t.Parallel()
	schema := `CREATE TABLE tracker.issues (
    id       text PRIMARY KEY,
    priority smallint NOT NULL DEFAULT 2
);
ALTER TABLE tracker.issues
    ADD COLUMN archive_note text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS label text;
ALTER TABLE tracker.tokens ADD COLUMN token_note text;`
	got := issueTextColumns(schema)
	want := []string{"id", "archive_note", "label"}
	if !slices.Equal(got, want) {
		t.Errorf("issueTextColumns = %v, want %v", got, want)
	}
}

// TestSizesConnectErrorNeverRepeatsThePassword fails when the error Sizes returns
// on a failed connect repeats the DSN's password, whether the DSN does not parse
// or the dial fails.
func TestSizesConnectErrorNeverRepeatsThePassword(t *testing.T) {
	t.Parallel()
	const password = "s3cr3t-Pa55w0rd"
	// An empty directory holds no socket, so the dial fails at once.
	noSocket := url.PathEscape(t.TempDir())
	cases := []struct {
		name string
		dsn  string
	}{
		{"dial fails", "postgres://tracker:" + password + "@" + noSocket + "/tracker?sslmode=disable"},
		// An unencoded '@' in the password: pgx's redaction of its parse error misses it.
		{"DSN does not parse", "postgres://tracker:s3cr3t@" + password + "@db:x/tracker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := pgQuerier{dsn: tc.dsn}.Sizes(context.Background())
			if err == nil {
				t.Fatal("Sizes connected, want a connect error")
			}
			if strings.Contains(err.Error(), password) {
				t.Error("the connect error repeats the DSN password")
			}
		})
	}
}
