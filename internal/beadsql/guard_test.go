package beadsql

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bdTableRead matches SQL text that reads a table bd owns.
var bdTableRead = regexp.MustCompile("(?i)\\b(FROM|JOIN)\\s+(`?[%\\w]+`?\\.)?`?" +
	`(issues|wisps|labels|wisp_labels|comments|wisp_comments|events|wisp_events|dependencies|wisp_dependencies|config|schema_migrations)` +
	"`?(\\s|$|\\)|,)")

// guardExempt are the trees that open their own connections on purpose:
// this package, and the test harness that creates and drops databases.
var guardExempt = []string{"beadsql", "testutil"}

// TestNoDirectSQLReadsOfBdTablesOutsideBeadsql fails when a non-test Go file
// under internal/ both opens a database/sql connection itself and carries
// SQL that reads a bd table. Such a read skips the schema-level check and the
// read-only guard; it must take a *DB from this package instead.
func TestNoDirectSQLReadsOfBdTablesOutsideBeadsql(t *testing.T) {
	t.Parallel()
	root := ".."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			for _, ex := range guardExempt {
				if path == filepath.Join(root, ex) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, []byte(`"database/sql"`)) {
			return nil
		}
		if opens, reads := directBdRead(t, path, src); opens && reads != "" {
			t.Errorf("%s opens its own SQL connection and reads a bd table (%s); read through beadsql.Open instead", path, reads)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// directBdRead reports whether src calls database/sql's Open or OpenDB, and
// the first string literal in it that reads a bd table.
func directBdRead(t *testing.T, path string, src []byte) (opens bool, reads string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	sqlName := ""
	for _, imp := range f.Imports {
		if imp.Path.Value == `"database/sql"` {
			sqlName = "sql"
			if imp.Name != nil {
				sqlName = imp.Name.Name
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Name == sqlName && (n.Sel.Name == "Open" || n.Sel.Name == "OpenDB") {
				opens = true
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING && reads == "" {
				if s, err := strconv.Unquote(n.Value); err == nil {
					reads = bdTableRead.FindString(s)
				}
			}
		}
		return true
	})
	return opens, strings.TrimSpace(reads)
}

func TestBdTableReadPattern(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"SELECT COUNT(*) FROM issues",
		"SELECT id FROM wisps w LEFT JOIN wisp_labels l ON w.id = l.issue_id",
		"SELECT created_at FROM `%s`.issues WHERE id = ?",
		"SELECT issue_id FROM dependencies WHERE issue_id = ?",
		"(SELECT issue_id FROM labels)",
	} {
		if !bdTableRead.MatchString(q) {
			t.Errorf("pattern misses %q", q)
		}
	}
	for _, q := range []string{
		"SELECT commit_hash FROM `hq`.dolt_log",
		"SHOW DATABASES",
		"read from issues_cache",
		"SELECT * FROM wl_wanted",
	} {
		if bdTableRead.MatchString(q) {
			t.Errorf("pattern flags %q", q)
		}
	}
}
