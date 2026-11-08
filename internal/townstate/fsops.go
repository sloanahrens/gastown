package townstate

import (
	"io"
	"io/fs"
	"os"
	"time"
)

// townOps is the filesystem seam an export or import runs through. The zero
// value resolves to the host filesystem; a test replaces one operation so it
// can drive that failure path without an unwritable directory on the host.
type townOps struct {
	lstat    func(string) (fs.FileInfo, error)
	readDir  func(string) ([]string, error)
	readlink func(string) (string, error)
	open     func(string) (io.ReadCloser, error)
	mkdirAll func(string, fs.FileMode) error
	openFile func(string, int, fs.FileMode) (io.WriteCloser, error)
	chmod    func(string, fs.FileMode) error
	chtimes  func(string, time.Time, time.Time) error
	symlink  func(string, string) error
	remove   func(string) error
}

// osOps is the seam backed by the host filesystem.
func osOps() townOps {
	return townOps{
		lstat: os.Lstat,
		readDir: func(p string) ([]string, error) {
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			return names, nil
		},
		readlink: os.Readlink,
		open: func(p string) (io.ReadCloser, error) {
			return os.Open(p) //nolint:gosec // G304: the path is built under the town root
		},
		mkdirAll: os.MkdirAll,
		openFile: func(p string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
			return os.OpenFile(p, flag, perm) //nolint:gosec // G304: the path is checked against the manifest under the town root
		},
		chmod:   os.Chmod,
		chtimes: os.Chtimes,
		symlink: os.Symlink,
		remove:  os.Remove,
	}
}

// withDefaults fills every operation a caller left nil with the host one.
func (o townOps) withDefaults() townOps {
	d := osOps()
	if o.lstat == nil {
		o.lstat = d.lstat
	}
	if o.readDir == nil {
		o.readDir = d.readDir
	}
	if o.readlink == nil {
		o.readlink = d.readlink
	}
	if o.open == nil {
		o.open = d.open
	}
	if o.mkdirAll == nil {
		o.mkdirAll = d.mkdirAll
	}
	if o.openFile == nil {
		o.openFile = d.openFile
	}
	if o.chmod == nil {
		o.chmod = d.chmod
	}
	if o.chtimes == nil {
		o.chtimes = d.chtimes
	}
	if o.symlink == nil {
		o.symlink = d.symlink
	}
	if o.remove == nil {
		o.remove = d.remove
	}
	return o
}
