package config

import (
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
	isNamer := make(map[string]bool, len(named))
	var unlisted, stale []string
	for _, rel := range named {
		isNamer[rel] = true
		if _, ok := knownOwnerDataFileNamers[rel]; !ok {
			unlisted = append(unlisted, rel)
		}
	}
	for rel := range knownOwnerDataFileNamers {
		if !isNamer[rel] {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	if len(unlisted) != 0 {
		t.Errorf("these name a data file and are not in knownOwnerDataFileNamers; expand the placeholder in their reader and list them with the reason:\n%s",
			strings.Join(unlisted, "\n"))
	}
	if len(stale) != 0 {
		t.Errorf("these are listed in knownOwnerDataFileNamers but no longer name a data file:\n%s",
			strings.Join(stale, "\n"))
	}
}

// filesNamingOwnerDataFiles returns, sorted and relative to root, every regular
// file under root that names a data file in ownerDataFileNames. Tests, Markdown
// and documentation are skipped: they describe the files, they do not read them.
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
