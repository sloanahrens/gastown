package tmux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jonboulle/clockwork"
)

// execFunc runs a program and returns its stdout and stderr. Tmux sends every
// tmux, ps and kill invocation through one, so tests can record and answer
// them without starting processes.
type execFunc func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func realExec(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	hideConsoleWindow(cmd)
	if _, ok := ctx.Deadline(); ok {
		cmd.WaitDelay = 100 * time.Millisecond
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// runner returns the exec seam, falling back to realExec for a Tmux built
// without one (for example a struct literal).
func (t *Tmux) runner() execFunc {
	if t.exec == nil {
		return realExec
	}
	return t.exec
}

// clk returns the clock seam, falling back to the real clock.
func (t *Tmux) clk() clockwork.Clock {
	if t.clock == nil {
		return clockwork.NewRealClock()
	}
	return t.clock
}

// withSocket returns a Tmux on socket that shares t's seams.
func (t *Tmux) withSocket(socket string) *Tmux {
	return &Tmux{socketName: socket, exec: t.exec, clock: t.clock, sock: t.sock, socketDir: t.socketDir}
}

// socketOps is the filesystem and dial surface the new-session socket guard
// and the dead-socket cleanup touch. The real one works on the socket
// directory; tests script it so the unit tier opens no sockets.
type socketOps struct {
	lstat  func(path string) (os.FileInfo, error)
	remove func(path string) error
	// dial connects to the Unix socket at path and hangs up; nil means
	// something accepted the connection.
	dial func(path string) error
}

// sockets returns the socket seam, falling back to the real one.
func (t *Tmux) sockets() socketOps {
	if t.sock == nil {
		return realSocketOps()
	}
	return *t.sock
}

// socketPath is the file tmux binds for this Tmux's named socket.
func (t *Tmux) socketPath() string {
	dir := t.socketDir
	if dir == "" {
		dir = SocketDir()
	}
	return filepath.Join(dir, t.socketName)
}
