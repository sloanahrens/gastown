package tmux

import (
	"reflect"
	"testing"

	"github.com/jonboulle/clockwork"
)

// openshellPrefix is the wrapper a host-side client uses to drive a tmux
// server inside the sandbox gt-r-n.
func openshellPrefix() []string {
	return []string{"openshell", "sandbox", "exec", "-n", "gt-r-n", "--no-tty", "--no-login-shell", "--"}
}

// newPrefixedForTest builds a Tmux through NewTmuxWithExecPrefix whose
// processes are recorded by s and whose clock is supplied by the test.
func newPrefixedForTest(socket string, s *scripted, clk clockwork.Clock) *Tmux {
	tm := NewTmuxWithExecPrefix(socket, openshellPrefix())
	tm.exec = s.exec
	tm.clock = clk
	tm.getenv = func(string) string { return "" }
	return tm
}

// wrapped splits a recorded call into the program after the wrapper's "--"
// and its arguments. A call that carries no wrapper reads as itself.
func wrapped(c tmuxCall) (string, []string) {
	for i, a := range c.args {
		if a == "--" && i+1 < len(c.args) {
			return c.args[i+1], c.args[i+2:]
		}
	}
	return c.name, c.args
}

// answering dispatches a recorded call on the wrapped program and its
// arguments.
func answering(fn func(program string, args []string) reply) func(tmuxCall) reply {
	return func(c tmuxCall) reply {
		prog, args := wrapped(c)
		return fn(prog, args)
	}
}

// tmuxSub drops the "-u [-L socket]" preamble a wrapped tmux argv still
// carries, so a test can read the subcommand first.
func tmuxSub(args []string) []string {
	for len(args) > 0 {
		switch args[0] {
		case "-u":
			args = args[1:]
		case "-L":
			args = args[2:]
		default:
			return args
		}
	}
	return args
}

func TestNewTmuxWithExecPrefixHasSession(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := newPrefixedForTest("gt", s, nil)

	found, err := tm.HasSession("gt-r-n")
	if err != nil || !found {
		t.Fatalf("HasSession = %v, %v; want true, nil", found, err)
	}
	want := []string{"sandbox", "exec", "-n", "gt-r-n", "--no-tty", "--no-login-shell", "--",
		"tmux", "-u", "-L", "gt", "has-session", "-t", "=gt-r-n"}
	calls := s.all()
	if len(calls) != 1 || calls[0].name != "openshell" || !reflect.DeepEqual(calls[0].args, want) {
		t.Fatalf("calls = %v, want one openshell %v", calls, want)
	}
	if got := tm.ExecPrefix(); !reflect.DeepEqual(got, openshellPrefix()) {
		t.Fatalf("ExecPrefix() = %v, want %v", got, openshellPrefix())
	}
}

func TestExecPrefixReturnsACopy(t *testing.T) {
	t.Parallel()
	tm := newPrefixedForTest("gt", newScripted(nil), nil)

	tm.ExecPrefix()[0] = "changed"
	if got := tm.ExecPrefix()[0]; got != "openshell" {
		t.Fatalf("ExecPrefix()[0] = %q after the caller modified a returned slice, want openshell", got)
	}
}

func TestNewTmuxWithEmptyExecPrefixRunsDirectly(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := NewTmuxWithExecPrefix("gt", nil)
	tm.exec = s.exec
	tm.getenv = func(string) string { return "" }

	if _, err := tm.HasSession("gt-r-n"); err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	want := tmuxCall{name: "tmux", socket: "gt", args: []string{"has-session", "-t", "=gt-r-n"}}
	if calls := s.all(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// IsAgentAliveChecked's ps walks (the pane's descendants) read the sandbox's
// process table, so they run through the wrapper too (gt-gi2ur).
func TestNewTmuxWithExecPrefixAgentAliveReadsProcessesInside(t *testing.T) {
	t.Parallel()
	s := newScripted(answering(func(program string, args []string) reply {
		switch program {
		case "tmux":
			sub := tmuxSub(args)
			switch sub[0] {
			case "show-environment":
				if sub[len(sub)-1] == "GT_PROCESS_NAMES" {
					return ok("GT_PROCESS_NAMES=claude")
				}
				return ok(sub[len(sub)-1] + "=")
			case "display-message":
				switch sub[len(sub)-1] {
				case "#{pane_current_command}":
					return ok("zsh")
				case "#{pane_pid}":
					return ok("123")
				}
			}
		case "ps":
			return ok("  123     1 -zsh\n  124   123 claude\n")
		}
		return ok("")
	}))
	tm := newPrefixedForTest("gt", s, nil)

	alive, err := tm.IsAgentAliveChecked("gt-r-n")
	if err != nil || !alive {
		t.Fatalf("IsAgentAliveChecked = %v, %v; want true, nil", alive, err)
	}
	if bypassed := s.named("tmux"); len(bypassed) != 0 {
		t.Fatalf("tmux was run on the host: %v", bypassed)
	}
	var psCalls []tmuxCall
	for _, c := range s.all() {
		if prog, _ := wrapped(c); prog == "ps" {
			psCalls = append(psCalls, c)
		}
	}
	if len(psCalls) == 0 {
		t.Fatalf("no ps call recorded; calls = %v", s.all())
	}
	for _, c := range psCalls {
		if c.name != "openshell" || !c.has("sandbox", "exec", "-n", "gt-r-n", "--no-tty", "--no-login-shell", "--", "ps") {
			t.Fatalf("ps ran outside the wrapper: %s", c)
		}
	}
}

// KillSessionWithProcesses signals PIDs from the sandbox's process table, so
// its kill calls go through the wrapper as well (gt-gi2ur).
func TestNewTmuxWithExecPrefixKillSignalsInside(t *testing.T) {
	t.Parallel()
	clk := newFixedClock()
	s := newScripted(answering(func(program string, args []string) reply {
		switch program {
		case "tmux":
			if sub := tmuxSub(args); sub[0] == "display-message" && sub[len(sub)-1] == "#{pane_pid}" {
				return ok("42")
			}
		case "ps":
			if args[0] == "-axo" {
				return ok("   42     1 -zsh\n   43    42 sleep\n")
			}
			if args[0] == "-o" && args[1] == "pgid=" {
				return ok("0")
			}
		}
		return ok("")
	}))
	tm := newPrefixedForTest("gt", s, clk)

	done := make(chan error, 1)
	go func() { done <- tm.KillSessionWithProcesses("gt-r-n") }()
	if err := driveClock(t, clk, processKillGracePeriod, done); err != nil {
		t.Fatalf("KillSessionWithProcesses: %v", err)
	}

	var kills []string
	for _, c := range s.all() {
		prog, args := wrapped(c)
		if prog != "kill" {
			continue
		}
		if c.name != "openshell" || !c.has("sandbox", "exec", "-n", "gt-r-n", "--no-tty", "--no-login-shell", "--", "kill") {
			t.Fatalf("kill ran outside the wrapper: %s", c)
		}
		kills = append(kills, args[0]+" "+args[1])
	}
	want := []string{"-TERM 43", "-KILL 43", "-TERM 42", "-KILL 42"}
	if !reflect.DeepEqual(kills, want) {
		t.Fatalf("kill signals = %v, want %v", kills, want)
	}
}
