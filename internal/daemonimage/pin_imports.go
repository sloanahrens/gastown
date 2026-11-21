package daemonimage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// devopsModulePath prefixes every import path in the devops repository.
const devopsModulePath = "gt.local/devops/"

// devopsImports returns the devops directories one source file imports, in
// order without duplicates, or an error and no list when the source does not
// parse as Go anywhere or an import literal cannot be decoded (gt-886zbx).
func devopsImports(src string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse Go source: %w", err)
	}
	return devopsDirs(f)
}

// devopsDirs returns the devops directories the parsed file imports; a literal
// it cannot decode is an error, not a skip.
func devopsDirs(f *ast.File) ([]string, error) {
	var dirs []string
	seen := make(map[string]bool, len(f.Imports))
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("import path %s: %w", imp.Path.Value, err)
		}
		if dir, ok := strings.CutPrefix(path, devopsModulePath); ok && dir != "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}
