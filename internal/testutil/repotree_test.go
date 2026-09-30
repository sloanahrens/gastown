package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// repoTree is a module's Go source, walked and parsed once. The repo-wide
// guards in this package (TestNoParallelTestsReachProcessGlobalSwaps and
// TestHermeticHarnessEnforced) share one for the real module instead of each
// walking and parsing it, and neither runs the go command.
type repoTree struct {
	root   string
	module string
	fset   *token.FileSet
	dirs   []string               // every directory walked, sorted
	files  map[string][]*repoFile // dir -> its .go files, sorted by name
}

// repoFile is one parsed file, or the error parsing it.
type repoFile struct {
	path string
	name string
	ast  *ast.File
	err  error
}

var (
	moduleTreeOnce sync.Once
	moduleTree     *repoTree
	moduleTreeErr  error
)

// sharedRepoTree is the parse of this module, made on first use.
func sharedRepoTree(t *testing.T) *repoTree {
	t.Helper()
	moduleTreeOnce.Do(func() { moduleTree, moduleTreeErr = loadRepoTree(repoRoot(t)) })
	if moduleTreeErr != nil {
		t.Fatal(moduleTreeErr)
	}
	return moduleTree
}

// loadRepoTree walks root, skipping vendor, .git, testdata, node_modules and
// nested modules, and parses every .go file with comments, in parallel.
func loadRepoTree(root string) (*repoTree, error) {
	module, err := readModulePath(root)
	if err != nil {
		return nil, err
	}
	tree := &repoTree{root: root, module: module, fset: token.NewFileSet(), files: map[string][]*repoFile{}}
	var all []*repoFile
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if strings.HasSuffix(path, ".go") {
				dir := filepath.Dir(path)
				f := &repoFile{path: path, name: d.Name()}
				tree.files[dir] = append(tree.files[dir], f)
				all = append(all, f)
			}
			return nil
		}
		switch d.Name() {
		case "vendor", ".git", "testdata", "node_modules":
			return filepath.SkipDir
		}
		// A nested module (plugins/dolt-snapshots) has its own import paths;
		// resolving its packages against this module's prefix would be wrong.
		if path != root {
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
		}
		tree.dirs = append(tree.dirs, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(tree.dirs)

	work := make(chan *repoFile)
	var wg sync.WaitGroup
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				f.ast, f.err = parser.ParseFile(tree.fset, f.path, nil, parser.ParseComments)
			}
		}()
	}
	for _, f := range all {
		work <- f
	}
	close(work)
	wg.Wait()
	return tree, nil
}

func readModulePath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", os.ErrNotExist
}

// importPath is dir's import path in the tree's module.
func (tr *repoTree) importPath(dir string) string {
	rel, err := filepath.Rel(tr.root, dir)
	if err != nil || rel == "." {
		return tr.module
	}
	return tr.module + "/" + filepath.ToSlash(rel)
}
