package land

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCallWalksParent(t *testing.T) {
	t.Parallel()
	for expr, want := range map[string]bool{
		`filepath.WalkDir("../..", f)`: true, `filepath.Walk("..", f)`: true, `os.ReadDir("../sibling")`: true,
		`fs.WalkDir(fsys, "../..", f)`: true, `fs.WalkDir("../..", fsys, f)`: false,
		`filepath.WalkDir(t.TempDir(), f)`: false, `filepath.WalkDir("internal", f)`: false,
		// ".." is a parent only as a whole path element.
		`filepath.WalkDir("..", f)`: true, `filepath.WalkDir("../x", f)`: true,
		`filepath.WalkDir("..foo", f)`: false, `filepath.WalkDir(".", f)`: false,
		`filepath.WalkDir(".hidden", f)`: false,
		// A raw-string literal is read like an interpreted one.
		"filepath.WalkDir(`../x`, f)": true,
		// A call with fewer arguments than the walker takes is false, not a panic.
		`filepath.WalkDir()`: false, `os.ReadDir()`: false, `fs.WalkDir(fsys)`: false,
		// fp aliases path/filepath, but callWalksParent reads the package name
		// as spelled and does not resolve imports: an aliased walk is not seen.
		`fp.WalkDir("..", f)`: false,
	} {
		call, err := parser.ParseExpr(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		if got := callWalksParent(call.(*ast.CallExpr)); got != want {
			t.Errorf("callWalksParent(%s) = %v, want %v", expr, got, want)
		}
	}
}

func TestBodyWalksParent(t *testing.T) {
	t.Parallel()
	for decl, want := range map[string]bool{
		`func F()`: false, // no body
		`func F() { filepath.WalkDir("../..", f) }`:        true,
		`func F() { defer func() { os.ReadDir("..") }() }`: true,
		`func F() { filepath.WalkDir(t.TempDir(), f) }`:    false,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+decl, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", decl, err)
		}
		if got := bodyWalksParent(f.Decls[0].(*ast.FuncDecl).Body); got != want {
			t.Errorf("bodyWalksParent(%s) = %v, want %v", decl, got, want)
		}
	}
}

func TestScanFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"walks a parent", "package p\n\nfunc TestA(t *testing.T) { filepath.WalkDir(\"../..\", f) }", []string{"TestA"}},
		{"walks a TempDir", "package p\n\nfunc TestA(t *testing.T) { filepath.WalkDir(t.TempDir(), f) }", nil},
		{"non-test helper", "package p\n\nfunc helper() { filepath.WalkDir(\"../..\", f) }", nil},
		{"source order", "package p\n\nfunc TestB(t *testing.T) { os.ReadDir(\"..\") }\n\nfunc TestA(t *testing.T) { filepath.Walk(\"../x\", f) }", []string{"TestB", "TestA"}},
	} {
		got, err := scanFile("x_test.go", []byte(tc.src))
		if err != nil {
			t.Fatalf("%s: scanFile: %v", tc.name, err)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: scanFile = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestScanFileParseError(t *testing.T) {
	t.Parallel()
	_, err := scanFile("bad.go", []byte("package p\n\nfunc ("))
	if err == nil || !strings.HasPrefix(err.Error(), "bad.go") {
		t.Fatalf("scanFile error = %v, want one starting with bad.go", err)
	}
}

func TestParseGoFile(t *testing.T) {
	t.Parallel()
	for src, wantErr := range map[string]bool{
		"package p\n\nfunc F() {}": false,
		"package p\n\nfunc (":      true,
	} {
		f, err := parseGoFile("file.go", []byte(src))
		if !wantErr && (f == nil || err != nil) {
			t.Fatalf("parseGoFile(%q) = %v, %v; want a parsed file", src, f, err)
		}
		if wantErr && (err == nil || !strings.HasPrefix(err.Error(), "file.go") || errors.Unwrap(err) == nil) {
			t.Errorf("parseGoFile(%q) error = %v, want the name first and the parser error wrapped", src, err)
		}
	}
}

func TestIsRunTest(t *testing.T) {
	t.Parallel()
	for decl, want := range map[string]bool{
		"func Test(t *testing.T) {}": true, "func TestA() {}": true, "func Test1() {}": true,
		"func Test_x() {}":    true, // go test runs Test_x: '_' is not lower case
		"func Testimony() {}": false, "func testA() {}": false, "func (s S) TestA() {}": false,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+decl, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", decl, err)
		}
		if got := isRunTest(f.Decls[0].(*ast.FuncDecl)); got != want {
			t.Errorf("isRunTest(%s) = %v, want %v", decl, got, want)
		}
	}
}

// unreadableDir is a file system whose directories cannot be listed.
type unreadableDir struct{ fstest.MapFS }

func (unreadableDir) ReadDir(string) ([]fs.DirEntry, error) { return nil, errors.New("no listing") }

// walkTest is the source of a test function named Test<n> that walks from a parent.
func walkTest(n string) []byte {
	return []byte("package p\n\nfunc Test" + n + "(t *testing.T) { os.ReadDir(\"..\") }")
}

func TestScanningTestsWalksFS(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"root_test.go":           {Data: walkTest("R")},
		"a/a_test.go":            {Data: walkTest("B")},
		"a/b_test.go":            {Data: walkTest("A")},
		"z/z_test.go":            {Data: walkTest("Z")},
		"testdata/t_test.go":     {Data: walkTest("T")},
		"vendor/v_test.go":       {Data: walkTest("V")},
		"node_modules/n_test.go": {Data: walkTest("N")},
		".git/g_test.go":         {Data: walkTest("G")},
	}
	got, err := ScanningTests(fsys)
	want := []ScanningTest{{".", "TestR"}, {"a", "TestA"}, {"a", "TestB"}, {"z", "TestZ"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ScanningTests = %v, %v; want %v", got, err, want)
	}
}

// TestScanningTestsFindsGitFileListings checks that a body naming ls-files is
// found and one naming ls-tree is not.
func TestScanningTestsFindsGitFileListings(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"list_test.go": {Data: []byte("package p\n\nfunc TestList(t *testing.T) { exec.Command(\"git\", \"ls-files\") }")},
		"tree_test.go": {Data: []byte("package p\n\nfunc TestTree(t *testing.T) { exec.Command(\"git\", \"ls-tree\") }")},
	}
	got, err := ScanningTests(fsys)
	want := []ScanningTest{{".", "TestList"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ScanningTests = %v, %v; want %v", got, err, want)
	}
}

func TestScanningTestsReturnsTheFirstError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fsys fs.FS
		want string
	}{
		"a file scanFile cannot parse": {fstest.MapFS{"a/bad_test.go": {Data: []byte("package p\n\nfunc (")}}, "a/bad_test.go"},
		"a file that cannot be read":   {fstest.MapFS{"a_test.go": {Mode: fs.ModeSymlink, Data: []byte("gone")}}, "open a_test.go"},
		"a directory that cannot list": {unreadableDir{fstest.MapFS{"a_test.go": {Data: walkTest("A")}}}, "no listing"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ScanningTests(tc.fsys); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ScanningTests error = %v, want one holding %q", err, tc.want)
			}
		})
	}
}
