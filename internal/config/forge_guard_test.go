package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forgeOwnerDataFiles are the data files that name the Forgejo owner once, as
// ForgeOwnerPlaceholder, relative to the repository root (gt-ucigk3).
var forgeOwnerDataFiles = []string{
	"deploy/daemon/image-pins",
	"deploy/polecat/seed-urls",
	"plugins/app-promote/rigs.conf",
}

// literalForgeOwner matches DefaultForgeOwner as a whole name: a URL path
// segment, a repository owner or a Go string, but not the prefix of a longer
// name such as the fork module path, which is another account.
var literalForgeOwner = regexp.MustCompile(`(^|[^A-Za-z0-9._-])` + regexp.QuoteMeta(DefaultForgeOwner) + `([^A-Za-z0-9._-]|$)`)

// TestNoLiteralForgeOwnerOutsideTheSetting: neither the data files that name
// the owner as {owner} nor Go code under cmd/ spells the owner literally. The
// one allowed literal is the setting's default, DefaultForgeOwner (gt-ucigk3).
func TestNoLiteralForgeOwnerOutsideTheSetting(t *testing.T) {
	t.Parallel()
	hits, err := forgeOwnerLiteralHits("../..", forgeOwnerDataFiles)
	if err != nil {
		t.Fatalf("scanning the tree: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("these name the forge owner literally; write %s and let the reader expand it:\n%s",
			ForgeOwnerPlaceholder, strings.Join(hits, "\n"))
	}
}

// TestForgeOwnerGuardWalkReportsAPlantedLiteral drives the scan on a temp tree,
// so the real-tree run above cannot pass for the wrong reason: a literal in a
// non-test Go file under cmd/ and in a data file is reported, named by its path
// relative to the root, while the same literal in a _test.go file and in a
// Markdown file is ignored (gt-ucigk3).
func TestForgeOwnerGuardWalkReportsAPlantedLiteral(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	literal := "bd http://forgejo:3000/" + DefaultForgeOwner + "/beads.git 0123456789abcdef0123456789abcdef01234567\n"
	files := map[string]string{
		"cmd/x/main.go":                 "package main\n\nconst owner = \"" + DefaultForgeOwner + "\"\n",
		"cmd/x/main_test.go":            "package main\n\nconst owner = \"" + DefaultForgeOwner + "\"\n",
		"cmd/x/README.md":               "The owner is " + DefaultForgeOwner + ".\n",
		"deploy/daemon/image-pins":      literal,
		"deploy/polecat/seed-urls":      "devops http://forgejo:3000/{owner}/devops.git\n",
		"plugins/app-promote/rigs.conf": "fractals {owner}/fractals-nextjs\n",
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := forgeOwnerLiteralHits(root, forgeOwnerDataFiles)
	if err != nil {
		t.Fatalf("forgeOwnerLiteralHits: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (cmd/x/main.go and the pins file): %v", len(hits), hits)
	}
	for _, want := range []string{"cmd/x/main.go:3:", "deploy/daemon/image-pins:1:"} {
		if !containsPrefixed(hits, want) {
			t.Errorf("no hit names %s: %v", want, hits)
		}
	}
	for _, hit := range hits {
		if strings.Contains(hit, root) {
			t.Errorf("hit %q names the absolute path, want it relative to the root", hit)
		}
	}
}

// TestLiteralOwnerHitsReadsEachForm drives the scan with fixtures, so the
// real-tree run above cannot pass for the wrong reason: a URL, a repository
// owner and a Go string literal each count, and a longer name, a placeholder
// and the bare word do not.
func TestLiteralOwnerHitsReadsEachForm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"a forge URL", "bd http://forgejo:3000/" + DefaultForgeOwner + "/beads.git " + "0123456789abcdef0123456789abcdef01234567", true},
		{"a repository owner", "fractals " + DefaultForgeOwner + "/fractals-nextjs", true},
		{"a Go string literal", `const owner = "` + DefaultForgeOwner + `"`, true},
		{"a longer name", "fork " + DefaultForgeOwner + "ahrens/beads", false},
		{"a hyphenated longer name", DefaultForgeOwner + "-answers", false},
		{"the placeholder", "bd http://forgejo:3000/" + ForgeOwnerPlaceholder + "/beads.git", false},
		{"the setting's name", "forge.owner", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := literalOwnerHits("fixture", tt.line)
			if (len(got) == 1) != tt.want {
				t.Errorf("literalOwnerHits(%q) = %v, want a hit: %v", tt.line, got, tt.want)
			}
		})
	}
}

// forgeOwnerLiteralHits returns one line per owner literal in the data files
// (relative to root) and in the non-test Go files under root/cmd. Each hit names
// its file relative to root, so the operator can find it.
func forgeOwnerLiteralHits(root string, dataFiles []string) ([]string, error) {
	var hits []string
	for _, rel := range dataFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", rel, err)
		}
		hits = append(hits, literalOwnerHits(rel, string(data))...)
	}
	err := filepath.WalkDir(filepath.Join(root, "cmd"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hits = append(hits, literalOwnerHits(filepath.ToSlash(rel), string(data))...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking cmd: %w", err)
	}
	return hits, nil
}

// containsPrefixed reports whether some line of lines starts with prefix.
func containsPrefixed(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// literalOwnerHits returns one line per owner literal in data, naming the file
// and line so the operator can find it.
func literalOwnerHits(path, data string) []string {
	var hits []string
	for i, line := range strings.Split(data, "\n") {
		if literalForgeOwner.MatchString(line) {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
		}
	}
	return hits
}
