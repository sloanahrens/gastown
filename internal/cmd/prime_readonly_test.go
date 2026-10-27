package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// writeCalls are the calls that write a file. runPrime may make none of them:
// the system-prompt file is the runtime's, rendered once at spawn (gt-uhzl7).
var writeCalls = map[string]bool{
	"writeSystemPromptFile": true, "WriteFile": true, "Create": true,
	"OpenFile": true, "Rename": true, "WriteFileAtomic": true,
}

// TestRunPrimeMakesNoFileWrite reads runPrime's own source and fails on a call
// that writes a file, so prime leaves the system-prompt file untouched on a host
// and in a sandbox alike (gt-uhzl7). It checks the body of runPrime, the direct
// call sites; a write made by a helper runPrime calls is a finding for review.
func TestRunPrimeMakesNoFileWrite(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "prime.go", nil, 0)
	if err != nil {
		t.Fatalf("parse prime.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "runPrime" {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatal("runPrime not found in prime.go")
	}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if writeCalls[name] {
			t.Errorf("runPrime calls %s at %s; prime must not write a file", name, fset.Position(call.Pos()))
		}
		return true
	})
}
