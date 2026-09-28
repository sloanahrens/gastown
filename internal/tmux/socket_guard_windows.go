//go:build windows

package tmux

import (
	"errors"
	"os"

	"github.com/jonboulle/clockwork"
)

func (t *Tmux) ensureNewSessionSocketSafe() error {
	return nil
}

// unlinkDeadSocketFile is a no-op on Windows: tmux there address servers
// through named pipes rather than files in a socket directory.
func unlinkDeadSocketFile(_ clockwork.Clock, _ socketOps, socketPath string) {}

func realSocketOps() socketOps {
	return socketOps{
		lstat:  os.Lstat,
		remove: os.Remove,
		dial:   func(string) error { return errors.New("unix sockets are not used on Windows") },
	}
}
