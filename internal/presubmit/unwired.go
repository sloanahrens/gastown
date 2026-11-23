package presubmit

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"slices"
)

// FileLister lists the Go files of the module, repo-relative; tests answer from a slice, so unit
// tests run no process.
type FileLister func() ([]string, error)

// UnwiredFinding is an added exported function that no non-test file of the module names.
type UnwiredFinding struct {
	Path string // repo-relative path of the file that declares it
	Line int    // line of the declaration in that file
	Name string
}

// UnwiredResult is what the check judged on one branch: how many added exported functions it read,
// and those of them no non-test file names.
type UnwiredResult struct {
	Judged   int
	Findings []UnwiredFinding
}

// candidate is one added exported function and the lines its declaration spans.
type candidate struct {
	path, name string
	line, last int
	wired      bool
}

// JudgeUnwired reports the exported top-level functions base...HEAD adds to non-test Go files that
// no non-test file of the module names (gt-46ezdn). An added function is one whose name line the
// diff adds; a method, an unexported name, a test file and a file of a nested module are not judged.
// The result is advisory, so the caller prints the findings. A quoted diff path, a file that cannot
// be read or parsed, or a module that lists no Go file while a function was judged is an error: a
// branch the check could not read never reports as one with no unwired function.
func JudgeUnwired(diff io.Reader, list FileLister, read FileReader, nested func(string) bool) (UnwiredResult, error) {
	var res UnwiredResult
	added, err := addedLines(diff)
	if err != nil {
		return res, err
	}
	inModule := func(p string) bool { return isGoSource(p) && (nested == nil || !nested(p)) }
	addedAt := map[string]map[int]bool{} // the lines each changed file adds
	for _, a := range added {
		if a.path == "" {
			return res, fmt.Errorf("git quoted a changed path in the diff; its added functions cannot be judged")
		}
		if !inModule(a.path) {
			continue
		}
		if addedAt[a.path] == nil {
			addedAt[a.path] = map[int]bool{}
		}
		addedAt[a.path][a.line] = true
	}

	var cands []candidate
	for _, p := range slices.Sorted(maps.Keys(addedAt)) {
		src, err := read(p)
		if err != nil {
			return res, fmt.Errorf("reading %s: %w", p, err)
		}
		fset, f, err := parseGo(p, src)
		if err != nil {
			return res, err
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !ast.IsExported(fn.Name.Name) {
				continue
			}
			line := fset.Position(fn.Name.Pos()).Line
			if addedAt[p][line] {
				cands = append(cands, candidate{p, fn.Name.Name, line, fset.Position(fn.End()).Line, false})
			}
		}
	}
	res.Judged = len(cands)
	if len(cands) == 0 {
		return res, nil
	}

	files, err := list()
	if err != nil {
		return res, fmt.Errorf("listing the module's Go files: %w", err)
	}
	if len(files) == 0 {
		return res, fmt.Errorf("the module lists no Go file, so no added function's callers could be judged")
	}
	pending := map[string][]int{} // candidate indices still unwired, by name
	for i := range cands {
		pending[cands[i].name] = append(pending[cands[i].name], i)
	}
	slices.Sort(files)
	for _, p := range files {
		if len(pending) == 0 {
			break
		}
		if !inModule(p) {
			continue
		}
		src, err := read(p)
		if err != nil {
			return res, fmt.Errorf("reading %s: %w", p, err)
		}
		if !namesPresent(src, pending) {
			continue
		}
		fset, f, err := parseGo(p, src)
		if err != nil {
			return res, err
		}
		seen := namedIdents(fset, f)
		for name, idxs := range pending {
			for _, i := range idxs {
				for _, line := range seen[name] {
					// A name inside the declaration itself, such as a recursive call, does not wire it.
					if cands[i].path != p || line < cands[i].line || cands[i].last < line {
						cands[i].wired = true
						break
					}
				}
			}
			if idxs = slices.DeleteFunc(idxs, func(i int) bool { return cands[i].wired }); len(idxs) > 0 {
				pending[name] = idxs
			} else {
				delete(pending, name)
			}
		}
	}
	// The file list is sorted and each file's declarations come in source order, so findings are sorted.
	for _, c := range cands {
		if !c.wired {
			res.Findings = append(res.Findings, UnwiredFinding{Path: c.path, Line: c.line, Name: c.name})
		}
	}
	return res, nil
}

// parseGo parses one file, keeping the file set that gives its nodes their positions.
func parseGo(path string, src []byte) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return fset, f, nil
}

// namedIdents returns, per identifier name, every line its syntax names it on; comments and string
// literals name none.
func namedIdents(fset *token.FileSet, f *ast.File) map[string][]int {
	out := map[string][]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			out[id.Name] = append(out[id.Name], fset.Position(id.Pos()).Line)
		}
		return true
	})
	return out
}

// namesPresent reports whether src holds any pending name as text, so a file that cannot name a
// candidate is skipped without a parse.
func namesPresent(src []byte, pending map[string][]int) bool {
	for name := range pending {
		if bytes.Contains(src, []byte(name)) {
			return true
		}
	}
	return false
}
