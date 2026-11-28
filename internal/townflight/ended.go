package townflight

import (
	"fmt"
	"os"

	"github.com/gofrs/flock"
)

// waitEnded blocks until the command holding the entry at path has ended, then
// drops it. A removed entry counts as ended; a lock or drop error is returned.
func waitEnded(path string) error {
	return waitEndedWith(func(path string) entryLock { return flock.New(path) }, os.Remove, path)
}

// waitEndedWith is waitEnded with the lock and removal a test can fail.
func waitEndedWith(newLock func(string) entryLock, remove func(string) error, path string) error {
	lock := newLock(path)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking inflight file: %w", err)
	}
	removeErr := remove(path)
	unlockErr := lock.Unlock()
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("removing inflight entry: %w", removeErr)
	}
	if unlockErr != nil {
		return fmt.Errorf("unlocking inflight file: %w", unlockErr)
	}
	return nil
}
