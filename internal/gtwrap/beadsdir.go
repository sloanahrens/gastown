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
// an entry that is not a directory is passed over. readFile reads the redirect;
// the caller wires it with readFileOf, so a test can drive the walk with no host
// filesystem.
func nearestBeadsRel(root, dir string, stat func(string) (fs.FileInfo, error), readFile func(string) ([]byte, error)) (string, bool, error) {
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
			followed, err := followedBeadsRel(root, filepath.Join(rel, beadsDirName), stat, readFile)
			return followed, true, err
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
// taken as it is rather than followed again. The target's containment is checked
// again with its symlinks resolved, since a path lexically inside the town can
// name a directory outside it (gt-x43caq); the answer names the resolved
// directory, so an alias inside the town cannot name another rig. Anything that
// leaves the answer unchanged — no redirect file, a target that names nothing
// resolvable, one that leaves the town or names no existing .beads directory —
// keeps beadsRel, so the call keeps the refusal it has today, whose message
// names that directory. A redirect file that exists but cannot be read is not a
// missing one: it refuses, since read as absent it would name the worktree's own
// directory; so does a town root whose own symlinks cannot be resolved, since
// the containment of a target inside it cannot then be checked.
func followedBeadsRel(root, beadsRel string, stat func(string) (fs.FileInfo, error), readFile func(string) ([]byte, error)) (string, error) {
	redirect := filepath.Join(root, beadsRel, beadsRedirectName)
	data, err := readFile(redirect)
	if errors.Is(err, fs.ErrNotExist) {
		return beadsRel, nil
	}
	if err != nil {
		return "", fmt.Errorf("gtwrap: read the .beads redirect file %s, so the bd call cannot be told which rig it runs in: %w", redirect, err)
	}
	target := strings.TrimSpace(string(data))
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(filepath.Join(root, beadsRel)), target)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Base(rel) != beadsDirName {
		return beadsRel, nil
	}
	info, err := stat(resolved)
	if err != nil || info == nil || !info.IsDir() {
		return beadsRel, nil
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("gtwrap: resolve the symlinks of the town root %s, so the bd call cannot be told which rig it runs in: %w", root, err)
	}
	realTarget, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return beadsRel, nil
	}
	realRel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) || filepath.Base(realRel) != beadsDirName {
		return beadsRel, nil
	}
	return realRel, nil
}

// statDir is the stat a walk uses: the injected one, or os.Stat.
func statDir(stat func(string) (fs.FileInfo, error)) func(string) (fs.FileInfo, error) {
	if stat != nil {
		return stat
	}
	return os.Stat
}

// readFileOf is the read a walk uses: the injected one, or os.ReadFile.
func readFileOf(readFile func(string) ([]byte, error)) func(string) ([]byte, error) {
	if readFile != nil {
		return readFile
	}
	return os.ReadFile
}
