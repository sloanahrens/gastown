package bdshim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/cmdtree"
)

// TestEveryHandlerIsAllowlisted: every verb the registry answers must name a
// path in deploy/bd/verbs.json, and every word of every allowed path must be a
// command word bd's tree resolves in order, so a handler or a path bd cannot
// run fails the gate before it reaches the gateway. The test sits beside the
// registry so adding a handler is what fails it; internal/bdgateway tests the
// whole path against the tree on the policy side.
func TestEveryHandlerIsAllowlisted(t *testing.T) {
	t.Parallel()
	paths := allowedPaths(t)
	tree, _, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	for _, msg := range pathWordErrors(paths, tree) {
		t.Error(msg)
	}
	verbs := make([]string, 0, len(handlers))
	for verb := range handlers {
		verbs = append(verbs, verb)
	}
	for _, msg := range uncoveredHandlers(paths, verbs) {
		t.Error(msg)
	}
}

// TestPathWordErrorsNamesAMissingWord drives the word check's failing branch
// with a word bd's tree does not hold.
func TestPathWordErrorsNamesAMissingWord(t *testing.T) {
	t.Parallel()
	tree, _, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	got := pathWordErrors([]string{"show", "not-a-bd-verb"}, tree)
	if len(got) != 1 || !strings.Contains(got[0], "not-a-bd-verb") {
		t.Fatalf("pathWordErrors = %v, want one message naming not-a-bd-verb", got)
	}
}

// TestPathWordErrorsExcusesOnlyAKnownTreeGap: a path listed in bdTreeGaps is
// excused while the snapshot lacks it and reported as stale once the tree runs
// it, and the list excuses no other path.
func TestPathWordErrorsExcusesOnlyAKnownTreeGap(t *testing.T) {
	t.Parallel()
	snapshot, _, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	if got := pathWordErrors([]string{"sample add"}, snapshot); len(got) != 0 {
		t.Errorf("pathWordErrors(sample add) = %v, want the listed gap excused", got)
	}

	refreshed := cmdtree.NewTree()
	refreshed.Add([]string{"sample", "add"}, nil, false)
	refreshed.Add([]string{"sample", "list"}, nil, false)
	got := pathWordErrors([]string{"sample add", "sample purge"}, refreshed)
	if len(got) != 2 || !strings.Contains(got[0], "sample add") || !strings.Contains(got[1], "sample purge") {
		t.Errorf("pathWordErrors on a tree holding the gap = %v, want a stale row for sample add and a word error for sample purge", got)
	}
}

// TestUncoveredHandlersNamesAHandler drives the handler check's failing branch
// with a handler no path names.
func TestUncoveredHandlersNamesAHandler(t *testing.T) {
	t.Parallel()
	got := uncoveredHandlers([]string{"show"}, []string{"show", "no-such-handler"})
	if len(got) != 1 || !strings.Contains(got[0], "no-such-handler") {
		t.Fatalf("uncoveredHandlers = %v, want one message naming no-such-handler", got)
	}
}

// allowedPaths reads the verb paths deploy/bd/verbs.json allows.
func allowedPaths(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "bd", "verbs.json"))
	if err != nil {
		t.Fatalf("read deploy/bd/verbs.json: %v", err)
	}
	var doc struct {
		Verbs []struct {
			Path string `json:"path"`
		} `json:"verbs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse deploy/bd/verbs.json: %v", err)
	}
	paths := make([]string, 0, len(doc.Verbs))
	for _, v := range doc.Verbs {
		paths = append(paths, v.Path)
	}
	return paths
}

// bdTreeGaps names the allowed verbs the command tree snapshot
// (internal/cmdtree/bd-command-tree.json) predates, with the bd commit that
// offers them. The snapshot records beads b9f68260ed2a494972fe8aa922b6408a91905f0c,
// which predates the sample verb; the town runs bd
// b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f. An entry excuses only that exact
// path, and only while the tree lacks it: once the snapshot is refreshed with
// `make bd-command-tree BEADS_SRC=<checkout>` the row fails the gate until it
// is deleted.
var bdTreeGaps = map[string]string{
	"sample add":  "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
	"sample list": "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
}

// pathWordErrors returns one message per path word bd's tree does not resolve
// in order. A prefix may stop on a help-only parent, which Resolve reports as
// HelpOnly rather than Unknown. A path in bdTreeGaps is excused while the tree
// lacks it, and reported once the tree resolves it.
func pathWordErrors(paths []string, tree *cmdtree.Tree) []string {
	var errs []string
	for _, path := range paths {
		if _, known := bdTreeGaps[path]; known {
			if tree.Resolve(strings.Fields(path)).OK {
				errs = append(errs, fmt.Sprintf("bdTreeGaps lists %q, but bd's tree now runs it: refresh the snapshot and delete the row", path))
			}
			continue
		}
		words := strings.Fields(path)
		if len(words) == 0 {
			errs = append(errs, "deploy/bd/verbs.json holds a verb with no path")
			continue
		}
		for n := 1; n <= len(words); n++ {
			if res := tree.Resolve(words[:n]); res.Unknown != "" {
				errs = append(errs, fmt.Sprintf("deploy/bd/verbs.json path %q: word %q does not resolve in bd's tree", path, words[n-1]))
			}
		}
	}
	return errs
}

// uncoveredHandlers returns one message per handler name no allowed path starts
// with.
func uncoveredHandlers(paths, verbs []string) []string {
	first := make(map[string]bool, len(paths))
	for _, path := range paths {
		if words := strings.Fields(path); len(words) > 0 {
			first[words[0]] = true
		}
	}
	var errs []string
	for _, verb := range verbs {
		if !first[verb] {
			errs = append(errs, fmt.Sprintf("bdshim answers %q, but deploy/bd/verbs.json has no path that starts with it", verb))
		}
	}
	return errs
}
