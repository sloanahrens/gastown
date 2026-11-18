package gtwrap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// beadsDirVar is the variable a bd call names its .beads directory in. The
	// pod's bd reads it to tell which rig the call runs in.
	beadsDirVar = "BEADS_DIR"
	// beadsDirName is the directory name bd looks for at each step of its walk up
	// from the working directory.
	beadsDirName = ".beads"
)

// nearestBeadsRel walks from dir up to root and returns the .beads directory
// nearest at or above dir, relative to root: the walk bd itself makes, and the
// one a bd call needs when no BEADS_DIR tells it which rig it runs in
// (gt-iy4cpy). inside is false when dir is not under root, and rel is "" when no
// .beads entry lies at or above dir up to root. A .beads entry the stat cannot
// read refuses: read as absent, it would send the call to another rig's
// database, and an entry that is not a directory is passed over.
func nearestBeadsRel(root, dir string, stat func(string) (fs.FileInfo, error)) (string, bool, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false, fmt.Errorf("gtwrap: compare the working directory %s with the town %s, so the bd call cannot be told which rig it runs in: %w", dir, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, nil
	}
	root = filepath.Clean(root)
	for {
		candidate := filepath.Join(root, rel, beadsDirName)
		info, err := stat(candidate)
		if err == nil && info.IsDir() {
			return filepath.Join(rel, beadsDirName), true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("gtwrap: read the .beads entry %s, so the bd call cannot be told which rig it runs in: %w", candidate, err)
		}
		if rel == "." {
			return "", true, nil
		}
		rel = filepath.Dir(rel)
	}
}

// statDir is the stat a walk uses: the injected one, or os.Stat.
func statDir(stat func(string) (fs.FileInfo, error)) func(string) (fs.FileInfo, error) {
	if stat != nil {
		return stat
	}
	return os.Stat
}
