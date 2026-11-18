package testpolicy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RuleEnvPrint is a test failure message that prints the environment.
const RuleEnvPrint = "env-print"

const envPrintMarker = "testpolicy:env-print-ok"

// failureMethods are the testing calls whose arguments reach the test's output,
// with and without a format string. A credential placeholder in that output
// ends the seat running the test, so every one of them is judged.
var failureMethods = map[string]bool{
	"Fatal": true, "Fatalf": true,
	"Error": true, "Errorf": true,
	"Log": true, "Logf": true,
	"Skip": true, "Skipf": true,
}

// valueKind is what a syntax-only scan can know about a value: whether it
// carries an environment, a single value that cannot, or nothing at all.
type valueKind int

const (
	kindUnknown valueKind = iota
	// kindScalar is a string, number, bool or other single value.
	kindScalar
	// kindStrings is a []string or map of strings whose entries the scan
	// cannot know.
	kindStrings
	// kindSafeStrings is a []string or map of strings built only from literals
	// in the test, so printing it cannot carry a credential from the host.
	kindSafeStrings
	// kindEnv is a value that carries environment entries: a slice or map of
	// them, or one entry read out of one. Printing any of these can put a
	// placeholder into the test's output.
	kindEnv
)

// envFile is what a parsed test file and its package say about values. go/parser
// resolves identifiers within a file, so kinds are keyed by the declaration
// object and a shadowed name keeps its own kind; a name the file does not
// declare is looked up in the package the scanner read beside it.
type envFile struct {
	kinds   map[*ast.Object]valueKind
	results map[string][]valueKind
	imports map[string]bool
	pkg     *pkgInfo
}

// pkgInfo is what the other files of one package say about their package-level
// names, so an environment-name constant in another file is not mistaken for an
// environment, and a function's result type is known.
type pkgInfo struct {
	consts map[string]bool
	funcs  map[string][]valueKind
	vars   map[string]valueKind
}

// pkgInfoFor parses the package's other files once, keyed by directory, and
// keeps only the files of the same package: an external foo_test package cannot
// see the names of foo. A file it cannot read is an error, never a package with
// fewer names to judge.
func pkgInfoFor(cache map[string]*pkgInfo, fset *token.FileSet, dir string, paths []string, f *ast.File) (*pkgInfo, error) {
	key := dir + "\x00" + f.Name.Name
	if info, ok := cache[key]; ok {
		return info, nil
	}
	info := &pkgInfo{consts: map[string]bool{}, funcs: map[string][]valueKind{}, vars: map[string]valueKind{}}
	cache[key] = info
	for _, path := range paths {
		sibling, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		if sibling.Name.Name != f.Name.Name {
			continue
		}
		for _, d := range sibling.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				switch d.Tok {
				case token.CONST:
					for _, spec := range d.Specs {
						for _, name := range spec.(*ast.ValueSpec).Names {
							info.consts[name.Name] = true
						}
					}
				case token.VAR:
					for _, spec := range d.Specs {
						vs := spec.(*ast.ValueSpec)
						k := typeKind(vs.Type)
						if k == kindUnknown && len(vs.Values) == 1 {
							if lit, ok := vs.Values[0].(*ast.CompositeLit); ok {
								k = compositeKind(lit)
							}
						}
						if k == kindUnknown {
							continue
						}
						for _, name := range vs.Names {
							info.vars[name.Name] = k
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil || d.Type.Results == nil {
					continue
				}
				info.funcs[d.Name.Name] = resultKinds(d.Type.Results)
			}
		}
	}
	return info, nil
}

// resultKinds returns one kind per result, in order.
func resultKinds(results *ast.FieldList) []valueKind {
	var rs []valueKind
	for _, field := range results.List {
		k := typeKind(field.Type)
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			rs = append(rs, k)
		}
	}
	return rs
}

// CheckEnvPrints walks root recursively and reports every Fatal, Fatalf,
// Error, Errorf, Log, Logf, Skip or Skipf call whose arguments print the
// environment. An argument is judged as a whole expression: an identifier named
// for an environment or known to hold one, a call to os.Environ, a selector
// ending in .Env or .Environ, an index or slice of one of those, a function
// that returns a []string and is named for an environment (childEnv), or such a
// value nested inside a formatting call such as fmt.Sprintf or strings.Join.
//
// A count or length wrapped around the value, such as len(env), prints no
// entry and passes. So does a value built only from literals in the test, and a
// name a constant or a literal initializer of the package resolves: neither can
// carry a credential from the host.
//
// A "// testpolicy:env-print-ok <reason>" comment exempts exactly one call: the
// call it trails on its own line, or the first call starting on the line below
// a marker that stands alone on its line. A second call is judged on its own,
// so a bare environment print below a marked call is still reported. A reason is
// required; it names the value the message prints in place of the environment,
// so a reviewer can check the claim against the code.
func CheckEnvPrints(root string) ([]Violation, error) {
	fset := token.NewFileSet()
	// The walk names every file once, so the package of a test file is read
	// from the same listing rather than from a second pass over the directory.
	byDir := map[string][]string{}
	var tests []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		dir := filepath.Dir(path)
		byDir[dir] = append(byDir[dir], path)
		if strings.HasSuffix(path, "_test.go") {
			tests = append(tests, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pkgCache := map[string]*pkgInfo{}
	var vs []Violation
	for _, path := range tests {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		dir := filepath.Dir(path)
		calls := failureCalls(f)
		exempt, unreasoned := envPrintExemptions(fset, f, src, calls)
		for _, line := range unreasoned {
			vs = append(vs, Violation{
				Pos:  token.Position{Filename: path, Line: line},
				Rule: RuleEnvPrint,
				Msg:  "exemption has no reason; name the value the message prints in place of the environment",
			})
		}
		pkg, err := pkgInfoFor(pkgCache, fset, dir, byDir[dir], f)
		if err != nil {
			return nil, err
		}
		ef := newEnvFile(f, pkg)
		for _, c := range calls {
			if exempt[c.call] {
				continue
			}
			pos := fset.Position(c.call.Pos())
			for i, arg := range c.call.Args {
				if !ef.printsEnv(arg) {
					continue
				}
				vs = append(vs, Violation{
					Pos:  pos,
					Rule: RuleEnvPrint,
					Msg:  fmt.Sprintf("%s prints the environment in argument %d; name the key or a count instead, or declare the value safe with %q", c.method, i+1, "// "+envPrintMarker+" <reason>"),
				})
				break
			}
		}
	}
	return vs, nil
}

// failureCall is one testing failure call and the method that makes it one.
type failureCall struct {
	call   *ast.CallExpr
	method string
}

// failureCalls returns the testing failure calls of one file in source order.
func failureCalls(f *ast.File) []failureCall {
	var calls []failureCall
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok && failureMethods[sel.Sel.Name] {
			calls = append(calls, failureCall{call: c, method: sel.Sel.Name})
		}
		return true
	})
	return calls
}

// newEnvFile records the kinds of one file's parameters, named results,
// functions and local variables, so a printed identifier is judged by what it
// holds rather than by its name alone.
func newEnvFile(f *ast.File, pkg *pkgInfo) *envFile {
	ef := &envFile{kinds: map[*ast.Object]valueKind{}, results: map[string][]valueKind{}, imports: map[string]bool{}, pkg: pkg}
	for _, im := range f.Imports {
		name := ""
		if im.Name != nil {
			name = im.Name.Name
		} else {
			path := strings.Trim(im.Path.Value, `"`)
			name = path[strings.LastIndex(path, "/")+1:]
		}
		ef.imports[name] = true
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Type.Results != nil {
			ef.results[fd.Name.Name] = resultKinds(fd.Type.Results)
		}
		if fd.Type.Params != nil {
			for _, field := range fd.Type.Params.List {
				k := typeKind(field.Type)
				for _, name := range field.Names {
					if name.Obj != nil {
						ef.kinds[name.Obj] = k
					}
				}
			}
		}
	}
	// Walk in source order: a short declaration and a range variable can only
	// read names declared before them.
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GenDecl:
			if n.Tok != token.CONST {
				return true
			}
			for _, spec := range n.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if name.Obj != nil {
						ef.kinds[name.Obj] = kindScalar
					}
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				k := typeKind(n.Type)
				if k == kindUnknown && i < len(n.Values) {
					k = ef.exprKind(n.Values[i])
				}
				if name.Obj != nil && k != kindUnknown {
					ef.kinds[name.Obj] = k
				}
			}
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				// A multi-value call: results are positional.
				if len(n.Rhs) == 1 {
					if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
						ef.assignResults(n.Lhs, call)
					}
				}
				return true
			}
			for i, lhs := range n.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Obj == nil {
					continue
				}
				if k := ef.exprKind(n.Rhs[i]); k != kindUnknown {
					ef.kinds[id.Obj] = k
				}
			}
		case *ast.RangeStmt:
			value := kindUnknown
			switch ef.exprKind(n.X) {
			case kindEnv:
				value = kindEnv
			case kindSafeStrings:
				value = kindScalar
			}
			// The key is a number or a map key, never the slice itself.
			if id, ok := n.Key.(*ast.Ident); ok && id.Obj != nil {
				ef.kinds[id.Obj] = kindScalar
			}
			if id, ok := n.Value.(*ast.Ident); ok && id.Obj != nil {
				ef.kinds[id.Obj] = value
			}
		}
		return true
	})
	return ef
}

// assignResults records the kind of each value a multi-result call returns.
func (ef *envFile) assignResults(lhs []ast.Expr, call *ast.CallExpr) {
	var rs []valueKind
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		rs = ef.results[fun.Name]
	}
	if len(rs) != len(lhs) {
		return
	}
	for i, l := range lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Obj == nil || rs[i] == kindUnknown {
			continue
		}
		ef.kinds[id.Obj] = rs[i]
	}
}

// exprKind classifies an expression from syntax alone. kindUnknown means the
// scan cannot tell; callers then fall back to the name.
func (ef *envFile) exprKind(e ast.Expr) valueKind {
	switch e := e.(type) {
	case *ast.Ident:
		if e.Obj != nil {
			return ef.kinds[e.Obj]
		}
		if ef.pkg.consts[e.Name] {
			return kindScalar
		}
		if k, ok := ef.pkg.vars[e.Name]; ok {
			return k
		}
		if looksEnvName(e.Name) {
			return kindEnv
		}
		return kindUnknown
	case *ast.ParenExpr:
		return ef.exprKind(e.X)
	case *ast.StarExpr:
		return ef.exprKind(e.X)
	case *ast.UnaryExpr:
		return ef.exprKind(e.X)
	case *ast.BinaryExpr:
		// A comparison is a bool, not the values it compares; only a
		// concatenation carries the text of its operands into the message.
		if e.Op != token.ADD {
			return kindScalar
		}
		if k := ef.exprKind(e.X); k == kindEnv {
			return kindEnv
		}
		return ef.exprKind(e.Y)
	case *ast.SelectorExpr:
		if isEnvField(e.Sel.Name) {
			return kindEnv
		}
		return kindUnknown
	case *ast.IndexExpr:
		switch ef.exprKind(e.X) {
		case kindEnv:
			return kindEnv
		case kindSafeStrings:
			return kindScalar
		}
		return kindUnknown
	case *ast.SliceExpr:
		return ef.exprKind(e.X)
	case *ast.CompositeLit:
		return ef.litKind(e)
	case *ast.CallExpr:
		return ef.callKind(e)
	}
	return kindUnknown
}

// litKind classifies a composite literal. A []string or map of strings written
// in the test source cannot carry a host value unless an element is read from
// the environment, so such an element makes the literal an environment value.
func (ef *envFile) litKind(lit *ast.CompositeLit) valueKind {
	k := compositeKind(lit)
	if k != kindSafeStrings {
		return k
	}
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			elt = kv.Value
		}
		if ef.printsEnv(elt) {
			return kindEnv
		}
	}
	return k
}

// compositeKind classifies a literal by its type alone.
func compositeKind(lit *ast.CompositeLit) valueKind {
	switch t := lit.Type.(type) {
	case *ast.ArrayType:
		if isStringType(t.Elt) {
			return kindSafeStrings
		}
	case *ast.MapType:
		if isStringType(t.Key) && isStringType(t.Value) {
			return kindSafeStrings
		}
	}
	return kindUnknown
}

// callKind classifies a call's result. A function declared in the file or its
// package is judged by its result type; one that returns a []string and is
// named for an environment (childEnv) returns an environment.
func (ef *envFile) callKind(c *ast.CallExpr) valueKind {
	switch fun := c.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "append" && len(c.Args) > 0 {
			k := ef.exprKind(c.Args[0])
			for _, arg := range c.Args[1:] {
				switch ef.exprKind(arg) {
				case kindEnv:
					return kindEnv
				case kindUnknown, kindStrings:
					// A runtime value appended to a slice of test literals
					// makes the result runtime data.
					if k == kindSafeStrings {
						k = kindStrings
					}
				}
			}
			return k
		}
		rs, ok := ef.results[fun.Name]
		if !ok {
			rs, ok = ef.pkg.funcs[fun.Name]
		}
		if ok && len(rs) == 1 {
			// A function that returns a []string and is named for an
			// environment (childEnv) builds one.
			if rs[0] == kindStrings && looksEnvName(fun.Name) {
				return kindEnv
			}
			return rs[0]
		}
	case *ast.SelectorExpr:
		qualifier, isPkg := fun.X.(*ast.Ident)
		if !isPkg || qualifier.Obj != nil || !ef.imports[qualifier.Name] {
			// A method call. Its name says nothing about the result: Env
			// writes an environment file, PushWithEnv pushes a branch.
			return kindUnknown
		}
		switch qualifier.Name + "." + fun.Sel.Name {
		case "os.Environ":
			return kindEnv
		case "strings.Join", "strings.Repeat", "strings.Replace", "strings.ReplaceAll":
			return kindScalar
		case "fmt.Sprintf", "fmt.Sprint", "fmt.Sprintln", "fmt.Errorf":
			return kindScalar
		}
		switch fun.Sel.Name {
		case "Clone", "Concat":
			if len(c.Args) > 0 {
				return ef.exprKind(c.Args[0])
			}
		}
	}
	return kindUnknown
}

// printsEnv reports whether one argument of a failure call prints the
// environment.
func (ef *envFile) printsEnv(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		switch ef.exprKind(e) {
		case kindScalar, kindSafeStrings:
			return false
		case kindEnv:
			return true
		}
		return looksEnvName(e.Name)
	case *ast.SelectorExpr:
		if isEnvField(e.Sel.Name) {
			return true
		}
		return false
	case *ast.IndexExpr:
		return ef.printsEnv(e.X)
	case *ast.SliceExpr:
		return ef.printsEnv(e.X)
	case *ast.StarExpr:
		return ef.printsEnv(e.X)
	case *ast.ParenExpr:
		return ef.printsEnv(e.X)
	case *ast.UnaryExpr:
		return ef.printsEnv(e.X)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (ef.printsEnv(e.X) || ef.printsEnv(e.Y))
	case *ast.CallExpr:
		if isCountCall(e) {
			return false
		}
		if ef.isOSEnviron(e) {
			return true
		}
		if formatsItsArgs(e) {
			for _, arg := range e.Args {
				if ef.printsEnv(arg) {
					return true
				}
			}
			return false
		}
		return ef.callKind(e) == kindEnv
	}
	return false
}

// isOSEnviron reports whether a call is os.Environ, whose result is the
// process environment.
func (ef *envFile) isOSEnviron(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Environ" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	// x.Obj != nil means a local value shadows the package name.
	return ok && x.Obj == nil && x.Name == "os"
}

// isEnvField reports a field named Env or Environ, the shape exec.Cmd and
// configuration structs give an environment.
func isEnvField(name string) bool { return name == "Env" || name == "Environ" }

// looksEnvName reports a name that denotes an environment by convention: env,
// environ, or a name ending in Env or Environ such as cmdEnv or dockerEnv.
func looksEnvName(name string) bool {
	if name == "env" || name == "environ" {
		return true
	}
	return strings.HasSuffix(name, "Env") || strings.HasSuffix(name, "Environ")
}

// isCountCall reports len and cap, which print a number and never the value.
func isCountCall(c *ast.CallExpr) bool {
	id, ok := c.Fun.(*ast.Ident)
	return ok && (id.Name == "len" || id.Name == "cap")
}

// formatsItsArgs reports a call that builds its result from the text of its
// arguments, so an environment passed to it is printed.
func formatsItsArgs(c *ast.CallExpr) bool {
	fun, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := fun.X.(*ast.Ident)
	if !ok || pkg.Obj != nil {
		return false
	}
	switch pkg.Name + "." + fun.Sel.Name {
	case "fmt.Sprintf", "fmt.Sprint", "fmt.Sprintln", "fmt.Errorf",
		"strings.Join", "strings.Replace", "strings.ReplaceAll":
		return true
	}
	return false
}

// typeKind classifies a declared type.
func typeKind(e ast.Expr) valueKind {
	switch t := e.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string", "bool", "byte", "rune", "error", "any",
			"int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"float32", "float64", "complex64", "complex128":
			return kindScalar
		}
	case *ast.ArrayType:
		if isStringType(t.Elt) {
			return kindStrings
		}
	case *ast.MapType:
		if isStringType(t.Key) && isStringType(t.Value) {
			return kindStrings
		}
	}
	return kindUnknown
}

// isStringType reports the builtin string type. A defined type is left
// unknown: the scan does not follow type declarations.
func isStringType(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "string"
}

// envPrintExemptions returns the one call each env-print-ok comment exempts and
// the marker lines that carry no reason. A marker trailing a call exempts the
// call it trails on its own line; one standing alone on its line exempts only
// the first call that starts on the line below. No marker exempts a second
// call, so an unmarked print beside a marked one is reported on its own.
func envPrintExemptions(fset *token.FileSet, f *ast.File, src []byte, calls []failureCall) (map[*ast.CallExpr]bool, []int) {
	exempt := map[*ast.CallExpr]bool{}
	var unreasoned []int
	lines := strings.Split(string(src), "\n")
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			rest, ok := strings.CutPrefix(text, envPrintMarker)
			if !ok {
				continue
			}
			cpos := fset.Position(c.Pos())
			if strings.TrimSpace(rest) == "" {
				unreasoned = append(unreasoned, cpos.Line)
			}
			var cand *ast.CallExpr
			if commentStandsAlone(lines, cpos) {
				// The call the marker introduces: the first one starting on
				// the line below, and only that one.
				for _, call := range calls {
					if fset.Position(call.call.Pos()).Line == cpos.Line+1 && call.call.Pos() >= c.End() {
						cand = call.call
						break
					}
				}
			} else {
				// The call the marker trails: the nearest one ending before it
				// on the same line. A call starting on an earlier line is not
				// exempt, per the same-line rule.
				for _, call := range calls {
					if fset.Position(call.call.Pos()).Line != cpos.Line || call.call.End() > c.Pos() {
						continue
					}
					if cand == nil || call.call.End() > cand.End() {
						cand = call.call
					}
				}
			}
			if cand != nil {
				exempt[cand] = true
			}
		}
	}
	return exempt, unreasoned
}

// commentStandsAlone reports whether nothing but whitespace precedes the
// comment on its own line.
func commentStandsAlone(lines []string, pos token.Position) bool {
	i := pos.Line - 1
	if i < 0 || i >= len(lines) || pos.Column-1 > len(lines[i]) {
		return false
	}
	return strings.TrimSpace(lines[i][:pos.Column-1]) == ""
}
