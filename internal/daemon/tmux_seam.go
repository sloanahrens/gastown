package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/tmux"
)

// sessionTmux is the tmux surface the daemon drives directly. *tmux.Tmux is
// the production implementation; unit tests hand the Daemon an in-memory one
// built on tmuxfake, so no tmux, ps or kill runs and none of *tmux.Tmux's
// real-time waits (nudge debounce, kill grace period) are paid.
type sessionTmux interface {
	HasSession(name string) (bool, error)
	KillSession(name string) error
	KillSessionWithProcesses(name string) error
	IsAgentAliveChecked(session string) (bool, error)
	PaneDead(session string) (bool, error)
	CheckSessionHealth(session string, maxInactivity time.Duration) tmux.ZombieStatus
	IsAvailable() bool
	GetSessionCreatedTime(name string) (time.Time, error)
	GetPaneID(session string) (string, error)
	SetEnvironment(session, key, value string) error
	CapturePane(session string, lines int) (string, error)
	NudgeSession(session, message string) error
	DetectComposerStallTracked(session string, frozenFor time.Duration, clock *tmux.PendingInputClock) (tmux.ComposerStall, error)
	SubmitPendingInput(target string, queued bool) error
	EnsureSessionFreshWithCommandAndEnv(name, workDir, command string, env map[string]string) error
	WaitForCommand(session string, excludeCommands []string, timeout time.Duration) error
	AcceptStartupDialogs(session string) error
	ConfigureGasTownSession(session string, theme *tmux.Theme, rig, worker, role string) error
}

var _ sessionTmux = (*tmux.Tmux)(nil)

// dogSessions is the dog session surface the handler drives:
// *dog.SessionManager in production, a fake over the test's tmux in tests.
type dogSessions interface {
	SessionName(dogName string) string
	IsRunning(dogName string) (bool, error)
	Start(dogName string, opts dog.SessionStartOptions) error
	Stop(dogName string, force bool) error
}

var _ dogSessions = (*dog.SessionManager)(nil)
