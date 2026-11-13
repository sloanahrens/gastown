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
// ForgeOwnerPlaceholder, relative to this package (gt-ucigk3).
var forgeOwnerDataFiles = []string{
	"../../deploy/daemon/image-pins",
	"../../deploy/polecat/seed-urls",
	"../../plugins/app-promote/rigs.conf",
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
	var hits []string
	for _, file := range forgeOwnerDataFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		hits = append(hits, literalOwnerHits(file, string(data))...)
	}
	err := filepath.WalkDir("../../cmd", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hits = append(hits, literalOwnerHits(filepath.FromSlash(path), string(data))...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking cmd: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("these name the forge owner literally; write %s and let the reader expand it:\n%s",
			ForgeOwnerPlaceholder, strings.Join(hits, "\n"))
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
