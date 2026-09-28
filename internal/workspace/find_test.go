package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("realpath: %v", err)
	}
	return real
}

func TestFindWithPrimaryMarker(t *testing.T) {
	t.Parallel()
	// Create temp workspace structure
	root := realPath(t, t.TempDir())
	mayorDir := filepath.Join(root, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	townFile := filepath.Join(mayorDir, "town.json")
	if err := os.WriteFile(townFile, []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Create nested directory
	nested := filepath.Join(root, "some", "deep", "path")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	// Find from nested should return root
	found, err := Find(nested)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found != root {
		t.Errorf("Find = %q, want %q", found, root)
	}
}

func TestFindWithSecondaryMarker(t *testing.T) {
	t.Parallel()
	// Create temp workspace with just mayor/ directory
	root := realPath(t, t.TempDir())
	mayorDir := filepath.Join(root, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create nested directory
	nested := filepath.Join(root, "rigs", "test")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	// Find from nested should return root
	found, err := Find(nested)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found != root {
		t.Errorf("Find = %q, want %q", found, root)
	}
}

func TestFindNotFound(t *testing.T) {
	t.Parallel()
	// Create temp dir with no markers
	dir := t.TempDir()

	found, err := Find(dir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found != "" {
		t.Errorf("Find = %q, want empty string", found)
	}
}

func TestFindOrErrorNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := FindOrError(dir)
	if err != ErrNotFound {
		t.Errorf("FindOrError = %v, want ErrNotFound", err)
	}
}

func TestFindAtRoot(t *testing.T) {
	t.Parallel()
	// Create workspace at temp root level
	root := realPath(t, t.TempDir())
	mayorDir := filepath.Join(root, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	townFile := filepath.Join(mayorDir, "town.json")
	if err := os.WriteFile(townFile, []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Find from root should return root
	found, err := Find(root)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found != root {
		t.Errorf("Find = %q, want %q", found, root)
	}
}

func TestIsWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Not a workspace initially
	is, err := IsWorkspace(root)
	if err != nil {
		t.Fatalf("IsWorkspace: %v", err)
	}
	if is {
		t.Error("expected not a workspace initially")
	}

	// Add primary marker (mayor/town.json)
	mayorDir := filepath.Join(root, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	townFile := filepath.Join(mayorDir, "town.json")
	if err := os.WriteFile(townFile, []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Now is a workspace
	is, err = IsWorkspace(root)
	if err != nil {
		t.Fatalf("IsWorkspace: %v", err)
	}
	if !is {
		t.Error("expected to be a workspace")
	}
}

func TestFindFromSymlinkedDir(t *testing.T) {
	t.Parallel()
	root := realPath(t, t.TempDir())
	mayorDir := filepath.Join(root, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	townFile := filepath.Join(mayorDir, "town.json")
	if err := os.WriteFile(townFile, []byte(`{"type":"town"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	linkTarget := filepath.Join(root, "actual")
	if err := os.MkdirAll(linkTarget, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	linkName := filepath.Join(root, "linked")
	if err := os.Symlink(linkTarget, linkName); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	found, err := Find(linkName)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found != root {
		t.Errorf("Find = %q, want %q", found, root)
	}
}

func TestFindPreservesSymlinkPath(t *testing.T) {
	t.Parallel()
	realRoot := t.TempDir()
	resolved, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	symRoot := filepath.Join(t.TempDir(), "symlink-workspace")
	if err := os.Symlink(resolved, symRoot); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	mayorDir := filepath.Join(symRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	townFile := filepath.Join(mayorDir, "town.json")
	if err := os.WriteFile(townFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	subdir := filepath.Join(symRoot, "rigs", "project", "polecats", "worker")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	townRoot, err := Find(subdir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if townRoot != symRoot {
		t.Errorf("Find returned %q, want %q (symlink path preserved)", townRoot, symRoot)
	}

	relPath, err := filepath.Rel(townRoot, subdir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}

	if filepath.ToSlash(relPath) != "rigs/project/polecats/worker" {
		t.Errorf("Rel = %q, want 'rigs/project/polecats/worker'", relPath)
	}
}

func TestFindSkipsNestedWorkspaceInWorktree(t *testing.T) {
	t.Parallel()
	root := realPath(t, t.TempDir())

	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(`{"name":"outer"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	polecatDir := filepath.Join(root, "myrig", "polecats", "worker")
	if err := os.MkdirAll(filepath.Join(polecatDir, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(polecatDir, "mayor", "town.json"), []byte(`{"name":"inner"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	found, err := Find(polecatDir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if found != root {
		t.Errorf("Find = %q, want %q (should skip nested workspace in polecats/)", found, root)
	}

	rel, _ := filepath.Rel(found, polecatDir)
	if filepath.ToSlash(rel) != "myrig/polecats/worker" {
		t.Errorf("Rel = %q, want 'myrig/polecats/worker'", rel)
	}
}

func TestFindSkipsNestedWorkspaceInCrew(t *testing.T) {
	t.Parallel()
	root := realPath(t, t.TempDir())

	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(`{"name":"outer"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	crewDir := filepath.Join(root, "myrig", "crew", "worker")
	if err := os.MkdirAll(filepath.Join(crewDir, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(crewDir, "mayor", "town.json"), []byte(`{"name":"inner"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	found, err := Find(crewDir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if found != root {
		t.Errorf("Find = %q, want %q (should skip nested workspace in crew/)", found, root)
	}
}

// cwdIn is a process whose working directory is dir, with environment kv.
func cwdIn(dir string, kv map[string]string) procEnv {
	e := envWith(kv)
	e.getwd = func() (string, error) { return dir, nil }
	return e
}

func TestFindFromCwd(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	inner := filepath.Join(town, "gastown", "crew", "max")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}

	if root, err := cwdIn(inner, nil).findFromCwd(); err != nil || root != town {
		t.Errorf("findFromCwd = %q, %v; want %q", root, err, town)
	}
	if _, err := envWith(nil).findFromCwd(); err == nil || !strings.Contains(err.Error(), "getting current directory") {
		t.Errorf("findFromCwd without a cwd = %v, want a getwd error", err)
	}
}

func TestFindFromCwdOrError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	other := t.TempDir()
	notTown := t.TempDir()

	cases := []struct {
		name string
		e    procEnv
		want string
		err  string
	}{
		{"cwd inside a town", cwdIn(town, nil), town, ""},
		{"cwd outside, GT_TOWN_ROOT", cwdIn(other, map[string]string{"GT_TOWN_ROOT": town}), town, ""},
		{"cwd outside, GT_ROOT", cwdIn(other, map[string]string{"GT_ROOT": town}), town, ""},
		{"GT_TOWN_ROOT not a workspace falls through to GT_ROOT", cwdIn(other, map[string]string{"GT_TOWN_ROOT": notTown, "GT_ROOT": town}), town, ""},
		{"env names a non-workspace", cwdIn(other, map[string]string{"GT_TOWN_ROOT": notTown}), "", ErrNotFound.Error()},
		{"no cwd, env fallback", envWith(map[string]string{"GT_TOWN_ROOT": town}), town, ""},
		{"no cwd, no env", envWith(nil), "", "getting current directory"},
		{"env names the forbidden town", cwdIn(other, map[string]string{"GT_TOWN_ROOT": town, EnvForbiddenTownRoot: town}), "", ErrNotFound.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.e.findFromCwdOrError()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("findFromCwdOrError = %q, %v; want error containing %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("findFromCwdOrError = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestFindFromCwdWithFallback(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	inner := filepath.Join(town, "gastown")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()

	if root, cwd, err := cwdIn(inner, nil).findFromCwdWithFallback(); err != nil || root != town || cwd != inner {
		t.Errorf("with cwd = (%q, %q, %v), want (%q, %q, nil)", root, cwd, err, town, inner)
	}
	if _, _, err := cwdIn(other, nil).findFromCwdWithFallback(); !errors.Is(err, ErrNotFound) {
		t.Errorf("cwd outside any town: err = %v, want ErrNotFound", err)
	}
	// A deleted cwd (a nuked polecat worktree) falls back to GT_TOWN_ROOT.
	if root, cwd, err := envWith(map[string]string{"GT_TOWN_ROOT": town}).findFromCwdWithFallback(); err != nil || root != town || cwd != "" {
		t.Errorf("no cwd, GT_TOWN_ROOT = (%q, %q, %v), want (%q, \"\", nil)", root, cwd, err, town)
	}
	// ...but only to a real workspace.
	if _, _, err := envWith(map[string]string{"GT_TOWN_ROOT": other}).findFromCwdWithFallback(); err == nil {
		t.Error("no cwd, GT_TOWN_ROOT not a workspace: err = nil, want the getwd error")
	}
	if _, _, err := envWith(nil).findFromCwdWithFallback(); err == nil || !strings.Contains(err.Error(), "getting current directory") {
		t.Errorf("no cwd, no env: err = %v, want the getwd error", err)
	}
}

func TestGetTownName(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	makeTown(t, town)
	name, err := GetTownName(town)
	if err != nil || name != "x" {
		t.Errorf("GetTownName = %q, %v; want \"x\", nil", name, err)
	}
	if got := MustGetTownName(town); got != "x" {
		t.Errorf("MustGetTownName = %q, want x", got)
	}
	if _, err := GetTownName(t.TempDir()); err == nil {
		t.Error("GetTownName on a dir with no town.json: err = nil")
	}
	defer func() {
		if recover() == nil {
			t.Error("MustGetTownName on a dir with no town.json did not panic")
		}
	}()
	MustGetTownName(t.TempDir())
}
