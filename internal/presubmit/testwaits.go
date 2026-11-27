package presubmit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"slices"
	"strconv"
	"strings"
)

// FileReader reads one path as the branch commits it; tests answer from a map, so unit tests run no process.
type FileReader func(path string) ([]byte, error)

// TestWaitFinding is an added test line that waits with no bound.
type TestWaitFinding struct {
	Path string
	Line int
	Text string
}

// TestWaitResult is what the check judged on one branch: the lines it adds to _test.go files and those that wait with no bound.
type TestWaitResult struct {
	Judged   int
	Findings []TestWaitFinding
}

// ReadDiff reads base...HEAD as a context-free, rename-blind Go diff with the prefixes and quotePath
// pinned, so the operator's git config cannot hide a path (gt-4ndz7z). It refuses an empty base,
// which git would read as HEAD...HEAD and diff clean (gt-whjd81).
func ReadDiff(run Runner, base string) ([]byte, error) {
	if base == "" {
		return nil, fmt.Errorf("read diff: empty base")
	}
	revs := base + "...HEAD"
	out, err := run([]string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
		"--unified=0", "--src-prefix=a/", "--dst-prefix=b/", "--no-renames", "--end-of-options", revs, "--", "*.go"})
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", revs, err)
	}
	return out, nil
}

// JudgeTestWaits reports the lines base...HEAD adds to _test.go files that wait with no bound: a channel receive,
// a call to a method named Wait on any receiver, a `for` loop with no condition, or a `for range x` loop that names
// no key or value, each outside a select statement with a second clause; a line already on the base is not judged
// (gt-4ndz7z, gt-whjd81). synctest.Wait in a file that imports testing/synctest is the bubble barrier and is not a
// wait (gt-10cacr).
func JudgeTestWaits(diff io.Reader, read FileReader) (TestWaitResult, error) {
	var res TestWaitResult
	added, err := addedLines(diff)
	if err != nil {
		return res, err
	}
	byFile := map[string][]addedLine{}
	var files []string
	for _, a := range added {
		if a.path == "" {
			return res, fmt.Errorf("git quoted a changed path in the diff; its added test lines cannot be judged")
		}
		if !strings.HasSuffix(a.path, "_test.go") {
			continue
		}
		if _, seen := byFile[a.path]; !seen {
			files = append(files, a.path)
		}
		byFile[a.path] = append(byFile[a.path], a)
		res.Judged++
	}
	for _, p := range files {
		src, err := read(p)
		if err != nil {
			return res, fmt.Errorf("reading %s: %w", p, err)
		}
		findings, err := judgeTestFile(src, p, byFile[p])
		if err != nil {
			return res, fmt.Errorf("%s: %w", p, err)
		}
		res.Findings = append(res.Findings, findings...)
	}
	return res, nil
}

// judgeTestFile parses one branch test file and reports the added lines that hold a wait with no bound.
func judgeTestFile(src []byte, path string, added []addedLine) ([]TestWaitFinding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	text := make(map[int]string, len(added))
	for _, a := range added {
		text[a.line] = a.text
	}
	var out []TestWaitFinding
	reported := map[int]bool{}
	synctestBounded := importsSynctest(file)
	// stack says, for each node being walked, whether it is a select that bounds the waits inside it.
	var stack []bool
	bounded := func() bool { return slices.Contains(stack, true) }
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if !bounded() {
			if line, ok := waitNodeLine(fset, n, synctestBounded); ok {
				if lineText, wanted := text[line]; wanted && !reported[line] {
					reported[line] = true
					out = append(out, TestWaitFinding{Path: path, Line: line, Text: strings.TrimSpace(lineText)})
				}
			}
		}
		sel, isSelect := n.(*ast.SelectStmt)
		stack = append(stack, isSelect && sel.Body != nil && len(sel.Body.List) >= 2)
		return true
	})
	slices.SortFunc(out, func(a, b TestWaitFinding) int { return a.Line - b.Line })
	return out, nil
}

// importsSynctest reports whether file imports testing/synctest as synctest, which makes synctest.Wait the
// bubble barrier. An import under another name is not the package this file calls, so it does not count.
func importsSynctest(file *ast.File) bool {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "testing/synctest" {
			continue
		}
		return imp.Name == nil || imp.Name.Name == "synctest"
	}
	return false
}

// waitNodeLine reports the line of n when n waits with no bound: a channel receive, a call to a method named Wait,
// a `for` loop with no condition, or a `for range x` loop that names no key or value. The judge reads syntax alone,
// so it judges any method named Wait whatever its receiver, since it cannot tell a sync.WaitGroup or exec.Cmd from a
// type with a Wait that never returns. A range loop that binds the element is left alone: syntax cannot tell a
// channel, which ends only on close, from a table, which ends on its own, and table loops are the common case.
// synctest.Wait in a file that imports the package is the exception: that barrier returns once every goroutine in
// the bubble is blocked, or panics on deadlock, so it is always bounded (gt-10cacr).
func waitNodeLine(fset *token.FileSet, n ast.Node, synctestBounded bool) (int, bool) {
	switch n := n.(type) {
	case *ast.UnaryExpr:
		if n.Op == token.ARROW {
			return fset.Position(n.OpPos).Line, true
		}
	case *ast.CallExpr:
		if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "synctest" && synctestBounded {
				return 0, false
			}
			return fset.Position(sel.Sel.Pos()).Line, true
		}
	case *ast.ForStmt:
		if n.Cond == nil {
			return fset.Position(n.For).Line, true
		}
	case *ast.RangeStmt:
		if n.Key == nil && n.Value == nil {
			return fset.Position(n.For).Line, true
		}
	}
	return 0, false
}
