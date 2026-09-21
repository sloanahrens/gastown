//go:build !windows

package tmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	newSessionSocketDialTimeout  = 200 * time.Millisecond
	newSessionSocketProbeTimeout = time.Second
)

func (t *Tmux) ensureNewSessionSocketSafe() error {
	if t.socketName == "" {
		return nil
	}

	socketPath := filepath.Join(SocketDir(), t.socketName)
	info, err := os.Lstat(socketPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("lstat tmux socket %s: %w", socketPath, err)
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return fmt.Errorf("tmux socket %s is a symlink; refusing to start a new session because tmux could unlink or rebind the target (see gt-h9z)", socketPath)
	}
	if mode&os.ModeSocket == 0 {
		return fmt.Errorf("tmux socket %s is %s, not a Unix socket; refusing to start a new session because tmux could replace it (see gt-h9z)", socketPath, mode.Type())
	}

	return t.ensureLiveSocketSafe(socketPath)
}

func (t *Tmux) ensureLiveSocketSafe(socketPath string) error {
	if stale, err := unixSocketStale(socketPath); err != nil || stale {
		if stale {
			return nil
		}
		return fmt.Errorf("tmux socket %s exists but cannot be safely contacted: %w", socketPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), newSessionSocketProbeTimeout)
	defer cancel()
	if err := t.runListSessionsProbe(ctx); err == nil {
		return nil
	} else if errors.Is(err, ErrNoServer) {
		stale, recheckErr := unixSocketStale(socketPath)
		if recheckErr == nil && stale {
			return nil
		}
		if recheckErr != nil {
			err = fmt.Errorf("%w; socket recheck failed: %v", err, recheckErr)
		}
		return fmt.Errorf("tmux socket %s has a live listener but tmux reported no server; refusing to start a new session because tmux could unlink and rebind it (see gt-h9z): %w", socketPath, err)
	} else {
		return fmt.Errorf("tmux socket %s has a live listener but list-sessions failed; refusing to start a new session because tmux could unlink and rebind it (see gt-h9z): %w", socketPath, err)
	}
}

// socketUnlinkWait bounds how long unlinkDeadSocketFile waits for a just-killed
// server's socket to stop answering, and socketUnlinkInterval is its re-check
// period. kill-server returns once the server has exited, but the closed socket
// keeps completing connections ~11ms longer on macOS, so a single dial right
// after the kill still sees a listener.
const (
	socketUnlinkWait     = 500 * time.Millisecond
	socketUnlinkInterval = 10 * time.Millisecond
)

// unlinkDeadSocketFile removes a socket file nothing is listening on, and
// leaves it in place otherwise. tmux does not unlink its socket when the server
// exits, so without this the file outlives its server forever (gt-20di).
//
// A live listener is left alone: a refused connection is the only proof that
// nothing is behind the file. That also covers a path that cannot be dialed at
// all — a plain file sitting where a socket belongs, the shape gt-h9z guards
// against — which is not this function's state to change.
func unlinkDeadSocketFile(socketPath string) {
	deadline := time.Now().Add(socketUnlinkWait)
	for {
		stale, err := unixSocketStale(socketPath)
		if err != nil {
			return
		}
		if stale {
			_ = os.Remove(socketPath)
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(socketUnlinkInterval)
	}
}

func unixSocketStale(socketPath string) (bool, error) {
	conn, err := net.DialTimeout("unix", socketPath, newSessionSocketDialTimeout)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return true, nil
		}
		return false, err
	}
	_ = conn.Close()
	return false, nil
}

func (t *Tmux) runListSessionsProbe(ctx context.Context) error {
	stdout, err := os.CreateTemp("", "gt-tmux-probe-stdout-*")
	if err != nil {
		return err
	}
	stdoutPath := stdout.Name()
	defer func() { _ = os.Remove(stdoutPath) }()
	defer func() { _ = stdout.Close() }()

	stderr, err := os.CreateTemp("", "gt-tmux-probe-stderr-*")
	if err != nil {
		return err
	}
	stderrPath := stderr.Name()
	defer func() { _ = os.Remove(stderrPath) }()
	defer func() { _ = stderr.Close() }()

	args := []string{"list-sessions", "-F", ""}
	cmd := t.commandContext(ctx, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 100 * time.Millisecond

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("tmux list-sessions timed out after %s: %w", newSessionSocketProbeTimeout, ctxErr)
		}
		stderrBytes, readErr := os.ReadFile(stderrPath)
		if readErr != nil {
			return fmt.Errorf("tmux list-sessions: %w", err)
		}
		return t.wrapError(err, string(stderrBytes), args)
	}
	return nil
}
