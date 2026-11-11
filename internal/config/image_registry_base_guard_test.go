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
		h, err := registryURLHits(path, src)
		if err != nil {
			return err
		}
		hits = append(hits, h...)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the internal source tree: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("these build a registry URL by hand; use ImageRegistryBase instead:\n%s", strings.Join(hits, "\n"))
	}
}

// registryURLHits returns the position of each expression in one source file
// that builds a registry URL by hand. A file that never spells "http://" has
// none, so it is not parsed.
func registryURLHits(path string, src []byte) ([]string, error) {
	if !bytes.Contains(src, []byte("http://")) {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && prependsRegistryURL(e) {
			hits = append(hits, fset.Position(e.Pos()).String())
		}
		return true
	})
	return hits, nil
}

// TestRegistryURLHitsReadsFormatStrings: a file that builds the registry URL
// with a format string is read and flagged, not skipped for lacking a
// concatenation.
func TestRegistryURLHitsReadsFormatStrings(t *testing.T) {
	t.Parallel()
	src := []byte(`package config

import "fmt"

func url(cfg Cfg) string {
	return fmt.Sprintf("http://%s", cfg.Registry)
}
`)
	hits, err := registryURLHits("x.go", src)
	if err != nil {
		t.Fatalf("registryURLHits: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("registryURLHits = %v, want one hit for the Sprintf", hits)
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
		"sprintf setting":     {`fmt.Sprintf("http://%s", cfg.Registry)`, true},
		"sprintf manifest":    {`fmt.Sprintf("http://%s/v2/%s", registry, repo)`, true},
		"sprint literal arg":  {`fmt.Sprint("http://", cfg.Registry)`, true},
		"errorf setting":      {`fmt.Errorf("http://%s: refused", r.Cfg.Registry)`, true},
		"join of parts":       {`strings.Join([]string{"http://", cfg.Registry, "/v2"}, "")`, true},
		"sprintf tracker":     {`fmt.Sprintf("http://%s", net.JoinHostPort(h, p))`, false},
		"sprintf https":       {`fmt.Sprintf("https://%s", cfg.Registry)`, false},
		"sprintf no registry": {`fmt.Sprintf("http://%s", host)`, false},
		"sprintf no literal":  {`fmt.Sprintf("%s/v2", ImageRegistryBase(cfg.Registry))`, false},
		"join tracker":        {`strings.Join([]string{"http://", host}, "")`, false},
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

// prependsRegistryURL reports whether e builds a URL by hand from a literal
// "http://" and an identifier naming a registry. The forms it covers are a
// concatenation ("http://" + reg), a fmt print call with the literal as its
// first argument (fmt.Sprintf("http://%s", reg)), and a composite literal with
// the literal as its first element (strings.Join([]string{"http://", reg}, "")).
func prependsRegistryURL(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		return x.Op == token.ADD && startsWithHTTPLiteral(x) && namesRegistry(x)
	case *ast.CallExpr:
		return isFmtPrint(x.Fun) && len(x.Args) > 0 && isHTTPLiteral(x.Args[0]) && namesRegistry(x.Args[1:]...)
	case *ast.CompositeLit:
		return len(x.Elts) > 0 && isHTTPLiteral(x.Elts[0]) && namesRegistry(x.Elts[1:]...)
	}
	return false
}

// namesRegistry reports whether an identifier anywhere in exprs contains
// "registry", in any case.
func namesRegistry(exprs ...ast.Expr) bool {
	named := false
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "registry") {
				named = true
			}
			return !named
		})
	}
	return named
}

// isFmtPrint reports whether fun names one of the fmt functions that take a
// format or a list of operands and return a string or an error.
func isFmtPrint(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return false
	}
	switch sel.Sel.Name {
	case "Sprintf", "Sprint", "Sprintln", "Errorf":
		return true
	}
	return false
}

// startsWithHTTPLiteral reports whether the leftmost operand of a chain of
// concatenations is an "http://" literal.
func startsWithHTTPLiteral(e ast.Expr) bool {
	for {
		bin, ok := e.(*ast.BinaryExpr)
		if !ok || bin.Op != token.ADD {
			break
		}
		e = bin.X
	}
	return isHTTPLiteral(e)
}

// isHTTPLiteral reports whether e is a string literal whose value begins with
// "http://". A format string such as "http://%s" counts.
func isHTTPLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && strings.HasPrefix(v, "http://")
}
