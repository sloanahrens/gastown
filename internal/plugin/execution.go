package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BinaryPath returns the program the plugin's [execution] block names, joined
// under its directory, or "" when it names none; the name is an error when it
// leaves that directory.
func (p *Plugin) BinaryPath() (string, error) {
	if p.Execution == nil || p.Execution.Binary == "" {
		return "", nil
	}
	return resolveBinary(p.Path, p.Execution.Binary)
}

// resolveBinary joins rel onto dir and refuses a result that leaves dir or
// names dir itself: an absolute path, a .. that reaches above the plugin
// directory, or a symlink whose target lies outside. The daemon execs the
// result under the plugin's name and the operator's permissions, so a path
// that leaves the directory would run a program the plugin never shipped. A
// path that does not exist yet is checked lexically; symlinks are checked once
// it resolves, and the run fails on the missing file either way.
func resolveBinary(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("binary %q must be relative to the plugin directory", rel)
	}
	full := filepath.Join(dir, rel)
	if err := escapesDir(dir, full); err != nil {
		return "", fmt.Errorf("binary %q escapes the plugin directory: %w", rel, err)
	}
	// A symlink that resolves to nothing is not a binary yet: the lexical check
	// below would pass it, and a target that appears later would be run from
	// wherever the link points (dv-7k25.4's sibling finding, gt-tpg0p.1).
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if _, err := filepath.EvalSymlinks(full); err != nil {
			return "", fmt.Errorf("binary %q is a dangling symlink: %w", rel, err)
		}
	}

	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return full, nil // nothing exists to resolve; the lexical check stands
	}
	resolved, ok := resolveExisting(full)
	if !ok {
		return full, nil
	}
	if err := escapesDir(resolvedDir, resolved); err != nil {
		return "", fmt.Errorf("binary %q escapes the plugin directory through a symlink: %w", rel, err)
	}
	return full, nil
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
