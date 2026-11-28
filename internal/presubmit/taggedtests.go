package presubmit

import (
	"go/ast"
	"go/format"
	"go/token"
	"io"
	"maps"
	"slices"
)

// ChangedTaggedTests names, sorted, the top-level Test functions an
// integration-tagged test file gained or whose body changed between its content
// at the merge base and at HEAD, so post-land can say which integration tests a
// landing changed (gt-10zcdg). It is ChangedTests for the files the gate cannot
// run: an untagged head gives no names, as ChangedTests gives none for a tagged
// head, so a file's tests are judged by one of the two, never both. A nil base
// counts every test in head. Bodies compare as gofmt prints them, so a comment
// or layout edit is no change. A file that does not parse at either side is an
// error naming it.
func ChangedTaggedTests(path string, base, head []byte) ([]string, error) {
	if !IntegrationTagged(head) {
		return nil, nil
	}
	print := func(w io.Writer, body *ast.BlockStmt) error {
		return format.Node(w, token.NewFileSet(), body)
	}
	headTests, err := testBodies(path, head, print)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return slices.Sorted(maps.Keys(headTests)), nil
	}
	baseTests, err := testBodies(path, base, print)
	if err != nil {
		return nil, err
	}
	var changed []string
	for name, body := range headTests {
		if old, ok := baseTests[name]; !ok || old != body {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed, nil
}
