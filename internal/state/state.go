// ABOUTME: Global state management for Gas Town enable/disable toggle.
// ABOUTME: Uses XDG-compliant paths for per-machine state storage.

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/atomicfile"
)

// State represents the global Gas Town state.
type State struct {
	Enabled          bool      `json:"enabled"`
	Version          string    `json:"version"`
	MachineID        string    `json:"machine_id"`
	InstalledAt      time.Time `json:"installed_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	ShellIntegration string    `json:"shell_integration,omitempty"`
	LastDoctorRun    time.Time `json:"last_doctor_run,omitempty"`
}

// environ is the slice of the process environment this package reads.
// Production uses osEnviron; tests build one over a map and a fixed home.
type environ struct {
	getenv  func(string) string
	homeDir func() (string, error)
}

func osEnviron() environ {
	return environ{getenv: os.Getenv, homeDir: os.UserHomeDir}
}

// StateDir returns the XDG-compliant state directory.
// Uses ~/.local/state/gastown/ (per XDG Base Directory Specification).
func StateDir() string { return osEnviron().stateDir() }

func (e environ) stateDir() string {
	// Check XDG_STATE_HOME first
	if xdg := e.getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "gastown")
	}
	home, _ := e.homeDir()
	return filepath.Join(home, ".local", "state", "gastown")
}

// ConfigDir returns the XDG-compliant config directory.
// Uses ~/.config/gastown/
func ConfigDir() string { return osEnviron().configDir() }

func (e environ) configDir() string {
	if xdg := e.getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gastown")
	}
	home, _ := e.homeDir()
	return filepath.Join(home, ".config", "gastown")
}

// CacheDir returns the XDG-compliant cache directory.
// Uses ~/.cache/gastown/
func CacheDir() string { return osEnviron().cacheDir() }

func (e environ) cacheDir() string {
	if xdg := e.getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "gastown")
	}
	home, _ := e.homeDir()
	return filepath.Join(home, ".cache", "gastown")
}

// StatePath returns the path to state.json.
func StatePath() string { return osEnviron().statePath() }

func (e environ) statePath() string {
	return filepath.Join(e.stateDir(), "state.json")
}

// IsEnabled checks if Gas Town is globally enabled.
// Priority: env override > state file > default (false)
func IsEnabled() bool { return osEnviron().isEnabled() }

func (e environ) isEnabled() bool {
	// Environment overrides take priority
	if e.getenv("GASTOWN_DISABLED") == "1" {
		return false
	}
	if e.getenv("GASTOWN_ENABLED") == "1" {
		return true
	}

	// Check state file
	state, err := e.load()
	if err != nil {
		return false // Default to disabled if state unreadable
	}
	return state.Enabled
}

// Load reads the state from disk.
func Load() (*State, error) { return osEnviron().load() }

func (e environ) load() (*State, error) {
	data, err := os.ReadFile(e.statePath())
	if os.IsNotExist(err) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// Save writes the state to disk atomically with 0600 permissions.
func Save(s *State) error { return osEnviron().save(s) }

func (e environ) save(s *State) error {
	dir := e.stateDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	s.UpdatedAt = time.Now()

	return atomicfile.WriteJSONWithPerm(e.statePath(), s, 0600)
}

// Enable enables Gas Town globally.
func Enable(version string) error { return osEnviron().enable(version) }

func (e environ) enable(version string) error {
	s, err := e.load()
	if err != nil {
		// Create new state
		s = &State{
			InstalledAt: time.Now(),
			MachineID:   generateMachineID(),
		}
	}

	s.Enabled = true
	s.Version = version
	return e.save(s)
}

// Disable disables Gas Town globally.
func Disable() error { return osEnviron().disable() }

func (e environ) disable() error {
	s, err := e.load()
	if err != nil {
		// Nothing to disable, create disabled state
		s = &State{
			InstalledAt: time.Now(),
			MachineID:   generateMachineID(),
			Enabled:     false,
		}
		return e.save(s)
	}

	s.Enabled = false
	return e.save(s)
}

// generateMachineID creates a unique machine identifier.
func generateMachineID() string {
	return uuid.New().String()[:8]
}

// GetMachineID returns the machine ID, creating one if needed.
func GetMachineID() string { return osEnviron().getMachineID() }

func (e environ) getMachineID() string {
	s, err := e.load()
	if err != nil || s.MachineID == "" {
		return generateMachineID()
	}
	return s.MachineID
}

// SetShellIntegration records which shell integration is installed.
func SetShellIntegration(shell string) error { return osEnviron().setShellIntegration(shell) }

func (e environ) setShellIntegration(shell string) error {
	s, err := e.load()
	if err != nil {
		s = &State{
			InstalledAt: time.Now(),
			MachineID:   generateMachineID(),
		}
	}
	s.ShellIntegration = shell
	return e.save(s)
}

// RecordDoctorRun records when doctor was last run.
func RecordDoctorRun() error { return osEnviron().recordDoctorRun() }

func (e environ) recordDoctorRun() error {
	s, err := e.load()
	if err != nil {
		return err
	}
	s.LastDoctorRun = time.Now()
	return e.save(s)
}
