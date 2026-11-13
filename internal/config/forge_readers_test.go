package config

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ownerDataFileNames are the names of the data files that carry the owner
// placeholder, matched wherever a file names them (gt-ucigk3).
var ownerDataFileNames = []string{"image-pins", "seed-urls", "rigs.conf"}

// knownOwnerDataFileNamers lists every file outside tests and docs that names a
// data file, with what it does with it. Each reader expands the placeholder
// before it parses; a file that only passes the path or names it in a comment is
// listed too. A new file that names a data file fails the test until it is
// listed here, so a new reader that skips expansion is a decision, not a surprise
// (gt-ucigk3).
var knownOwnerDataFileNamers = map[string]string{
	"Makefile":                                    "comment: the bd pin's commit",
	".forgejo/workflows/images.yml":               "passes the path to the Go build commands",
	"deploy/ci/images.workflow.yml":               "passes the path to the Go build commands",
	"cmd/forgejo-sources-secret/main.go":          "comment: the URL names the two files hold",
	"internal/ci/preflight/preflight.go":          "comment: the pins' host",
	"internal/cmd/daemon_image.go":                "reader: readImagePins expands image-pins",
	"internal/cmd/daemon_image_pins.go":           "reader: readImagePinsFile expands image-pins, and the refresh writes back the unexpanded text",
	"internal/daemonimage/build.go":               "comment: the pins file Plan names",
	"internal/daemonimage/pins.go":                "reader: ParsePins, and PinsPath",
	"internal/polecat/imagectx/imagectx.go":       "reader: ParseSeedURLs expands and refuses the placeholder",
	"internal/polecat/imagectx/cmd/build/main.go": "reader: readFile expands image-pins and seed-urls",
	"plugins/app-promote/main.go":                 "reader: promoteAll expands rigs.conf",
}

// TestOwnerPlaceholderFilesHaveOnlyKnownReaders: the files that name a data file
// are exactly the ones knownOwnerDataFileNamers lists. A file it does not list
// fails the test; a listed file that no longer names a data file fails it too, so
// the list stays the truth (gt-ucigk3).
func TestOwnerPlaceholderFilesHaveOnlyKnownReaders(t *testing.T) {
	t.Parallel()
	named, err := filesNamingOwnerDataFiles("../..")
	if err != nil {
		t.Fatalf("scanning the tree: %v", err)
	}
	unlisted, stale := diffOwnerNamers(named, knownOwnerDataFileNamers)
	if len(unlisted) != 0 {
		t.Errorf("these name a data file and are not in knownOwnerDataFileNamers; expand the placeholder in their reader and list them with the reason:\n%s",
			strings.Join(unlisted, "\n"))
	}
	if len(stale) != 0 {
		t.Errorf("these are listed in knownOwnerDataFileNamers but no longer name a data file:\n%s",
			strings.Join(stale, "\n"))
	}
}

// diffOwnerNamers compares the files the scan found with the listed ones: unlisted
// names a data file and is not listed, stale is listed and no longer names one.
// Both come back sorted. It is the guard's decision, apart from the walk, so a test
// can drive both failing branches with plain values.
func diffOwnerNamers(named []string, known map[string]string) (unlisted, stale []string) {
	isNamer := make(map[string]bool, len(named))
	for _, rel := range named {
		isNamer[rel] = true
		if _, ok := known[rel]; !ok {
			unlisted = append(unlisted, rel)
		}
	}
	for rel := range known {
		if !isNamer[rel] {
			stale = append(stale, rel)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	return unlisted, stale
}

// The guard's failing branches: a file that names a data file and is not listed
// is reported, and so is a listed file that no longer names one. A matching pair
// of lists reports nothing (gt-ucigk3).
func TestDiffOwnerNamersReportsUnlistedAndStale(t *testing.T) {
	t.Parallel()
	known := map[string]string{"a.go": "reader", "b.go": "reader", "c.go": "comment"}
	tests := []struct {
		name         string
		named        []string
		wantUnlisted []string
		wantStale    []string
	}{
		{name: "matching lists", named: []string{"a.go", "b.go", "c.go"}},
		{name: "a new reader is unlisted", named: []string{"a.go", "b.go", "c.go", "new/reader.go"}, wantUnlisted: []string{"new/reader.go"}},
		{name: "a listed file stopped naming a data file", named: []string{"a.go", "b.go"}, wantStale: []string{"c.go"}},
		{name: "both at once, sorted", named: []string{"z.go", "a.go", "y.go"}, wantUnlisted: []string{"y.go", "z.go"}, wantStale: []string{"b.go", "c.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			unlisted, stale := diffOwnerNamers(tt.named, known)
			if strings.Join(unlisted, ",") != strings.Join(tt.wantUnlisted, ",") {
				t.Errorf("unlisted = %v, want %v", unlisted, tt.wantUnlisted)
			}
			if strings.Join(stale, ",") != strings.Join(tt.wantStale, ",") {
				t.Errorf("stale = %v, want %v", stale, tt.wantStale)
			}
		})
	}
}

// The scan itself, on a tree the test builds: a code file and a build script that
// name a data file are found, and a test file, a Markdown file, a file under docs,
// a binary and a file that names none are not. The live tree is not read.
func TestFilesNamingOwnerDataFilesWalksATempTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel string, data []byte) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cmd/reader.go", []byte("var p = \"deploy/daemon/image-pins\"\n"))
	write("scripts/build.sh", []byte("cat deploy/polecat/seed-urls\n"))
	write("cmd/reader_test.go", []byte("image-pins\n"))
	write("README.md", []byte("image-pins\n"))
	write("docs/design.txt", []byte("image-pins\n"))
	write("bin/tool", []byte("image-pins\x00binary"))
	write("cmd/other.go", []byte("package cmd\n"))

	got, err := filesNamingOwnerDataFiles(root)
	if err != nil {
		t.Fatalf("filesNamingOwnerDataFiles: %v", err)
	}
	if want := "cmd/reader.go,scripts/build.sh"; strings.Join(got, ",") != want {
		t.Errorf("files naming a data file = %v, want %s", got, want)
	}
}

// filesNamingOwnerDataFiles returns, sorted and relative to root, every regular
// file under root that names a data file in ownerDataFileNames. Tests, Markdown
// and documentation are skipped: they describe the files, they do not read them.
// Binaries are skipped too, since a build output embeds strings from its sources.
func filesNamingOwnerDataFiles(root string) ([]string, error) {
	var named []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "docs", "scratch", ".beads", ".runtime", ".claude", "node_modules":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			// A built binary carries strings from its sources; it reads no data file.
			return nil
		}
		for _, dataName := range ownerDataFileNames {
			if strings.Contains(string(data), dataName) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				named = append(named, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	sort.Strings(named)
	return named, err
}
