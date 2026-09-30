//go:build !windows

package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// FlockAcquire opens a flock file and acquires an exclusive advisory lock.
// Returns a cleanup function that releases the lock and closes the file.
// This is a general-purpose cross-process lock suitable for any read-modify-write
// operation that needs serialization across separate CLI invocations.
func FlockAcquire(path string) (func(), error) {
	return flockAcquire(path)
}

// flockAcquire opens a flock file and acquires an exclusive advisory lock.
// Returns a cleanup function that releases the lock and closes the file.
// The flock prevents concurrent Acquire() calls from racing on the same lock path.
func flockAcquire(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644) //nolint:gosec // G304,G306: lock files are internal operational data
	if err != nil {
		return nil, fmt.Errorf("opening flock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquiring flock: %w", err)
	}

	cleanup := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}
	return cleanup, nil
}

// FlockTryAcquire attempts a non-blocking exclusive advisory lock on the given path.
// Returns (cleanup, true, nil) if the lock was acquired, or (nil, false, nil) if
// another process already holds it. The cleanup function releases the lock and
// closes the file descriptor.
func FlockTryAcquire(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644) //nolint:gosec // G304,G306: lock files are internal operational data
	if err != nil {
		return nil, false, fmt.Errorf("opening flock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("acquiring flock: %w", err)
	}

	cleanup := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}
	return cleanup, true, nil
}

// ErrFlockHeld reports that another process holds the flock. FlockTryAcquireStable
// returns it in place of an error so that a caller can tell a lock that is taken
// from a check that could not run.
var ErrFlockHeld = errors.New("flock is held by another process")

// FlockTryAcquireStable is FlockTryAcquire with the unlink race closed: the lock
// it returns is always on the file the path currently names.
//
// FlockTryAcquire opens and locks in two syscalls, so a waiter whose open landed
// before a holder unlinked the file it still holds locks the inode the unlink
// left nameless the instant that holder lets go. The waiter believes it holds
// the lock while the next process to open the path finds a fresh inode and locks
// that — two holders of one lock (gt-xtfnq). This variant reads the link count
// of the descriptor it locked and starts over from a new open when the name is
// gone.
//
// One check after locking is enough: a peer that wants to remove the name must
// take the flock first, and cannot while we hold it.
//
// It returns ErrFlockHeld both for contention and for a path replaced
// flockStableAttempts times running: the caller refuses and retries rather than
// hold a lock it cannot tie to a name.
func FlockTryAcquireStable(path string) (func(), error) {
	return flockTryAcquireStable(func() (*os.File, error) { return flockOpenFile(path) })
}

// flockStableAttempts bounds FlockTryAcquireStable's retries. Exhausting it takes
// a peer unlinking the file we just locked that many times over, which only a
// release/sweep storm does.
const flockStableAttempts = 8

// flockTryAcquireStable locks the file open returns, retrying while that file
// has no name left. open is called once per attempt, and the descriptor of an
// attempt that is abandoned is closed here.
func flockTryAcquireStable(open func() (*os.File, error)) (func(), error) {
	for range flockStableAttempts {
		f, err := open()
		if err != nil {
			return nil, fmt.Errorf("opening flock file: %w", err)
		}
		err = flockFileTry(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		linked, err := flockFileIsLinked(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if linked {
			return unlockFlockFile(f), nil
		}
		f.Close()
	}
	return nil, ErrFlockHeld
}

// flockOpenFile opens path for locking, creating it when it does not exist.
func flockOpenFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644) //nolint:gosec // G304,G306: lock files are internal operational data
	if err != nil {
		return nil, fmt.Errorf("opening flock file: %w", err)
	}
	return f, nil
}

// flockFileTry takes the exclusive flock on f without blocking.
func flockFileTry(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			return ErrFlockHeld
		}
		return fmt.Errorf("acquiring flock: %w", err)
	}
	return nil
}

// unlockFlockFile is the cleanup a successful acquire returns.
func unlockFlockFile(f *os.File) func() {
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}
}

// flockFileIsLinked reports whether the file f names still has a name. The
// kernel drops an inode's link count to zero when its last name is removed,
// which is exactly the case FlockTryAcquireStable guards: the holder that owned
// the path unlinked it before letting its flock go.
func flockFileIsLinked(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("statting locked file: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("statting locked file: no link count in %T", fi.Sys())
	}
	return st.Nlink > 0, nil
}
