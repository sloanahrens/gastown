package doctor

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
)

// bdScript is a beads.BDRunner for the unit tier (CheckContext.bdRun): it
// records every bd call and answers it with answer (stdout, stderr and an
// exit code, 0 for success). A nil answer is emptyBeads.
type bdScript struct {
	mu     sync.Mutex
	calls  []beads.BDCall
	answer func(c beads.BDCall) (stdout, stderr string, code int)
}

func (s *bdScript) run(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	answer := s.answer
	s.mu.Unlock()
	if answer == nil {
		answer = emptyBeads
	}
	stdout, stderr, code := answer(c)
	if code != 0 {
		return []byte(stdout), []byte(stderr), bdExit(code)
	}
	return []byte(stdout), []byte(stderr), nil
}

// ctx is a CheckContext for townRoot whose bd calls s answers.
func (s *bdScript) ctx(townRoot, rigName string) *CheckContext {
	return &CheckContext{TownRoot: townRoot, RigName: rigName, bdRun: s.run}
}

// commands returns "<subcommand> <first argument> dir=<dir>" for each call
// whose subcommand is one of names.
func (s *bdScript) commands(names ...string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		cmd, rest := bdCommand(c)
		for _, n := range names {
			if cmd == n {
				first := flagValue(rest, "id")
				for _, a := range rest {
					if first == "" && !strings.HasPrefix(a, "-") {
						first = a
					}
				}
				out = append(out, fmt.Sprintf("%s %s dir=%s", cmd, first, c.Dir))
			}
		}
	}
	return out
}

// bdExit is a bd exit status, matched like *exec.ExitError.
type bdExit int

func (e bdExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e bdExit) ExitCode() int { return int(e) }

// bdCommand is c's subcommand and the arguments after it, skipping the
// global flags (--allow-stale and the like) before it.
func bdCommand(c beads.BDCall) (string, []string) {
	for i, a := range c.Args {
		if !strings.HasPrefix(a, "-") {
			return a, c.Args[i+1:]
		}
	}
	return "", nil
}

// flagValue is the value of --name=value in args.
func flagValue(args []string, name string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v
		}
	}
	return ""
}

// emptyBeads answers as a database holding no issues and no wisps: list is
// empty, show finds nothing, create echoes the new bead, anything else
// succeeds silently.
func emptyBeads(c beads.BDCall) (string, string, int) {
	cmd, rest := bdCommand(c)
	switch cmd {
	case "version":
		return "bd version 1.0.0\n", "", 0
	case "list":
		return "[]\n", "", 0
	case "mol":
		if len(rest) >= 2 && rest[0] == "wisp" && rest[1] == "list" {
			return `{"wisps":[]}` + "\n", "", 0
		}
		return "", "", 1
	case "show":
		return "", "Error: no issue found\n", 1
	case "create":
		return fmt.Sprintf(`{"id":%q,"title":%q,"status":"open","labels":["gt:agent"]}`+"\n", flagValue(rest, "id"), flagValue(rest, "title")), "", 0
	case "update":
		return "{}\n", "", 0
	}
	return "", "", 0
}

// noBeadsDatabase answers every bd call as bd does where it finds no beads
// database.
func noBeadsDatabase(beads.BDCall) (string, string, int) {
	return "", "Error: no beads database found\n", 1
}

// noBD is a CheckContext for townRoot whose bd finds no database anywhere.
func noBD(townRoot string) *CheckContext {
	return (&bdScript{answer: noBeadsDatabase}).ctx(townRoot, "")
}
