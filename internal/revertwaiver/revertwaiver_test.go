package revertwaiver

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestRevertedPathsNamesOnlyAtAPathBoundary (gt-j5q4i): a reverted path counts
// as named only when the description carries it as a whole path. A longer path
// ending in it does not name it, so README.md and go.mod stay unnamed while the
// path the description really writes — closed by a sentence period — is waived.
func TestRevertedPathsNamesOnlyAtAPathBoundary(t *testing.T) {
	t.Parallel()
	reverted := []git.RevertedMerge{{Commit: "aaaaaaaaaaaa", Paths: []string{
		"README.md", "go.mod", "internal/x.go", "docs/README.md",
	}}}
	all, unnamed := Paths(reverted, "Delete docs/README.md and internal/go.mod, then internal/x.go.")

	wantAll := []string{"README.md", "go.mod", "internal/x.go", "docs/README.md"}
	if !reflect.DeepEqual(all, wantAll) {
		t.Errorf("all = %q, want %q", all, wantAll)
	}
	wantUnnamed := []string{"README.md", "go.mod"}
	if !reflect.DeepEqual(unnamed, wantUnnamed) {
		t.Errorf("unnamed = %q, want %q", unnamed, wantUnnamed)
	}
}

// TestNamedInDescriptionBoundaries (gt-j5q4i): the boundary rule, case by case.
// A path character on either side disqualifies the match, a sentence's trailing
// period or comma does not, and an occurrence inside a longer path does not
// hide a later occurrence that stands on its own.
func TestNamedInDescriptionBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		description string
		path        string
		want        bool
	}{
		{"longer directory path does not name it", "Delete docs/README.md.", "README.md", false},
		{"longer directory path does not name go.mod", "Delete internal/go.mod.", "go.mod", false},
		{"extension suffix does not name it", "Keep internal/x.go.bak.", "internal/x.go", false},
		{"prefixed path does not name it", "Keep xinternal/x.go.", "internal/x.go", false},
		{"backticks", "Delete `internal/x.go` now.", "internal/x.go", true},
		{"double quotes", `Delete "internal/x.go" now.`, "internal/x.go", true},
		{"parentheses", "Delete (internal/x.go) now.", "internal/x.go", true},
		{"after a space", "Delete internal/x.go now.", "internal/x.go", true},
		{"start of the text", "internal/x.go is gone.", "internal/x.go", true},
		{"start of a line", "Unrelated.\ninternal/x.go is gone.", "internal/x.go", true},
		{"before a sentence period", "Delete internal/x.go.", "internal/x.go", true},
		{"before a sentence comma", "Delete internal/x.go, which we replaced.", "internal/x.go", true},
		{"standalone later occurrence", "Delete docs/README.md; README.md is unrelated.", "README.md", true},
		{"empty path names nothing", "Delete internal/x.go.", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NamedInDescription(tc.description, tc.path); got != tc.want {
				t.Errorf("NamedInDescription(%q, %q) = %v, want %v", tc.description, tc.path, got, tc.want)
			}
		})
	}
}

// TestNamedInDescriptionDirectories (gt-vpjtk, gt-vjdkl): a directory of at
// least two components written with its trailing slash names every file under
// it, at any depth, on a path boundary only. A directory without the slash, a
// single top-level directory, or a sibling file, names nothing under it.
func TestNamedInDescriptionDirectories(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		description string
		path        string
		want        bool
	}{
		{"directory names a file under it", "Delete internal/foo/ entirely.", "internal/foo/a.go", true},
		{"directory names a nested file", "Delete internal/foo/ entirely.", "internal/foo/sub/deep/a.go", true},
		{"directory in backticks", "Delete `internal/foo/` entirely.", "internal/foo/a.go", true},
		{"directory before a sentence period", "Delete internal/foo/.", "internal/foo/a.go", true},
		{"directory before a sentence comma", "Delete internal/foo/, then rebuild.", "internal/foo/a.go", true},
		{"two-component directory at the start of the text", "internal/foo/ goes.", "internal/foo/a.go", true},
		{"top-level directory at the start of the text names nothing", "docs/ goes.", "docs/x.go", false},
		{"top-level directory in backticks names nothing", "Rework `internal/` later.", "internal/done/x.go", false},
		{"top-level directory before a period names nothing", "Retire docs/.", "docs/a/b.go", false},
		{"nested directory names its own files", "Delete internal/foo/sub/.", "internal/foo/sub/a.go", true},
		{"nested directory does not name the parent's files", "Delete internal/foo/sub/.", "internal/foo/a.go", false},
		{"prefixed directory does not name it", "Delete mydocs/ now.", "docs/x.go", false},
		{"directory under another does not name a top-level one", "Delete sub/docs/ now.", "docs/x.go", false},
		{"top-level directory does not name a nested one", "Delete docs/ now.", "sub/docs/x.go", false},
		{"top-level directory does not name a prefixed one", "Delete docs/ now.", "mydocs/x.go", false},
		{"a two-component directory does not name a sibling top-level one", "Delete internal/foo/.", "internal/x.go", false},
		{"directory without a slash names nothing under it", "Delete internal/foo entirely.", "internal/foo/a.go", false},
		{"directory without a slash before a period names nothing", "Delete internal/foo.", "internal/foo/a.go", false},
		{"a file under the directory does not name its siblings", "Delete internal/foo/a.go.", "internal/foo/b.go", false},
		{"a file still has to be named whole", "Delete internal/foo/a.go.bak.", "internal/foo/a.go", false},
		{"a longer directory name does not name it", "Delete internal/foo/bar/.", "internal/foo/a.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NamedInDescription(tc.description, tc.path); got != tc.want {
				t.Errorf("NamedInDescription(%q, %q) = %v, want %v", tc.description, tc.path, got, tc.want)
			}
		})
	}
}
