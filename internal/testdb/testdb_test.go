package testdb

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestIsTestDatabaseName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"testdb_0123abcd", "TESTDB_X", "beads_test_1", "beads_t9", "beads_pt_x",
		"beads_vr_x", "doctest_x", "doctortest_x", "benchdb_x",
		"dolt_remotes_check_x", "dolt_remotes_check_pool_3",
		MintPrefix + "x", RemotesCheckPrefix + "pool_0",
	} {
		if !IsTestDatabaseName(name) {
			t.Errorf("IsTestDatabaseName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"hq", "gastown", "beads", "om", "mango", "wl_commons", "testdb", "doltremotes", ""} {
		if IsTestDatabaseName(name) {
			t.Errorf("IsTestDatabaseName(%q) = true, want false", name)
		}
	}
}

func TestPrefixesIsACopy(t *testing.T) {
	t.Parallel()
	p := Prefixes()
	p[0] = "hq"
	if IsTestDatabaseName("hq") {
		t.Fatal("mutating the Prefixes() result changed the shared list")
	}
}

// repoRoot is the gastown module root, two directories up.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s has no go.mod: %v", root, err)
	}
	return root
}

// TestOneDefinition fails when any non-test Go file in the main module
// outside this package spells a test-database prefix as a string literal:
// the list forked into seven copies that disagreed (deep review B2-03,
// gt-fcxe9.9). Import this package instead.
func TestOneDefinition(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	self := filepath.Join(root, "internal", "testdb")
	// A prefix counts where a name would start: at the start of the literal
	// or after a non-identifier byte (space, |, (, ^, quote), so a word that
	// merely contains one, like "beads_types", is not a copy.
	prefixUse := make([]*regexp.Regexp, len(prefixes))
	for i, p := range prefixes {
		prefixUse[i] = regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(p))
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == self || strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			// A nested module (e.g. a plugin) cannot import internal/;
			// TestPluginCopyMatches covers the one that keeps a copy.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for i, p := range prefixes {
				if prefixUse[i].MatchString(v) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s:%d: string literal %q names test-database prefix %q; use internal/testdb", rel, fset.Position(lit.Pos()).Line, v, p)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPluginCopyMatches: plugins/dolt-snapshots is its own module and cannot
// import internal/testdb, so it keeps a copy; this keeps the copy equal.
func TestPluginCopyMatches(t *testing.T) {
	t.Parallel()
	path := filepath.Join(repoRoot(t), "plugins", "dolt-snapshots", "main.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "testDatabasePrefixes" || len(vs.Values) != 1 {
			return true
		}
		cl, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, e := range cl.Elts {
			if lit, ok := e.(*ast.BasicLit); ok {
				s, _ := strconv.Unquote(lit.Value)
				got = append(got, s)
			}
		}
		return false
	})
	if !reflect.DeepEqual(got, Prefixes()) {
		t.Fatalf("plugins/dolt-snapshots/main.go testDatabasePrefixes = %q, want %q (internal/testdb)", got, Prefixes())
	}
}
