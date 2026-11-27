package testdb

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		uses, err := literalPrefixUses(path, src)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, u := range uses {
			t.Errorf("%s:%d: string literal %q names test-database prefix %q; use internal/testdb", rel, u.Line, u.Literal, u.Prefix)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := emptyScanErr(scanned, root); err != nil {
		t.Fatal(err)
	}
}

// prefixUse is a line and the string literal that names a test-database prefix there.
type prefixUse struct {
	Line            int
	Literal, Prefix string
}

// prefixAt matches a prefix where a name would start, so a longer word is not a copy.
var prefixAt = func() []*regexp.Regexp {
	re := make([]*regexp.Regexp, len(prefixes))
	for i, p := range prefixes {
		re[i] = regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(p))
	}
	return re
}()

// literalPrefixUses returns every string literal in src that names a
// test-database prefix, and the parse error if src does not parse. A source
// holding no prefix returns unparsed, its syntax error left to the compile.
func literalPrefixUses(name string, src []byte) ([]prefixUse, error) {
	if !slices.ContainsFunc(prefixes, func(p string) bool { return bytes.Contains(src, []byte(p)) }) {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var uses []prefixUse
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, perr := strconv.Unquote(lit.Value); perr == nil {
			for i, re := range prefixAt {
				if re.MatchString(v) {
					uses = append(uses, prefixUse{fset.Position(lit.Pos()).Line, v, prefixes[i]})
				}
			}
		}
		return true
	})
	return uses, nil
}

func TestLiteralPrefixUses(t *testing.T) {
	t.Parallel()
	for _, p := range Prefixes() {
		lit, src := p+"sample", []byte("package p\n\nconst Name = \""+p+"sample\"\n")
		if uses, err := literalPrefixUses("f.go", src); err != nil || len(uses) != 1 || uses[0] != (prefixUse{3, lit, p}) {
			t.Errorf("%q: literalPrefixUses = %v, %v; want one use on line 3", lit, uses, err)
		}
	}
	for _, tc := range []struct {
		name    string
		src     string
		wantErr bool
	}{
		{"prefix inside a longer word", "package p\n\nconst Name = \"x" + MintPrefix + "\"\n", false},
		{"prefix in a comment", "package p\n\n// const Name = \"testdb_a\"\nconst Name = \"plain\"\n", false},
		{"unparseable, no prefix", "package p\n\nfunc {\n", false},
		{"unparseable, prefix", "package p\n\nconst Name = \"testdb_a\"\n\nfunc {\n", true},
	} {
		if uses, err := literalPrefixUses("f.go", []byte(tc.src)); (err != nil) != tc.wantErr || len(uses) != 0 {
			t.Errorf("%s: literalPrefixUses = %v, %v; want no uses, error %v", tc.name, uses, err, tc.wantErr)
		}
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
