package polecat

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/slot"
)

// The one definition of "this polecat seat has a live verification suite,
// right now", asked by both guards that need to know whether a seat is working
// rather than gone: the daemon's overrun gate exemption
// (internal/daemon/overrun_guard_gate.go) and the cmd Stop-hook guard
// (internal/cmd/tap_polecat_stop.go) (gt-a1xhd).
//
// The two guards disagree about policy, not about the question, so every
// disagreement is an input here rather than a second copy: which role
// spellings count as the seat's own (SuiteQuery.Roles), which invocations
// count as a suite (SuiteQuery.Suites), how an argv is tokenized
// (SuiteQuery.Split), and what an unreadable read means (LiveSuite returns the
// error and the caller maps it).

// Invocation is one command reduced to the pair of tokens that identify it in
// another process's argv: the program and its first argument that is not a
// flag, both lowercased basenames.
type Invocation struct {
	Program string
	Target  string
}

// SplitFunc tokenizes an argv string into the command words the invocations are
// matched against.
type SplitFunc func(argv string) []string

// Proc is one row of the process table a pane scan reads: the pid, its parent,
// and the full argv.
type Proc struct {
	PID  int
	PPID int
	Args string
}

// SuiteReads supplies the evidence one live-suite question needs. Each read
// returns the error it failed with; how a read's failure is judged stays with
// the caller.
type SuiteReads interface {
	// PanePIDs lists the pane pids of a session.
	PanePIDs(sessionName string) ([]int, error)
	// ProcessTable reads the process table.
	ProcessTable() ([]Proc, error)
	// Slots is the container-gate pool's flock picture.
	Slots() (slot.Report, error)
}

// SuiteQuery is one seat's live-suite question.
type SuiteQuery struct {
	// Session is the seat's tmux session name; every pane of it is scanned.
	Session string
	// Roles are the role strings a gate run of this seat holds a
	// container-gate slot under.
	Roles []string
	// Suites are the invocations that count as a live suite, in match order.
	Suites []Invocation
	// Split tokenizes an argv; nil is GateTokens.
	Split SplitFunc
}

// Found is what a live-suite check found, one of the two and never both.
type Found struct {
	// Slot is the container-gate pool slot the seat's own suite holds. Held is
	// false when the suite was found in the seat's pane instead.
	Slot slot.SlotState
	// Invocation names the suite live under the seat's own pane, empty when the
	// suite holds a slot instead.
	Invocation string
}

// Busy reports whether a suite was found at all.
func (f Found) Busy() bool { return f.Slot.Held || f.Invocation != "" }

// LiveSuite reports whether the seat has a live verification suite — a
// container-gate pool slot held under one of its own roles, or a suite
// invocation live under one of its own panes — and says what it found.
//
// A read that fails does not end the question: the reads are asked in turn and
// a failed one leaves the reads after it to answer, so a slot read the host
// refuses still lets the pane scan find the suite the seat is running. The Stop
// hook asks the slot read first, and short-circuiting it there hid a live pane
// suite from it (gt-a1xhd).
//
// The error comes out with whatever was found, and each caller maps it: the
// overrun guard counts all of the seat's time on any error, the direction that
// stops it, while the Stop hook reads an error as "not busy" unless a suite was
// found, its turn-boundary semantics.
func LiveSuite(reads SuiteReads, q SuiteQuery) (Found, error) {
	rep, slotErr := reads.Slots()
	if slotErr != nil {
		slotErr = fmt.Errorf("container-gate slot status: %w", slotErr)
	} else if held, ok := SlotHeldBy(rep, q.Roles); ok {
		return Found{Slot: held}, nil
	}

	name, paneErr := paneSuite(reads, q)
	switch {
	case paneErr != nil && slotErr != nil:
		return Found{}, slotErr // the first read that failed is the one to report
	case paneErr != nil:
		return Found{}, paneErr
	case name != "":
		return Found{Invocation: name}, slotErr
	}
	return Found{}, slotErr
}

// paneSuite names a suite invocation live under one of the seat's own panes, ""
// when the panes hold none.
func paneSuite(reads SuiteReads, q SuiteQuery) (string, error) {
	panes, err := reads.PanePIDs(q.Session)
	if err != nil {
		return "", err
	}
	if len(panes) == 0 {
		return "", fmt.Errorf("no pane found for session %s", q.Session)
	}
	procs, err := reads.ProcessTable()
	if err != nil {
		return "", err
	}

	split := q.Split
	if split == nil {
		split = GateTokens
	}
	// Every pane of the session is scanned, not just the first: a split session
	// can hold the suite in another pane, and a suite this misses reads as a
	// seat that is not gating.
	for _, argv := range DescendantArgvs(panes, procs) {
		if name, ok := SuiteArgv(argv, split, q.Suites); ok {
			return name, nil
		}
	}
	return "", nil
}

// SlotHeldBy returns the first pool slot held under one of roles.
func SlotHeldBy(rep slot.Report, roles []string) (slot.SlotState, bool) {
	for _, role := range roles {
		if held := rep.HeldBy(role); len(held) > 0 {
			return held[0], true
		}
	}
	return slot.SlotState{}, false
}

// ParsePanes picks sessionName's pane pids out of
// `list-panes -a -F "#{session_name}\t#{pane_pid}"` output.
func ParsePanes(out, sessionName string) []int {
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

// ParseProcessTable parses `ps -ww -eo pid,ppid,args` output: the first two
// fields are fixed and args carries the rest of the line, whitespace included.
func ParseProcessTable(out string) []Proc {
	var procs []Proc
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
		procs = append(procs, Proc{PID: pid, PPID: ppid, Args: strings.Join(fields[2:], " ")})
	}
	return procs
}

// DescendantArgvs returns the argv of every process descended from one of
// roots, from one snapshot. The roots are excluded: a pane's root is the
// polecat's own agent process, and the question is what that agent has running,
// not whether the agent exists.
func DescendantArgvs(roots []int, procs []Proc) []string {
	children := make(map[int][]Proc, len(procs))
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

// GateTokens splits argv into the command words it carries, including the words
// inside a wrapper's quoted body: Claude Code runs a background Bash through a
// wrapper whose argv ends in `eval 'make gate' < /dev/null`, so the invocation
// arrives inside one quoted argument. Quotes, backslashes and grouping
// parentheses are word boundaries rather than grouping, and a shell separator
// is a token of its own so a matcher can see where one command ends and the
// next begins.
//
// That flattening is looser than the Stop hook's shlex pass: a quoted *mention*
// of a gate ("grep -c 'make test'") is read as an invocation, so a live process
// that merely names one is charged to the gate for as long as it lives. Hiding
// a gate the seat really is running is the worse error — that is the stop this
// check exists to prevent — so the read leans toward revealing one.
func GateTokens(argv string) []string {
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

// InvocationTokens reduces argv to the tokens the invocation matchers compare:
// split's words, lowercased, each reduced to its basename.
//
// The basename is load-bearing. A program reaches the process table as the path
// it was exec'd with ("/Library/Developer/CommandLineTools/usr/bin/make test"),
// so "make test" from a rig's config matches only once both sides are basenames.
func InvocationTokens(argv string, split SplitFunc) []string {
	if split == nil {
		split = GateTokens
	}
	tokens := split(argv)
	for i, tok := range tokens {
		tokens[i] = strings.ToLower(filepath.Base(tok))
	}
	return tokens
}

// InvocationFromCommand reduces a configured command to its program/target
// pair. Leading VAR=value assignments are stripped, since the formula runs the
// test command in that shape ("GOFLAGS=-p=8 make test"), and the target is the
// first argument that is not a flag, since only the target decides what runs
// ("make -j4 test" is still make test).
//
// A command that names a program and no argument is still an Invocation with an
// empty target: what to do with one is the caller's, and the two guards differ
// there (see overrunGateFromCommand and stopCheckGateFromCommand).
func InvocationFromCommand(split SplitFunc, command string) (Invocation, bool) {
	if split == nil {
		split = GateTokens
	}
	_, args := slot.SplitEnvPrefix(split(strings.TrimSpace(command)))
	if len(args) == 0 {
		return Invocation{}, false
	}
	inv := Invocation{Program: strings.ToLower(filepath.Base(args[0]))}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if strings.HasPrefix(arg, "-") {
			if invocationProgram(inv.Program, "make") && makeOptionTakesNext(arg, rest[i+1:]) {
				i++
			}
			continue
		}
		inv.Target = strings.ToLower(filepath.Base(arg))
		break
	}
	return inv, true
}

// makeOptionTakesNext reports whether a make option consumes the word after it
// as its value ("make -C sub test", "make -j 4 test"), so that word is not the
// goal. -j takes one only when it is a number, as GNU make reads it; -j test
// leaves test as the goal. The attached forms ("-j4", "-Csub") carry their value
// in the same word and take none.
func makeOptionTakesNext(opt string, after []string) bool {
	switch opt {
	case "-C", "-f", "-I", "-o", "-W",
		"--directory", "--file", "--makefile", "--include-dir", "--old-file", "--assume-old", "--what-if", "--assume-new", "--new-file":
		return len(after) > 0
	case "-j", "--jobs":
		return len(after) > 0 && isDigits(after[0])
	}
	return false
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// InvocationIndex returns the index of the program token the target follows, or
// -1. make runs every goal it is given, so its target may follow options and
// other goals ("make -j4 test"); every other program keeps the adjacent pair.
func InvocationIndex(tokens []string, program, target string) int {
	for i := 0; i+1 < len(tokens); i++ {
		if !invocationProgram(tokens[i], program) {
			continue
		}
		if program == "make" {
			for _, tok := range tokens[i+1:] {
				if isTokenSeparator(tok) {
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

// invocationProgram reports whether tok names program in an argv. make is also
// reached as gmake, the name GNU make carries on macOS, so a seat running
// "gmake test" is running the rig's configured "make test" and its goals are
// read the same way through either name (gt-y1zks, gt-gy5ro). Any other program
// keeps the exact word. Both sides are InvocationTokens' reading, already
// lowercased basenames, so no path needs stripping here.
func invocationProgram(tok, program string) bool {
	return tok == program || (program == "make" && tok == "gmake")
}

// isTokenSeparator reports whether tok ends one command and begins the next, so
// a matcher reaching it must stop reading the current command's arguments.
func isTokenSeparator(tok string) bool {
	switch tok {
	case ";", "&", "|", "&&", "||":
		return true
	}
	return false
}

// InvocationPresent reports whether tokens carry the invocation. An invocation
// with no target matches its program anywhere, the reading a rig whose config
// names a bare program gets (gmake included, as invocationProgram reads it).
func InvocationPresent(tokens []string, inv Invocation) bool {
	if inv.Target == "" {
		return slices.ContainsFunc(tokens, func(tok string) bool { return invocationProgram(tok, inv.Program) })
	}
	return InvocationIndex(tokens, inv.Program, inv.Target) >= 0
}

// SuiteArgv reports whether a process's argv is a live verification suite for
// the given invocations, and names it for a log or a deferral message.
func SuiteArgv(argv string, split SplitFunc, suites []Invocation) (string, bool) {
	tokens := InvocationTokens(argv, split)
	if len(tokens) == 0 {
		return "", false
	}
	// gt slot run is the formula's wrapper around a container-backed suite, and
	// the one invocation that is live while holding no slot — the queue wait,
	// which is gate time too.
	if i := InvocationIndex(tokens, "gt", "slot"); i >= 0 && i+2 < len(tokens) && tokens[i+2] == "run" {
		return "gt slot run", true
	}
	for _, suite := range suites {
		if InvocationPresent(tokens, suite) {
			return strings.TrimSpace(suite.Program + " " + suite.Target), true
		}
	}
	return "", false
}

// ConfiguredCommands returns the rig's configured test, lint and build
// commands: the gates a polecat runs itself. Setup and typecheck are the
// refinery's gates and run there.
func ConfiguredCommands(townRoot, rigName string) []string {
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
	if mq == nil {
		return nil
	}
	return []string{mq.TestCommand, mq.LintCommand, mq.BuildCommand}
}
