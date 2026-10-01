package tmux

import (
	"errors"
	"testing"
)

// These run the session-freshness and agent-liveness logic against
// fakeServer: which pane command and process tree a session has is set up
// directly instead of waited for in a real pane.

func TestEnsureSessionFresh_NoExistingSession(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	tm, _ := f.tmux(nil)
	if err := tm.EnsureSessionFresh("gt-x", ""); err != nil {
		t.Fatalf("EnsureSessionFresh: %v", err)
	}
	if !f.has("gt-x") {
		t.Error("expected session to exist after EnsureSessionFresh")
	}
}

// TestEnsureSessionFresh_ZombieSession: a session with no agent running is a
// zombie; it is killed and recreated rather than erroring with "session
// already exists".
func TestEnsureSessionFresh_ZombieSession(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	old := f.addSession("gt-x", "bash")
	clk := newFixedClock()
	tm, s := f.tmux(clk)
	if alive, err := tm.IsAgentAliveChecked("gt-x"); err != nil || alive {
		t.Fatal("fixture: a bare shell must not read as a live agent")
	}
	if err := driven(t, clk, processKillGracePeriod, func() error { return tm.EnsureSessionFresh("gt-x", "") }); err != nil {
		t.Fatalf("EnsureSessionFresh on zombie: %v", err)
	}
	if got := f.session("gt-x"); got == nil || got == old {
		t.Fatal("zombie session was not replaced")
	}
	if len(s.find("kill-session")) != 1 {
		t.Errorf("kill-session calls = %v, want the zombie killed once", s.find("kill-session"))
	}
}

// TestEnsureSessionFresh_KeepsLiveAgent: a session whose pane runs a
// non-shell command is left alone.
func TestEnsureSessionFresh_KeepsLiveAgent(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	live := f.addSession("gt-x", "claude")
	tm, s := f.tmux(nil)
	if err := tm.EnsureSessionFresh("gt-x", ""); err != nil {
		t.Fatalf("EnsureSessionFresh: %v", err)
	}
	if f.session("gt-x") != live || len(s.find("kill-session")) != 0 {
		t.Fatal("a session with a running agent was replaced")
	}
}

func TestEnsureSessionFresh_IdempotentOnZombie(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	clk := newFixedClock()
	tm, _ := f.tmux(clk)
	for i := 0; i < 3; i++ {
		if err := driven(t, clk, processKillGracePeriod, func() error { return tm.EnsureSessionFresh("gt-x", "") }); err != nil {
			t.Fatalf("EnsureSessionFresh attempt %d: %v", i+1, err)
		}
	}
	if !f.has("gt-x") {
		t.Error("expected session to exist after multiple EnsureSessionFresh calls")
	}
}

func TestEnsureSessionFreshWithCommand_NoExisting(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	err := createAndDrive(t, newScripted(f.answer), func(tm *Tmux) error {
		return tm.EnsureSessionFreshWithCommand("gt-x", "", "sleep 10")
	})
	if err != nil {
		t.Fatalf("EnsureSessionFreshWithCommand: %v", err)
	}
	if cmd, _ := unitTmux(newScripted(f.answer), nil).GetPaneCommand("gt-x"); cmd != "sleep" {
		t.Errorf("pane command = %q, want sleep", cmd)
	}
}

func TestEnsureSessionFreshWithCommand_KillsZombie(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	old := f.addSession("gt-x", "bash")
	err := createAndDrive(t, newScripted(f.answer), func(tm *Tmux) error {
		return tm.EnsureSessionFreshWithCommand("gt-x", "", "sleep 10")
	})
	if err != nil {
		t.Fatalf("EnsureSessionFreshWithCommand on zombie: %v", err)
	}
	s := f.session("gt-x")
	if s == nil || s == old || s.panes[0].cmd != "sleep" {
		t.Fatalf("zombie not replaced by a session running sleep: %+v", s)
	}
}

func TestEnsureSessionFreshWithCommand_RefusesLiveAgent(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "claude")
	tm, _ := f.tmux(nil)
	if err := tm.EnsureSessionFreshWithCommand("gt-x", "", "sleep 10"); !errors.Is(err, ErrSessionRunning) {
		t.Fatalf("err = %v, want ErrSessionRunning", err)
	}
	if err := tm.EnsureSessionFreshWithCommandAndEnv("gt-x", "", "sleep 10", nil); !errors.Is(err, ErrSessionRunning) {
		t.Fatalf("AndEnv err = %v, want ErrSessionRunning", err)
	}
}

func TestEnsureSessionFresh_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := unitTmux(s, nil)
	for _, err := range []error{
		tm.EnsureSessionFresh("bad name", ""),
		tm.EnsureSessionFreshWithCommand("bad.name", "", "sleep 1"),
		tm.NewSession("bad:name", ""),
	} {
		if !errors.Is(err, ErrInvalidSessionName) {
			t.Errorf("err = %v, want ErrInvalidSessionName", err)
		}
	}
	if len(s.all()) != 0 {
		t.Errorf("calls = %v, want none for invalid names", s.all())
	}
}

func TestIsAgentRunning(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "bash")
	tm, _ := f.tmux(nil)

	tests := []struct {
		name         string
		processNames []string
		wantRunning  bool
	}{
		{"empty process list: a shell is not an agent", []string{}, false},
		{"matching shell process", []string{"bash"}, true},
		{"claude agent (node) - not running", []string{"node"}, false},
		{"multiple process names with match", []string{"nonexistent", "bash", "also-nonexistent"}, true},
		{"multiple process names without match", []string{"nonexistent1", "nonexistent2"}, false},
		{"empty names never match", []string{""}, false},
	}
	for _, tt := range tests {
		if got := tm.IsAgentRunning("gt-x", tt.processNames...); got != tt.wantRunning {
			t.Errorf("%s: IsAgentRunning(%v) = %v, want %v", tt.name, tt.processNames, got, tt.wantRunning)
		}
	}
	f.with(func() { f.sessions["gt-x"].panes[0].cmd = "claude" })
	if !tm.IsAgentRunning("gt-x") {
		t.Error("a non-shell pane command reads as an agent with no expected names")
	}
}

func TestIsAgentRunning_NonexistentSession(t *testing.T) {
	t.Parallel()
	tm, _ := newFakeServer().tmux(nil)
	if tm.IsAgentRunning("nonexistent-session-xyz", "node", "gemini", "cursor-agent") {
		t.Error("IsAgentRunning on nonexistent session should return false")
	}
}

func TestIsRuntimeRunningChecked_NonexistentSessionErrors(t *testing.T) {
	t.Parallel()
	tm, _ := newFakeServer().tmux(nil)
	running, err := tm.IsRuntimeRunningChecked("gt-missing", []string{"sleep"})
	if err == nil {
		t.Fatal("expected checked runtime query to return an error for missing session")
	}
	if running {
		t.Fatal("expected missing session to report not running")
	}
	if tm.IsRuntimeRunning("gt-missing", []string{"sleep"}) {
		t.Fatal("legacy bool wrapper should collapse query errors to false")
	}
}

func TestIsRuntimeRunning(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "bash")
	tm, _ := f.tmux(nil)
	if tm.IsRuntimeRunning("gt-x", []string{"node", "claude"}) {
		t.Error("IsRuntimeRunning() = true for a bare shell")
	}
	if tm.IsRuntimeRunning("gt-x", nil) {
		t.Error("IsRuntimeRunning() = true with no process names")
	}
}

func TestIsRuntimeRunning_ShellWithNodeChild(t *testing.T) {
	t.Parallel()
	t.Run("direct", func(t *testing.T) {
		f := newFakeServer()
		f.addSession("gt-x", "sleep")
		tm, _ := f.tmux(nil)
		if !tm.IsRuntimeRunning("gt-x", []string{"sleep"}) {
			t.Error("IsRuntimeRunning should return true for direct process")
		}
	})
	// Shell+child: the pane command is sh and the agent is a descendant, as
	// for an npm-installed agent.
	t.Run("shell_with_child", func(t *testing.T) {
		f := newFakeServer()
		s := f.addSession("gt-x", "sh")
		f.spawn(s.panes[0].pid, "sleep")
		tm, _ := f.tmux(nil)
		if !tm.IsRuntimeRunning("gt-x", []string{"sleep"}) {
			t.Error("IsRuntimeRunning should return true for child process")
		}
	})
	// Version-as-argv[0]: the pane reports "2.1.30" but ps names the binary.
	t.Run("version_named_pane", func(t *testing.T) {
		f := newFakeServer()
		s := f.addSession("gt-x", "2.1.30")
		f.with(func() { f.procs[s.panes[0].pid].comm = "/usr/local/bin/claude" })
		tm, _ := f.tmux(nil)
		if !tm.IsRuntimeRunning("gt-x", []string{"claude"}) {
			t.Error("IsRuntimeRunning should match the process binary behind a version-named pane")
		}
	})
	// A secondary pane running the agent is found by the all-panes scan.
	t.Run("agent_in_second_pane", func(t *testing.T) {
		f := newFakeServer()
		f.addSession("gt-x", "bash")
		f.addPane("gt-x", "claude")
		tm, _ := f.tmux(nil)
		if !tm.IsRuntimeRunning("gt-x", []string{"claude"}) {
			t.Error("IsRuntimeRunning missed an agent in a second pane")
		}
	})
	// GT_PANE_ID pins the check to the declared pane (gt-qmsx).
	t.Run("declared_pane", func(t *testing.T) {
		f := newFakeServer()
		s := f.addSession("gt-x", "claude")
		f.addPane("gt-x", "bash")
		f.with(func() { s.env["GT_PANE_ID"] = s.panes[1].id })
		tm, _ := f.tmux(nil)
		if tm.IsRuntimeRunning("gt-x", []string{"claude"}) {
			t.Error("IsRuntimeRunning looked past the declared (shell) pane")
		}
	})
}

// TestGetPaneCommand_MultiPane verifies that GetPaneCommand, GetPanePID and
// GetPaneWorkDir target the first window/pane explicitly, so a split pane
// that became active cannot make health checks see its shell (gs-2v7).
func TestGetPaneCommand_MultiPane(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "sleep")
	f.addPane("gt-x", "bash")
	tm, s := f.tmux(nil)

	if cmd, err := tm.GetPaneCommand("gt-x"); err != nil || cmd != "sleep" {
		t.Fatalf("GetPaneCommand = %q, %v; want sleep", cmd, err)
	}
	if _, err := tm.GetPanePID("gt-x"); err != nil {
		t.Fatal(err)
	}
	if wd, err := tm.GetPaneWorkDir("gt-x"); err != nil || wd != "/fake-cwd" {
		t.Fatalf("GetPaneWorkDir = %q, %v; want the pane's directory", wd, err)
	}
	targets := map[string]string{}
	for _, c := range s.find("display-message") {
		targets[c.last()] = flagValue(c.args, "-t")
	}
	for format, want := range map[string]string{
		"#{pane_current_command}": "gt-x:^",
		"#{pane_pid}":             "gt-x:^",
		"#{pane_current_path}":    "gt-x:0.0",
	} {
		if targets[format] != want {
			t.Errorf("%s targeted %q, want %q", format, targets[format], want)
		}
	}
	// A pane-ID target is used as is.
	if _, err := tm.GetPanePID("%2"); err != nil {
		t.Fatal(err)
	}
	if last := s.find("display-message"); flagValue(last[len(last)-1].args, "-t") != "%2" {
		t.Errorf("GetPanePID(%%2) targeted %v", last[len(last)-1])
	}
}
