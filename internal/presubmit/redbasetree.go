package presubmit

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// RedBaseMkdir makes one directory and every parent it needs, as os.MkdirAll does.
type RedBaseMkdir func(dir string) error

// RedBaseWriter writes one file's content to path.
type RedBaseWriter func(path string, data []byte) error

// RedBaseTree makes the merge-base worktree through RedBaseWorktree and copies the named files, as HEAD has them, into it under their own paths, each parent directory made first, and returns the tree with its removal (gt-mc0uv9). A file path that is empty, absolute or leaves the tree, and a failed read, parent-directory make or write are errors; once the tree exists every error removes it and a failed removal is joined into the error, so the caller never receives a tree or a removal for a tree that is already gone.
func RedBaseTree(run Runner, read FileReader, mkdir RedBaseMkdir, write RedBaseWriter, base, dir string, files []string) (tree string, remove func() error, err error) {
	tree, remove, err = RedBaseWorktree(run, base, dir)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, func() error, error) {
		if rmErr := remove(); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return "", nil, err
	}
	for _, p := range files {
		rel, err := redBaseTreeRel(p)
		if err != nil {
			return fail(err)
		}
		data, err := read(rel)
		if err != nil {
			return fail(fmt.Errorf("red-base tree: reading %s: %w", p, err))
		}
		dst := filepath.Join(tree, filepath.FromSlash(rel))
		if err := mkdir(filepath.Dir(dst)); err != nil {
			return fail(fmt.Errorf("red-base tree: making %s: %w", filepath.Dir(dst), err))
		}
		if err := write(dst, data); err != nil {
			return fail(fmt.Errorf("red-base tree: writing %s: %w", dst, err))
		}
	}
	return tree, remove, nil
}

// redBaseTreeRel returns p cleaned, refusing an empty, absolute or escaping path, so an injected write can only reach inside the tree (gt-mc0uv9).
func redBaseTreeRel(p string) (string, error) {
	if p == "" {
		return "", errors.New("red-base tree: empty file path")
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("red-base tree: file path %q is absolute", p)
	}
	rel := path.Clean(p)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("red-base tree: file path %q leaves the tree", p)
	}
	return rel, nil
}
