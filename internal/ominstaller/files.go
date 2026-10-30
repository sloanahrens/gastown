package ominstaller

import (
	"errors"
	"io/fs"
	"os"
)

// Files is the filesystem the sign and keep steps touch. The real one is used
// when Config.Files is nil; a test passes a fake, so no install writes a file or
// touches the installed om.
type Files interface {
	// Stat returns path's mode bits, following a symbolic link. An absent path
	// returns an error for which errors.Is(err, fs.ErrNotExist) holds.
	Stat(path string) (fs.FileMode, error)
	// Lstat returns the mode bits of the entry at path itself, never following a
	// symbolic link, so a link reads as a link and a dangling one still exists.
	Lstat(path string) (fs.FileMode, error)
	// ReadFile returns path's bytes.
	ReadFile(path string) ([]byte, error)
	// WriteFile writes data to path, creating or truncating it.
	WriteFile(path string, data []byte, perm fs.FileMode) error
	// Chmod sets path's permissions.
	Chmod(path string, perm fs.FileMode) error
	// Rename moves oldpath over newpath atomically, as rename(2) does.
	Rename(oldpath, newpath string) error
	// Remove deletes path.
	Remove(path string) error
}

// osFiles is the real filesystem.
type osFiles struct{}

func (osFiles) Stat(path string) (fs.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode(), nil
}

func (osFiles) Lstat(path string) (fs.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode(), nil
}

func (osFiles) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osFiles) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (osFiles) Chmod(path string, perm fs.FileMode) error { return os.Chmod(path, perm) }

func (osFiles) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osFiles) Remove(path string) error { return os.Remove(path) }

// files is the filesystem a run uses: Config.Files when set, else the real one.
func (c Config) files() Files {
	if c.Files == nil {
		return osFiles{}
	}
	return c.Files
}

// lstatOptional returns the mode of the entry at path, a symbolic link included,
// and whether an entry exists there. An absent path returns false, not an error. A
// dangling link exists, so it is not absent. Any other failure to look is an
// error, so a caller fails closed rather than reading "cannot tell" as "absent".
func lstatOptional(files Files, path string) (mode fs.FileMode, exists bool, err error) {
	mode, err = files.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return mode, true, nil
}
