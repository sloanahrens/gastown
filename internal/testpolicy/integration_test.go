package testpolicy

import (
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestIntegrationPackages pins the selection the gate's integration vet runs
// on: a package the tag adds files to is selected, one it does not is not, and
// the directories the Go tool ignores are not walked (gt-w3aby).
func TestIntegrationPackages(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.invalid/root\n\ngo 1.21\n")
	write("tagged/a.go", "//go:build integration\n\npackage tagged\n")
	write("taggedtest/x_test.go", "//go:build integration\n\npackage taggedtest\n")
	write("plain/b.go", "package plain\n")
	write("negated/c.go", "//go:build !integration\n\npackage negated\n")
	// On this host the file builds either way, so the unit tier already
	// compiles it.
	write("alsoplain/d.go", "//go:build integration || "+build.Default.GOOS+"\n\npackage alsoplain\n")
	write("testdata/skip/e.go", "//go:build integration\n\npackage skip\n")
	write("vendor/skip/f.go", "//go:build integration\n\npackage skip\n")
	write("_hidden/g.go", "//go:build integration\n\npackage hidden\n")
	write(".hidden/h.go", "//go:build integration\n\npackage hidden\n")
	write("nested/go.mod", "module example.invalid/nested\n\ngo 1.21\n")
	write("nested/integrated/i.go", "//go:build integration\n\npackage integrated\n")
	write("generated/skipped/j.go", "//go:build integration\n\npackage skipped\n")

	got, err := IntegrationPackages(root, "generated")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nested/integrated", "tagged", "taggedtest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IntegrationPackages = %v, want %v", got, want)
	}
}
