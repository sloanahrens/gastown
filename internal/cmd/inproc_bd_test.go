package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
)

// inprocBD answers bd calls in process, in place of a shell-script bd stub
// put first on PATH. PATH is process state, so a test with a stub on it
// cannot run in parallel; a *beads.Beads built with inprocBD.run as its
// BDRunner never looks at PATH.
//
// answer sees each call the way the shell stubs did after their
// `while [ "$1" = "--allow-stale" ]; do shift; done; cmd="$1"; shift`
// prologue: the subcommand, then the rest of argv. The --allow-stale
// capability probe gets no answer, so argv stays unprefixed.
type inprocBD struct {
	mu     sync.Mutex
	lines  []string
	answer func(f *inprocBD, cmd string, args []string) bdAnswer
}

// bdAnswer is one call's reply. A non-zero code fails the call with that
// exit status, as bd's own exit would.
type bdAnswer struct {
	stdout, stderr string
	code           int
}

// bdOut answers stdout with exit 0.
func bdOut(stdout string) bdAnswer { return bdAnswer{stdout: stdout} }

// inprocBDExit is a bd exit status, as *exec.ExitError carries one.
type inprocBDExit int

func (e inprocBDExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e inprocBDExit) ExitCode() int { return int(e) }

func (f *inprocBD) run(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
	args := c.Args
	for len(args) > 0 && args[0] == "--allow-stale" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, nil, nil
	}
	cmd, rest := args[0], args[1:]
	if cmd == "version" {
		return nil, nil, nil
	}
	a := f.answer(f, cmd, rest)
	if a.code != 0 {
		return []byte(a.stdout), []byte(a.stderr), inprocBDExit(a.code)
	}
	return []byte(a.stdout), []byte(a.stderr), nil
}

// logLine appends one line to the log, as a stub's `echo ... >> log` did.
func (f *inprocBD) logLine(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, line)
}

// log is the whole log, one line per logLine, as the stub's log file read.
func (f *inprocBD) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lines) == 0 {
		return ""
	}
	return strings.Join(f.lines, "\n") + "\n"
}

// logged reports whether line was logged exactly (`grep -qx line log`).
func (f *inprocBD) logged(line string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lines {
		if l == line {
			return true
		}
	}
	return false
}

// nonFlagArgs is args without the --flags, as the stubs' `case "$arg" in
// --*) continue` loops read them.
func nonFlagArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			out = append(out, a)
		}
	}
	return out
}

// argsMention is the stubs' `echo "$*" | grep -q -- needle`.
func argsMention(args []string, needle string) bool {
	return strings.Contains(strings.Join(args, " "), needle)
}

// envMap is a getenv over a fixed set of variables; any other is unset.
func envMap(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// firstArg is the stubs' beadID="$1".
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
