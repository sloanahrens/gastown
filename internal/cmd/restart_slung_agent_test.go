package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

type recordingStarter struct {
	name  string
	opts  polecat.SessionStartOptions
	start bool
}

func (r *recordingStarter) Start(name string, opts polecat.SessionStartOptions) error {
	r.name, r.opts, r.start = name, opts, true
	return nil
}

// noBackend ignores the town: every agent resolves to host tmux without reading
// its settings, so a test of the agent a restart starts does not depend on them.
func noBackend(agent string) (session.Backend, error) {
	return session.NewTmuxBackend(tmux.NewTmuxWithSocket("gt-test")), nil
}

// TestRestartWithoutTownSettingsWritesNothing: a town with no settings file
// restarts its seat on the host tmux default and leaves no settings file behind.
func TestRestartWithoutTownSettingsWritesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), "deepseek-flash", "test", time.Now()); err != nil {
		t.Fatalf("MarkSlung: %v", err)
	}
	s := &recordingStarter{}
	backendFor := townSeatBackend(townRoot, tmux.NewTmuxWithSocket("gt-test"))
	if err := restartPolecatOnSlungAgent(s, backendFor, townRoot, "gastown", "nux", polecat.SessionStartOptions{}); err != nil {
		t.Fatalf("restartPolecatOnSlungAgent: %v", err)
	}
	if s.opts.Backend == nil || s.opts.Backend.Name() != session.BackendTmux {
		t.Errorf("Backend = %v, want %s", s.opts.Backend, session.BackendTmux)
	}
	if _, err := os.Stat(config.TownSettingsPath(townRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restart left a settings file behind: stat err = %v, want not-exist", err)
	}
}

// TestRestartPolecatOnSlungAgent: a restarted seat runs the agent its sling
// recorded, and a seat whose record names none keeps the role default (gt-rsrnr).
func TestRestartPolecatOnSlungAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		slungOn   string // "" = MarkSlung on the role default; "-" = no record at all
		optsAgent string
		want      string
	}{
		{"record names an agent", "deepseek-flash", "", "deepseek-flash"},
		{"record names no agent", "", "", ""},
		{"no record", "-", "", ""},
		{"explicit agent wins over the record", "deepseek-flash", "claude-haiku", "claude-haiku"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tt.slungOn != "-" {
				if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), tt.slungOn, "test", time.Now()); err != nil {
					t.Fatalf("MarkSlung: %v", err)
				}
			}
			s := &recordingStarter{}
			err := restartPolecatOnSlungAgent(s, noBackend, townRoot, "gastown", "nux", polecat.SessionStartOptions{Agent: tt.optsAgent})
			if err != nil {
				t.Fatalf("restartPolecatOnSlungAgent: %v", err)
			}
			if s.name != "nux" || s.opts.Agent != tt.want {
				t.Errorf("Start(%q, Agent=%q), want nux on %q", s.name, s.opts.Agent, tt.want)
			}
		})
	}
}

// TestRestartPolecatOnSlungAgentUnreadableRecord: a record that cannot be
// parsed must not stop the restart; the seat starts on the role default.
func TestRestartPolecatOnSlungAgentUnreadableRecord(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := polecatIntentSeat("gastown", "nux").Path(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "{not json")
	s := &recordingStarter{}
	if err := restartPolecatOnSlungAgent(s, noBackend, townRoot, "gastown", "nux", polecat.SessionStartOptions{}); err != nil {
		t.Fatalf("restartPolecatOnSlungAgent: %v", err)
	}
	if s.opts.Agent != "" {
		t.Errorf("Agent = %q, want the role default", s.opts.Agent)
	}
}

// TestRestartPolecatOnSlungAgentResolvesBackend: the restart starts the seat
// on the backend its agent resolves to, so an agent in an openshell seat is
// never restarted on the host; a backend that cannot be resolved refuses the
// restart instead of falling back to tmux (gt-da3ea).
func TestRestartPolecatOnSlungAgentResolvesBackend(t *testing.T) {
	t.Parallel()
	sentinel := session.NewTmuxBackend(tmux.NewTmuxWithSocket("gt-test"))
	resolveErr := errors.New("seat deepseek-flash-sandbox: backend openshell is not linked into this binary")
	tests := []struct {
		name      string
		slungOn   string
		opts      polecat.SessionStartOptions
		resolve   seatBackendResolver
		wantAsked bool
		wantAgent string // what the resolver was asked about
		wantErr   bool
	}{
		{
			name:      "record names a sandboxed agent",
			slungOn:   "deepseek-flash-sandbox",
			resolve:   func(agent string) (session.Backend, error) { return sentinel, nil },
			wantAsked: true,
			wantAgent: "deepseek-flash-sandbox",
		},
		{
			name:    "explicit backend wins without asking",
			slungOn: "deepseek-flash-sandbox",
			opts:    polecat.SessionStartOptions{Backend: sentinel},
			resolve: func(string) (session.Backend, error) {
				return nil, errors.New("resolver must not be called")
			},
		},
		{
			name:    "unresolvable backend refuses the restart",
			slungOn: "deepseek-flash-sandbox",
			resolve: func(string) (session.Backend, error) { return nil, resolveErr },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), tt.slungOn, "test", time.Now()); err != nil {
				t.Fatalf("MarkSlung: %v", err)
			}
			asked, askedFor := "", false
			resolve := func(agent string) (session.Backend, error) {
				asked, askedFor = agent, true
				return tt.resolve(agent)
			}
			s := &recordingStarter{}
			err := restartPolecatOnSlungAgent(s, resolve, townRoot, "gastown", "nux", tt.opts)
			if tt.wantErr {
				if !errors.Is(err, resolveErr) {
					t.Fatalf("err = %v, want the resolver's error", err)
				}
				if s.start {
					t.Error("Start was called for a seat whose backend could not be resolved")
				}
				return
			}
			if err != nil {
				t.Fatalf("restartPolecatOnSlungAgent: %v", err)
			}
			if askedFor != tt.wantAsked || asked != tt.wantAgent {
				t.Errorf("resolver asked %v about %q, want %v about %q", askedFor, asked, tt.wantAsked, tt.wantAgent)
			}
			if s.opts.Backend != sentinel {
				t.Errorf("Backend = %v, want the sentinel backend", s.opts.Backend)
			}
		})
	}
}

// TestRestartPolecatOnSlungAgentSandboxSeat: an intent naming an openshell
// seat's agent comes back on that backend, read from the town settings the
// way a sling reads them, so the restart cannot put the sandboxed seat on the
// host with the host worktree and file-backed secrets (gt-da3ea).
func TestRestartPolecatOnSlungAgentSandboxSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sandbox  config.SandboxProfile // the seat's sandbox profile
		wantBack string
		wantErr  string
	}{
		{
			name:     "seat with a complete sandbox profile",
			sandbox:  config.SandboxProfile{Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml"},
			wantBack: session.BackendOpenShell,
		},
		{
			name:    "seat whose sandbox profile is incomplete",
			sandbox: config.SandboxProfile{Image: "gastown-polecat:dev"},
			wantErr: "polecat-default.policy: required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			ts := config.NewTownSettings()
			ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{
				"deepseek-flash":         {Cap: 1},
				"deepseek-flash-sandbox": {Cap: 1, Backend: config.BackendOpenShell, Sandbox: "polecat-default"},
			}}
			ts.Sandboxes = map[string]config.SandboxProfile{"polecat-default": tt.sandbox}
			if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
				t.Fatalf("SaveTownSettings: %v", err)
			}
			if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), "deepseek-flash-sandbox", "test", time.Now()); err != nil {
				t.Fatalf("MarkSlung: %v", err)
			}
			s := &recordingStarter{}
			backendFor := townSeatBackend(townRoot, tmux.NewTmuxWithSocket("gt-test"))
			err := restartPolecatOnSlungAgent(s, backendFor, townRoot, "gastown", "nux", polecat.SessionStartOptions{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tt.wantErr)
				}
				if s.start {
					t.Error("Start was called for a seat that must be refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("restartPolecatOnSlungAgent: %v", err)
			}
			if s.opts.Backend == nil || s.opts.Backend.Name() != tt.wantBack {
				t.Errorf("Backend = %v, want %s", s.opts.Backend, tt.wantBack)
			}
			if s.opts.Agent != "deepseek-flash-sandbox" {
				t.Errorf("Agent = %q, want the recorded seat agent", s.opts.Agent)
			}
		})
	}
}
