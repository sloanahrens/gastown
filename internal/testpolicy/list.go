package testpolicy

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ReadList reads a list of repo-relative package directories, one per line.
func ReadList(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m[line] = true
	}
	return m, sc.Err()
}

// PackageDirs returns every directory under root that holds .go files.
func PackageDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != root && (name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			return filepath.SkipDir
		}
		matches, err := filepath.Glob(filepath.Join(p, "*.go"))
		if err != nil {
			return err
		}
		if len(matches) > 0 {
			dirs = append(dirs, p)
		}
		return nil
	})
	return dirs, err
}
