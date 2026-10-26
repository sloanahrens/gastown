//go:build unix

package spans

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"
)

// lockWait bounds how long an append waits for a span log's lock: first for the
// other goroutines of this process, then for the other processes that hold the
// lock file. A holder that never lets go fails the append after this long
// instead of hanging the caller.
const lockWait = 60 * time.Second

// lockPoll is how often a waiting append asks the kernel for the record lock
// again while another process holds it.
const lockPoll = 10 * time.Millisecond

// logMutex is the in-process lock of one span log's lock file, with the number
// of goroutines that hold it or wait on it. The entry is removed when the last
// one leaves, so the map holds only the logs in use. held is a one-slot channel
// rather than a sync.Mutex, so a waiter can give up at a deadline.
type logMutex struct {
	held chan struct{}
	refs int
}

// logMutexes maps a lock file path to its logMutex. A POSIX record lock belongs
// to the process, so it cannot serialize two goroutines of one process: the
// mutex does that, and the record lock serializes processes.
var (
	logMutexesMu sync.Mutex
	logMutexes   = map[string]*logMutex{}
)

// logLock is the append lock of one bead's span log.
type logLock struct {
	key string
	m   *logMutex
	f   *os.File
}

// lockLog takes the append lock at path, waiting at most lockWait.
func lockLog(path string) (*logLock, error) {
	return lockLogWithin(path, lockWait)
}

// lockLogWithin takes the append lock at path: the in-process lock, then an
// exclusive POSIX record lock on the lock file, which waits for another process
// to release it. The two waits share one deadline, wait from the call. A record
// lock is not inherited across fork, so a child process cannot hold the lock
// after the release.
func lockLogWithin(path string, wait time.Duration) (*logLock, error) {
	deadline := time.Now().Add(wait)

	logMutexesMu.Lock()
	m := logMutexes[path]
	if m == nil {
		m = &logMutex{held: make(chan struct{}, 1)}
		logMutexes[path] = m
	}
	m.refs++
	logMutexesMu.Unlock()

	giveUp := func(err error) (*logLock, error) {
		releaseLogMutex(path, m)
		return nil, err
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case m.held <- struct{}{}:
	case <-timer.C:
		return giveUp(fmt.Errorf("waiting for span log %s: gave up after %s, another goroutine holds its lock", path, wait))
	}

	// release gives back the in-process lock taken above, on every path that
	// does not return a lock to the caller.
	release := func() { <-m.held }

	// O_NOFOLLOW: a symlink at the lock path is refused, not followed to a file the
	// store does not own (gt-rm0ab).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		release()
		return giveUp(fmt.Errorf("opening span lock %s: %w", path, err))
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	for {
		err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk)
		if err == nil {
			break
		}
		// EAGAIN and EACCES are the kernel's answer for a record lock another
		// process holds; any other error is not a wait.
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EACCES) {
			f.Close()
			release()
			return giveUp(fmt.Errorf("locking span log %s: %w", path, err))
		}
		if !time.Now().Before(deadline) {
			f.Close()
			release()
			return giveUp(fmt.Errorf("locking span log %s: gave up after %s, another process holds it", path, wait))
		}
		time.Sleep(lockPoll)
	}
	return &logLock{key: path, m: m, f: f}, nil
}

// unlock releases the record lock and the in-process lock, and forgets the
// entry once no goroutine uses it. Closing the descriptor drops the record lock.
func (l *logLock) unlock() {
	_ = l.f.Close()
	<-l.m.held
	releaseLogMutex(l.key, l.m)
}

// releaseLogMutex drops one reference to m, and removes the path's entry when
// none is left.
func releaseLogMutex(path string, m *logMutex) {
	logMutexesMu.Lock()
	defer logMutexesMu.Unlock()
	m.refs--
	if m.refs == 0 {
		delete(logMutexes, path)
	}
}

// openLogNoBlock opens a span log without blocking on a FIFO or device: a named
// pipe at the log's path would otherwise hold Summarize until a writer appears,
// before the regular-file check could refuse it. The caller checks the descriptor
// is a regular file (gt-rm0ab).
func openLogNoBlock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
