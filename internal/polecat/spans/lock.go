//go:build unix

package spans

import (
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
)

// logMutex is the in-process mutex of one span log's lock file, with the number
// of goroutines that hold it or wait on it. The entry is removed when the last
// one leaves, so the map holds only the logs in use.
type logMutex struct {
	mu   sync.Mutex
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

// lockLog takes the append lock at path: the in-process mutex, then an
// exclusive POSIX record lock (fcntl F_SETLKW) on the lock file, which waits for
// another process to release it. A record lock is not inherited across fork, so
// a child process cannot hold the lock after the release.
func lockLog(path string) (*logLock, error) {
	logMutexesMu.Lock()
	m := logMutexes[path]
	if m == nil {
		m = &logMutex{}
		logMutexes[path] = m
	}
	m.refs++
	logMutexesMu.Unlock()

	m.mu.Lock()
	// O_NOFOLLOW: a symlink at the lock path is refused, not followed to a file the
	// store does not own (gt-rm0ab).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		m.mu.Unlock()
		releaseLogMutex(path, m)
		return nil, fmt.Errorf("opening span lock %s: %w", path, err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &lk); err != nil {
		f.Close()
		m.mu.Unlock()
		releaseLogMutex(path, m)
		return nil, fmt.Errorf("locking span log %s: %w", path, err)
	}
	return &logLock{key: path, m: m, f: f}, nil
}

// unlock releases the record lock and the mutex, and forgets the mutex once no
// goroutine uses it. Closing the descriptor drops the record lock.
func (l *logLock) unlock() {
	_ = l.f.Close()
	l.m.mu.Unlock()
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
