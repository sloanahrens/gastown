package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// workspaceMarkers are what a directory holds when a beads workspace sits in it: the .beads
// directory itself, or the metadata.json bd keeps its store's settings in.
var workspaceMarkers = []string{".beads", "metadata.json"}

// guardRun refuses a recording that bd init could aim at a workspace the recorder did not make. bd
// init walks up from the directory it runs in and rewrites the first workspace it finds before it
// aborts, so a copy under a tree that holds one changes that tree's store (gt-p85pw.103). A
// directory is refused when it, or any parent of it, holds a workspace marker; both the working
// directory and the scratch directory are checked, at the path their symlinks lead to.
//
// A -scratch that is not there yet is made by run once this check finds the tree it would sit in
// clear, so bd only runs in a directory the recorder made for it.
func guardRun(getwd func() (string, error), scratch string) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("name the working directory: %w", err)
	}
	roots := []string{cwd}
	if scratch != "" {
		if !filepath.IsAbs(scratch) {
			scratch = filepath.Join(cwd, scratch)
		}
		roots = append(roots, scratch)
	}
	for _, root := range roots {
		real, err := realPath(root)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", root, err)
		}
		workspace, found, err := enclosingWorkspace(real)
		if err != nil {
			return err
		}
		if found {
			where := root
			if real != root {
				where = fmt.Sprintf("%s (that is %s)", root, real)
			}
			return fmt.Errorf("%s sits in or under the beads workspace %s, where bd init would rewrite that workspace's store; run the recorder outside any .beads tree", where, workspace)
		}
	}
	return nil
}

// realPath returns path with every symlink on it resolved, since bd follows them too: a -scratch
// that is a symlink into a workspace aims bd init at that workspace even though the lexical path
// never passes it (gt-p85pw.103). The part of the path that is not there yet has no symlink to
// resolve, so the deepest existing directory is resolved and the rest is joined on.
func realPath(path string) (string, error) {
	missing := ""
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(real, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = filepath.Join(filepath.Base(path), missing)
		path = parent
	}
}

// enclosingWorkspace returns the nearest directory at or above dir that holds a workspace marker.
// A marker it cannot read is an error rather than an absent one, so an unreadable tree is never
// taken for a safe one.
func enclosingWorkspace(dir string) (string, bool, error) {
	for {
		for _, marker := range workspaceMarkers {
			path := filepath.Join(dir, marker)
			_, err := os.Stat(path)
			if err == nil {
				return dir, true, nil
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", false, fmt.Errorf("check for a beads workspace at %s: %w", path, err)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}
