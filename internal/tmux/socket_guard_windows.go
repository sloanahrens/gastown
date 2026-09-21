//go:build windows

package tmux

func (t *Tmux) ensureNewSessionSocketSafe() error {
	return nil
}

// unlinkDeadSocketFile is a no-op on Windows: tmux there address servers
// through named pipes rather than files in a socket directory.
func unlinkDeadSocketFile(socketPath string) {}
