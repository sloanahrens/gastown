package daemon

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

var _ sessionTmux = (*fakeTmux)(nil)

// fakeTmux is the daemon's sessionTmux for unit tests: tmuxfake's in-memory
// server for the session surface it shares with *tmux.Tmux, plus the few
// daemon-only calls (creation time, composer probes, startup dialogs), which
// it answers from state the test sets and records.
type fakeTmux struct {
	*tmuxfake.Server
	clock clockwork.Clock

	mu          sync.Mutex
	created     map[string]time.Time
	createdErr  error
	unavailable bool
	hasErr      error
	stalls      map[string][]tmux.ComposerStall // per session, consumed in order; the last repeats
	stallErr    map[string]error
	calls       []string // daemon-only calls, "<method> <session>", in order
}

// newFakeTmux returns an empty fake whose waits and creation times run on clk.
func newFakeTmux(clk clockwork.Clock) *fakeTmux {
	return &fakeTmux{
		Server:   tmuxfake.New(clk),
		clock:    clk,
		created:  map[string]time.Time{},
		stalls:   map[string][]tmux.ComposerStall{},
		stallErr: map[string]error{},
	}
}

// addSession creates session running command (empty for a bare shell), as
// tmux would report it created at created.
func (f *fakeTmux) addSession(name, command string, created time.Time) {
	if err := f.NewSessionWithCommandAndEnv(name, "", command, nil); err != nil {
		panic(fmt.Sprintf("fakeTmux.addSession(%q): %v", name, err))
	}
	f.mu.Lock()
	f.created[name] = created
	f.mu.Unlock()
}

func (f *fakeTmux) record(method, session string) {
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+session)
	f.mu.Unlock()
}

// daemonCalls returns the recorded daemon-only calls.
func (f *fakeTmux) daemonCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeTmux) HasSession(name string) (bool, error) {
	f.mu.Lock()
	err := f.hasErr
	f.mu.Unlock()
	if err != nil {
		return false, err
	}
	return f.Server.HasSession(name)
}

// IsAgentAlive reports whether the pane runs something other than a shell,
// the fake's stand-in for *tmux.Tmux's process-tree check.
func (f *fakeTmux) IsAgentAlive(session string) bool {
	return f.IsAgentRunning(session)
}

// CheckSessionHealth mirrors *tmux.Tmux without the activity level, which
// needs window activity the fake does not model: SessionDead for a missing
// session, AgentDead for a pane back at a shell, else SessionHealthy.
func (f *fakeTmux) CheckSessionHealth(session string, _ time.Duration) tmux.ZombieStatus {
	if alive, err := f.HasSession(session); err != nil || !alive {
		return tmux.SessionDead
	}
	if !f.IsAgentAlive(session) {
		return tmux.AgentDead
	}
	return tmux.SessionHealthy
}

func (f *fakeTmux) IsAvailable() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unavailable
}

// GetSessionCreatedTime answers like *tmux.Tmux: ErrSessionNotFound for a
// missing session, else the time addSession recorded (the clock's time for a
// session created some other way).
func (f *fakeTmux) GetSessionCreatedTime(name string) (time.Time, error) {
	f.mu.Lock()
	err := f.createdErr
	created, ok := f.created[name]
	f.mu.Unlock()
	if err != nil {
		return time.Time{}, err
	}
	if has, _ := f.Server.HasSession(name); !has {
		return time.Time{}, tmux.ErrSessionNotFound
	}
	if !ok {
		return f.clock.Now(), nil
	}
	return created, nil
}

func (f *fakeTmux) GetPaneID(session string) (string, error) {
	if has, _ := f.Server.HasSession(session); !has {
		return "", fmt.Errorf("tmux display-message: can't find pane: %s:0.0: %w", session, tmux.ErrPaneNotFound)
	}
	return "%1", nil
}

// DetectComposerStallTracked replays the stalls set for session, in order,
// repeating the last one.
func (f *fakeTmux) DetectComposerStallTracked(session string, _ time.Duration, _ *tmux.PendingInputClock) (tmux.ComposerStall, error) {
	f.record("DetectComposerStallTracked", session)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stallErr[session]; err != nil {
		return tmux.ComposerStall{}, err
	}
	q := f.stalls[session]
	if len(q) == 0 {
		return tmux.ComposerStall{}, nil
	}
	s := q[0]
	if len(q) > 1 {
		f.stalls[session] = q[1:]
	}
	return s, nil
}

func (f *fakeTmux) SubmitPendingInput(target string, _ bool) error {
	f.record("SubmitPendingInput", target)
	return nil
}

// EnsureSessionFreshWithCommandAndEnv replaces any existing session, as the
// real one does for a zombie.
func (f *fakeTmux) EnsureSessionFreshWithCommandAndEnv(name, workDir, command string, env map[string]string) error {
	f.record("EnsureSessionFresh", name)
	_ = f.KillSession(name)
	return f.NewSessionWithCommandAndEnv(name, workDir, command, env)
}

func (f *fakeTmux) AcceptStartupDialogs(session string) error {
	f.record("AcceptStartupDialogs", session)
	return nil
}

func (f *fakeTmux) ConfigureGasTownSession(session string, _ *tmux.Theme, _, _, _ string) error {
	f.record("ConfigureGasTownSession", session)
	return nil
}

// errNoSessionDate is what a fakeTmux set with createdErr answers for a
// session tmux cannot date: *tmux.Tmux fails to parse an empty
// #{session_created}.
var errNoSessionDate = errors.New(`parsing session created time "": EOF`)
