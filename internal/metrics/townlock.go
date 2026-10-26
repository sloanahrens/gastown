package metrics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// ErrLockHeld means another run holds the town's metrics lock.
var ErrLockHeld = errors.New("town-metrics lock is held by another run")

// fileID names a lock file by its device and inode, so two paths to one file (a
// hard link, a symlink) are one lock.
type fileID struct {
	dev int64
	ino uint64
}

// townLocksMu guards townLocks and serializes every open of a lock file in this
// process. Closing any descriptor of a file drops every record lock this process
// holds on that file, so no open may overlap a held lock on the same file.
var townLocksMu sync.Mutex

// townLocks holds the lock each file this process holds belongs to. A POSIX
// record lock cannot refuse a second acquire from its own process, so this table
// does.
var townLocks = map[fileID]*TownLock{}

// TownLock is the single-flight lock of one town-metrics run: an exclusive POSIX
// record lock on the lock file, which the kernel drops when the holder exits,
// however it exits. A record lock belongs to the process and is not inherited
// across fork, so a child process never holds it.
type TownLock struct {
	id       fileID
	f        *os.File
	released bool
}

// AcquireTownLock takes the lock at path without waiting. It returns ErrLockHeld
// when this process or another process holds the file, under any path. The file
// keeps the holder's pid for an operator to read; it is never removed, because
// removing it would let a run lock a file no one else is looking at.
func AcquireTownLock(path string) (*TownLock, error) {
	townLocksMu.Lock()
	defer townLocksMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	// Refuse before opening: the file may already be held by this process under
	// another name, and the open is not safe to make then (townLocksMu).
	if id, ok, err := statFileID(path); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	} else if ok {
		if _, held := townLocks[id]; held {
			return nil, ErrLockHeld
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	id, err := openFileID(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	lock := &TownLock{id: id, f: f}
	townLocks[id] = lock
	return lock, nil
}

// Release gives the lock up. It forgets the lock in this process and then closes
// the file, which drops the record lock. A second call does nothing, and it
// never forgets a lock another acquire has taken since.
func (l *TownLock) Release() error {
	townLocksMu.Lock()
	defer townLocksMu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	if townLocks[l.id] == l {
		delete(townLocks, l.id)
	}
	return l.f.Close()
}

// statFileID is the identity of the file at path, and false when there is none.
func statFileID(path string) (fileID, bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileID{}, false, nil
	}
	if err != nil {
		return fileID{}, false, err
	}
	id, err := sysFileID(fi)
	return id, err == nil, err
}

// openFileID is the identity of an open lock file.
func openFileID(f *os.File) (fileID, error) {
	fi, err := f.Stat()
	if err != nil {
		return fileID{}, err
	}
	return sysFileID(fi)
}

// sysFileID reads the device and inode a file system reports for fi.
func sysFileID(fi os.FileInfo) (fileID, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, fmt.Errorf("no device and inode for %s", fi.Name())
	}
	// Dev is int32 on darwin and uint64 on linux; int64 is a real conversion on both,
	// so the conversion is not redundant on either platform.
	return fileID{dev: int64(st.Dev), ino: st.Ino}, nil
}
