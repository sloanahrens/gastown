package forgejoprobe

import (
	"io/fs"
	"os"
	"time"
)

// osFS is the FS the green record is written through in production: the
// operating system's files.
type osFS struct{}

func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

func (osFS) CreateTemp(dir, pattern string) (TempFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osFS) Remove(name string) error { return os.Remove(name) }

// systemClock is the probe's clock in production: the wall clock, and a real
// sleep between status reads.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(d time.Duration) { time.Sleep(d) }
