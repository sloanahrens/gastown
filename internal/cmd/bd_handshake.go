package cmd

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/deps"
)

// bdHandshakeGatedCommands are the commands that run the town: they start
// the daemon, start agent sessions, or spawn polecats. Each refuses to run
// unless the bd on PATH passes the startup handshake (deps.CheckBDHandshake).
// Every other command, including every read-only one, runs against whatever
// bd is installed. TestBDHandshakeGatedCommandsExist pins that each path
// exists, so a rename cannot drop a command out of the gate.
var bdHandshakeGatedCommands = map[string]bool{
	"gt up":               true,
	"gt start":            true,
	"gt daemon start":     true,
	"gt daemon run":       true,
	"gt daemon restart":   true,
	"gt sling":            true,
	"gt boot spawn":       true,
	"gt crew start":       true,
	"gt crew restart":     true,
	"gt deacon start":     true,
	"gt deacon restart":   true,
	"gt mayor start":      true,
	"gt mayor restart":    true,
	"gt refinery start":   true,
	"gt refinery restart": true,
	"gt witness start":    true,
	"gt witness restart":  true,
	"gt rig start":        true,
	"gt rig restart":      true,
	"gt rig boot":         true,
	"gt session start":    true,
	"gt session restart":  true,
}

// bdHandshakeTimeout bounds the handshake's two bd calls.
const bdHandshakeTimeout = 30 * time.Second

var (
	// bdHandshakeCheck runs the handshake against the bd on PATH from the
	// town root. Tests replace it; nothing else should.
	bdHandshakeCheck = defaultBDHandshakeCheck

	bdHandshakeOnce sync.Once
	bdHandshakeErr  error
)

func defaultBDHandshakeCheck(ctx context.Context) (*deps.BDHandshake, error) {
	dir := detectTownRootFromCwd()
	if dir == "" {
		dir, _ = os.Getwd()
	}
	path, _ := exec.LookPath("bd")
	return deps.CheckBDHandshake(ctx, path, deps.NewBDProcessRunner(dir))
}

// requiresBDHandshake reports whether cmd is a town-running command.
func requiresBDHandshake(cmd *cobra.Command) bool {
	return cmd != nil && bdHandshakeGatedCommands[cmd.CommandPath()]
}

// requireBDHandshake runs the handshake once per process and returns its
// refusal, if any.
func requireBDHandshake() error {
	bdHandshakeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), bdHandshakeTimeout)
		defer cancel()
		_, bdHandshakeErr = bdHandshakeCheck(ctx)
	})
	return bdHandshakeErr
}
