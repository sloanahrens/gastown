package tmux

import (
	"reflect"
	"strings"
	"testing"
	"time"

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

// A prefixed Tmux's socket is inside the sandbox. A file of the same name on
// this host is not that socket, so the new-session guard must not refuse over
// it, and the create must run through the wrapper (gt-gi2ur).
func TestNewTmuxWithExecPrefixNewSessionIgnoresHostSocket(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/fake-sock/gt", sockFile)
	s := newScripted(nil)
	tm := newPrefixedForTest("gt", s, newFixedClock())
	tm.sock = fs.ops()
	tm.socketDir = "/fake-sock"

	if err := tm.NewSession("gt-r-n", ""); err != nil {
		t.Fatalf("NewSession = %v; the host-side guard must not run for a prefixed Tmux", err)
	}
	var created []tmuxCall
	for _, c := range s.all() {
		if prog, args := wrapped(c); prog == "tmux" && tmuxSub(args)[0] == "new-session" {
			created = append(created, c)
		}
	}
	if len(created) != 1 || created[0].name != "openshell" ||
		!created[0].has("sandbox", "exec", "-n", "gt-r-n", "--no-tty", "--no-login-shell", "--", "tmux", "-u", "-L", "gt", "new-session") {
		t.Fatalf("new-session calls = %v, want one through the wrapper", created)
	}
	if len(fs.removed) != 0 {
		t.Fatalf("host socket files removed: %v", fs.removed)
	}
}

// KillServer's socket-file cleanup reads the host's socket directory, which a
// prefixed Tmux does not use: a stale host file at its name is left alone.
func TestNewTmuxWithExecPrefixKillServerLeavesHostSocketAlone(t *testing.T) {
	t.Parallel()
	fs := newFakeSockets()
	fs.set("/fake-sock/gt", sockStale)
	s := newScripted(nil)
	tm := newPrefixedForTest("gt", s, newFixedClock())
	tm.sock = fs.ops()
	tm.socketDir = "/fake-sock"

	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer = %v", err)
	}
	if len(fs.removed) != 0 {
		t.Fatalf("host socket files removed: %v", fs.removed)
	}
	calls := s.all()
	if len(calls) != 1 || calls[0].name != "openshell" || !calls[0].has("--", "tmux", "-u", "-L", "gt", "kill-server") {
		t.Fatalf("calls = %v, want one kill-server through the wrapper", calls)
	}
}

// Helpers that switch to another socket (the split-brain check runs on
// "default") keep the wrapper, so their commands also reach the sandbox.
func TestNewTmuxWithExecPrefixWithSocketKeepsPrefix(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := newPrefixedForTest("gt", s, nil)

	other := tm.withSocket("default")
	if got := other.ExecPrefix(); !reflect.DeepEqual(got, openshellPrefix()) {
		t.Fatalf("withSocket(default).ExecPrefix() = %v, want %v", got, openshellPrefix())
	}
	if _, err := other.HasSession("gt-r-n"); err != nil {
		t.Fatalf("HasSession on default socket: %v", err)
	}
	calls := s.all()
	if len(calls) != 1 || calls[0].name != "openshell" || !calls[0].has("--", "tmux", "-u", "-L", "default", "has-session") {
		t.Fatalf("calls = %v, want one has-session through the wrapper", calls)
	}
}

// createPrefixedAndDrive runs create on a prefixed Tmux whose clock the test
// advances through the session health check's waits.
func createPrefixedAndDrive(t *testing.T, s *scripted, create func(tm *Tmux) error) error {
	t.Helper()
	clk := newFixedClock()
	tm := newPrefixedForTest("gt", s, clk)
	tm.sock = newFakeSockets().ops()
	tm.socketDir = "/fake-sock"
	done := make(chan error, 1)
	go func() { done <- create(tm) }()
	return driveClock(t, clk, 50*time.Millisecond, done)
}

// A prefixed Tmux's session runs inside the sandbox, so its workDir and the
// command's binary exist only there: session creation must not stat either on
// this host, where both are missing (gt-jqg8s).
func TestNewTmuxWithExecPrefixSessionSkipsHostPathChecks(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir() + "/sandbox-workdir" // never created
	binary := t.TempDir() + "/claude-code"      // never created
	command := "exec env GT_ROLE=polecat " + binary + " --settings /sandbox/settings.json"

	tests := []struct {
		name   string
		create func(tm *Tmux) error
	}{
		{"NewSessionWithCommand", func(tm *Tmux) error {
			return tm.NewSessionWithCommand("gt-r-n", workDir, command)
		}},
		{"NewSessionWithCommandAndEnv", func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("gt-r-n", workDir, command, map[string]string{"GT_ROLE": "polecat"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(nil)
			if err := createPrefixedAndDrive(t, s, tt.create); err != nil {
				t.Fatalf("%s = %v; want nil, workDir and command exist only inside the sandbox", tt.name, err)
			}
			var created []tmuxCall
			for _, c := range s.all() {
				if prog, args := wrapped(c); prog == "tmux" && len(tmuxSub(args)) > 0 && tmuxSub(args)[0] == "new-session" {
					created = append(created, c)
				}
			}
			if len(created) != 1 {
				t.Fatalf("new-session calls = %v, want one through the wrapper", created)
			}
		})
	}
}

// A Tmux with no exec prefix runs on this host, so the workDir and binary
// checks still run and still refuse what is missing (gt-jqg8s).
func TestNewSessionWithoutExecPrefixRejectsMissingHostPaths(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir() + "/sandbox-workdir" // never created
	binary := t.TempDir() + "/claude-code"      // never created
	command := "exec env GT_ROLE=polecat " + binary

	tests := []struct {
		name   string
		create func(tm *Tmux) error
		want   string
	}{
		{"NewSessionWithCommand/missing-workDir", func(tm *Tmux) error {
			return tm.NewSessionWithCommand("gt-x", workDir, "true")
		}, "invalid work directory"},
		{"NewSessionWithCommand/missing-binary", func(tm *Tmux) error {
			return tm.NewSessionWithCommand("gt-x", "", command)
		}, "command binary not found"},
		{"NewSessionWithCommandAndEnv/missing-workDir", func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("gt-x", workDir, "true", nil)
		}, "invalid work directory"},
		{"NewSessionWithCommandAndEnv/missing-binary", func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("gt-x", "", command, nil)
		}, "command binary not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(nil)
			err := tt.create(unitTmux(s, nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
			if got := s.find("new-session"); len(got) != 0 {
				t.Fatalf("created a session for a path missing on this host: %v", got)
			}
		})
	}
}
