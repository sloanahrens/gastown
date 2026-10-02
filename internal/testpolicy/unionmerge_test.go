package testpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// growOnlyHeader is the header every list .gitattributes marks merge=union
// must carry. Union keeps both sides' lines, so a list it covers may only
// gain them; an edit on one side with an add on the other would keep both
// texts silently.
const growOnlyHeader = "This list only grows"

// TestUnionMergeOnlyOnGrowOnlyLists holds the repo-root .gitattributes to one
// rule: every path it marks merge=union must exist and say in its header that
// it only grows. A shrinking or rewritten file (a ratchet baseline, a
// changelog) loses or duplicates text under the union driver, so it must
// never get the marking.
//
// The paths it reads lie outside this package, so internal/land's guard
// selection names .gitattributes and the marked lists as inputs
// (treeWideGuards in internal/land/changed.go).
func TestUnionMergeOnlyOnGrowOnlyLists(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		t.Fatalf("reading .gitattributes: %v", err)
	}
	marked := 0
	for _, line := range strings.Split(string(attrs), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !setsAttr(fields[1:], "merge=union") {
			continue
		}
		marked++
		path := filepath.FromSlash(fields[0])
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Errorf(".gitattributes marks %q merge=union, but that is not a readable file: %v", path, err)
			continue
		}
		if !strings.Contains(string(body), growOnlyHeader) {
			t.Errorf(".gitattributes marks %q merge=union, but its header does not say %q: union keeps both sides' lines, so the list must only grow", path, growOnlyHeader)
		}
	}
	if marked == 0 {
		t.Error(".gitattributes marks no path merge=union, want internal/testpolicy/gitfree.txt")
	}
}

// setsAttr reports whether attrs sets attr. A pattern that unsets it (-attr,
// !attr) or leaves it unspecified does not.
func setsAttr(attrs []string, attr string) bool {
	for _, a := range attrs {
		if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "!") {
			continue
		}
		if a == attr {
			return true
		}
	}
	return false
}
