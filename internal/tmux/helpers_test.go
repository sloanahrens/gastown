package tmux

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// newTmuxForTest builds a Tmux whose processes and clock are supplied by the
// test. A nil ex or clk falls back to the real one.
func newTmuxForTest(socket string, ex execFunc, clk clockwork.Clock) *Tmux {
	return &Tmux{socketName: socket, exec: ex, clock: clk}
}

// driveClock advances clk by step every time a goroutine blocks on it, until
// done delivers a value. It fails the test if nothing blocks within 10 s.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}

// exitError is an error carrying a process exit code, as *exec.ExitError does.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// reply is one canned answer from a scripted runner.
type reply struct {
	stdout string
	stderr string
	err    error
}

func ok(stdout string) reply { return reply{stdout: stdout} }

// fail answers with stderr and exit status 1, the shape tmux uses for every
// error that wrapError classifies.
func fail(stderr string) reply { return reply{stderr: stderr, err: exitError(1)} }

// tmuxCall is one recorded invocation, with the "-u -L <socket>" prefix of a
// tmux call stripped so tests read the subcommand first.
type tmuxCall struct {
	name string   // program: "tmux", "ps", "kill", ...
	args []string // for tmux: subcommand and its arguments
}

func (c tmuxCall) sub() string {
	if len(c.args) == 0 {
		return ""
	}
	return c.args[0]
}

func (c tmuxCall) String() string { return c.name + " " + strings.Join(c.args, " ") }

// has reports whether args contains every one of want, in order, contiguously.
func (c tmuxCall) has(want ...string) bool {
	for i := 0; i+len(want) <= len(c.args); i++ {
		match := true
		for j, w := range want {
			if c.args[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// scripted is an execFunc that records every call and answers through a
// test-supplied function. It is safe for concurrent use.
type scripted struct {
	mu     sync.Mutex
	calls  []tmuxCall
	answer func(c tmuxCall) reply
}

// newScripted returns a scripted runner. A nil answer replies ok("") to
// everything.
func newScripted(answer func(c tmuxCall) reply) *scripted {
	if answer == nil {
		answer = func(tmuxCall) reply { return ok("") }
	}
	return &scripted{answer: answer}
}

func (s *scripted) exec(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	c := tmuxCall{name: name, args: append([]string(nil), args...)}
	if name == "tmux" {
		a := c.args
		if len(a) > 0 && a[0] == "-u" {
			a = a[1:]
		}
		if len(a) > 1 && a[0] == "-L" {
			a = a[2:]
		}
		c.args = a
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	r := s.answer(c)
	return []byte(r.stdout), []byte(r.stderr), r.err
}

// all returns a copy of every recorded call.
func (s *scripted) all() []tmuxCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tmuxCall(nil), s.calls...)
}

// subs returns the recorded tmux subcommands in order.
func (s *scripted) subs() []string {
	var out []string
	for _, c := range s.all() {
		if c.name == "tmux" {
			out = append(out, c.sub())
		}
	}
	return out
}

// find returns the recorded tmux calls whose subcommand is sub.
func (s *scripted) find(sub string) []tmuxCall {
	var out []tmuxCall
	for _, c := range s.all() {
		if c.name == "tmux" && c.sub() == sub {
			out = append(out, c)
		}
	}
	return out
}

// named returns the recorded calls to program name.
func (s *scripted) named(name string) []tmuxCall {
	var out []tmuxCall
	for _, c := range s.all() {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// bySub builds an answer function from a map of tmux subcommand to reply.
// Unlisted subcommands and non-tmux programs reply ok("").
func bySub(m map[string]reply) func(tmuxCall) reply {
	return func(c tmuxCall) reply {
		if c.name != "tmux" {
			return ok("")
		}
		if r, found := m[c.sub()]; found {
			return r
		}
		return ok("")
	}
}

// unitTmux returns a Tmux on socket gt-test-unit driven by s and clk.
func unitTmux(s *scripted, clk clockwork.Clock) *Tmux {
	if clk == nil {
		clk = clockwork.NewFakeClock()
	}
	return newTmuxForTest("gt-test-unit", s.exec, clk)
}
