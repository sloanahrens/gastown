package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// issueTextColumn matches one text column of a schema's column list: a name, then
// the type text, as tracker.issues declares them.
var issueTextColumn = regexp.MustCompile(`(?m)^\s+(\w+)\s+text\b`)

// issuesTable is the body of the CREATE TABLE tracker.issues statement: from its
// opening line to the closing ");" that ends it.
var issuesTable = regexp.MustCompile(`(?s)CREATE TABLE tracker\.issues \((.*?)\n\);`)

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
		for _, table := range issuesTable.FindAllStringSubmatch(string(raw), -1) {
			for _, col := range issueTextColumn.FindAllStringSubmatch(table[1], -1) {
				columns = append(columns, col[1])
			}
		}
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
