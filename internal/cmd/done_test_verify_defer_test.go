package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// addContainerBackedPackage adds a tiny passing package at internal/cmd to
// the verify test module. Its import path ends in "/internal/cmd", which is
// on containerSuitePackages, so the gate must treat it as container-backed
// even though this stand-in spins nothing.
func addContainerBackedPackage(t *testing.T, dir string) {
	t.Helper()
	pkg := filepath.Join(dir, "internal", "cmd")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "cmd.go"), []byte("package cmd\n\nfunc Name() string { return \"cmd\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "cmd_test.go"), []byte("package cmd\n\nimport \"testing\"\n\nfunc TestName(t *testing.T) {\n\tif Name() != \"cmd\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func changePkga(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pkga", "a.go"), []byte("package pkga\n\nfunc Add(a, b int) int { return a + b }\nfunc Triple(a int) int { return a * 3 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsContainerSuitePackage(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"github.com/steveyegge/gastown/internal/cmd":       true,
		"github.com/steveyegge/gastown/internal/beads":     true,
		"internal/refinery":                                true,
		"github.com/steveyegge/gastown/internal/slot":      false,
		"github.com/steveyegge/gastown/internal/cmd/extra": true, // sub-packages inherit the container scope
		"example.test/pkga":                                false,
	} {
		if got := isContainerSuitePackage(path); got != want {
			t.Errorf("isContainerSuitePackage(%q) = %v, want %v", path, got, want)
		}
	}
}
