package tmuxfake

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
)

var _ Sessions = (*Server)(nil)

type session struct {
	workDir string
	command string
	env     map[string]string
	paneCmd string
	screen  []string
	sent    []string
	idle    bool
}

// Server is an in-memory tmux server. Methods mirror *tmux.Tmux signatures.
type Server struct {
	mu       sync.Mutex
	clock    clockwork.Clock
	sessions map[string]*session
	changed  chan struct{} // closed and replaced on every mutation; wakes WaitFor*
}

// New returns an empty server whose waits run on clk.
func New(clk clockwork.Clock) *Server {
	return &Server{clock: clk, sessions: map[string]*session{}, changed: make(chan struct{})}
}

// mutate runs f under the lock and wakes any waiter.
func (s *Server) mutate(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	close(s.changed)
	s.changed = make(chan struct{})
}

// sessionName maps a tmux target ("name", "name:0", "name:0.1", "=name") to
// the session it addresses.
func sessionName(target string) string {
	target = strings.TrimPrefix(target, "=")
	if i := strings.IndexByte(target, ':'); i >= 0 {
		return target[:i]
	}
	return target
}

func (s *Server) NewSession(name, workDir string) error {
	return s.NewSessionWithCommandAndEnv(name, workDir, "", nil)
}

func (s *Server) NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error {
	var err error
	s.mutate(func() {
		if _, ok := s.sessions[name]; ok {
			err = tmux.ErrSessionExists
			return
		}
		e := map[string]string{}
		for k, v := range env {
			e[k] = v
		}
		s.sessions[name] = &session{workDir: workDir, command: command, env: e, paneCmd: paneCommand(command)}
	})
	return err
}

// paneCommand is the name tmux reports as #{pane_current_command} for a pane
// started with command: its first word's base name, or a shell when empty.
func paneCommand(command string) string {
	if f := strings.Fields(command); len(f) > 0 {
		return filepath.Base(f[0])
	}
	return "zsh"
}

func (s *Server) HasSession(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[name]
	return ok, nil
}

func (s *Server) ListSessions() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.sessions))
	for n := range s.sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (s *Server) KillSession(name string) error {
	var err error
	s.mutate(func() {
		if _, ok := s.sessions[name]; !ok {
			err = tmux.ErrSessionNotFound
			return
		}
		delete(s.sessions, name)
	})
	return err
}

func (s *Server) KillSessionWithProcesses(name string) error {
	if err := s.KillSession(name); err != nil && err != tmux.ErrSessionNotFound {
		return err
	}
	return nil
}

func (s *Server) SetEnvironment(session, key, value string) error {
	var err error
	s.mutate(func() {
		ss, ok := s.sessions[session]
		if !ok {
			err = tmux.ErrSessionNotFound
			return
		}
		ss.env[key] = value
	})
	return err
}

func (s *Server) GetEnvironment(session, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[session]
	if !ok {
		return "", tmux.ErrSessionNotFound
	}
	v, ok := ss.env[key]
	if !ok {
		return "", fmt.Errorf("tmux show-environment: unknown variable: %s", key)
	}
	return v, nil
}

func (s *Server) GetPaneCommand(session string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionName(session)]
	if !ok {
		return "", tmux.ErrSessionNotFound
	}
	return ss.paneCmd, nil
}

// tail returns the last n screen lines of session (all of them when n <= 0).
func (s *Server) tail(session string, n int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionName(session)]
	if !ok {
		return nil, tmux.ErrSessionNotFound
	}
	lines := ss.screen
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]string(nil), lines...), nil
}

func (s *Server) CapturePane(session string, lines int) (string, error) {
	l, err := s.tail(session, lines)
	if err != nil {
		return "", err
	}
	return strings.Join(l, "\n"), nil
}

func (s *Server) CapturePaneLines(session string, lines int) ([]string, error) {
	return s.tail(session, lines)
}

func (s *Server) send(session, keys string) error {
	var err error
	s.mutate(func() {
		ss, ok := s.sessions[sessionName(session)]
		if !ok {
			err = tmux.ErrSessionNotFound
			return
		}
		ss.sent = append(ss.sent, keys)
	})
	return err
}

func (s *Server) SendKeys(session, keys string) error    { return s.send(session, keys) }
func (s *Server) SendKeysRaw(session, keys string) error { return s.send(session, keys) }
func (s *Server) SendKeysDebounced(session, keys string, debounceMs int) error {
	return s.send(session, keys)
}
func (s *Server) NudgeSession(session, message string) error { return s.send(session, message) }

func (s *Server) RespawnPane(pane, command string) error {
	var err error
	s.mutate(func() {
		ss, ok := s.sessions[sessionName(pane)]
		if !ok {
			err = tmux.ErrSessionNotFound
			return
		}
		ss.command = command
		ss.paneCmd = paneCommand(command)
	})
	return err
}

func (s *Server) IsIdle(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionName(session)]
	return ok && ss.idle
}

func (s *Server) WaitForIdle(session string, timeout time.Duration) error {
	return s.waitFor(timeout, func() bool {
		ss := s.sessions[sessionName(session)]
		return ss != nil && ss.idle
	})
}

func isShell(cmd string) bool {
	for _, sh := range constants.SupportedShells {
		if cmd == sh {
			return true
		}
	}
	return false
}

func (s *Server) IsAgentRunning(session string, expectedPaneCommands ...string) bool {
	cmd, err := s.GetPaneCommand(session)
	if err != nil {
		return false
	}
	if len(expectedPaneCommands) > 0 {
		for _, e := range expectedPaneCommands {
			if e != "" && cmd == e {
				return true
			}
		}
		return false
	}
	return cmd != "" && !isShell(cmd)
}

func (s *Server) WaitForCommand(session string, excludeCommands []string, timeout time.Duration) error {
	return s.waitFor(timeout, func() bool {
		ss := s.sessions[sessionName(session)]
		if ss == nil {
			return false
		}
		for _, ex := range excludeCommands {
			if ss.paneCmd == ex {
				return false
			}
		}
		return true
	})
}

// WaitForRuntimeReady mirrors *tmux.Tmux: no Tmux config means ready at once;
// no prompt prefix means a fixed ReadyDelayMs wait (capped at timeout);
// otherwise it waits for a screen line starting with the prompt prefix.
func (s *Server) WaitForRuntimeReady(session string, rc *config.RuntimeConfig, timeout time.Duration) error {
	if rc == nil || rc.Tmux == nil {
		return nil
	}
	if rc.Tmux.ReadyPromptPrefix == "" {
		if rc.Tmux.ReadyDelayMs <= 0 {
			return nil
		}
		d := time.Duration(rc.Tmux.ReadyDelayMs) * time.Millisecond
		if d > timeout {
			d = timeout
		}
		s.clock.Sleep(d)
		return nil
	}
	norm := func(x string) string { return strings.ReplaceAll(x, " ", " ") }
	prefix := norm(rc.Tmux.ReadyPromptPrefix)
	err := s.waitFor(timeout, func() bool {
		ss := s.sessions[sessionName(session)]
		if ss == nil {
			return false
		}
		for _, l := range ss.screen {
			l = norm(strings.TrimSpace(l))
			if strings.HasPrefix(l, prefix) || (strings.TrimSpace(prefix) != "" && l == strings.TrimSpace(prefix)) {
				return true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("timeout waiting for runtime prompt")
	}
	return nil
}

// waitFor blocks until cond holds (checked under the lock) or timeout passes on
// the injected clock.
func (s *Server) waitFor(timeout time.Duration, cond func() bool) error {
	deadline := s.clock.After(timeout)
	for {
		s.mu.Lock()
		ok, ch := cond(), s.changed
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-deadline:
			return fmt.Errorf("timeout after %s", timeout)
		}
	}
}

// SetScreen replaces the pane content CapturePane returns for session.
func (s *Server) SetScreen(session string, lines ...string) {
	s.mutate(func() {
		if ss := s.sessions[session]; ss != nil {
			ss.screen = append([]string(nil), lines...)
		}
	})
}

// SetPaneCommand sets what GetPaneCommand reports for session.
func (s *Server) SetPaneCommand(session, cmd string) {
	s.mutate(func() {
		if ss := s.sessions[session]; ss != nil {
			ss.paneCmd = cmd
		}
	})
}

// SetIdle sets whether the agent in session reads as idle.
func (s *Server) SetIdle(session string, idle bool) {
	s.mutate(func() {
		if ss := s.sessions[session]; ss != nil {
			ss.idle = idle
		}
	})
}

// Exit ends session as if its process exited and tmux destroyed it.
func (s *Server) Exit(session string) {
	s.mutate(func() { delete(s.sessions, session) })
}

// Sent returns a copy of everything sent to session, in order.
func (s *Server) Sent(session string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[session]; ss != nil {
		return append([]string(nil), ss.sent...)
	}
	return nil
}

// Env returns a copy of session's environment.
func (s *Server) Env(session string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	if ss := s.sessions[session]; ss != nil {
		for k, v := range ss.env {
			out[k] = v
		}
	}
	return out
}
