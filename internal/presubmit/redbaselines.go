package presubmit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
)

// RedBaseTest is one named test in a test file at HEAD, with the line its func keyword is on.
type RedBaseTest struct {
	Name string
	File string
	Line int
}

// RedBaseTests reports, in file order, each package-level function in head whose name is in names, with the line of its func keyword and File set to path (gt-ko70aq). A method sharing a name and a function not in names give no entry. A head that does not parse is an error naming path, so a file the check could not read never reads as a file with no test to run.
func RedBaseTests(path string, head []byte, names []string) ([]RedBaseTest, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, head, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var out []RedBaseTest
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || !slices.Contains(names, fd.Name.Name) {
			continue
		}
		out = append(out, RedBaseTest{Name: fd.Name.Name, File: path, Line: fset.Position(fd.Pos()).Line})
	}
	return out, nil
}
