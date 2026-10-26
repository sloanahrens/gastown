package testpolicy

import (
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IntegrationPackages returns the directories under root that the integration
// build tag adds files to, relative to root and sorted. skip names
// root-relative directories the walk does not enter, for trees generated in
// place that are not part of this module.
//
// A directory is selected when a Go file there is excluded by the build
// constraints the host resolves without extra tags and included once
// "integration" is set. A file that builds either way is already compiled by
// the unit tier, so it selects nothing. Nested modules are walked like any
// other directory: the returned path is what `go vet` resolves from inside it.
func IntegrationPackages(root string, skip ...string) ([]string, error) {
	tagged := build.Default
	tagged.BuildTags = append(append([]string(nil), build.Default.BuildTags...), "integration")
	skipped := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipped[filepath.ToSlash(filepath.Clean(s))] = true
	}
	var pkgs []string
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		if rel != "." && skipped[rel] {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		adds, err := addsIntegrationFiles(dir, entries, &tagged)
		if err != nil {
			return err
		}
		if adds {
			pkgs = append(pkgs, rel)
		}
		for _, e := range entries {
			// The Go tool ignores these, so a package there is not part of
			// the module and go vet would not resolve it.
			if !e.IsDir() || skipForGoTool(e.Name()) {
				continue
			}
			if err := walk(filepath.Join(dir, e.Name()), filepath.ToSlash(filepath.Join(rel, e.Name()))); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, "."); err != nil {
		return nil, err
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// addsIntegrationFiles reports whether any non-directory entry of dir is a Go
// file the integration tag adds.
func addsIntegrationFiles(dir string, entries []os.DirEntry, tagged *build.Context) (bool, error) {
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		plain, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return false, err
		}
		if plain {
			continue
		}
		withTag, err := tagged.MatchFile(dir, name)
		if err != nil {
			return false, err
		}
		if withTag {
			return true, nil
		}
	}
	return false, nil
}

// skipForGoTool reports whether a directory with this name holds no package of
// this module: the Go tool ignores vendor, testdata and names starting with a
// dot or underscore, and node_modules belongs to the npm package.
func skipForGoTool(name string) bool {
	return name == "vendor" || name == "testdata" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}
