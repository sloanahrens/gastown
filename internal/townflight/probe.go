package townflight

import (
	"fmt"

	"github.com/gofrs/flock"
)

// probeLock is the lock a probe tries on an entry file.
type probeLock interface {
	entryLock
	TryLock() (bool, error)
}

// probe reports whether the command that holds the entry at path is still
// running. A free lock means the command has ended, so the entry is removed and
// running is false. A held lock means running is true, with the entry's label.
// A lock error, or a failed removal, is returned.
func probe(path string) (label string, running bool, err error) {
	return probeWith(func(path string) probeLock { return flock.New(path) }, path)
}

// probeWith is probe with the lock a test can fail.
func probeWith(newLock func(string) probeLock, path string) (label string, running bool, err error) {
	lock := newLock(path)
	acquired, err := lock.TryLock()
	if err != nil {
		return "", false, fmt.Errorf("locking inflight file: %w", err)
	}
	if !acquired {
		return entryLabel(path), true, nil
	}
	if err = (&entry{path: path, lock: lock}).drop(); err != nil {
		return "", false, err
	}
	return "", false, nil
}
