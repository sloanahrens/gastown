package forgejotest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// guardedImport is the import path this package has, which only test files may use.
const guardedImport = "github.com/steveyegge/gastown/internal/forgejotest"

// TestForgejotestOnlyImportedByTests walks the module and fails on any non-test Go file
// that imports this package, so the fake cannot reach production code. The walk must see
// the production files it is checking, or a walk that found nothing would pass.
func TestForgejotestOnlyImportedByTests(t *testing.T) {
	t.Parallel()
	for _, problem := range importGuardProblems(os.DirFS(filepath.Join("..", "..")), ".") {
		t.Error(problem)
	}
}

// TestImportGuardRefusals drives each branch of the guard over an in-memory module, so a
// refusal is shown to be reported and a clean tree to pass, without a real walk.
func TestImportGuardRefusals(t *testing.T) {
	t.Parallel()
	const importer = "package run\n\nimport \"github.com/steveyegge/gastown/internal/forgejotest\"\n"
	const clean = "package run\n\nimport \"strings\"\n"
	cases := []struct {
		name    string
		tree    fs.FS
		root    string
		want    string
		wantNil bool
	}{
		{
			name: "a production file that imports the fake is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(importer)}},
			root: ".",
			want: "internal/run/run.go",
		},
		{
			name: "a test file may import the fake",
			tree: fstest.MapFS{
				"internal/run/run.go":      {Data: []byte(clean)},
				"internal/run/run_test.go": {Data: []byte(importer)},
			},
			root:    ".",
			wantNil: true,
		},
		{
			name:    "a clean production tree passes",
			tree:    fstest.MapFS{"internal/run/run.go": {Data: []byte(clean)}},
			root:    ".",
			wantNil: true,
		},
		{
			name:    "a hidden directory is not walked",
			tree:    fstest.MapFS{".cache/run.go": {Data: []byte(importer)}, "internal/run/run.go": {Data: []byte(clean)}},
			root:    ".",
			wantNil: true,
		},
		{
			name: "a tree with no production files is refused, so the guard cannot pass on nothing",
			tree: fstest.MapFS{"internal/run/run_test.go": {Data: []byte(clean)}},
			root: ".",
			want: "no production files",
		},
		{
			name: "a root the walk cannot find is refused",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(clean)}},
			root: "missing",
			want: "walk the module",
		},
		{
			name: "a production file that cannot be read is refused, not passed as clean",
			tree: unreadable{fstest.MapFS{"internal/run/run.go": {Data: []byte(clean)}}, "internal/run/run.go"},
			root: ".",
			want: "walk the module",
		},
		{
			name: "a production file that does not parse is refused",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte("package run\n\nimport (\n")}},
			root: ".",
			want: "walk the module",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := importGuardProblems(tc.tree, tc.root)
			if tc.wantNil {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %v, want one naming %q", problems, tc.want)
			}
		})
	}
}

// unreadable is a tree whose Open fails for one path, so a file the guard cannot read is an
// error, not a file it can pass over. It has no ReadFile of its own, so fs.ReadFile goes
// through Open.
type unreadable struct {
	tree fstest.MapFS
	path string
}

func (u unreadable) Open(name string) (fs.File, error) {
	if name == u.path {
		return nil, fs.ErrPermission
	}
	return u.tree.Open(name)
}

// importGuardProblems returns what the guard finds under root in fsys: a walk that failed,
// a walk that saw no production file, or a production file that imports the fake. It
// returns none for a tree that passes.
func importGuardProblems(fsys fs.FS, root string) []string {
	checked, offenders, err := importersOf(fsys, root, guardedImport)
	switch {
	case err != nil:
		return []string{fmt.Sprintf("walk the module: %v", err)}
	case checked == 0:
		return []string{"the walk found no production files to check"}
	case len(offenders) != 0:
		return []string{fmt.Sprintf("non-test files import %s: %v", guardedImport, offenders)}
	}
	return nil
}

// importersOf walks fsys from root and returns how many non-test Go files it parsed and
// which of them import self. Hidden directories are skipped.
func importersOf(fsys fs.FS, root, self string) (checked int, offenders []string, err error) {
	fset := token.NewFileSet()
	err = fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == self {
				offenders = append(offenders, path)
			}
		}
		return nil
	})
	return checked, offenders, err
}
