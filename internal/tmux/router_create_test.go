package tmux

import (
	"strings"
	"testing"
	"time"
)

// A plain client whose router sends the session to a prefixed client must let
// that client create it: the workDir and binary exist only in the sandbox, and
// the host client's own checks would refuse them (gt-byiyo).
//
//testpolicy:allow parallel — installs the process-wide session router, which another test must not see mid-run
func TestRoutedSessionCreateSkipsHostChecksOnThePlainClient(t *testing.T) {
	workDir := t.TempDir() + "/sandbox-workdir" // never created
	command := "exec env GT_ROLE=polecat " + t.TempDir() + "/claude-code"

	creates := map[string]func(tm *Tmux) error{
		"NewSessionWithCommand": func(tm *Tmux) error {
			return tm.NewSessionWithCommand("gt-r-n", workDir, command)
		},
		"NewSessionWithCommandAndEnv": func(tm *Tmux) error {
			return tm.NewSessionWithCommandAndEnv("gt-r-n", workDir, command, map[string]string{"GT_ROLE": "polecat"})
		},
	}
	for name, create := range creates {
		t.Run(name, func(t *testing.T) {
			var hostCalls [][]string
			s := newScripted(nil)
			clk := newFixedClock()
			routed := newPrefixedForTest("gt", s, clk)
			routed.sock = newFakeSockets().ops()
			routed.socketDir = "/fake-sock"
			host := newTmuxForTest("gt-test", recordExec(&hostCalls, ""), clk)
			SetSessionRouter(func(session string) *Tmux {
				if session == "gt-r-n" {
					return routed
				}
				return nil
			})
			t.Cleanup(func() { SetSessionRouter(nil) })

			done := make(chan error, 1)
			go func() { done <- create(host) }()
			if err := driveClock(t, clk, 50*time.Millisecond, done); err != nil {
				t.Fatalf("%s through the router = %v; want nil, workDir and command exist only in the sandbox", name, err)
			}
			if len(hostCalls) != 0 {
				t.Fatalf("host client ran %v, want nothing", hostCalls)
			}
			created := 0
			for _, c := range s.all() {
				if prog, args := wrapped(c); prog == "tmux" && len(tmuxSub(args)) > 0 && tmuxSub(args)[0] == "new-session" {
					created++
				}
			}
			if created != 1 {
				t.Fatalf("new-session through the wrapper = %d, want 1", created)
			}
		})
	}
}

// With no router the plain client still refuses a missing workDir and a
// missing command binary.
func TestUnroutedSessionCreateStillChecksHostPaths(t *testing.T) {
	t.Parallel()
	host := newTmuxForTest("gt-test", recordExec(new([][]string), ""), nil)
	err := host.NewSessionWithCommand("gt-r-n", t.TempDir()+"/missing", "sleep 1")
	if err == nil || !strings.Contains(err.Error(), "invalid work directory") {
		t.Fatalf("NewSessionWithCommand = %v, want invalid work directory", err)
	}
	err = host.NewSessionWithCommandAndEnv("gt-r-n", t.TempDir()+"/missing", "sleep 1", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid work directory") {
		t.Fatalf("NewSessionWithCommandAndEnv = %v, want invalid work directory", err)
	}
	binary := t.TempDir() + "/claude-code" // never created
	err = host.NewSessionWithCommand("gt-r-n", t.TempDir(), binary)
	if err == nil || !strings.Contains(err.Error(), "command binary not found") {
		t.Fatalf("NewSessionWithCommand = %v, want command binary not found", err)
	}
	err = host.NewSessionWithCommandAndEnv("gt-r-n", t.TempDir(), binary, nil)
	if err == nil || !strings.Contains(err.Error(), "command binary not found") {
		t.Fatalf("NewSessionWithCommandAndEnv = %v, want command binary not found", err)
	}
}
