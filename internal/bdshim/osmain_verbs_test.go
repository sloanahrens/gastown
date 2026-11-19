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
	tree, snap, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	for _, msg := range pathWordErrors(paths, tree, gapsFor(snap.Commit)) {
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
	got := pathWordErrors([]string{"show", "not-a-bd-verb"}, tree, nil)
	if len(got) != 1 || !strings.Contains(got[0], "not-a-bd-verb") {
		t.Fatalf("pathWordErrors = %v, want one message naming not-a-bd-verb", got)
	}
}

// TestPathWordErrorsReportsAGapWhenNoneIsExcused: with no excuse list, a verb
// the tree lacks is a word error, so the excuse can only come from the list.
func TestPathWordErrorsReportsAGapWhenNoneIsExcused(t *testing.T) {
	t.Parallel()
	tree := cmdtree.NewTree()
	tree.Add([]string{"show"}, nil, false)
	got := pathWordErrors([]string{"sample add"}, tree, nil)
	if len(got) == 0 || !strings.Contains(got[0], `"sample add"`) {
		t.Fatalf("pathWordErrors(sample add, no excuses) = %v, want word errors for sample add", got)
	}
}

// TestGapsForExcusesOnlyTheSnapshotItWasWrittenAgainst: the excuses hold while
// the embedded snapshot is the commit bdTreeGaps was written against, and a
// refreshed snapshot at any other commit excuses nothing.
func TestGapsForExcusesOnlyTheSnapshotItWasWrittenAgainst(t *testing.T) {
	t.Parallel()
	if got := gapsFor(bdTreeGapsSnapshot); len(got) != len(bdTreeGaps) {
		t.Errorf("gapsFor(recorded snapshot) has %d rows, want %d", len(got), len(bdTreeGaps))
	}
	if got := gapsFor("0000000000000000000000000000000000000000"); len(got) != 0 {
		t.Errorf("gapsFor(other snapshot) = %v, want no excuses", got)
	}
}

// TestPathWordErrorsExcusesOnlyAKnownTreeGap: a path listed in the excuse list
// is excused while the tree lacks it and reported as stale once the tree runs
// it, and the list excuses no other path. The tree is built here, so the test
// does not depend on what the embedded snapshot holds.
func TestPathWordErrorsExcusesOnlyAKnownTreeGap(t *testing.T) {
	t.Parallel()
	absent := cmdtree.NewTree()
	absent.Add([]string{"show"}, nil, false)
	if got := pathWordErrors([]string{"sample add"}, absent, bdTreeGaps); len(got) != 0 {
		t.Errorf("pathWordErrors(sample add) = %v, want the listed gap excused", got)
	}

	refreshed := cmdtree.NewTree()
	refreshed.Add([]string{"sample", "add"}, nil, false)
	refreshed.Add([]string{"sample", "list"}, nil, false)
	got := pathWordErrors([]string{"sample add", "sample purge"}, refreshed, bdTreeGaps)
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
// which predates the sample verb. The town's pin is bd
// 0f9f640fe0f1a4162f3aebdef7994135b1167a85, which it runs only after the
// schema-67 migration window (gt-jgsmoj) rolls the pin pair; the predecessor
// b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f predates the sample verb (be-1qqu).
// An entry excuses only that exact path, and only while the tree lacks it: once
// the snapshot is refreshed with `make bd-command-tree BEADS_SRC=<checkout>`
// the row fails the gate until it is deleted.
var bdTreeGaps = map[string]string{
	"sample add":  "0f9f640fe0f1a4162f3aebdef7994135b1167a85",
	"sample list": "0f9f640fe0f1a4162f3aebdef7994135b1167a85",
}

// bdTreeGapsSnapshot is the command tree snapshot commit bdTreeGaps is written
// against. The excuses apply only while the embedded snapshot is this commit,
// so a refreshed snapshot re-checks every verb.
const bdTreeGapsSnapshot = "b9f68260ed2a494972fe8aa922b6408a91905f0c"

// gapsFor returns the excuses that apply to a tree snapshot at commit: bdTreeGaps
// for the snapshot it was written against, none for any other.
func gapsFor(commit string) map[string]string {
	if commit == bdTreeGapsSnapshot {
		return bdTreeGaps
	}
	return nil
}

// pathWordErrors returns one message per path word bd's tree does not resolve
// in order. A prefix may stop on a help-only parent, which Resolve reports as
// HelpOnly rather than Unknown. A path in gaps is excused while the tree lacks
// it, and reported once the tree resolves it.
func pathWordErrors(paths []string, tree *cmdtree.Tree, gaps map[string]string) []string {
	var errs []string
	for _, path := range paths {
		if _, known := gaps[path]; known {
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
