//go:build integration

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goListTimeout = 60 * time.Second // a hung go list fails the test rather than waiting

// TestIntegrationSandboxCommonWorkSharesTheCacheKey shows that Go names a
// build's files from $PWD, so two seat directories entered through one link
// share a cache key and the same two by their real paths do not.
func TestIntegrationSandboxCommonWorkSharesTheCacheKey(t *testing.T) {
	t.Parallel()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go command is not on PATH (%v): this test builds a throwaway module with it", err)
	}
	root := t.TempDir()
	cache := filepath.Join(root, "gocache")
	first, second := writeOnePackageModule(t, root, "first"), writeOnePackageModule(t, root, "second")
	link := filepath.Join(root, "work")
	if err := os.Symlink(first, link); err != nil {
		t.Fatalf("linking %s to %s: %v", link, first, err)
	}
	viaFirst := goListExport(t, goTool, cache, link)
	if err := os.Remove(link); err != nil {
		t.Fatalf("removing %s: %v", link, err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatalf("relinking %s to %s: %v", link, second, err)
	}
	viaSecond := goListExport(t, goTool, cache, link)
	if viaFirst != viaSecond {
		t.Fatalf("the two directories entered through %s gave different export paths (%s and %s): the working directory is back in the cache key", link, viaFirst, viaSecond)
	}
	if realFirst, realSecond := goListExport(t, goTool, cache, first), goListExport(t, goTool, cache, second); realFirst == realSecond {
		t.Fatalf("both real directories gave the export path %s: the test cannot show that the working directory decides the key", realFirst)
	}
}

// writeOnePackageModule writes a module with no imports under root; both calls write the same bytes, so only their paths differ.
func writeOnePackageModule(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/throwaway\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg.go"), []byte("package throwaway\n\nfunc Value() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("writing pkg.go: %v", err)
	}
	return dir
}

// goListExport runs go list -export in dir and returns the printed export path,
// whose file name is the cache key. PWD is dir: os.Getwd prefers it to the real one.
func goListExport(t *testing.T, goTool, cache, dir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), goListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool, "list", "-export", "-f", "{{.Export}}", "./...")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PWD=" + dir, "GOCACHE=" + cache,
		"GOPROXY=off", "GOFLAGS=", "GOENV=off", "GOTOOLCHAIN=local"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -export in %s: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}
