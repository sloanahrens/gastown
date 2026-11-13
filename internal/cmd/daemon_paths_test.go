package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPathWithin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, root string
		want       bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/x/y", "/a/b", false},
		{"/a/b/../c", "/a/b", false},
	} {
		if got := pathWithin(tc.path, tc.root); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

// resolveThroughExisting resolves the nearest existing ancestor and appends the
// rest, and returns an error that is not "does not exist" as it is.
func TestResolveThroughExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	got, err := resolveThroughExisting(filepath.Join(link, "a", "b.yml"), filepath.EvalSymlinks)
	if err != nil || got != filepath.Join(resolvedReal, "a", "b.yml") {
		t.Errorf("resolveThroughExisting = %q, %v; want the link resolved and the absent tail appended", got, err)
	}
	boom := errors.New("permission denied")
	if _, err := resolveThroughExisting("/x/y", func(string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("a non-ENOENT error = %v, want it returned", err)
	}
	if _, err := resolveThroughExisting("/x/y", func(string) (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Error("a path with no existing ancestor, not even the root, resolved")
	}
}
