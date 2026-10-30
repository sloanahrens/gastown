package uninstallgt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
)

// names are the two paths a run removes from the install directory, in the
// order it removes them.
var names = []string{"gt", "bd"}

// ErrAbsent reports that the path a run would remove is not there: the run
// reports it as not present and removes nothing.
var ErrAbsent = errors.New("not present")

// Files is the filesystem a removal makes. It offers no recursive or
// directory operation on purpose: a run removes exactly the two paths it
// names, and this interface gives neither a fake nor the code a way to remove
// anything wider.
type Files interface {
	// Lstat reports the path itself, so a symlink is a file to remove rather
	// than the file it points at.
	Lstat(path string) (fs.FileInfo, error)
	// Stat follows a symlink; a run asks it only whether the directory exists.
	Stat(path string) (fs.FileInfo, error)
	// Remove removes path and nothing below it.
	Remove(path string) error
}

// OSFiles is the host's filesystem.
type OSFiles struct{}

// Lstat reports path itself, without following a final symlink.
func (OSFiles) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }

// Stat reports path, following a final symlink.
func (OSFiles) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// Remove removes path and nothing below it.
func (OSFiles) Remove(path string) error { return os.Remove(path) }

// Exec runs one command to completion and reports its error.
type Exec func(name string, args ...string) error

// OSExec runs the command on the host.
func OSExec(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// Remove clears path's OS-immutable flag and removes path itself, reporting
// ErrAbsent when there is no file or symlink to remove.
//
// An error reading path is returned rather than read as absence: a path the
// run cannot examine may hold a binary it would leave behind, and "not
// present" has to mean the path was looked at and was not there.
func Remove(files Files, run Exec, path string) error {
	info, err := files.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrAbsent
		}
		return err
	}
	clearImmutable(run, path, info.Mode()&fs.ModeSymlink != 0)
	if err := files.Remove(path); err != nil {
		return err
	}
	// The run reports a removal only once it has seen the path go: a remove
	// that answered without removing anything is a failure, not a success.
	_, err = files.Lstat(path)
	if err == nil {
		return fmt.Errorf("%s: still present after its removal", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// clearImmutable takes the OS-immutable flag off path, which the installer
// leaves set and the kernel refuses to unlink through (gt-vya0s). The commands
// are tried in turn and every failure is ignored: a host without the tool, or a
// filesystem without the flag, still has a file to remove.
//
// -h makes chflags act on a symlink itself, so a symlinked bd keeps its target
// untouched. The two fallbacks have no -h form and follow a symlink to its
// target, so a symlink gets the -h attempt alone: clearing the flag on a file
// outside the directory is a change to a path the run never removes.
func clearImmutable(run Exec, path string, link bool) {
	if run == nil {
		return
	}
	_ = run("chflags", "-h", "nouchg", path)
	if link {
		return
	}
	_ = run("chflags", "nouchg", path)
	_ = run("chattr", "-i", path)
}
