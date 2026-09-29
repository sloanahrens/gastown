package tmux

import (
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"
)

// sockState is what sits at a path in a fakeSockets directory.
type sockState int

const (
	sockAbsent  sockState = iota
	sockStale             // a socket file with no listener: dial is refused
	sockLive              // a socket file with a listener: dial connects
	sockFile              // a regular file
	sockDir               // a directory
	sockSymlink           // a symlink
)

// fakeSockets is a scripted socket directory for the socketOps seam. Its dial
// answers are the ones real connect(2) gives for each kind of file; the
// TestIntegration socket tests pin that behavior. The two kernels differ on a
// path that is not a socket: macOS answers ENOTSOCK, Linux ECONNREFUSED — the
// same answer as a dead socket (af_unix refuses any inode that is not a
// socket). linuxConnect selects Linux's answer.
type fakeSockets struct {
	mu           sync.Mutex
	files        map[string]sockState
	dialErrs     map[string][]error // queued answers, consumed before the state's
	removed      []string
	linuxConnect bool
}

func newFakeSockets() *fakeSockets {
	return &fakeSockets{files: map[string]sockState{}, dialErrs: map[string][]error{}}
}

func (f *fakeSockets) set(path string, st sockState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = st
}

// queueDial makes the next dials of path fail with errs, in order.
func (f *fakeSockets) queueDial(path string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialErrs[path] = append(f.dialErrs[path], errs...)
}

func (f *fakeSockets) state(path string) sockState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[path]
}

func (f *fakeSockets) ops() *socketOps {
	return &socketOps{
		lstat: func(path string) (os.FileInfo, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			var mode fs.FileMode
			switch f.files[path] {
			case sockAbsent:
				return nil, &os.PathError{Op: "lstat", Path: path, Err: syscall.ENOENT}
			case sockStale, sockLive:
				mode = fs.ModeSocket | 0o700
			case sockFile:
				mode = 0o600
			case sockDir:
				mode = fs.ModeDir | 0o700
			case sockSymlink:
				mode = fs.ModeSymlink | 0o777
			}
			return fakeInfo{name: path, mode: mode}, nil
		},
		remove: func(path string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.files[path] == sockAbsent {
				return &os.PathError{Op: "remove", Path: path, Err: syscall.ENOENT}
			}
			delete(f.files, path)
			f.removed = append(f.removed, path)
			return nil
		},
		dial: func(path string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if q := f.dialErrs[path]; len(q) > 0 {
				f.dialErrs[path] = q[1:]
				return q[0]
			}
			op := func(e error) error { return &os.SyscallError{Syscall: "connect", Err: e} }
			switch f.files[path] {
			case sockAbsent:
				return op(syscall.ENOENT)
			case sockStale:
				return op(syscall.ECONNREFUSED)
			case sockLive:
				return nil
			default: // regular file, directory, symlink to a non-socket
				if f.linuxConnect {
					return op(syscall.ECONNREFUSED)
				}
				return op(syscall.ENOTSOCK)
			}
		},
	}
}

type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

// socketTmux is a Tmux on socket whose socket directory is fs (rooted at
// /fake-sock), tmux calls are scripted by s, and clock is a fake one.
func socketTmux(socket string, fs *fakeSockets, s *scripted) *Tmux {
	tm := newTmuxForTest(socket, s.exec, newFixedClock())
	tm.sock = fs.ops()
	tm.socketDir = "/fake-sock"
	return tm
}
