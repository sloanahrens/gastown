//go:build integration

package docslint

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestIntegrationMarkupChecksOnDisk runs the line checks over a real tree. The
// fake cannot show that a docs/research directory which does not exist is an
// empty tier rather than an error, or that OSFiles hands the checks the same
// repo-relative paths the fakes use.
func TestIntegrationMarkupChecksOnDisk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		"AGENTS.md":       "Run the gate.\n",
		"docs/plans/p.md": "# Plan without a header\n",
		"docs/guide.md":   "> Status: Live\n<tool_call>\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fsys := OSFiles{Root: root}

	headers, err := CheckStatusHeaders(fsys)
	if err != nil {
		t.Fatalf("CheckStatusHeaders() on a real tree: %v (docs/research is absent and must be an empty tier)", err)
	}
	wantHeaders := []string{`docs/plans/p.md:1: status-header: first non-blank line must start with "> Status:"`}
	if got := findingStrings(headers); !reflect.DeepEqual(got, wantHeaders) {
		t.Errorf("CheckStatusHeaders() = %q, want %q", got, wantHeaders)
	}

	markup, err := CheckStrayMarkup(fsys)
	if err != nil {
		t.Fatalf("CheckStrayMarkup() on a real tree: %v", err)
	}
	wantMarkup := []string{"docs/guide.md:2: stray-markup: committed tool-call or merge-conflict markup"}
	if got := findingStrings(markup); !reflect.DeepEqual(got, wantMarkup) {
		t.Errorf("CheckStrayMarkup() = %q, want %q", got, wantMarkup)
	}
}
