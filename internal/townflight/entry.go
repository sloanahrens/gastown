// Package townflight tracks the gt commands running inside the daemon pod.
package townflight

import (
	"fmt"
	"io"
	"os"

	"github.com/gofrs/flock"
)

// entry is one command in flight: a locked file whose contents are its label.
type entry struct {
	path string
	lock entryLock
}

// entryFile is the open file a label is written to.
type entryFile interface {
	io.StringWriter
	io.Closer
	Name() string
}

// entryLock is the exclusive lock held on an entry file.
type entryLock interface {
	Lock() error
	Unlock() error
}

// entryFiles is the file operations createEntry uses. A test substitutes one
// whose steps fail, to take each failure.
type entryFiles struct {
	createTemp func(dir, pattern string) (entryFile, error)
	newLock    func(path string) entryLock
}

// osFiles is the real file system and lock.
func osFiles() entryFiles {
	return entryFiles{
		createTemp: func(dir, pattern string) (entryFile, error) { return os.CreateTemp(dir, pattern) },
		newLock:    func(path string) entryLock { return flock.New(path) },
	}
}

// createEntry makes dir and returns an entry holding label, in a uniquely
// named file that does not end in .cmd.
func createEntry(dir, label string) (*entry, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating inflight directory: %w", err)
	}
	return openEntry(osFiles(), dir, label)
}

func openEntry(files entryFiles, dir, label string) (*entry, error) {
	f, err := files.createTemp(dir, "inflight-")
	if err != nil {
		return nil, fmt.Errorf("creating inflight file: %w", err)
	}
	path := f.Name()
	// The label is written only while the lock is held, so a probe that can
	// lock the file never reads one and mistakes a command for a dead entry.
	lock := files.newLock(path)
	if err := lock.Lock(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("locking inflight file: %w", err)
	}
	if _, err := f.WriteString(label); err != nil {
		_ = lock.Unlock()
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("writing inflight label: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = lock.Unlock()
		_ = os.Remove(path)
		return nil, fmt.Errorf("closing inflight file: %w", err)
	}
	return &entry{path: path, lock: lock}, nil
}

// drop removes the entry's file and releases its lock.
func (e *entry) drop() error {
	removeErr := os.Remove(e.path)
	unlockErr := e.lock.Unlock()
	if removeErr == nil {
		removeErr = unlockErr
	}
	if removeErr != nil {
		return fmt.Errorf("dropping inflight entry: %w", removeErr)
	}
	return nil
}
