package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/tmux"
)

// overrunGateReadTimeout bounds the ps and tmux reads of one cycle. The guard
// runs on the daemon's heartbeat with a single-flight flag, and a wedged host
// read must not hold that flag for good.
const overrunGateReadTimeout = 30 * time.Second

// The gate exemption: the time a seat spends inside its own gate does not count
// toward the 12 and 20 minute rule (gt-vfwuc). A gate runs 10 to 25 minutes on a
// loaded host, so a seat waiting on one is working, not overrunning. Either
// answer below is the exemption, and the elapsed calculation that subtracts the
// time is in overrun_guard_decide.go:
//
//   - a slot of the town's container-gate pool held under the seat's own role. A
//     container-backed suite runs through `gt slot run`, and the wait before the
//     slot is granted is gate time too.
//   - a live gate suite under the seat's own tmux pane: the rig's configured
//     test, lint and build commands, plus the gates a polecat runs whatever the
//     config names them (go test, go vet, make test, make gate). This is the
//     only signal for a suite that holds no slot.
//
// The question itself is polecat.LiveSuite, the one definition the Stop-hook
// guard asks too (gt-a1xhd). What is local here is the reading: this guard
// holds the pool's config, caches each read for the cycle, and reads an argv
// through polecat.GateTokens rather than the Stop hook's shlex pass.

// overrunGateReader answers "is this seat inside its own gate" for one cycle.
// The town's pool, the process table and the pane listing are town-wide or
// session-wide, not per seat, and are read once and reused: a read per seat
// would spawn a `ps` and a `tmux` per seat per tick.
type overrunGateReader struct {
	d *Daemon
	// pool is the container-gate pool this town's seats acquire from, read once
	// per cycle so a slot count changed while the daemon runs is seen.
	pool slot.Pool
	// run runs the ps command; nil runs it for real. panes lists the pane pids
	// of a session; nil asks tmux. Tests set both.
	run   cmdRunFunc
	panes func(sessionName string) ([]int, error)
	// sandboxReads answers for a sandboxed seat, whose panes and processes are in
	// its sandbox and invisible to the host's list-panes and ps; nil is a tmux
	// client that routes. Tests set it.
	sandboxReads sandboxSessionReads

	// The per-cycle reads, each kept with the error its read failed with so
	// every seat on this tick gets the same answer.
	slotReport  slot.Report
	slotLoaded  bool
	slotErr     error
	procs       []polecat.Proc
	procsLoaded bool
	procErr     error
	panesOut    string
	panesLoaded bool
	panesErr    error
	gates       map[string][]polecat.Invocation
	// sandboxes is the registry's answer per session, read once per cycle: a
	// seat's pane and process reads each ask whether it is sandboxed.
	sandboxes map[string]sandboxAnswer
}

// sandboxAnswer is one session's registry read, kept with its error.
type sandboxAnswer struct {
	on  bool
	err error
}

// sandboxSessionReads is the routed tmux reads a sandboxed seat is asked
// through (tmux.Tmux.SessionPanes and ProcessTable).
type sandboxSessionReads interface {
	SessionPanes(session string) (string, error)
	ProcessTable(session string) (string, error)
}

// sandboxed reports whether the session runs in a sandbox. A registry that
// cannot be read is an error, so the caller counts all of the seat's time (the
// direction that stops) instead of reading the wrong table.
func (r *overrunGateReader) sandboxed(sessionName string) (bool, error) {
	if a, ok := r.sandboxes[sessionName]; ok {
		return a.on, a.err
	}
	if r.sandboxes == nil {
		r.sandboxes = map[string]sandboxAnswer{}
	}
	on, err := session.IsSandboxed(r.d.config.TownRoot, sessionName)
	if err != nil {
		on, err = false, fmt.Errorf("sandbox registry: %w", err)
	}
	r.sandboxes[sessionName] = sandboxAnswer{on: on, err: err}
	return on, err
}

func (r *overrunGateReader) sandboxSessionReads() sandboxSessionReads {
	if r.sandboxReads != nil {
		return r.sandboxReads
	}
	return tmux.NewTmux()
}

// newOverrunGateReader returns the reader for one cycle, with the real ps and
// tmux.
func newOverrunGateReader(d *Daemon) *overrunGateReader {
	return &overrunGateReader{d: d, pool: slot.PoolForTown(d.config.TownRoot), gates: map[string][]polecat.Invocation{}}
}

// running reports whether the seat is inside its own gate now. An error means
// the seat's gate state could not be read; the caller then counts all of its
// time, the direction that stops rather than lets a seat run unbounded
// (gt-vfwuc).
func (r *overrunGateReader) running(seat overrunSeat) (bool, error) {
	found, err := polecat.LiveSuite(r, polecat.SuiteQuery{
		Session: session.PolecatSessionName(r.d.prefixRegistry().PrefixForRig(seat.Rig), seat.Polecat),
		Roles:   overrunSeatRoles(seat),
		Suites:  r.gateInvocations(seat.Rig),
		Split:   polecat.GateTokens,
	})
	if err != nil {
		return false, err
	}
	return found.Busy(), nil
}

// Slots is the pool's flock picture, read once per cycle.
//
// Only the flock picture is read (StatusPoolLocksOnly): the container half
// shells out to `docker ps`, says nothing about which seat holds what, and
// would be paid for on every tick.
func (r *overrunGateReader) Slots() (slot.Report, error) {
	if !r.slotLoaded {
		r.slotLoaded = true
		r.slotReport, r.slotErr = slot.StatusPoolLocksOnly(r.d.config.TownRoot, r.pool)
	}
	return r.slotReport, r.slotErr
}

// overrunSeatRoles are the role strings a seat's own gate run can hold the slot
// under: `<rig>/<polecat>` is what `gt slot run --role` documents and what a
// polecat's gate wrap passes, and `<rig>/polecats/<polecat>` is the GT_ROLE the
// session itself carries, which a nested wrap with no role of its own inherits
// (slot.InheritedRole).
func overrunSeatRoles(seat overrunSeat) []string {
	return []string{
		seat.Rig + "/" + seat.Polecat,
		seat.Rig + "/polecats/" + seat.Polecat,
	}
}

// PanePIDs lists the pane pids of one session, through the reader's pane seam
// or from the all-sessions listing read once per cycle.
func (r *overrunGateReader) PanePIDs(sessionName string) ([]int, error) {
	sandboxed, err := r.sandboxed(sessionName)
	if err != nil {
		return nil, err
	}
	if sandboxed {
		out, err := r.sandboxSessionReads().SessionPanes(sessionName)
		if err != nil {
			return nil, fmt.Errorf("listing sandbox panes: %w", err)
		}
		return polecat.ParsePanes(out, sessionName), nil
	}
	if r.panes != nil {
		return r.panes(sessionName)
	}
	if !r.panesLoaded {
		r.panesLoaded = true
		ctx, cancel := context.WithTimeout(context.Background(), overrunGateReadTimeout)
		defer cancel()
		out, err := tmux.BuildCommandContext(ctx, "list-panes", "-a", "-F", "#{session_name}\t#{pane_pid}").Output()
		if err != nil {
			r.panesErr = fmt.Errorf("listing tmux panes: %w", err)
		} else {
			r.panesOut = string(out)
		}
	}
	if r.panesErr != nil {
		return nil, r.panesErr
	}
	return polecat.ParsePanes(r.panesOut, sessionName), nil
}

// gateInvocations is the rig's gate pairs, read once per rig per cycle.
func (r *overrunGateReader) gateInvocations(rigName string) []polecat.Invocation {
	if gates, ok := r.gates[rigName]; ok {
		return gates
	}
	gates := overrunGateInvocations(r.d.config.TownRoot, rigName)
	r.gates[rigName] = gates
	return gates
}

// SessionProcessTable is polecat.SessionProcessReads: a sandboxed seat's
// processes are read in its sandbox, every other seat's in the host table read
// once per cycle.
func (r *overrunGateReader) SessionProcessTable(sessionName string) ([]polecat.Proc, error) {
	sandboxed, err := r.sandboxed(sessionName)
	if err != nil {
		return nil, err
	}
	if !sandboxed {
		return r.ProcessTable()
	}
	out, err := r.sandboxSessionReads().ProcessTable(sessionName)
	if err != nil {
		return nil, fmt.Errorf("listing sandbox processes: %w", err)
	}
	return polecat.ParseProcessTable(out), nil
}

// ProcessTable reads the whole process table once per cycle.
func (r *overrunGateReader) ProcessTable() ([]polecat.Proc, error) {
	if !r.procsLoaded {
		r.procsLoaded = true
		r.procs, r.procErr = r.readProcessTable()
	}
	return r.procs, r.procErr
}

// readProcessTable runs `ps -ww -eo pid,ppid,args` through the reader's command
// seam or for real, bounded by overrunGateReadTimeout. -ww because the
// invocation being looked for sits at the end of a long wrapper argv (a
// background Bash reaches the table as `... eval 'make gate' < /dev/null`); a
// width-truncated line drops exactly the part that carries it.
func (r *overrunGateReader) readProcessTable() ([]polecat.Proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), overrunGateReadTimeout)
	defer cancel()
	cmd := boundDaemonCmd(exec.CommandContext(ctx, "ps", "-ww", "-eo", "pid,ppid,args")) //nolint:gosec // G204: fixed command
	out, _, err := runWith(r.run, cmd)
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return polecat.ParseProcessTable(string(out)), nil
}

// overrunGateInvocations reduces the rig's configured test, lint and build
// commands, plus the invocations a polecat runs as its own gate whatever the rig
// configured, to the pairs the pane scan matches.
func overrunGateInvocations(townRoot, rigName string) []polecat.Invocation {
	suites := slices.Clone(overrunBareGateInvocations)
	for _, command := range polecat.ConfiguredCommands(townRoot, rigName) {
		if suite, ok := overrunGateFromCommand(command); ok {
			suites = append(suites, suite)
		}
	}
	return suites
}

// overrunBareGateInvocations are the gates recognized in a polecat's pane even
// when the rig's own config names its suite some other way: make gate and make
// test because a Makefile gate runs its suite through them whatever the config
// calls the suite, and the Go quality gate a polecat runs bare.
var overrunBareGateInvocations = []polecat.Invocation{
	{Program: "go", Target: "test"},
	{Program: "go", Target: "vet"},
	{Program: "make", Target: "gate"},
	{Program: "make", Target: "test"},
}

// overrunGateFromCommand reduces a configured gate command to the pair the pane
// scan matches, refusing a command that names no target.
//
// Matching a program alone would exempt a seat for any live process whose argv
// mentions the word, so a rig configured that way is left to the bare
// invocations above. The Stop-hook guard reads the same command the other way
// (targetless matches the program anywhere), which is why the choice lives here
// and not in polecat.InvocationFromCommand.
func overrunGateFromCommand(command string) (polecat.Invocation, bool) {
	suite, ok := polecat.InvocationFromCommand(polecat.GateTokens, command)
	if !ok || suite.Target == "" {
		return polecat.Invocation{}, false
	}
	return suite, true
}
