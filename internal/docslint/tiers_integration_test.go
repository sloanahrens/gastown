//go:build integration

package docslint

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestIntegrationOSFilesReadsACheckout drives OSFiles over a real tree: the
// fake cannot show that WalkDir and filepath.Rel report repo-relative slash
// paths, or that a missing root is an empty tier rather than an error.
func TestIntegrationOSFilesReadsACheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, contents string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", rel, err)
		}
	}
	write("AGENTS.md", "# agents\n")
	write("docs/design/retired.md", "> Status: historical\n")
	write("docs/design/live.md", "# live\n")
	write("plugins/p/plugin.md", "# plugin\n")
	write("internal/pkg/a.go", "package pkg\n")
	write("internal/pkg/a_test.go", "package pkg\n")

	fsys := OSFiles{Root: root}

	// Exists reports a regular file: a directory that exists is not a file.
	if !fsys.Exists("AGENTS.md") {
		t.Error("Exists(AGENTS.md) = false, want true")
	}
	if fsys.Exists("docs/plans") || fsys.Exists("docs") || fsys.Exists("missing.md") {
		t.Error("Exists reported a path that is not a regular file")
	}
	if data, err := fsys.ReadFile("docs/design/live.md"); err != nil || string(data) != "# live\n" {
		t.Errorf("ReadFile(docs/design/live.md) = %q, %v", data, err)
	}

	wantHistorical := []string{"docs/design/retired.md"}
	got, err := Historical(fsys)
	if err != nil {
		t.Fatalf("Historical(): %v", err)
	}
	if !reflect.DeepEqual(got, wantHistorical) {
		t.Errorf("Historical() = %q, want %q", got, wantHistorical)
	}

	wantGo := []string{"internal/pkg/a.go"}
	if got, err := Go(fsys); err != nil || !reflect.DeepEqual(got, wantGo) {
		t.Errorf("Go() = %q, %v; want %q", got, err, wantGo)
	}

	wantAgentFacing := []string{"AGENTS.md", "plugins/p/plugin.md"}
	if got, err := AgentFacing(fsys); err != nil || !reflect.DeepEqual(got, wantAgentFacing) {
		t.Errorf("AgentFacing() = %q, %v; want %q", got, err, wantAgentFacing)
	}

	// The walker reports repo-relative slash paths, never the root prefix.
	for _, dir := range []string{"docs", "plugins", "internal", "cmd"} {
		files, err := fsys.WalkFiles(dir)
		if err != nil {
			t.Fatalf("WalkFiles(%q): %v", dir, err)
		}
		for _, p := range files {
			if strings.HasPrefix(p, root) || strings.HasPrefix(p, "./") || strings.Contains(p, "\\") {
				t.Errorf("WalkFiles(%q) returned %q, want a clean repo-relative slash path", dir, p)
			}
		}
	}

	// A root that does not exist is an empty tier, not a failure: a docs-only
	// checkout has no cmd/ directory.
	missing, err := fsys.WalkFiles("cmd")
	if err != nil || len(missing) != 0 {
		t.Errorf("WalkFiles(\"cmd\") on a tree without one = %q, %v; want empty, nil", missing, err)
	}
	if got, err := Go(fsys); err != nil || !reflect.DeepEqual(got, wantGo) {
		t.Errorf("Go() with no cmd/ = %q, %v; want %q", got, err, wantGo)
	}
}

// TestIntegrationWalkFilesReportsAnUnreadableSubtree pins the fail-closed walk:
// a subtree the process may not read fails the tier, where the script's
// `find ... 2>/dev/null` listed the rest and reported a clean slice.
func TestIntegrationWalkFilesReportsAnUnreadableSubtree(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory, so the walk cannot fail")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "docs", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "x.md"), []byte("# x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Restored before t.TempDir's own cleanup runs, which could not read it.
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	fsys := OSFiles{Root: root}
	if got, err := fsys.WalkFiles("docs"); err == nil {
		t.Fatalf("WalkFiles over an unreadable subtree = %q, nil; want an error", got)
	}
	if got, err := Reference(fsys); err == nil {
		t.Errorf("Reference() over an unreadable subtree = %q, nil; want an error", got)
	}
}
