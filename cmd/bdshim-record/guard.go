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
// directory and the scratch directory are checked.
//
// A -scratch that is not there yet is made by run once this check finds the tree it would sit in
// clear, so bd only runs in a directory the recorder made for it.
func guardRun(getwd func() (string, error), scratch string) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("name the working directory: %w", err)
	}
	roots := []string{cwd}
	scratchDir := ""
	if scratch != "" {
		scratchDir = scratch
		if !filepath.IsAbs(scratchDir) {
			scratchDir = filepath.Join(cwd, scratchDir)
		}
		roots = append(roots, scratchDir)
	}
	for _, root := range roots {
		workspace, found, err := enclosingWorkspace(root)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("%s sits in or under the beads workspace %s, where bd init would rewrite that workspace's store; run the recorder outside any .beads tree", root, workspace)
		}
	}
	return nil
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
