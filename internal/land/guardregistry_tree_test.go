package land

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// TestEveryTreeScanningTestIsRegistered fails the gate when the real tree holds
// a test that scans the source tree and is neither a treeWideGuards entry nor a
// line of internal/land/testdata/unregistered-scanners.txt, so the next scanner
// like TestImageRegistryBaseNotPrependedInSource cannot land unregistered
// (gt-yboxur).
func TestEveryTreeScanningTestIsRegistered(t *testing.T) {
	t.Parallel()
	repo := os.DirFS("../..")
	text, err := fs.ReadFile(repo, "internal/land/testdata/unregistered-scanners.txt")
	if err != nil {
		t.Fatalf("reading the exemption file: %v", err)
	}
	exempt, err := parseExemptions(string(text))
	if err != nil {
		t.Fatalf("parsing the exemption file: %v", err)
	}
	found, err := ScanningTests(repo)
	if err != nil {
		t.Fatalf("scanning the source tree: %v", err)
	}
	if lines := guardRegistryFailures(found, treeWideGuards, exempt); len(lines) > 0 {
		t.Errorf("tree-scanning tests are neither registered nor exempt:\n%s", strings.Join(lines, "\n"))
	}
}
