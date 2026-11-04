package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BinaryPath returns the program the plugin's [execution] block names,
// preferring this platform's build beside it when that is a regular file under
// the plugin directory, or "" when the block names none; a name that leaves
// the directory is an error.
func (p *Plugin) BinaryPath() (string, error) {
	if p.Execution == nil || p.Execution.Binary == "" {
		return "", nil
	}
	return resolveBinary(p.Path, p.Execution.Binary)
}

// resolveBinary joins rel onto dir, refuses a result that leaves dir or names
// dir itself, and prefers this platform's build of that binary when one is
// present as a regular file under dir. The daemon execs the result under the
// plugin's name and the operator's permissions, so a path that leaves the
// directory would run a program the plugin never shipped. A path that does not
// exist yet is checked lexically; symlinks are checked once it resolves, and
// the run fails on the missing file either way.
func resolveBinary(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("binary %q must be relative to the plugin directory", rel)
	}
	full := filepath.Join(dir, rel)
	if err := checkBinary(dir, full, rel); err != nil {
		return "", err
	}

	platformRel := filepath.Join(filepath.Dir(rel), runtime.GOOS+"-"+runtime.GOARCH, filepath.Base(rel))
	platformPath := filepath.Join(dir, platformRel)
	if fi, err := os.Stat(platformPath); err == nil && fi.Mode().IsRegular() {
		// The daemon execs this path too, so a candidate whose symlink resolves
		// outside the plugin directory is refused here as well.
		if err := checkBinary(dir, platformPath, platformRel); err != nil {
			return "", err
		}
		return platformPath, nil
	}
	return full, nil
}

// checkBinary refuses a path that is dir itself or outside it, lexically or
// through a symlink, and a symlink that resolves to nothing.
func checkBinary(dir, full, rel string) error {
	if err := escapesDir(dir, full); err != nil {
		return fmt.Errorf("binary %q escapes the plugin directory: %w", rel, err)
	}
	// A symlink that resolves to nothing is not a binary yet: the lexical check
	// above would pass it, and a target that appears later would be run from
	// wherever the link points (dv-7k25.4's sibling finding, gt-tpg0p.1).
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if _, err := filepath.EvalSymlinks(full); err != nil {
			return fmt.Errorf("binary %q is a dangling symlink: %w", rel, err)
		}
	}

	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil // nothing exists to resolve; the lexical check stands
	}
	resolved, ok := resolveExisting(full)
	if !ok {
		return nil
	}
	if err := escapesDir(resolvedDir, resolved); err != nil {
		return fmt.Errorf("binary %q escapes the plugin directory through a symlink: %w", rel, err)
	}
	return nil
}

// escapesDir reports why target is dir itself or outside it. A comparison
// that cannot be made refuses too: an unreadable path is not a file under dir.
func escapesDir(dir, target string) error {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is not a file under %s", target, dir)
	}
	return nil
}

// resolveExisting returns target with its symlinks resolved. When target
// itself does not exist, it resolves the parent instead, so a symlinked
// directory is still caught before the file exists; ok is false when neither
// resolves.
func resolveExisting(target string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, true
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", false
	}
	return filepath.Join(parent, filepath.Base(target)), true
}
