//go:build !windows

package tmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/jonboulle/clockwork"
)

const (
	newSessionSocketDialTimeout = 200 * time.Millisecond
	// newSessionSocketProbeTimeout bounds the list-sessions probe. It has to
	// outlast a healthy server on a loaded host, where the tmux client's own
	// exec is slow: at 1s a concurrent create at load 30 was refused as a
	// hijacked socket (gt-h9z false positive, 2026-09-27). A listener that
	// never answers is still refused, just after this long.
	newSessionSocketProbeTimeout = 5 * time.Second
)

func (t *Tmux) ensureNewSessionSocketSafe() error {
	if t.socketName == "" {
		return nil
	}

	socketPath := t.socketPath()
	info, err := t.sockets().lstat(socketPath)
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
	if stale, err := t.socketStaleWithin(socketPath, newSessionSocketProbeTimeout); err != nil || stale {
		if stale {
			return nil
		}
		return fmt.Errorf("tmux socket %s exists but cannot be safely contacted: %w", socketPath, err)
	}

	ctx, cancel := clockwork.WithTimeout(context.Background(), t.clk(), newSessionSocketProbeTimeout)
	defer cancel()
	if err := t.runListSessionsProbe(ctx); err == nil {
		return nil
	} else if errors.Is(err, ErrNoServer) {
		stale, recheckErr := t.socketStaleWithin(socketPath, newSessionSocketProbeTimeout)
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
func unlinkDeadSocketFile(clk clockwork.Clock, ops socketOps, socketPath string) {
	deadline := clk.Now().Add(socketUnlinkWait)
	for {
		stale, err := unixSocketStale(ops, socketPath)
		if err != nil && !isDialTimeout(err) {
			return
		}
		if stale {
			_ = ops.remove(socketPath)
			return
		}
		if !clk.Now().Before(deadline) {
			return
		}
		clk.Sleep(socketUnlinkInterval)
	}
}

// isDialTimeout reports whether a dial ran out of time. That says nothing about
// the socket: under host load a dial to a dead socket can time out before the
// kernel's refusal is read.
func isDialTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

// socketStaleWithin is unixSocketStale, re-dialing a timed-out dial every
// socketUnlinkInterval until budget has passed on the clock.
func (t *Tmux) socketStaleWithin(socketPath string, budget time.Duration) (bool, error) {
	clk := t.clk()
	deadline := clk.Now().Add(budget)
	for {
		stale, err := unixSocketStale(t.sockets(), socketPath)
		if err == nil || !isDialTimeout(err) || !clk.Now().Before(deadline) {
			return stale, err
		}
		clk.Sleep(socketUnlinkInterval)
	}
}

// unixSocketStale reports whether nothing listens on socketPath: a refused
// connection, or no file, is the only proof.
func unixSocketStale(ops socketOps, socketPath string) (bool, error) {
	if err := ops.dial(socketPath); err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func realSocketOps() socketOps {
	return socketOps{
		lstat:  os.Lstat,
		remove: os.Remove,
		dial: func(path string) error {
			conn, err := net.DialTimeout("unix", path, newSessionSocketDialTimeout)
			if err != nil {
				return err
			}
			return conn.Close()
		},
	}
}

func (t *Tmux) runListSessionsProbe(ctx context.Context) error {
	args := []string{"list-sessions", "-F", ""}
	_, stderr, err := t.runner()(ctx, "tmux", t.tmuxArgs(args)...)
	if err != nil {
		// Done, not Err: a clock-driven context's Err blocks until it is done.
		select {
		case <-ctx.Done():
			return fmt.Errorf("tmux list-sessions timed out after %s: %w", newSessionSocketProbeTimeout, ctx.Err())
		default:
		}
		return t.wrapError(err, string(stderr), args)
	}
	return nil
}
