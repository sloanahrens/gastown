package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/townstate"
)

// daemonWritePaths maps each call that creates, changes or removes a path to
// the argument positions that name the path.
var daemonWritePaths = map[string][]int{
	"os.WriteFile":         {0},
	"os.Create":            {0},
	"os.OpenFile":          {0},
	"os.Mkdir":             {0},
	"os.MkdirAll":          {0},
	"os.Remove":            {0},
	"os.RemoveAll":         {0},
	"os.Rename":            {0, 1},
	"os.Symlink":           {1},
	"os.Link":              {1},
	"os.Chmod":             {0},
	"os.Chown":             {0},
	"os.Lchown":            {0},
	"os.Chtimes":           {0},
	"os.Truncate":          {0},
	"os.CreateTemp":        {0},
	"os.MkdirTemp":         {0},
	"ioutil.WriteFile":     {0},
	"atomicfile.WriteFile": {0},
}

// runtimeDir is the .runtime directory's name in a resolved path.
const runtimeDir = ".runtime"

// unresolved stands for text the guard cannot read: a struct field, a parameter
// with no caller in view, a closure's argument. It never matches a bird name.
const unresolved = "\x00"

// maxInlineDepth bounds how many calls the guard follows from one function.
const maxInlineDepth = 4

// maxResolveDepth bounds how many local names and helper returns one path follows.
const maxResolveDepth = 12

// formatVerb matches a fmt verb, or the %% escape, in a Sprintf format.
var formatVerb = regexp.MustCompile(`%%|%[a-zA-Z]`)

// TestDaemonNeverWritesIntoABirdDirectory fails when daemon code writes below
// a .runtime/<bird> directory, one the manifest classes session (gt-gg9hv.67).
// In the pod only the daemon-owned set is on the volume, so such a write would
// land in a directory the daemon does not have.
//
// The guard resolves each write's path through literals, filepath.Join and
// path.Join, string concatenation, fmt.Sprintf formats, TownRuntimePath, package
// constants, local assignments, same-package helpers whose single return is the
// path, and calls into same-package writers with the path passed in (up to
// maxInlineDepth). A segment after .runtime that names a bird, or that the guard
// cannot read, is refused.
//
// THE LIMIT, stated: this is a reading of source. Function values, interfaces,
// and paths built from a struct field, a closure argument with no caller in view,
// or data read at run time (a directory listing, a registry file) are not
// followed; those names read as unresolved and are not classed. It is one of
// three layers, not a proof. The second is internal/townstate's
// TestTownStateBirdNamesStayInTheCommandPackages, which does not read write
// calls at all: only the commands that spawn birds may import the packages that
// know a bird's name. The third is `gt town state verify`, whose static checks
// (an uncovered path, two entries with different owners) see the tree itself and
// whose recency finding is information, never a failure.
func TestDaemonNeverWritesIntoABirdDirectory(t *testing.T) {
	t.Parallel()
	birds := townstate.BirdRuntimeNames()

	cases := []struct {
		name    string
		src     string
		refused bool
	}{
		{
			name: "os.Symlink into a bird directory is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeIntoABird(root string) error {
	return os.Symlink("target", filepath.Join(root, ".runtime", "wren", "link")) // WRITE
}
`,
			refused: true,
		},
		{
			name: "os.Link into a bird directory is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeIntoABird(root string) error {
	return os.Link("target", filepath.Join(root, ".runtime", "wren", "link")) // WRITE
}
`,
			refused: true,
		},
		{
			name: "os.Chmod into a bird directory is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeIntoABird(root string) error {
	return os.Chmod(filepath.Join(root, ".runtime", "wren", "state.json"), 0o600) // WRITE
}
`,
			refused: true,
		},
		{
			name: "os.Chtimes into a bird directory is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
	"time"
)

func writeIntoABird(root string) error {
	return os.Chtimes(filepath.Join(root, ".runtime", "wren", "state.json"), time.Time{}, time.Time{}) // WRITE
}
`,
			refused: true,
		},
		{
			name: "os.Truncate into a bird directory is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeIntoABird(root string) error {
	return os.Truncate(filepath.Join(root, ".runtime", "wren", "state.json"), 0) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a literal bird directory is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeIntoABird(root string) error {
	return os.WriteFile(filepath.Join(root, ".runtime", "wren", "state.json"), nil, 0o644) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a bird named through a variable is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeIntoABird(root, name string) error {
	return os.WriteFile(filepath.Join(root, ".runtime", name, "state.json"), nil, 0o644) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a bird named by concatenation is refused",
			src: `package daemon

import "os"

func writeIntoABird(root, name string) error {
	return os.WriteFile(root+"/.runtime/"+name+"/state.json", nil, 0o644) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a bird named by a format string is refused",
			src: `package daemon

import (
	"fmt"
	"os"
)

func writeIntoABird(root, name string) error {
	return os.WriteFile(fmt.Sprintf("%s/.runtime/%s/state.json", root, name), nil, 0o644) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a bird below TownRuntimePath is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/constants"
)

func writeIntoABird(root string) error {
	return os.WriteFile(filepath.Join(constants.TownRuntimePath(root), "wren", "state.json"), nil, 0o644) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a bird directory named by a helper's return is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func birdDir(root, name string) string {
	return filepath.Join(root, ".runtime", name)
}

func writeIntoABird(root string) error {
	return os.WriteFile(filepath.Join(birdDir(root, "wren"), "state.json"), nil, 0o644) // WRITE
}
`,
			refused: true,
		},
		{
			name: "a helper fed a bird directory by its caller is refused",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeTo(dir string) error {
	return os.WriteFile(filepath.Join(dir, "state.json"), nil, 0o644) // WRITE
}

func caller(root string) error {
	return writeTo(filepath.Join(root, ".runtime", "wren"))
}
`,
			refused: true,
		},
		{
			name: "a stdlib WriteFile is not the daemon's own method of that name",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

type files struct{}

func (files) WriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func writeTo(dir string) error {
	return os.WriteFile(filepath.Join(dir, "state.json"), nil, 0o644) // WRITE
}

func caller(root string) error {
	return writeTo(filepath.Join(root, ".runtime", "wren"))
}
`,
			refused: true,
		},
		{
			name: "a write into a daemon subsystem directory is allowed",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func writeUsage(root, name string) error {
	return os.WriteFile(filepath.Join(root, ".runtime", "usage", name), nil, 0o644) // WRITE
}
`,
		},
		{
			name: "a read from a bird directory is allowed",
			src: `package daemon

import (
	"os"
	"path/filepath"
)

func readFromABird(root string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, ".runtime", "wren", "reports", "daily.json")) // WRITE
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sites, err := birdRuntimeWriteSites(map[string][]byte{"control.go": []byte(tc.src)}, birds)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if tc.refused {
				want = []string{fmt.Sprintf("control.go:%d", lineOf(tc.src, "// WRITE"))}
			}
			if !slices.Equal(sites, want) {
				t.Errorf("sites = %v, want %v: the guard must name the file and line of the write", sites, want)
			}
		})
	}

	files := map[string][]byte{}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p) //nolint:gosec // G304: this package's own sources
		if err != nil {
			return err
		}
		files[p] = src
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A scan gone blind passes everything; the package has well over this.
	if len(files) < 100 {
		t.Fatalf("scanned %d daemon source files (floor 100); the walk has stopped seeing them", len(files))
	}
	sites, err := birdRuntimeWriteSites(files, birds)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		t.Errorf("%s writes a path that names a .runtime/<bird> directory or a .runtime child the guard cannot read; write under a daemon subsystem directory instead (gt-gg9hv.67)", site)
	}
}

// lineOf returns the 1-based line of the first line of src holding marker.
func lineOf(src, marker string) int {
	return strings.Count(src[:strings.Index(src, marker)], "\n") + 1
}

// birdRuntimeWriteSites names "<rel>:<line>" for every write call in files whose
// path the guard resolves to a bird's .runtime directory, or to a .runtime child
// it cannot read.
func birdRuntimeWriteSites(files map[string][]byte, birds []string) ([]string, error) {
	ds, err := indexDaemonSources(files, birds)
	if err != nil {
		return nil, err
	}
	return ds.sites(), nil
}

// daemonSource is the parsed package the guard reads: its functions by name,
// its package-level string constants, and which functions write a path.
type daemonSource struct {
	fset    *token.FileSet
	decls   []*ast.FuncDecl
	funcs   map[string][]*ast.FuncDecl
	writers map[*ast.FuncDecl]bool
	consts  map[string]string
	imports map[string]bool
	birds   []string
}

// indexDaemonSources parses files, named by their relative path, into a daemonSource.
func indexDaemonSources(files map[string][]byte, birds []string) (*daemonSource, error) {
	ds := &daemonSource{
		fset:    token.NewFileSet(),
		funcs:   map[string][]*ast.FuncDecl{},
		writers: map[*ast.FuncDecl]bool{},
		consts:  map[string]string{},
		imports: map[string]bool{},
		birds:   birds,
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, rel := range names {
		af, err := parser.ParseFile(ds.fset, rel, files[rel], parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", rel, err)
		}
		for _, imp := range af.Imports {
			ds.imports[importName(imp)] = true
		}
		for _, decl := range af.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				ds.decls = append(ds.decls, d)
				ds.funcs[d.Name.Name] = append(ds.funcs[d.Name.Name], d)
			case *ast.GenDecl:
				if d.Tok != token.CONST && d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, id := range vs.Names {
						if i < len(vs.Values) {
							if s, ok := stringLiteral(vs.Values[i]); ok {
								ds.consts[id.Name] = s
							}
						}
					}
				}
			}
		}
	}
	for _, d := range ds.decls {
		ast.Inspect(d.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if _, ok := writePathArgsOf(call); ok {
					ds.writers[d] = true
				}
			}
			return true
		})
	}
	return ds, nil
}

// sites returns "<rel>:<line>" for each refused write, sorted.
func (ds *daemonSource) sites() []string {
	found := map[string]bool{}
	for _, fn := range ds.decls {
		ds.walk(fn, newScope(fn, nil), 0, found)
	}
	out := make([]string, 0, len(found))
	for site := range found {
		out = append(out, site)
	}
	sort.Strings(out)
	return out
}

// walk checks every write call in fn under sc, then follows the calls that
// reach a same-package writer with the arguments they pass.
func (ds *daemonSource) walk(fn *ast.FuncDecl, sc *scope, depth int, found map[string]bool) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if idxs, ok := writePathArgsOf(call); ok {
			for _, i := range idxs {
				if i < len(call.Args) && ds.refuses(ds.resolve(call.Args[i], sc, 0)) {
					pos := ds.fset.Position(call.Pos())
					found[fmt.Sprintf("%s:%d", pos.Filename, pos.Line)] = true
				}
			}
		}
		if depth < maxInlineDepth {
			for _, callee := range ds.writerCallees(call) {
				ds.walk(callee, ds.bindCall(callee, call, sc), depth+1, found)
			}
		}
		return true
	})
}

// writerCallees returns the same-package writers call may reach, by name, when
// the call passes one argument per parameter. A call through an imported
// package never reaches one: os.WriteFile is not a daemon method that happens
// to share its name.
func (ds *daemonSource) writerCallees(call *ast.CallExpr) []*ast.FuncDecl {
	if ds.isPackageCall(call) {
		return nil
	}
	var out []*ast.FuncDecl
	for _, d := range ds.funcs[calleeName(call)] {
		if ds.writers[d] && len(paramNames(d)) == len(call.Args) {
			out = append(out, d)
		}
	}
	return out
}

// bindCall returns the scope of callee when call passes its arguments, read in sc.
func (ds *daemonSource) bindCall(callee *ast.FuncDecl, call *ast.CallExpr, sc *scope) *scope {
	bind := map[string]string{}
	for i, name := range paramNames(callee) {
		bind[name] = ds.resolve(call.Args[i], sc, 0)
	}
	return newScope(callee, bind)
}

// refuses reports whether a resolved path names a bird's .runtime directory, or
// a .runtime child the guard cannot read.
func (ds *daemonSource) refuses(p string) bool {
	segs := strings.Split(p, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] != runtimeDir && segs[i] != unresolved+runtimeDir {
			continue
		}
		next := segs[i+1]
		if strings.Contains(next, unresolved) || slices.Contains(ds.birds, next) {
			return true
		}
	}
	return false
}

// resolve returns the text of expression e in sc, with unresolved wherever the
// guard cannot read it.
func (ds *daemonSource) resolve(e ast.Expr, sc *scope, depth int) string {
	if depth > maxResolveDepth {
		return unresolved
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if s, ok := stringLiteral(x); ok {
			return s
		}
	case *ast.ParenExpr:
		return ds.resolve(x.X, sc, depth+1)
	case *ast.Ident:
		return ds.resolveName(x.Name, sc, depth+1)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return ds.resolve(x.X, sc, depth+1) + ds.resolve(x.Y, sc, depth+1)
		}
	case *ast.CallExpr:
		return ds.resolveCall(x, sc, depth+1)
	}
	return unresolved
}

// resolveName returns what name holds in sc: a bound argument, a local
// assignment made once, or a package constant.
func (ds *daemonSource) resolveName(name string, sc *scope, depth int) string {
	if v, ok := sc.bind[name]; ok {
		return v
	}
	if sc.params[name] {
		return unresolved
	}
	if rhs, ok := sc.assigns[name]; ok {
		if rhs == nil {
			return unresolved
		}
		return ds.resolve(rhs, sc, depth)
	}
	if v, ok := ds.consts[name]; ok {
		return v
	}
	return unresolved
}

// resolveCall returns the path a call builds: a join, TownRuntimePath, a format
// string, or the single return of a same-package helper.
func (ds *daemonSource) resolveCall(call *ast.CallExpr, sc *scope, depth int) string {
	if isJoin(call) {
		parts := make([]string, 0, len(call.Args))
		for _, a := range call.Args {
			parts = append(parts, ds.resolve(a, sc, depth))
		}
		return strings.Join(parts, "/")
	}
	name := calleeName(call)
	if name == "TownRuntimePath" {
		return runtimeDir
	}
	if isSprintf(call) && len(call.Args) > 0 {
		format, ok := stringLiteral(call.Args[0])
		if !ok {
			return unresolved
		}
		return formatVerb.ReplaceAllStringFunc(format, func(verb string) string {
			if verb == "%%" {
				return "%"
			}
			return unresolved
		})
	}
	if ds.isPackageCall(call) {
		return unresolved
	}
	var helpers []*ast.FuncDecl
	for _, d := range ds.funcs[name] {
		if len(paramNames(d)) == len(call.Args) {
			helpers = append(helpers, d)
		}
	}
	if len(helpers) != 1 {
		return unresolved
	}
	ret := singleReturn(helpers[0])
	if ret == nil {
		return unresolved
	}
	bind := map[string]string{}
	for i, pname := range paramNames(helpers[0]) {
		bind[pname] = ds.resolve(call.Args[i], sc, depth)
	}
	return ds.resolve(ret, newScope(helpers[0], bind), depth)
}

// scope is what a name means inside one function: its bound arguments, its
// parameters, and its local assignments. A name assigned more than once maps to
// nil, which resolves to unresolved.
type scope struct {
	bind    map[string]string
	params  map[string]bool
	assigns map[string]ast.Expr
}

// newScope reads fn's parameters and local assignments, with bind as the values
// its parameters hold.
func newScope(fn *ast.FuncDecl, bind map[string]string) *scope {
	sc := &scope{bind: bind, params: map[string]bool{}, assigns: map[string]ast.Expr{}}
	if sc.bind == nil {
		sc.bind = map[string]string{}
	}
	for _, p := range paramNames(fn) {
		sc.params[p] = true
	}
	rhss := map[string][]ast.Expr{}
	add := func(id *ast.Ident, rhs ast.Expr) {
		rhss[id.Name] = append(rhss[id.Name], rhs)
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if len(s.Lhs) == len(s.Rhs) {
					add(id, s.Rhs[i])
				} else {
					add(id, nil)
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if i < len(s.Values) {
					add(id, s.Values[i])
				} else {
					add(id, nil)
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{s.Key, s.Value} {
				if id, ok := e.(*ast.Ident); ok {
					add(id, nil)
				}
			}
		}
		return true
	})
	for name, list := range rhss {
		if len(list) == 1 {
			sc.assigns[name] = list[0]
		} else {
			sc.assigns[name] = nil
		}
	}
	return sc
}

// paramNames returns fn's parameter names in order; an unnamed parameter is "_".
func paramNames(fn *ast.FuncDecl) []string {
	var names []string
	if fn.Type.Params == nil {
		return names
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			names = append(names, "_")
			continue
		}
		for _, id := range field.Names {
			names = append(names, id.Name)
		}
	}
	return names
}

// singleReturn returns the one result expression of fn's only return statement,
// or nil when fn has no single return of one value.
func singleReturn(fn *ast.FuncDecl) ast.Expr {
	var rets []*ast.ReturnStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if r, ok := n.(*ast.ReturnStmt); ok {
			rets = append(rets, r)
		}
		return true
	})
	if len(rets) != 1 || len(rets[0].Results) != 1 {
		return nil
	}
	return rets[0].Results[0]
}

// writePathArgsOf returns the path argument positions of a write call, and
// whether call is one.
func writePathArgsOf(call *ast.CallExpr) ([]int, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	idxs, ok := daemonWritePaths[pkg.Name+"."+sel.Sel.Name]
	return idxs, ok
}

// calleeName is the name a call refers to: the function, or the method's selector.
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// isPackageCall reports whether call is a selector on an imported package, such
// as os.WriteFile, rather than a method on a value.
func (ds *daemonSource) isPackageCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && ds.imports[pkg.Name]
}

// importName is the name an import spec binds: its alias, or the last element
// of its path.
func importName(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	p := strings.Trim(imp.Path.Value, "\"`")
	return p[strings.LastIndex(p, "/")+1:]
}

// isJoin reports whether call is filepath.Join or path.Join.
func isJoin(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && (pkg.Name == "filepath" || pkg.Name == "path")
}

// isSprintf reports whether call is fmt.Sprintf.
func isSprintf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt"
}

// stringLiteral returns the value of a string literal expression.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
