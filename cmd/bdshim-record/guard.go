package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// workspaceMarkers are what a directory holds when a beads workspace sits in it: the .beads
// directory itself, or the metadata.json bd keeps its store's settings in.
var workspaceMarkers = []string{".beads", "metadata.json"}

// Resolver reports the beads workspace bd resolves for a run in dir carrying env. The recorder
// answers it with bd where --json, the one read-only call that names the active .beads location.
type Resolver func(dir string, env []string) (string, error)

// guardRun refuses a recording that bd init could aim at a workspace the recorder did not make. bd
// init walks up from the directory it runs in and rewrites the first workspace it finds, so a copy
// under a tree that holds one changes that tree's store (gt-p85pw.103). Every directory under the
// town root holds one, so a scratch is refused unless it owns a workspace of its own: the guard
// makes one with bd init, then asks bd which workspace it resolves there, carrying the environment
// every child is given, and passes only when the answer is inside the scratch (gt-p85pw.115).
//
// The recorder's own working directory is checked as before, since a recorder started inside a
// workspace is where the incident began. Every refusal comes before anything is recorded, and the
// one bd call the guard makes is the init that gives the scratch its workspace.
func guardRun(getwd func() (string, error), scratch string, child childBD) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("name the working directory: %w", err)
	}
	real, err := realPath(cwd)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", cwd, err)
	}
	workspace, found, err := enclosingWorkspace(real)
	if err != nil {
		return err
	}
	if found {
		where := cwd
		if real != cwd {
			where = fmt.Sprintf("%s (that is %s)", cwd, real)
		}
		return fmt.Errorf("%s sits in or under the beads workspace %s, where bd init would rewrite that workspace's store; run the recorder outside any .beads tree", where, workspace)
	}
	if scratch == "" {
		return nil // a flags recording makes no copies, so it has no scratch directory to give a workspace
	}
	if !filepath.IsAbs(scratch) {
		scratch = filepath.Join(cwd, scratch)
	}
	scratch = filepath.Clean(scratch)
	if link, found, err := symlinkOnPath(scratch); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%s is behind the symlink %s; name a scratch directory whose path holds no symlink, since bd follows them and the workspace the recorder makes is then not the one it named", scratch, link)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return fmt.Errorf("make the scratch directory %s: %w", scratch, err)
	}
	name := newDatabaseName()
	if err := initDatabase(child.Run, scratch, name); err != nil {
		return fmt.Errorf("give the scratch directory %s a workspace of its own: %w", scratch, err)
	}
	resolved, err := child.Where(scratch, child.Env(scratch))
	if err != nil {
		// The database the init made stays: the guard refuses rather than making more bd calls, and
		// its name carries the test prefix a later cleanup drops it by.
		return fmt.Errorf("ask bd which workspace it resolves in %s: %w", scratch, err)
	}
	if err := scratchOwnsWorkspace(scratch, resolved); err != nil {
		return err
	}
	// The workspace existed for the check alone. Both parts of its cleanup run, so a failed drop
	// still removes the directory, and either failure is reported rather than left behind.
	if err := errors.Join(dropDatabase(child.Run, scratch, name), os.RemoveAll(beadsDirIn(scratch))); err != nil {
		return fmt.Errorf("clean up the scratch workspace %s: %w", scratch, err)
	}
	return nil
}

// scratchOwnsWorkspace refuses an answer that is not an absolute path inside the scratch, after
// following the symlinks on both. A workspace bd did not name, and one it names anywhere else, are
// both refusals: the scratch has to own the workspace the recording runs in.
func scratchOwnsWorkspace(scratch, workspace string) error {
	if workspace == "" {
		return fmt.Errorf("bd named no workspace for %s; the scratch must own the workspace it records in", scratch)
	}
	if !filepath.IsAbs(workspace) {
		return fmt.Errorf("bd named the workspace %q for %s, which is not an absolute path, so the recorder cannot tell which workspace that is", workspace, scratch)
	}
	found, err := realPath(workspace)
	if err != nil {
		return fmt.Errorf("resolve the workspace %s bd named: %w", workspace, err)
	}
	// The scratch itself needs no resolving: symlinkOnPath refused every path that could lead
	// somewhere other than where it says.
	if !isWithin(filepath.Clean(scratch), found) {
		return fmt.Errorf("bd resolves the workspace %s for %s, which is outside the scratch; the scratch must own the workspace it records in", found, scratch)
	}
	return nil
}

// isWithin reports whether path is dir or sits under it. A pair it cannot compare is not proof of
// a workspace inside the scratch, so it reads as outside.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// symlinkOnPath returns the first symlink at path or among its parents. bd follows symlinks, so a
// scratch behind one is a directory the recorder cannot name with any confidence: the workspace it
// makes could be made somewhere else entirely (gt-p85pw.115). The walk starts at the root, since a
// symlink above the scratch moves it just as a symlink at it does.
func symlinkOnPath(path string) (string, bool, error) {
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	at := volume + string(filepath.Separator)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		switch {
		case err == nil && info.Mode()&fs.ModeSymlink != 0:
			return at, true, nil
		case err == nil:
			// A directory in the path, so the next component is checked on its own.
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// Nothing at or below this component can hold a symlink, and the mkdir that follows
			// names the directory that cannot be made.
			return "", false, nil
		default:
			return "", false, fmt.Errorf("check %s for a symlink: %w", at, err)
		}
	}
	return "", false, nil
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
		if parent == path { // the root is not there, so nothing above it can be either
			return filepath.Join(path, missing), nil
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
