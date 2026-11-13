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

// pathWordErrors returns one message per path word bd's tree does not resolve
// in order. A prefix may stop on a help-only parent, which Resolve reports as
// HelpOnly rather than Unknown.
func pathWordErrors(paths []string, tree *cmdtree.Tree) []string {
	var errs []string
	for _, path := range paths {
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
