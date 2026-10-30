package docslint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestRunDocDoesNotRestateUsage holds the Run doc comment to the contract: the
// option list is the command's usage output, and a comment that copies it goes
// stale when the options change (docs/writing-for-agents.md R3).
func TestRunDocDoesNotRestateUsage(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}
	var doc string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Run" && fn.Doc != nil {
			doc = fn.Doc.Text()
		}
	}
	if doc == "" {
		t.Fatal("Run has no doc comment")
	}
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "docs-lint ") {
			t.Errorf("Run doc restates the usage block: %q", strings.TrimSpace(line))
		}
	}
}
