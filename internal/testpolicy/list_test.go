package testpolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A nested module (its own go.mod below root) is not part of the root
// module's packages: its tests cannot import the root module's harness, so the
// root policies (unit-tier TestMain, allowed tools, docker lists) do not
// judge it. Each nested module carries its own rules.
func TestPackageDirsSkipsNestedModules(t *testing.T) {
	t.Parallel() // unit tier: a temp tree of a few files
	root := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod":              "module example.com/root\n",
		"a/a.go":              "package a\n",
		"tracker/go.mod":      "module example.com/root/tracker\n",
		"tracker/t.go":        "package tracker\n",
		"tracker/store/s.go":  "package store\n",
		"plugins/x/go.mod":    "module example.com/x\n",
		"plugins/x/y/y.go":    "package y\n",
		"plugins/shared/z.go": "package shared\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := PackageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, d := range got {
		r, _ := filepath.Rel(root, d)
		rel = append(rel, filepath.ToSlash(r))
	}
	want := []string{"a", "plugins/shared"}
	if !reflect.DeepEqual(rel, want) {
		t.Errorf("PackageDirs = %v, want %v: directories of a nested module belong to that module", rel, want)
	}
}
