package polecat

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// fakeSessionTmux is a tmux for the SessionManager: fakeProbe's sessions and
// liveness, plus the calls Start makes that tmuxfake does not model, each
// answered as a healthy, unblocked session would.
type fakeSessionTmux struct {
	*fakeProbe
}

var _ sessionTmux = (*fakeSessionTmux)(nil)

func newFakeSessionTmux() *fakeSessionTmux { return &fakeSessionTmux{newFakeProbe()} }

func (f *fakeSessionTmux) missing(session string) error {
	if ok, _ := f.HasSession(session); !ok {
		return fmt.Errorf("can't find session: %s", session)
	}
	return nil
}

func (f *fakeSessionTmux) GetSessionInfo(name string) (*tmux.SessionInfo, error) {
	if err := f.missing(name); err != nil {
		return nil, err
	}
	return &tmux.SessionInfo{Name: name, Windows: 1}, nil
}

func (f *fakeSessionTmux) GetPaneID(session string) (string, error) {
	if err := f.missing(session); err != nil {
		return "", err
	}
	return "%1", nil
}

func (f *fakeSessionTmux) ConfigureGasTownSession(session string, _ *tmux.Theme, _, _, _ string) error {
	return f.missing(session)
}

func (f *fakeSessionTmux) SetPaneDiedHook(session, _ string) error { return f.missing(session) }
func (f *fakeSessionTmux) AcceptStartupDialogs(session string) error {
	return f.missing(session)
}
func (f *fakeSessionTmux) CheckStartupBlocked(string) error { return nil }
func (f *fakeSessionTmux) AttachSession(session string) error {
	return f.missing(session)
}

func (f *fakeSessionTmux) CheckSessionHealth(session string, _ time.Duration) tmux.ZombieStatus {
	if f.missing(session) != nil {
		return tmux.SessionDead
	}
	return tmux.SessionHealthy
}

// newTestSessionManager is NewSessionManager with a fake tmux and git
// answered by a fresh world.
func newTestSessionManager(r *rig.Rig) *SessionManager {
	return &SessionManager{tmux: newFakeSessionTmux(), rig: r, gits: newWorld().opener()}
}
