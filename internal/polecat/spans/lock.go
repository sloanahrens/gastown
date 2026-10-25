package spans

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// logMutexes holds one mutex per lock file, so goroutines of one process
// serialize on the mutex. A POSIX record lock belongs to the process, so it
// cannot serialize two goroutines of the same process: the mutex does that, and
// the record lock serializes processes.
var logMutexes sync.Map // map[string]*sync.Mutex

// logLock is the append lock of one bead's span log.
type logLock struct {
	mu *sync.Mutex
	f  *os.File
}

// lockLog takes the append lock at path: the in-process mutex, then an
// exclusive POSIX record lock (fcntl F_SETLKW) on the lock file, which waits for
// another process to release it. A record lock is not inherited across fork, so
// a child process cannot hold the lock after the release.
func lockLog(path string) (*logLock, error) {
	m, _ := logMutexes.LoadOrStore(path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("opening span lock %s: %w", path, err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &lk); err != nil {
		f.Close()
		mu.Unlock()
		return nil, fmt.Errorf("locking span log %s: %w", path, err)
	}
	return &logLock{mu: mu, f: f}, nil
}

// unlock releases the record lock and the mutex. Closing the descriptor drops
// the record lock.
func (l *logLock) unlock() {
	_ = l.f.Close()
	l.mu.Unlock()
}
