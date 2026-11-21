package testpolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// The guard tests build root lexically (filepath.Abs of "../.."), so a checkout
// reached through a symlink hands PackageDirs a symlink. filepath.WalkDir does
// not follow a symlinked root, which once made the tree look empty with a nil
// error and failed eight guard tests with unrelated messages (gt-91d613).
func TestPackageDirsFollowsASymlinkedRoot(t *testing.T) {
	t.Parallel() // unit tier: a temp tree of one file and a symlink
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a", "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := PackageDirs(link)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(link, "a")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PackageDirs(symlinked root) = %v, want %v: the root must be followed and the paths spelled under the root given", got, want)
	}
}

func TestPackageDirsRefusesAMissingRoot(t *testing.T) {
	t.Parallel() // unit tier: no files
	missing := filepath.Join(t.TempDir(), "no-such-root")
	got, err := PackageDirs(missing)
	if err == nil {
		t.Fatalf("PackageDirs(%s) = %v with a nil error, want an error naming the root", missing, got)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error %q does not name the root %s", err, missing)
	}
}

func TestPackageDirsRefusesARootThatIsNotADirectory(t *testing.T) {
	t.Parallel() // unit tier: one file
	file := filepath.Join(t.TempDir(), "root.go")
	if err := os.WriteFile(file, []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := PackageDirs(file)
	if err == nil {
		t.Fatalf("PackageDirs(%s) = %v with a nil error, want an error: a file is not a root", file, got)
	}
	if !strings.Contains(err.Error(), file) {
		t.Errorf("the error %q does not name the root %s", err, file)
	}
}
