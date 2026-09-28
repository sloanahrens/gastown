package tmux

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// Tests for the two-step session creation (new-session + respawn-pane) and
// checkSessionAfterCreate health check introduced to eliminate blank windows.

// paneState answers display-message queries for a pane that is alive (dead
// "0") or dead with the given exit status.
func paneState(dead, status string) func(tmuxCall) reply {
	return func(c tmuxCall) reply {
		if c.name != "tmux" || c.sub() != "display-message" {
			return ok("")
		}
		switch c.last() {
		case "#{pane_dead}":
			return ok(dead)
		case "#{pane_dead_status}":
			return ok(status)
		}
		return ok("")
	}
}

// createAndDrive runs create on a fresh fake clock and advances it through
// checkSessionAfterCreate's waits.
func createAndDrive(t *testing.T, s *scripted, create func(tm *Tmux) error) error {
	t.Helper()
	clk := clockwork.NewFakeClock()
	tm := unitTmux(s, clk)
	done := make(chan error, 1)
	go func() { done <- create(tm) }()
	return driveClock(t, clk, 50*time.Millisecond, done)
}

// TestNewSessionWithCommand_BadBinary verifies that NewSessionWithCommand returns
// an error when the command binary doesn't exist, instead of leaving a dead session.
func TestNewSessionWithCommand_BadBinary(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	if err := unitTmux(s, nil).NewSessionWithCommand("gt-x", "", "/nonexistent/binary --flag"); err == nil {
		t.Error("NewSessionWithCommand should return error for missing binary")
	}
	if got := s.find("new-session"); len(got) != 0 {
		t.Errorf("created a session for a missing binary: %v", got)
	}
}

// TestNewSessionWithCommand_BadWorkDir verifies workDir validation rejects
// non-existent directories before creating the session.
func TestNewSessionWithCommand_BadWorkDir(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	if err := unitTmux(s, nil).NewSessionWithCommand("gt-x", "/tmp/gastown-nonexistent-dir-99999", "echo hello"); err == nil {
		t.Error("NewSessionWithCommand should return error for non-existent workDir")
	}
	if got := s.all(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

// TestNewSessionWithCommand_ExecEnvBadBinary verifies the exact gastown polecat
// startup pattern (exec env VAR=val binary) returns an error for missing binaries.
func TestNewSessionWithCommand_ExecEnvBadBinary(t *testing.T) {
	t.Parallel()
	cmd := `exec env GT_TEST=1 GT_ROLE=test /nonexistent/claude-code --settings /tmp`
	if err := unitTmux(newScripted(nil), nil).NewSessionWithCommand("gt-x", "", cmd); err == nil {
		t.Error("NewSessionWithCommand should return error for exec env with missing binary")
	}
}

// TestNewSessionWithCommand_Success checks the two-step create: a shell
// session, remain-on-exit on, the command respawned into the pane in workDir,
// and remain-on-exit restored once the pane is still alive.
func TestNewSessionWithCommand_Success(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := `exec env GT_RIG=testrig GT_POLECAT=testcat sleep 5`
	s := newScripted(paneState("0", ""))
	if err := createAndDrive(t, s, func(tm *Tmux) error { return tm.NewSessionWithCommand("gt-x", dir, cmd) }); err != nil {
		t.Fatalf("NewSessionWithCommand failed: %v", err)
	}

	var order []string
	for _, c := range s.all() {
		switch {
		case c.sub() == "new-session":
			if !c.has("-d", "-s", "gt-x", "-c", dir) {
				t.Errorf("new-session = %v", c)
			}
			order = append(order, "new")
		case c.has("remain-on-exit", "on"):
			order = append(order, "remain-on")
		case c.sub() == "respawn-pane":
			if !c.has("-k", "-t", "gt-x", "-c", dir, cmd) {
				t.Errorf("respawn-pane = %v, want the command in workDir", c)
			}
			order = append(order, "respawn")
		case c.has("remain-on-exit", "off"):
			order = append(order, "remain-off")
		}
	}
	if strings.Join(order, ",") != "new,remain-on,respawn,remain-off" {
		t.Fatalf("create order = %v", order)
	}
	// Identity variables are cleared from the server's global environment
	// before the session exists (gt-xyr).
	if unset := s.find("set-environment"); len(unset) == 0 || !unset[0].has("-g", "-u", "CLAUDECODE") {
		t.Errorf("set-environment calls = %v, want CLAUDECODE unset globally", unset)
	}
}

// TestNewSessionWithCommand_CommandExitsNonZero is checkSessionAfterCreate's
// reason to exist: a command that dies at once reports its status and the
// dead session is killed.
func TestNewSessionWithCommand_CommandExitsNonZero(t *testing.T) {
	t.Parallel()
	s := newScripted(paneState("1", "127"))
	err := createAndDrive(t, s, func(tm *Tmux) error { return tm.NewSessionWithCommand("gt-x", "", "nosuchcmd") })
	if err == nil || !strings.Contains(err.Error(), "status 127") {
		t.Fatalf("err = %v, want exit status 127", err)
	}
	if len(s.find("kill-session")) == 0 {
		t.Error("dead session was not killed")
	}
}

// TestNewSessionWithCommand_CommandExitsCleanly: a zero exit is not an error,
// but the dead session is still cleaned up.
func TestNewSessionWithCommand_CommandExitsCleanly(t *testing.T) {
	t.Parallel()
	s := newScripted(paneState("1", "0"))
	if err := createAndDrive(t, s, func(tm *Tmux) error { return tm.NewSessionWithCommand("gt-x", "", "true") }); err != nil {
		t.Fatalf("err = %v, want nil for a clean exit", err)
	}
	if len(s.find("kill-session")) == 0 {
		t.Error("dead session was not killed")
	}
}

// TestNewSessionWithCommand_Duplicate verifies duplicate session creation is rejected.
func TestNewSessionWithCommand_Duplicate(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"new-session": fail("duplicate session: gt-x")}))
	err := unitTmux(s, nil).NewSessionWithCommand("gt-x", "", "sleep 10")
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err = %v, want ErrSessionExists", err)
	}
	if got := s.find("respawn-pane"); len(got) != 0 {
		t.Errorf("respawned into an existing session: %v", got)
	}
}

// TestNewSessionWithCommandAndEnv_PassesSortedEnv checks the -e flags reach
// new-session in a stable order, before the command is respawned.
func TestNewSessionWithCommandAndEnv_PassesSortedEnv(t *testing.T) {
	t.Parallel()
	s := newScripted(paneState("0", ""))
	env := map[string]string{"GT_ROLE": "witness", "BEADS_DOLT_PORT": "3307"}
	if err := createAndDrive(t, s, func(tm *Tmux) error {
		return tm.NewSessionWithCommandAndEnv("gt-x", "", "sleep 30", env)
	}); err != nil {
		t.Fatal(err)
	}
	ns := s.find("new-session")
	if len(ns) != 1 || !ns[0].has("-e", "BEADS_DOLT_PORT=3307", "-e", "GT_ROLE=witness") {
		t.Fatalf("new-session = %v, want sorted -e flags", ns)
	}
}

// TestWaitForCommand_Timeout verifies WaitForCommand returns an error when the
// pane command remains a shell (agent never started).
func TestWaitForCommand_Timeout(t *testing.T) {
	t.Parallel()
	s := newScripted(func(c tmuxCall) reply {
		if c.sub() == "display-message" {
			return ok("bash")
		}
		if c.sub() == "show-environment" {
			return fail("unknown variable: GT_AGENT_READY")
		}
		return ok("")
	})
	clk := clockwork.NewFakeClock()
	tm := unitTmux(s, clk)
	done := make(chan error, 1)
	go func() { done <- tm.WaitForCommand("gt-x", []string{"bash", "zsh", "sh"}, 500*time.Millisecond) }()
	if err := driveClock(t, clk, 100*time.Millisecond, done); err == nil {
		t.Error("WaitForCommand should timeout when shell is still running")
	}
	if u := s.find("set-environment"); len(u) == 0 || !u[0].has("-u", "-t", "gt-x", EnvAgentReady) {
		t.Errorf("WaitForCommand did not clear the stale %s sentinel first: %v", EnvAgentReady, u)
	}
}

// TestWaitForCommand_AgentReadySentinel is the ZFC fallback: a wrapped agent
// whose pane still reads as a shell counts once its hook sets GT_AGENT_READY.
func TestWaitForCommand_AgentReadySentinel(t *testing.T) {
	t.Parallel()
	s := newScripted(func(c tmuxCall) reply {
		switch c.sub() {
		case "display-message":
			return ok("bash")
		case "show-environment":
			return ok(EnvAgentReady + "=1")
		}
		return ok("")
	})
	clk := clockwork.NewFakeClock()
	done := make(chan error, 1)
	go func() { done <- unitTmux(s, clk).WaitForCommand("gt-x", []string{"bash"}, time.Second) }()
	if err := driveClock(t, clk, 100*time.Millisecond, done); err != nil {
		t.Fatalf("WaitForCommand = %v, want nil once the sentinel is set", err)
	}
}

// TestSanitizeNudgeMessage verifies control character stripping.
func TestSanitizeNudgeMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"passthrough", "hello world", "hello world"},
		{"strips ESC", "hello\x1bworld", "helloworld"},
		{"strips CR", "hello\rworld", "helloworld"},
		{"tab to space", "hello\tworld", "hello world"},
		{"preserves newline", "hello\nworld", "hello\nworld"},
		{"preserves unicode", "hello 世界", "hello 世界"},
		{"strips BS", "hello\x08world", "helloworld"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeNudgeMessage(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeNudgeMessage(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestContainsRewindIndicators verifies detection of Claude Code's Rewind menu.
func TestContainsRewindIndicators(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"normal prompt", "❯ hello world", false},
		{"busy indicator", "⏵⏵ Running tool... esc to interrupt", false},
		{"rewind with enter and esc", "Rewind\nPress Enter to select, Esc to go back", true},
		{"rewind case insensitive", "rewind history\nenter to continue\nesc to exit", true},
		{"enter to continue + esc to exit", "Some UI\nEnter to continue\nEsc to exit", true},
		{"enter to accept + esc to cancel", "Enter to accept changes\nEsc to cancel", true},
		{"enter to select + esc to cancel", "Choose a checkpoint:\nEnter to select\nEsc to cancel", true},
		{"only rewind no actions", "Rewind history shown here", false},
		{"only enter no esc", "Enter to continue", false},
		{"only esc no enter", "Esc to exit", false},
		{"conversation mentioning rewind", "User said: please rewind the video\n❯ ", false},
		{"partial match no pair", "Enter to continue\nSome other text", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsRewindIndicators(tt.content)
			if got != tt.want {
				t.Errorf("containsRewindIndicators(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestSendMessageToTarget_Chunking verifies that long messages are chunked,
// each chunk sent literally with "--" and nothing lost.
func TestSendMessageToTarget_Chunking(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	clk := clockwork.NewFakeClock()
	msg := strings.Repeat("A", 600)
	done := make(chan error, 1)
	go func() { done <- unitTmux(s, clk).sendMessageToTarget("gt-x", msg) }()
	if err := driveClock(t, clk, 10*time.Millisecond, done); err != nil {
		t.Fatalf("sendMessageToTarget: %v", err)
	}
	sends := s.find("send-keys")
	if len(sends) < 2 {
		t.Fatalf("send-keys calls = %d, want the message split into chunks", len(sends))
	}
	var got strings.Builder
	for _, c := range sends {
		if !c.has("-t", "gt-x", "-l", "--") || len(c.last()) > sendKeysChunkSize {
			t.Fatalf("chunk call = %v", c)
		}
		got.WriteString(c.last())
	}
	if got.String() != msg {
		t.Fatalf("reassembled %d bytes, want the 600-byte message", got.Len())
	}
}
