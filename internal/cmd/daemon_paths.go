package cmd

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

// pathWithin reports whether path is root or lies under it, by their cleaned
// absolute spellings.
func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveThroughExisting resolves path's symbolic links when the file need not
// exist yet: the nearest existing ancestor is resolved and the rest appended.
func resolveThroughExisting(path string, eval func(string) (string, error)) (string, error) {
	var rest []string
	for p := filepath.Clean(path); ; {
		resolved, err := eval(p)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return "", err
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}
