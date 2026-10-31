package ring

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// holdSites are the production files that act on the Ring: each calls Hold once,
// and nothing else from the package beyond the key resolver and its type. A
// fourth reader that tested the Waned state or its expiry beside Hold would
// re-implement the rule the helper states once (gt-6mlik.5).
var holdSites = map[string]bool{
	"dispatch/hold.go":         true,
	"supervisor/supervisor.go": true,
	"cmd/rig_helpers.go":       true,
}

// siteRingSelectors are the ring package names a site may use: Hold, the key
// resolver and its type. Anything else from the package is the state itself.
var siteRingSelectors = map[string]bool{
	"Hold":                true,
	"TownKey":             true,
	"KeyFunc":             true,
	"ErrKeyNotConfigured": true,
}

// TestHoldSitesAreTheOnlyReaders walks the production code under internal/ and
// fails when Hold is called from anywhere but holdSites, or when a site reaches
// past the helper to the Ring's state or expiry. The walk is over the source
// tree, not the town.
func TestHoldSitesAreTheOnlyReaders(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	fset := token.NewFileSet()
	callers := map[string]int{}
	var problems []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "ring" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok && id.Name == "ring" && holdSites[rel] && !siteRingSelectors[n.Sel.Name] {
					problems = append(problems, rel+": reaches past the helper to ring."+n.Sel.Name)
				}
				if holdSites[rel] && n.Sel.Name == "ExpiresAt" {
					problems = append(problems, rel+": reads ExpiresAt itself")
				}
				if id, ok := n.X.(*ast.Ident); ok && id.Name == "ring" && n.Sel.Name == "Hold" {
					callers[rel]++
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) == 0 {
		t.Fatal("found no ring.Hold call: the walk has gone blind, not found the sites")
	}
	for rel := range holdSites {
		if callers[rel] != 1 {
			t.Errorf("%s calls ring.Hold %d times, want exactly 1", rel, callers[rel])
		}
	}
	for rel := range callers {
		if !holdSites[rel] {
			t.Errorf("%s calls ring.Hold, which is not one of the hold sites", rel)
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
}
