package presubmit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"slices"
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
// pinned, so the operator's git config cannot hide a path (gt-4ndz7z).
func ReadDiff(run Runner, base string) ([]byte, error) {
	// An empty base makes git reject the range, so the check fails rather than passes.
	revs := base + "...HEAD"
	out, err := run([]string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
		"--unified=0", "--src-prefix=a/", "--dst-prefix=b/", "--no-renames", "--end-of-options", revs, "--", "*.go"})
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", revs, err)
	}
	return out, nil
}

// JudgeTestWaits reports the lines base...HEAD adds to _test.go files that wait with no bound: a channel receive,
// a call to a method named Wait, or a for loop with no condition, each outside a select statement with a second
// clause; a line already on the base is not judged (gt-4ndz7z).
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
	// stack says, for each node being walked, whether it is a select that bounds the waits inside it.
	var stack []bool
	bounded := func() bool { return slices.Contains(stack, true) }
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if !bounded() {
			if line, ok := waitNodeLine(fset, n); ok {
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

// waitNodeLine reports the line of n when n waits with no bound: a channel receive, a call to a method named Wait, or
// a for loop with no condition. The judge reads syntax alone, so a Wait it cannot type is judged rather than passed.
func waitNodeLine(fset *token.FileSet, n ast.Node) (int, bool) {
	switch n := n.(type) {
	case *ast.UnaryExpr:
		if n.Op == token.ARROW {
			return fset.Position(n.OpPos).Line, true
		}
	case *ast.CallExpr:
		if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
			return fset.Position(sel.Sel.Pos()).Line, true
		}
	case *ast.ForStmt:
		if n.Cond == nil {
			return fset.Position(n.For).Line, true
		}
	}
	return 0, false
}
