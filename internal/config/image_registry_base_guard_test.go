package config

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestImageRegistryBaseNotPrependedInSource: no non-test file under internal/
// prepends "http://" to an expression that names the registry, so the setting
// becomes a URL only through ImageRegistryBase (gt-vv9ir0).
func TestImageRegistryBaseNotPrependedInSource(t *testing.T) {
	t.Parallel()
	var hits []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, []byte(`"http://"`)) {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && prependsRegistryURL(e) {
				hits = append(hits, fset.Position(e.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("reading the internal source tree: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("these build a registry URL by hand; use ImageRegistryBase instead:\n%s", strings.Join(hits, "\n"))
	}
}

// TestPrependsRegistryURLDetector: the guard flags "http://" joined to an
// expression that names a registry, and only that. A tracker host and the helper
// itself are not flagged.
func TestPrependsRegistryURLDetector(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		expr string
		want bool
	}{
		"setting field":       {`"http://" + cfg.Registry`, true},
		"manifest path":       {`"http://"+registry+"/v2/"+repo`, true},
		"nested selector":     {`"http://" + r.Cfg.Registry`, true},
		"tracker host":        {`"http://" + net.JoinHostPort(h, p)`, false},
		"helper call":         {`ImageRegistryBase(cfg.Registry)`, false},
		"https prefix":        {`"https://" + cfg.Registry`, false},
		"no literal in front": {`cfg.Registry + "/v2"`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, err := parser.ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tt.expr, err)
			}
			got := false
			ast.Inspect(e, func(n ast.Node) bool {
				if x, ok := n.(ast.Expr); ok && prependsRegistryURL(x) {
					got = true
				}
				return true
			})
			if got != tt.want {
				t.Errorf("prependsRegistryURL(%s) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// prependsRegistryURL reports whether e is a string concatenation that starts
// with the literal "http://" and has an identifier naming a registry anywhere in
// its operands.
func prependsRegistryURL(e ast.Expr) bool {
	bin, ok := e.(*ast.BinaryExpr)
	if !ok || bin.Op != token.ADD || !startsWithHTTPLiteral(bin) {
		return false
	}
	named := false
	ast.Inspect(bin, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "registry") {
			named = true
		}
		return !named
	})
	return named
}

// startsWithHTTPLiteral reports whether the leftmost operand of a chain of
// concatenations is the string literal "http://".
func startsWithHTTPLiteral(e ast.Expr) bool {
	for {
		bin, ok := e.(*ast.BinaryExpr)
		if !ok || bin.Op != token.ADD {
			break
		}
		e = bin.X
	}
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == "http://"
}
