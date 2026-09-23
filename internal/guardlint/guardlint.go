// Package guardlint is the checker half of the guard.Result migration
// (gt-udrrw): a heuristic scan for the shape every cataloged instance shares
// — a function whose last two results are (bool, error) that, from inside a
// branch already holding a non-nil error, returns a literal `true` for the
// bool or a literal `nil` for the error. Either one throws away the
// information guard.Result exists to keep: that the check did not run,
// rather than that it ran and passed.
//
// The scan is syntactic, not type-checked — it matches "the last two result
// types read as bool and error" and "the condition reads as an err-like
// identifier compared to nil", not verified against go/types. That keeps it
// fast enough to run in make lint and correct enough for its job: everything
// it flags is a function shaped exactly like the reported instances, for a
// caller to look at and either migrate or add to the baseline.
package guardlint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one guard function returning ok/true or a swallowed nil from
// inside a branch that already observed a non-nil error.
type Finding struct {
	File string // relative to the scanned root
	Line int
	Func string
	Slot string // "bool" or "error" — which result the offending literal sits in
}

// Key identifies a Finding stably across unrelated edits elsewhere in the
// file: the function that has the problem, not the line it currently sits
// on. A baseline keyed this way survives the file growing or shrinking above
// or below the finding.
func (f Finding) Key() string {
	return f.File + ":" + f.Func
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: func %s returns a literal %s on a path that already holds a non-nil error", f.File, f.Line, f.Func, f.Slot)
}

// Check walks every non-test .go file under root and returns one Finding per
// offending return statement, sorted for deterministic output. root is
// included in each Finding's File as a relative path from root's parent, so
// two calls with different roots under the same repo produce comparable keys.
func Check(root string) ([]Finding, error) {
	var findings []Finding
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(filepath.Dir(root), path)
		if rerr != nil {
			rel = path
		}
		findings = append(findings, checkFile(fset, file, rel)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Func != findings[j].Func {
			return findings[i].Func < findings[j].Func
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

func checkFile(fset *token.FileSet, file *ast.File, relFile string) []Finding {
	var findings []Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Type.Results == nil {
			continue
		}
		slots := resultSlots(fn.Type.Results)
		if len(slots) < 2 {
			continue
		}
		boolSlot, errSlot := len(slots)-2, len(slots)-1
		if exprString(slots[boolSlot]) != "bool" || exprString(slots[errSlot]) != "error" {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			name = exprString(fn.Recv.List[0].Type) + "." + name
		}
		w := &walker{fset: fset, file: relFile, funcName: name, boolSlot: boolSlot, errSlot: errSlot}
		w.walkStmts(fn.Body.List, false)
		findings = append(findings, w.findings...)
	}
	return findings
}

// resultSlots expands a result field list into one type expression per
// return position, matching Go's return-value ordering (a field shared by
// several names contributes one slot per name).
func resultSlots(results *ast.FieldList) []ast.Expr {
	var slots []ast.Expr
	for _, field := range results.List {
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			slots = append(slots, field.Type)
		}
	}
	return slots
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	default:
		return ""
	}
}

type walker struct {
	fset              *token.FileSet
	file, funcName    string
	boolSlot, errSlot int
	findings          []Finding
}

// walkStmts recurses through stmts, tracking whether the current position is
// inside a branch that already tested an err-like identifier against nil.
// Constructs that don't change that fact (loops, switches, plain blocks) just
// propagate inErrBranch through their own bodies.
func (w *walker) walkStmts(stmts []ast.Stmt, inErrBranch bool) {
	for _, stmt := range stmts {
		w.walkStmt(stmt, inErrBranch)
	}
}

func (w *walker) walkStmt(stmt ast.Stmt, inErrBranch bool) {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		if inErrBranch {
			w.checkReturn(s)
		}
	case *ast.IfStmt:
		bodyInErrBranch := inErrBranch || isErrNilCheck(s.Cond)
		w.walkStmts(s.Body.List, bodyInErrBranch)
		if s.Else != nil {
			w.walkStmt(s.Else, inErrBranch)
		}
	case *ast.BlockStmt:
		w.walkStmts(s.List, inErrBranch)
	case *ast.ForStmt:
		if s.Body != nil {
			w.walkStmts(s.Body.List, inErrBranch)
		}
	case *ast.RangeStmt:
		if s.Body != nil {
			w.walkStmts(s.Body.List, inErrBranch)
		}
	case *ast.SwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				w.walkStmts(cc.Body, inErrBranch)
			}
		}
	case *ast.TypeSwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				w.walkStmts(cc.Body, inErrBranch)
			}
		}
	case *ast.SelectStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CommClause); ok {
				w.walkStmts(cc.Body, inErrBranch)
			}
		}
	case *ast.LabeledStmt:
		w.walkStmt(s.Stmt, inErrBranch)
	}
}

func (w *walker) checkReturn(ret *ast.ReturnStmt) {
	if len(ret.Results) <= w.errSlot {
		return // naked return, or fewer values than the signature (invalid Go) — nothing to inspect
	}
	if isIdent(ret.Results[w.boolSlot], "true") {
		w.record(ret.Pos(), "bool")
		return
	}
	if isIdent(ret.Results[w.errSlot], "nil") {
		w.record(ret.Pos(), "error")
	}
}

func (w *walker) record(pos token.Pos, slot string) {
	w.findings = append(w.findings, Finding{
		File: w.file,
		Line: w.fset.Position(pos).Line,
		Func: w.funcName,
		Slot: slot,
	})
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// isErrNilCheck reports whether cond reads as (or contains, through && / ||)
// an err-like identifier compared to nil — err != nil, or a compound
// condition such as `err != nil || output == ""` that guardlint's own worked
// example (state_collapse.go, before gt-udrrw) used to swallow a read failure
// through.
func isErrNilCheck(cond ast.Expr) bool {
	switch c := cond.(type) {
	case *ast.ParenExpr:
		return isErrNilCheck(c.X)
	case *ast.BinaryExpr:
		switch c.Op {
		case token.LAND, token.LOR:
			return isErrNilCheck(c.X) || isErrNilCheck(c.Y)
		case token.NEQ:
			return (isErrLikeIdent(c.X) && isIdent(c.Y, "nil")) || (isIdent(c.X, "nil") && isErrLikeIdent(c.Y))
		}
	}
	return false
}

func isErrLikeIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && strings.Contains(strings.ToLower(id.Name), "err")
}
