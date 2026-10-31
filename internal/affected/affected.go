// Package affected selects the Go packages whose tests a diff can affect, so a
// gate can test those and leave the rest of the suite to a slower cadence
// (gt-c3lp9). It fails wide: a change to a build input, a file no package can
// place, or a diff it cannot read selects every package.
package affected

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Runner runs one command in the repository root and returns its standard
// output; argv holds the program name first. An error means the command failed,
// and it must not carry the command's output, which can hold a secret. Tests
// answer it from canned output, so the unit tier starts no process.
type Runner func(argv []string) ([]byte, error)

// Selection is what a diff selects: All means the caller runs every test, All
// false with no packages means the diff is empty and no test has to run, and
// otherwise Packages holds the sorted import paths to test.
type Selection struct {
	Packages []string
	All      bool
}

// Select returns the packages whose tests a diff from base to head can affect:
// every changed package, and every package whose test imports one, directly or
// through other packages. The Selection is All for the inputs that must run
// everything, with an error naming the input it could not read or place
// (gt-c3lp9).
//
// wd is the directory git and go list run in. The go list pattern is relative
// to it, so a wd below the module root would list a subtree: the closure would
// lose every importer outside it and select too little. Select therefore reads
// the module root go list reports and returns All unless wd is that root.
func Select(run Runner, wd, base, head string) (Selection, error) {
	revs := base + "..." + head
	out, err := run([]string{"git", "diff", "--name-only", "--no-renames", "-z", "--end-of-options", revs})
	if err != nil {
		return Selection{All: true}, fmt.Errorf("git diff %s: %w", revs, err)
	}
	files, err := splitPaths(out)
	if err != nil {
		return Selection{All: true}, fmt.Errorf("git diff %s: %w", revs, err)
	}
	for _, file := range files {
		if widensToAll(file) {
			return Selection{All: true}, nil
		}
	}
	if len(files) == 0 {
		// An empty diff selects no package; the caller prints NONE.
		return Selection{}, nil
	}
	graph, err := readGraph(run)
	if err != nil {
		return Selection{All: true}, err
	}
	if !sameDir(wd, graph.root) {
		return Selection{All: true}, fmt.Errorf("go list ran in %q, not the module root %q: run affected at the repository root", wd, graph.root)
	}
	return graph.selectFiles(files)
}

// splitPaths reads `git diff --name-only -z` output, which is a NUL-terminated
// path per record. Output that holds no terminator, or an empty path, is
// unreadable: guessing at it would select less than the diff changes.
func splitPaths(out []byte) ([]string, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if out[len(out)-1] != 0 {
		return nil, errors.New("output is not NUL-terminated")
	}
	fields := strings.Split(string(out[:len(out)-1]), "\x00")
	for _, field := range fields {
		if field == "" {
			return nil, errors.New("output holds an empty path")
		}
	}
	return fields, nil
}

// widensToAll reports whether a changed path forces every package: a build
// input the whole module reads, or a file of the build that any test may load
// as data. testdata is read by tests of any package, so it selects All rather
// than the package that happens to hold it.
func widensToAll(file string) bool {
	switch path.Base(file) {
	case "go.mod", "go.sum", "Makefile":
		return true
	}
	for _, elem := range strings.Split(file, "/") {
		if elem == ".forgejo" || elem == "testdata" {
			return true
		}
	}
	return false
}

// pkgGraph is what one `go list -deps -test -json ./...` run reports: the
// import path each repo directory holds, the edges of the import graph, and the
// main module path a package the graph does not know yet is named from.
type pkgGraph struct {
	module string
	root   string
	byDir  map[string]string
	deps   map[string][]string
}

// listPackage is the part of one `go list -json` record the selector reads.
type listPackage struct {
	ImportPath string
	Dir        string
	Deps       []string
	ForTest    string
	Module     *listModule
	Error      *listError
}

type listModule struct {
	Path string
	Dir  string
	Main bool
}

type listError struct {
	Err string
}

// readGraph runs go list for the import graph of every package in the module,
// with the test variants -test adds, so a test-only import is an edge.
func readGraph(run Runner) (*pkgGraph, error) {
	out, err := run([]string{"go", "list", "-deps", "-test", "-json", "./..."})
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	return parseGraph(out)
}

// parseGraph reads the stream of records `go list -json` writes. A record that
// reports an error, and a stream that names no main module, are errors: the
// graph is incomplete, and a closure over part of it would select too little.
func parseGraph(out []byte) (*pkgGraph, error) {
	var records []listPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var rec listPackage
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("go list output: %w", err)
		}
		if rec.Error != nil {
			return nil, fmt.Errorf("%s: %s", rec.ImportPath, rec.Error.Err)
		}
		records = append(records, rec)
	}
	graph := &pkgGraph{byDir: map[string]string{}, deps: map[string][]string{}}
	forTest := map[string]string{}
	var moduleDir string
	for _, rec := range records {
		if graph.module == "" && rec.Module != nil && rec.Module.Main && rec.Module.Dir != "" {
			graph.module, moduleDir = rec.Module.Path, rec.Module.Dir
		}
		if rec.ForTest != "" {
			forTest[rec.ImportPath] = rec.ForTest
		}
	}
	if graph.module == "" {
		return nil, errors.New("go list named no main module")
	}
	graph.root = moduleDir
	for _, rec := range records {
		if isTestVariant(rec.ImportPath) {
			continue
		}
		if rel, ok := repoDir(moduleDir, rec.Dir); ok {
			graph.byDir[rel] = rec.ImportPath
		}
	}
	for _, rec := range records {
		src := baseImportPath(rec.ImportPath, forTest)
		for _, dep := range rec.Deps {
			graph.deps[src] = append(graph.deps[src], baseImportPath(dep, forTest))
		}
	}
	return graph, nil
}

// repoDir is dir as a slash-separated path from moduleDir; it reports false
// for a directory outside the module, such as a dependency in the module cache,
// which no path in the diff can name.
func repoDir(moduleDir, dir string) (string, bool) {
	if dir == moduleDir {
		return ".", true
	}
	prefix := moduleDir + string(filepath.Separator)
	if !strings.HasPrefix(dir, prefix) {
		return "", false
	}
	return filepath.ToSlash(strings.TrimPrefix(dir, prefix)), true
}

// sameDir reports whether two paths name one directory. It compares the cleaned
// paths first, then the filesystem's view, so a checkout reached through a
// symlink (macOS /tmp, a linked worktree) still matches. A path it cannot
// resolve is not the same directory: the caller then fails wide.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

// isTestVariant reports whether go list named a test variant of a package
// rather than the package: a variant's id carries the parent in brackets, and a
// test binary's ends in ".test". Neither has a directory of its own, so both
// would otherwise overwrite the package's entry.
func isTestVariant(importPath string) bool {
	return strings.ContainsRune(importPath, ' ') || strings.HasSuffix(importPath, ".test")
}

// baseImportPath names the package a go list id belongs to, so a test import is
// an edge from the package whose tests make it. forTest maps a variant's id to
// its parent, which the id alone cannot say: the external test package of p is
// named p_test, and that is no importable path, so it belongs to p. A test
// binary's id ("p.test") carries no ForTest and is p as well.
func baseImportPath(id string, forTest map[string]string) string {
	if parent, ok := forTest[id]; ok {
		return parent
	}
	if i := strings.IndexRune(id, ' '); i >= 0 {
		return id[:i]
	}
	return strings.TrimSuffix(id, ".test")
}

// selectFiles maps the files to the packages that own them and takes the
// reverse closure over the import graph.
func (g *pkgGraph) selectFiles(files []string) (Selection, error) {
	pkgs, unplaced := packagesOfFiles(files, g.byDir, g.module)
	if len(unplaced) > 0 {
		return Selection{All: true}, fmt.Errorf("no package owns %s", unplaced[0])
	}
	return Selection{Packages: reverseClosure(pkgs, g.deps)}, nil
}

// packagesOfFiles names the import path of the package that owns each file, and
// reports the files no package can place.
func packagesOfFiles(files []string, byDir map[string]string, module string) ([]string, []string) {
	var pkgs, unplaced []string
	seen := map[string]bool{}
	for _, file := range files {
		importPath, ok := packageOfFile(file, byDir, module)
		if !ok {
			unplaced = append(unplaced, file)
			continue
		}
		if seen[importPath] {
			continue
		}
		seen[importPath] = true
		pkgs = append(pkgs, importPath)
	}
	sort.Strings(pkgs)
	return pkgs, unplaced
}

// packageOfFile names the import path of the package that owns one file. A .go
// file belongs to its directory's package, which the graph may not know yet: a
// package the diff adds is named from the module path, so it is selected rather
// than dropped. A file of any other kind belongs to the nearest ancestor
// directory the graph knows, because that is the package whose build or tests
// read it.
func packageOfFile(file string, byDir map[string]string, module string) (string, bool) {
	dir := path.Dir(file)
	if strings.HasSuffix(file, ".go") {
		if importPath, ok := byDir[dir]; ok {
			return importPath, true
		}
		if skippedDir(dir) {
			return "", false
		}
		return joinImportPath(module, dir), true
	}
	for {
		if importPath, ok := byDir[dir]; ok {
			return importPath, true
		}
		if dir == "." || skippedDir(dir) {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// skippedDir reports whether the go tool ignores dir: it has an element that
// begins with "." or "_", and the ./... pattern never matches one, so no
// package builds a file there.
func skippedDir(dir string) bool {
	for _, elem := range strings.Split(dir, "/") {
		if elem == "." {
			continue
		}
		if strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") {
			return true
		}
	}
	return false
}

// joinImportPath is the import path of the package in dir, a path from the
// module root.
func joinImportPath(module, dir string) string {
	if dir == "." {
		return module
	}
	return module + "/" + dir
}

// reverseClosure returns start and every package that imports one of them,
// directly or through other packages, sorted. deps maps a package to the
// packages it imports.
func reverseClosure(start []string, deps map[string][]string) []string {
	dependents := map[string][]string{}
	for pkg, imports := range deps {
		for _, dep := range imports {
			dependents[dep] = append(dependents[dep], pkg)
		}
	}
	seen := map[string]bool{}
	var queue []string
	for _, pkg := range start {
		if !seen[pkg] {
			seen[pkg] = true
			queue = append(queue, pkg)
		}
	}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		for _, dependent := range dependents[pkg] {
			if seen[dependent] {
				continue
			}
			seen[dependent] = true
			queue = append(queue, dependent)
		}
	}
	out := make([]string, 0, len(seen))
	for pkg := range seen {
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}
