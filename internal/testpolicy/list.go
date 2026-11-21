package testpolicy

import (
	"bufio"
	"fmt"
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

// PackageDirs returns every directory under root that holds .go files and
// belongs to the root module: a directory with its own go.mod, and everything
// under it, is a nested module that carries its own rules.
//
// root may be a symlink to the checkout: filepath.WalkDir does not follow a
// symlinked root and would report an empty tree with a nil error, so the walk
// runs on the resolved root and each directory is returned spelled under the
// root the caller gave, because callers trim that spelling off the paths.
func PackageDirs(root string) ([]string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(resolved); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("testpolicy: root %s is not a directory", root)
	}
	var dirs []string
	err = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != resolved && (name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			return filepath.SkipDir
		}
		if p != resolved {
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
		}
		matches, err := filepath.Glob(filepath.Join(p, "*.go"))
		if err != nil {
			return err
		}
		if len(matches) > 0 {
			rel, err := filepath.Rel(resolved, p)
			if err != nil {
				return err
			}
			dirs = append(dirs, filepath.Join(root, rel))
		}
		return nil
	})
	return dirs, err
}
