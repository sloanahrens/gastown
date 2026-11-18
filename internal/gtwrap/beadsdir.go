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
	// beadsRedirectName is the file in a .beads directory that names another
	// beads directory: how a polecat and crew worktree shares its rig's beads.
	beadsRedirectName = "redirect"
)

// nearestBeadsRel walks from dir up to root and returns the .beads directory
// nearest at or above dir, relative to root: the walk bd itself makes, and the
// one a bd call needs when no BEADS_DIR tells it which rig it runs in
// (gt-iy4cpy). A .beads directory that holds a redirect is named by its target
// instead (gt-8y5mcn), so a worktree's shared beads reach the rig that holds
// them. inside is false when dir is not under root, and rel is "" when no .beads
// entry lies at or above dir up to root. A .beads entry the stat cannot read
// refuses: read as absent, it would send the call to another rig's database, and
// an entry that is not a directory is passed over.
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
			return followedBeadsRel(root, filepath.Join(rel, beadsDirName), stat), true, nil
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

// followedBeadsRel returns the beads directory beadsRel names after the redirect
// it may hold, the file a polecat worktree shares its rig's beads through: bd
// resolves its target against the directory holding the .beads (gt-8y5mcn). The
// gateway workspace holds a rig's beads directory, never a worktree's redirect
// directory, so the worktree path must not reach BEADS_DIR, and the target is
// taken as it is rather than followed again. Anything that leaves the answer
// unchanged — no redirect file, one that cannot be read or names nothing
// resolvable, one that leaves the town or names no existing .beads directory —
// keeps beadsRel, so the call keeps the refusal it has today, whose message
// names that directory.
func followedBeadsRel(root, beadsRel string, stat func(string) (fs.FileInfo, error)) string {
	data, err := os.ReadFile(filepath.Join(root, beadsRel, beadsRedirectName)) //nolint:gosec // G304: path is a .beads entry inside the town the walk found
	if err != nil {
		return beadsRel
	}
	target := strings.TrimSpace(string(data))
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(filepath.Join(root, beadsRel)), target)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Base(rel) != beadsDirName {
		return beadsRel
	}
	info, err := stat(resolved)
	if err != nil || info == nil || !info.IsDir() {
		return beadsRel
	}
	return rel
}

// statDir is the stat a walk uses: the injected one, or os.Stat.
func statDir(stat func(string) (fs.FileInfo, error)) func(string) (fs.FileInfo, error) {
	if stat != nil {
		return stat
	}
	return os.Stat
}
