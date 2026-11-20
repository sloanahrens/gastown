package modguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/gosrc"
)

// TestModuleTree checks the shared tree's root, identity and source read.
func TestModuleTree(t *testing.T) {
	t.Parallel()
	root := tree.Root()
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("tree root %s holds no go.mod: %v", root, err)
	}
	if again := gosrc.For(root); again != tree {
		t.Errorf("gosrc.For(%s) = %p, want the package's tree %p", root, again, tree)
	}
	path := filepath.Join(root, "internal", "modguard", "main_test.go")
	got, err := tree.Source(path)
	if err != nil {
		t.Fatalf("reading %s through the tree: %v", path, err)
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: the file the tree was asked to read
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the tree served %d bytes of %s, want the file's %d", len(got), path, len(want))
	}
}
