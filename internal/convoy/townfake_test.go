package convoy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
)

// bdScript is a beads.BDRunner for the unit tier: it records every bd call
// and answers it with answer (stdout, stderr and an exit code, 0 for
// success). A nil answer prints nothing and succeeds.
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
		return nil, nil, nil
	}
	stdout, stderr, code := answer(c)
	if code != 0 {
		return []byte(stdout), []byte(stderr), bdExit(code)
	}
	return []byte(stdout), []byte(stderr), nil
}

// argvs returns each call's arguments joined by spaces.
func (s *bdScript) argvs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// ran returns the calls whose first positional argument is cmd.
func (s *bdScript) ran(cmd string) []beads.BDCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []beads.BDCall
	for _, c := range s.calls {
		if pos := positional(c.Args); len(pos) > 0 && pos[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

// positional drops the flags from args, leaving the subcommand and its
// operands.
func positional(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// bdExit is a bd exit status, matched like *exec.ExitError.
type bdExit int

func (e bdExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e bdExit) ExitCode() int { return int(e) }

// gtCall is one gt notice child: its directory, environment and arguments.
type gtCall struct {
	Dir  string
	Env  []string
	Args []string
}

// gtScript is a gtRunner that records every gt call and fails none.
type gtScript struct {
	mu    sync.Mutex
	calls []gtCall
}

func (s *gtScript) run(dir string, env []string, args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, gtCall{Dir: dir, Env: env, Args: args})
	return nil
}

func (s *gtScript) recorded() []gtCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gtCall(nil), s.calls...)
}

// testTown is a Town at root whose bd and gt calls go to bd and gt.
func testTown(root string, bd *bdScript, gt *gtScript) Town {
	t := Town{Root: root}
	if bd != nil {
		t.Run = bd.run
	}
	if gt != nil {
		t.gtRun = gt.run
	}
	return t
}
