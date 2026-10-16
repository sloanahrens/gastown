package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
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
// The matcher reads an argv through the reduced tokenizer below, not the shlex
// pass internal/cmd's Stop-hook guard uses — that tokenizer is internal/cmd's to
// export, and gt-a1xhd tracks uniting the two.

// overrunGateProc is one row of the process table the pane scan reads: the pid,
// its parent, and the full argv the gate patterns are matched against.
type overrunGateProc struct {
	PID  int
	PPID int
	Args string
}

// overrunGate is one gate command reduced to the pair of tokens that identify
// it in another process's argv: the program and its first non-flag argument,
// both lowercased basenames.
type overrunGate struct {
	Program string
	Target  string
}

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

	// The per-cycle reads, each kept with the error its read failed with so
	// every seat on this tick gets the same answer.
	slotReport  slot.Report
	slotLoaded  bool
	slotErr     error
	procs       []overrunGateProc
	procsLoaded bool
	procErr     error
	panesOut    string
	panesLoaded bool
	panesErr    error
	gates       map[string][]overrunGate
}

// newOverrunGateReader returns the reader for one cycle, with the real ps and
// tmux.
func newOverrunGateReader(d *Daemon) *overrunGateReader {
	return &overrunGateReader{d: d, pool: slot.PoolForTown(d.config.TownRoot), gates: map[string][]overrunGate{}}
}

// running reports whether the seat is inside its own gate now. An error means
// the seat's gate state could not be read; the caller then counts all of its
// time, the direction that stops rather than lets a seat run unbounded
// (gt-vfwuc).
func (r *overrunGateReader) running(seat overrunSeat) (bool, error) {
	held, err := r.slotHeld(seat)
	if err != nil {
		return false, err
	}
	if held {
		return true, nil
	}
	suite, err := r.paneSuite(seat)
	if err != nil {
		return false, err
	}
	return suite != "", nil
}

// slotHeld reports whether a slot of the town's container-gate pool is held
// under one of this seat's roles.
//
// Only the flock picture is read (StatusPoolLocksOnly): the container half
// shells out to `docker ps`, says nothing about which seat holds what, and
// would be paid for on every tick.
func (r *overrunGateReader) slotHeld(seat overrunSeat) (bool, error) {
	rep, err := r.status()
	if err != nil {
		return false, err
	}
	for _, role := range overrunSeatRoles(seat) {
		if len(rep.HeldBy(role)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// status is the pool's flock picture, read once per cycle.
func (r *overrunGateReader) status() (slot.Report, error) {
	if !r.slotLoaded {
		r.slotLoaded = true
		r.slotReport, r.slotErr = slot.StatusPoolLocksOnly(r.d.config.TownRoot, r.pool)
		if r.slotErr != nil {
			r.slotErr = fmt.Errorf("container-gate slot status: %w", r.slotErr)
		}
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

// paneSuite names a live gate suite running under the seat's own tmux panes:
// a gate that holds no slot at all — the devops one, and every rig whose suite
// needs no container — shows up nowhere else. It returns "" when the seat's
// panes hold no gate, and an error when the panes or the process table could
// not be read.
//
// Every pane of the session is read, not just the first: a split session can
// hold the suite in another pane, and a suite this misses reads as a seat that
// is not gating.
func (r *overrunGateReader) paneSuite(seat overrunSeat) (string, error) {
	sessionName := session.PolecatSessionName(r.d.prefixRegistry().PrefixForRig(seat.Rig), seat.Polecat)
	panes, err := r.sessionPanePIDs(sessionName)
	if err != nil {
		return "", err
	}
	if len(panes) == 0 {
		return "", fmt.Errorf("no pane found for session %s", sessionName)
	}
	procs, err := r.processTable()
	if err != nil {
		return "", err
	}
	gates := r.gateInvocations(seat.Rig)
	for _, argv := range overrunDescendantArgvs(panes, procs) {
		if suite, ok := overrunGateArgv(argv, gates); ok {
			return suite, nil
		}
	}
	return "", nil
}

// sessionPanePIDs lists the pane pids of one session, through the reader's pane
// seam or from the all-sessions listing read once per cycle.
func (r *overrunGateReader) sessionPanePIDs(sessionName string) ([]int, error) {
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
	return parseOverrunPanes(r.panesOut, sessionName), nil
}

// gateInvocations is the rig's gate pairs, read once per rig per cycle.
func (r *overrunGateReader) gateInvocations(rigName string) []overrunGate {
	if gates, ok := r.gates[rigName]; ok {
		return gates
	}
	gates := overrunGateInvocations(r.d.config.TownRoot, rigName)
	r.gates[rigName] = gates
	return gates
}

// processTable reads the whole process table once per cycle.
func (r *overrunGateReader) processTable() ([]overrunGateProc, error) {
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
func (r *overrunGateReader) readProcessTable() ([]overrunGateProc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), overrunGateReadTimeout)
	defer cancel()
	cmd := boundDaemonCmd(exec.CommandContext(ctx, "ps", "-ww", "-eo", "pid,ppid,args")) //nolint:gosec // G204: fixed command
	out, _, err := runWith(r.run, cmd)
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return parseOverrunProcessTable(string(out)), nil
}

// parseOverrunPanes picks sessionName's pane pids out of
// `list-panes -a -F "#{session_name}\t#{pane_pid}"` output.
func parseOverrunPanes(out, sessionName string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		name, pidStr, found := strings.Cut(line, "\t")
		if !found || name != sessionName {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// parseOverrunProcessTable parses `ps -ww -eo pid,ppid,args` output: the first
// two fields are fixed and args carries the rest of the line, whitespace
// included.
func parseOverrunProcessTable(out string) []overrunGateProc {
	var procs []overrunGateProc
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue // header line
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		procs = append(procs, overrunGateProc{PID: pid, PPID: ppid, Args: strings.Join(fields[2:], " ")})
	}
	return procs
}

// overrunDescendantArgvs returns the argv of every process descended from one
// of roots, from one snapshot. The roots are excluded: a pane's root is the
// polecat's own agent process, and the question is what that agent has running,
// not whether the agent exists.
func overrunDescendantArgvs(roots []int, procs []overrunGateProc) []string {
	children := make(map[int][]overrunGateProc, len(procs))
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p)
	}
	seen := make(map[int]bool, len(procs))
	var argvs []string
	var walk func(pid int)
	walk = func(pid int) {
		for _, child := range children[pid] {
			if seen[child.PID] {
				continue // a pid reused within the snapshot would otherwise loop
			}
			seen[child.PID] = true
			argvs = append(argvs, child.Args)
			walk(child.PID)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return argvs
}

// overrunGateInvocations reduces the rig's configured test, lint and build
// commands, plus the invocations a polecat runs as its own gate whatever the
// rig configured, to the pairs the pane scan matches. Test, lint and build are
// the gates a polecat runs itself, so a live one means it is waiting on its own
// gate; setup and typecheck are the refinery's gates and run there.
func overrunGateInvocations(townRoot, rigName string) []overrunGate {
	gates := slices.Clone(overrunBareGateInvocations)
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
	if mq == nil {
		return gates
	}
	for _, command := range []string{mq.TestCommand, mq.LintCommand, mq.BuildCommand} {
		if gate, ok := overrunGateFromCommand(command); ok {
			gates = append(gates, gate)
		}
	}
	return gates
}

// overrunBareGateInvocations are the gates recognized in a polecat's pane even
// when the rig's own config names its suite some other way: make gate and make
// test because a Makefile gate runs its suite through them whatever the config
// calls the suite, and the Go quality gate a polecat runs bare.
var overrunBareGateInvocations = []overrunGate{
	{Program: "go", Target: "test"},
	{Program: "go", Target: "vet"},
	{Program: "make", Target: "gate"},
	{Program: "make", Target: "test"},
}

// overrunGateFromCommand reduces a configured gate command to its
// program/target pair. Leading VAR=value assignments are stripped, since the
// formula runs the test command in that shape ("GOFLAGS=-p=8 make test"), and
// the target is the first argument that is not a flag, since only the target
// decides what runs ("make -j4 test" is still make test).
//
// A command that names a program and no argument is not a pair: matching its
// program alone would exempt a seat for any live process whose argv mentions
// the word, so a rig configured that way is left to the bare invocations below.
func overrunGateFromCommand(command string) (overrunGate, bool) {
	_, args := slot.SplitEnvPrefix(overrunGateTokens(strings.TrimSpace(command)))
	if len(args) == 0 {
		return overrunGate{}, false
	}
	gate := overrunGate{Program: strings.ToLower(filepath.Base(args[0]))}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		gate.Target = strings.ToLower(filepath.Base(arg))
		break
	}
	if gate.Target == "" {
		return overrunGate{}, false
	}
	return gate, true
}

// overrunGateArgv reports whether a process's argv is a live gate suite, and
// names it for the log.
func overrunGateArgv(argv string, gates []overrunGate) (string, bool) {
	tokens := overrunGateInvocationTokens(argv)
	if len(tokens) == 0 {
		return "", false
	}
	// gt slot run is the formula's wrapper around a container-backed suite, and
	// the one invocation that is live while holding no slot — the queue wait,
	// which is gate time too.
	if i := findOverrunInvocation(tokens, "gt", "slot"); i >= 0 && i+2 < len(tokens) && tokens[i+2] == "run" {
		return "gt slot run", true
	}
	for _, gate := range gates {
		if overrunGateMatches(tokens, gate) {
			return strings.TrimSpace(gate.Program + " " + gate.Target), true
		}
	}
	return "", false
}

// overrunGateInvocationTokens reduces argv to the tokens the gate matchers
// compare: lowercased, each reduced to its basename. The basename is
// load-bearing — a program reaches the process table as the path it was exec'd
// with ("/Library/Developer/CommandLineTools/usr/bin/make test"), so "make test"
// from the rig's config matches only once both sides are basenames.
func overrunGateInvocationTokens(argv string) []string {
	tokens := overrunGateTokens(argv)
	for i, tok := range tokens {
		tokens[i] = strings.ToLower(filepath.Base(tok))
	}
	return tokens
}

// overrunGateTokens splits argv into the command words it carries, including
// the words inside a wrapper's quoted body: Claude Code runs a background Bash
// through a wrapper whose argv ends in `eval 'make gate' < /dev/null`, so the
// invocation arrives inside one quoted argument. Quotes, backslashes and
// grouping parentheses are word boundaries rather than grouping, and a shell
// separator is a token of its own so a matcher can see where one command ends
// and the next begins.
//
// That flattening is a looser read than the shlex pass internal/cmd's guard
// uses (see the file comment): a quoted *mention* of a gate ("grep -c 'make
// test'") is read as an invocation, so a live process that merely names one is
// charged to the gate for as long as it lives. Hiding a gate the seat really is
// running is the worse error — that is the stop this check exists to prevent —
// so the read leans toward revealing one.
func overrunGateTokens(argv string) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = nil
		}
	}
	for _, r := range argv {
		switch r {
		case ' ', '\t', '\n', '\'', '"', '`', '\\', '(', ')':
			flush()
		case ';', '&', '|':
			flush()
			out = append(out, string(r))
		default:
			word = append(word, r)
		}
	}
	flush()
	return out
}

// overrunGateSeparator reports whether tok ends one command and begins the
// next, so a matcher reaching it must stop reading the current command's
// arguments.
func overrunGateSeparator(tok string) bool {
	switch tok {
	case ";", "&", "|", "&&", "||":
		return true
	}
	return false
}

// findOverrunInvocation returns the index of the program token a target
// follows, or -1. make runs every goal it is given, so its target may follow
// options ("make -j4 test") and other goals; every other program keeps the
// adjacent pair.
func findOverrunInvocation(tokens []string, program, target string) int {
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] != program {
			continue
		}
		if program == "make" {
			for _, tok := range tokens[i+1:] {
				if overrunGateSeparator(tok) {
					break
				}
				if tok == target {
					return i
				}
			}
			continue
		}
		if tokens[i+1] == target {
			return i
		}
	}
	return -1
}

// overrunGateMatches reports whether tokens contain the gate's invocation.
func overrunGateMatches(tokens []string, gate overrunGate) bool {
	return findOverrunInvocation(tokens, gate.Program, gate.Target) >= 0
}
