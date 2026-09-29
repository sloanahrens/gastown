package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/bdgate"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/deps"
)

// bdHandshakeGatedCommands are the commands that run the town: they start
// the daemon, start agent sessions, or spawn polecats. Each refuses to run
// unless the town config files parse (daemon.CheckTownConfig, gt-fcxe9.10)
// and the bd on PATH passes the startup handshake (deps.CheckBDHandshake).
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
	"gt scheduler run":    true,
	"gt formula run":      true,
	"gt synthesis start":  true,
}

// bdHandshakeTownVerbs are the command names that start or dispatch things.
// Every command with one of these names must be gated or listed in
// bdHandshakeNotTownRunning with the reason, so a new start/run command
// cannot silently skip the gate (TestBDHandshakeClassifiesEveryTownVerb).
var bdHandshakeTownVerbs = map[string]bool{
	"up": true, "start": true, "restart": true, "run": true, "spawn": true,
	"sling": true, "boot": true, "resume": true,
}

// bdHandshakeNotTownRunning are start/run-named commands that start no agent
// session, daemon or polecat, with the reason each is exempt.
var bdHandshakeNotTownRunning = map[string]string{
	"gt boot":             "command group; its spawn verb is gated",
	"gt resume":           "reads the inbox for handoff messages",
	"gt agent resume":     "clears a pause flag; starts no session",
	"gt deacon resume":    "clears a pause flag; starts no session",
	"gt mountain resume":  "re-enables wave dispatch, which goes through gt sling (gated)",
	"gt quota resume":     "nudges existing sessions",
	"gt scheduler resume": "clears a pause flag; dispatch goes through gt sling (gated)",
	"gt plugin run":       "runs one plugin gate; starts no agent session",
	"gt reaper run":       "closes stale beads through bd; starts nothing",
	"gt mq batch run":     "assembles a merge batch; starts no session",
	"gt slot run":         "wraps a test suite in the container slot",
	"gt dolt start":       "starts the Dolt server, which recovery needs when bd is broken",
	"gt dolt restart":     "restarts the Dolt server, which recovery needs when bd is broken",
}

// bdHandshakeTimeout bounds the handshake's two bd calls.
const bdHandshakeTimeout = 30 * time.Second

var (
	// bdHandshakeCheck runs the handshake against the bd on PATH from the
	// town root. Tests replace it; nothing else should.
	bdHandshakeCheck = defaultBDHandshakeCheck

	// bdHandshakePassed remembers a pass for the life of the process. A
	// refusal is not remembered: the daemon starts sessions for hours, and
	// once the operator installs the right bd or Dolt recovers, the next
	// session start must see it.
	bdHandshakeMu     sync.Mutex
	bdHandshakePassed bool
)

func defaultBDHandshakeCheck(ctx context.Context) (*deps.BDHandshake, error) {
	// Read the town's own database level, never whatever beads database the
	// current directory happens to resolve to.
	dir := detectTownRootFromCwd()
	if dir == "" {
		return nil, fmt.Errorf("%w: not in a Gas Town workspace, so the town database's schema level cannot be read", deps.ErrBDHandshake)
	}
	path, _ := exec.LookPath("bd")
	return deps.CheckBDHandshake(ctx, path, deps.NewBDProcessRunner(dir))
}

// requiresBDHandshake reports whether cmd is a town-running command.
func requiresBDHandshake(cmd *cobra.Command) bool {
	return cmd != nil && bdHandshakeGatedCommands[cmd.CommandPath()]
}

// requireBDHandshake returns the handshake's refusal, if any. A pass is
// cached for the process; a refusal is re-checked on the next call.
func requireBDHandshake() error {
	bdHandshakeMu.Lock()
	defer bdHandshakeMu.Unlock()
	if bdHandshakePassed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bdHandshakeTimeout)
	defer cancel()
	if _, err := bdHandshakeCheck(ctx); err != nil {
		return err
	}
	bdHandshakePassed = true
	return nil
}

// townConfigCheck reports a town config file that does not parse. Tests
// replace it; nothing else should.
var townConfigCheck = defaultTownConfigCheck

// defaultTownConfigCheck checks the config files of the town the working
// directory (or GT_TOWN_ROOT / GT_ROOT) belongs to. Outside a town it
// refuses: files it cannot find are not files that parse.
func defaultTownConfigCheck() error {
	dir := detectTownRootFromCwd()
	if dir == "" {
		return errors.New("not in a Gas Town workspace, so the town config files cannot be checked")
	}
	return daemon.CheckTownConfig(dir)
}

// requireTownStart is the startup gate for town-running commands and agent
// session starts: the town config files must parse (checked on every call,
// so a file broken while the daemon runs stops its next session start), then
// the bd handshake must pass (a pass is cached).
func requireTownStart() error {
	if err := townConfigCheck(); err != nil {
		return err
	}
	return requireBDHandshake()
}

// resetBDHandshakeCache forgets a cached pass (tests).
func resetBDHandshakeCache() {
	bdHandshakeMu.Lock()
	defer bdHandshakeMu.Unlock()
	bdHandshakePassed = false
}

// installSessionGate makes every agent-session start in this process run the
// town config check and the handshake first (bdgate.Require in session.StartSession and the role
// managers' Start). It covers session starts the command list above does not
// name, and the daemon's own restarts. Only Execute installs it, so package
// tests that call Start directly run ungated.
func installSessionGate() {
	bdgate.Set(requireTownStart)
}
