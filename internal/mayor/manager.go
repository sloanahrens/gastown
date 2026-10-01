package mayor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/bdgate"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Common errors
var (
	ErrNotRunning     = errors.New("mayor not running")
	ErrAlreadyRunning = errors.New("mayor already running")
	ErrACPActive      = errors.New("ACP mayor is active")
)

// Mode represents the mayor session mode.
type Mode string

const (
	ModeTMUX Mode = "tmux"
	ModeACP  Mode = "acp"
	ModeBoth Mode = "both"
	ModeNone Mode = "none"
)

// MayorStatus represents the combined status of the mayor across all modes.
type MayorStatus struct {
	Active  bool
	Mode    Mode
	Tmux    *tmux.SessionInfo
	ACPPid  int
	Running bool // Deprecated: use Active
}

// Manager handles mayor lifecycle operations.
type Manager struct {
	townRoot string

	// StopKill, when set, ends the session in Stop: the supervisor's logged
	// operator stop for gt mayor stop (gt-4k3fj.4.1). nil kills through
	// tmux, as the daemon's restart executor does once the supervisor has
	// already guarded and recorded the restart.
	StopKill func(sessionID string) error
	// Respawn, when set, runs StartTMUX's replacement of a session whose
	// agent exited, so the supervisor can refuse it (an e-stop, a park, a gt
	// down in progress) and log it (gt-4k3fj.4.1). nil runs it directly.
	Respawn func(reason string, run func() error) error
}

// CombinedStatus returns the combined status of the mayor across all modes.
func (m *Manager) CombinedStatus() (*MayorStatus, error) {
	status := &MayorStatus{
		Mode: ModeNone,
	}

	// Check TMUX
	tmuxRunning, _ := m.IsRunning()
	if tmuxRunning {
		info, err := m.Status()
		if err == nil {
			status.Tmux = info
			status.Active = true
			status.Mode = ModeTMUX
		}
	}

	// Check ACP
	if IsACPActive(m.townRoot) {
		status.Active = true
		if status.Mode == ModeTMUX {
			status.Mode = ModeBoth
		} else {
			status.Mode = ModeACP
		}
		pid, _ := GetACPPid(m.townRoot)
		status.ACPPid = pid
	}

	return status, nil
}

// IsActive checks if the mayor session is active in any mode.
func (m *Manager) IsActive() (bool, Mode) {
	status, _ := m.CombinedStatus()
	return status.Active, status.Mode
}

// NewManager creates a new mayor manager for a town.
func NewManager(townRoot string) *Manager {
	return &Manager{
		townRoot: townRoot,
	}
}

// SessionName returns the tmux session name for the mayor.
// This is a package-level function for convenience.
func SessionName() string {
	return session.MayorSessionName()
}

// SessionName returns the tmux session name for the mayor.
func (m *Manager) SessionName() string {
	return SessionName()
}

// mayorDir returns the working directory for the mayor.
func (m *Manager) mayorDir() string {
	return filepath.Join(m.townRoot, "mayor")
}

// Start starts the mayor session.
// It checks both TMUX and ACP modes and returns ErrAlreadyRunning if active.
// agentOverride optionally specifies a different agent alias to use.
func (m *Manager) Start(agentOverride string) error {
	// Refuse before any side effect when the gt binary's bd handshake failed.
	if err := bdgate.Require(); err != nil {
		return err
	}
	status, err := m.CombinedStatus()
	if err == nil && status.Active {
		switch status.Mode {
		case ModeACP, ModeBoth:
			return ErrACPActive
		case ModeTMUX:
			return ErrAlreadyRunning
		}
	}
	return m.StartTMUX(agentOverride)
}

// StartTMUX starts the mayor session in TMUX mode.
// agentOverride optionally specifies a different agent alias to use.
func (m *Manager) StartTMUX(agentOverride string) error {
	if IsACPActive(m.townRoot) {
		return ErrAlreadyRunning
	}

	t := tmux.NewTmux()
	sessionID := m.SessionName()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return ErrAlreadyRunning
	}
	if !running {
		return m.launch(t, sessionID, agentOverride)
	}
	// Only a confirmed dead agent is replaced (a zombie: tmux alive, agent
	// gone). A failed liveness query is UNKNOWN: refuse rather than kill a
	// session that may be working (gt-fcxe9.1).
	if alive, err := t.IsAgentAliveChecked(sessionID); err != nil || alive {
		return ErrAlreadyRunning
	}
	replace := func() error {
		if err := t.KillSessionWithProcesses(sessionID); err != nil {
			return fmt.Errorf("killing session %s: %w", sessionID, err)
		}
		return m.launch(t, sessionID, agentOverride)
	}
	if m.Respawn == nil {
		return replace()
	}
	return m.Respawn("mayor start: replace a session whose agent exited", replace)
}

// launch creates the mayor session and starts its agent.
func (m *Manager) launch(t *tmux.Tmux, sessionID, agentOverride string) error {
	// Ensure mayor directory exists (for Claude settings)
	mayorDir := m.mayorDir()
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		return fmt.Errorf("creating mayor directory: %w", err)
	}

	// Resolve CLAUDE_CONFIG_DIR from accounts.json so the mayor session
	// uses the correct account. Same pattern as crew startup (start.go).
	accountsPath := constants.MayorAccountsPath(m.townRoot)
	claudeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if claudeConfigDir == "" {
		claudeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}

	// Use unified session lifecycle for config → settings → command → create → env → theme → wait.
	theme := tmux.ResolveSessionTheme(m.townRoot, "", "mayor", "")
	_, err := session.StartSession(t, session.SessionConfig{
		SessionID:        sessionID,
		WorkDir:          mayorDir,
		Role:             "mayor",
		TownRoot:         m.townRoot,
		AgentName:        "Mayor",
		RuntimeConfigDir: claudeConfigDir,
		Beacon: session.BeaconConfig{
			Recipient: "mayor",
			Sender:    "human",
			Topic:     "cold-start",
		},
		AgentOverride: agentOverride,
		Theme:         theme,
		WaitForAgent:  true,
		WaitFatal:     true,
		AcceptBypass:  true,
	})
	if err != nil {
		return err
	}

	time.Sleep(session.ShutdownDelay())

	return nil
}

// Stop stops the mayor session.
func (m *Manager) Stop() error {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	// Check if session exists
	running, err := t.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrNotRunning
	}

	// Try graceful shutdown first (best-effort interrupt)
	_ = t.SendKeysRaw(sessionID, "C-c")
	time.Sleep(100 * time.Millisecond)

	// Kill the session and all its processes
	if m.StopKill != nil {
		if err := m.StopKill(sessionID); err != nil {
			return fmt.Errorf("killing session: %w", err)
		}
		return nil
	}
	if err := t.KillSessionWithProcesses(sessionID); err != nil {
		return fmt.Errorf("killing session: %w", err)
	}

	return nil
}

// IsRunning checks if the mayor session is active in TMUX mode.
func (m *Manager) IsRunning() (bool, error) {
	t := tmux.NewTmux()
	return t.HasSession(m.SessionName())
}

// Status returns information about the mayor session.
func (m *Manager) Status() (*tmux.SessionInfo, error) {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return nil, ErrNotRunning
	}

	return t.GetSessionInfo(sessionID)
}

// GetMayorPrime returns the rendered mayor prime context as a raw string.
// This includes the formula from templates and a timestamp, suitable for
// ACP initialize responses where the full context needs to be provided
// as a single string payload.
func GetMayorPrime(townRoot string) (string, error) {
	tmpl, err := templates.New()
	if err != nil {
		return "", fmt.Errorf("loading templates: %w", err)
	}

	townName, err := workspace.GetTownName(townRoot)
	if err != nil {
		townName = "unknown"
	}

	data := templates.RoleData{
		Role:         "mayor",
		TownRoot:     townRoot,
		TownName:     townName,
		WorkDir:      townRoot,
		MayorSession: session.MayorSessionName(),
	}

	content, err := tmpl.RenderRole("mayor", data)
	if err != nil {
		return "", fmt.Errorf("rendering mayor template: %w", err)
	}

	// Append timestamp
	timestamp := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf("[prime-rendered-at: %s]\n\n%s", timestamp, content), nil
}
