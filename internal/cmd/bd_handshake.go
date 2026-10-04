package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/bdgate"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// bdHandshakeGatedCommands are the commands that run the town: they start
// the daemon, start agent sessions, or spawn polecats. Each refuses to run
// unless the town config files parse (townconfig.Check, gt-fcxe9.10, gt-y3pgh.1)
// and the bd on PATH passes the startup handshake (deps.CheckBDHandshake).
// Every other command, including every read-only one, runs against whatever
// bd is installed. TestBDHandshakeGatedCommandsExist pins that each path
// exists, so a rename cannot drop a command out of the gate.
var bdHandshakeGatedCommands = map[string]bool{
	"gt up":              true,
	"gt daemon start":    true,
	"gt daemon run":      true,
	"gt daemon restart":  true,
	"gt sling":           true,
	"gt crew start":      true,
	"gt crew restart":    true,
	"gt session start":   true,
	"gt session restart": true,
	"gt scheduler run":   true,
	"gt formula run":     true,
}

// bdHandshakeTownVerbs are the command names that start or dispatch things.
// Every command with one of these names must be gated or listed in
// bdHandshakeNotTownRunning with the reason, so a new start/run command
// cannot silently skip the gate (TestBDHandshakeClassifiesEveryTownVerb).
var bdHandshakeTownVerbs = map[string]bool{
	"up": true, "start": true, "restart": true, "run": true, "spawn": true,
	"sling": true, "resume": true,
}

// bdHandshakeNotTownRunning are start/run-named commands that start no agent
// session, daemon or polecat, with the reason each is exempt.
var bdHandshakeNotTownRunning = map[string]string{
	"gt agent resume":     "clears a pause flag; starts no session",
	"gt scheduler resume": "clears a pause flag; dispatch goes through gt sling (gated)",
	"gt plugin run":       "runs one plugin gate; starts no agent session",
	"gt reaper run":       "closes stale beads through bd; starts nothing",
	"gt slot run":         "wraps a test suite in the container slot",
	"gt dolt start":       "starts the Dolt server, which recovery needs when bd is broken",
	"gt dolt restart":     "restarts the Dolt server, which recovery needs when bd is broken",
}

// bdHandshakeTimeout bounds the handshake's two bd calls.
const bdHandshakeTimeout = 30 * time.Second

// townStartGate is the startup gate for town-running commands and agent
// session starts: the town config files must parse (checked on every call,
// so a file broken while the daemon runs stops its next session start), then
// the bd handshake must pass. A pass of the handshake is remembered for the
// life of the gate; a refusal is not: the daemon starts sessions for hours,
// and once the operator installs the right bd or Dolt recovers, the next
// session start must see it.
type townStartGate struct {
	// townRoot finds the town the command runs in ("" outside one).
	townRoot func() string
	// config reports a town config file under townRoot that does not parse.
	config func(townRoot string) error
	// handshake runs the bd handshake against townRoot's database.
	handshake func(ctx context.Context, townRoot string) (*deps.BDHandshake, error)

	mu     sync.Mutex
	passed bool
}

// processTownStartGate is the gate of this process: the cwd's town (or
// GT_TOWN_ROOT / GT_ROOT), its config files, and the bd on PATH.
var processTownStartGate = &townStartGate{
	townRoot:  detectTownRootFromCwd,
	config:    checkTownConfig,
	handshake: runBDHandshake,
}

// runBDHandshake runs the handshake against the bd on PATH from townRoot.
func runBDHandshake(ctx context.Context, townRoot string) (*deps.BDHandshake, error) {
	// Read the town's own database level, never whatever beads database the
	// current directory happens to resolve to.
	if townRoot == "" {
		return nil, fmt.Errorf("%w: not in a Gas Town workspace, so the town database's schema level cannot be read", deps.ErrBDHandshake)
	}
	path, _ := exec.LookPath("bd")
	return deps.CheckBDHandshake(ctx, path, deps.NewBDProcessRunner(townRoot))
}

// requiresBDHandshake reports whether cmd is a town-running command.
func requiresBDHandshake(cmd *cobra.Command) bool {
	return cmd != nil && bdHandshakeGatedCommands[cmd.CommandPath()]
}

// gateTownCommand runs gate for a town-running cmd and passes every other
// command through.
func gateTownCommand(cmd *cobra.Command, gate *townStartGate) error {
	if !requiresBDHandshake(cmd) {
		return nil
	}
	return gate.require()
}

// requireHandshake returns the handshake's refusal, if any. A pass is
// cached; a refusal is re-checked on the next call.
func (g *townStartGate) requireHandshake() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.passed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bdHandshakeTimeout)
	defer cancel()
	if _, err := g.handshake(ctx, g.townRoot()); err != nil {
		return err
	}
	g.passed = true
	return nil
}

// checkTownConfig checks the config files of the town at townRoot. Outside a
// town ("") it refuses: files it cannot find are not files that parse.
func checkTownConfig(townRoot string) error {
	if townRoot == "" {
		return errors.New("not in a Gas Town workspace, so the town config files cannot be checked")
	}
	// Load, not Check: a town-running command needs a whole town, so a root
	// found by its mayor/ directory alone (no mayor/town.json) is refused.
	town, err := townconfig.Load(townRoot)
	if err != nil {
		return err
	}
	warnLiteralSecretsOnce(os.Stderr, town)
	return nil
}

// require is the gate: the town config check first, then the handshake.
func (g *townStartGate) require() error {
	if err := g.config(g.townRoot()); err != nil {
		return err
	}
	return g.requireHandshake()
}

// installSessionGate makes every agent-session start in this process run the
// town config check and the handshake first (bdgate.Require in session.StartSession and the role
// managers' Start). It covers session starts the command list above does not
// name, and the daemon's own restarts. Only Execute installs it, so package
// tests that call Start directly run ungated.
func installSessionGate() {
	bdgate.Set(processTownStartGate.require)
}
