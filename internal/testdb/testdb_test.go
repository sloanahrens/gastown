package testdb

import (
	"fmt"
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

// resolveRoot makes rel absolute and follows symlinks. A sandbox seat reaches
// its checkout through a symlink, and filepath.WalkDir does not descend a
// symlink root, so an unresolved root is walked as a single file.
func resolveRoot(rel string) (string, error) {
	root, err := filepath.Abs(rel)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

// emptyScanErr is the failure for a walk that read no Go file: such a check
// passes on any tree, so it proves nothing.
func emptyScanErr(scanned int, root string) error {
	if scanned > 0 {
		return nil
	}
	return fmt.Errorf("scanned no Go file under %s", root)
}

// repoRoot is the gastown module root, two directories up.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := resolveRoot(filepath.Join("..", ".."))
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
	scanned := 0
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
		scanned++
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
	if err := emptyScanErr(scanned, root); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRootFollowsASymlink(t *testing.T) {
	t.Parallel()
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	visited := func(root string) int {
		n := 0
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(path, ".go") {
				n++
			}
			return nil
		})
		return n
	}
	if got := visited(link); got != 0 {
		t.Fatalf("premise: WalkDir over the symlink visited %d Go files, want 0", got)
	}
	root, err := resolveRoot(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := visited(root); got != 1 {
		t.Fatalf("WalkDir over the resolved root %s visited %d Go files, want 1", root, got)
	}
}

func TestEmptyScanIsAFailure(t *testing.T) {
	t.Parallel()
	if err := emptyScanErr(0, "/x"); err == nil {
		t.Fatal("an empty scan passed")
	}
	if err := emptyScanErr(3, "/x"); err != nil {
		t.Fatalf("a scan of 3 files failed: %v", err)
	}
}
