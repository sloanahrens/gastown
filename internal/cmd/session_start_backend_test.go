package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// sandboxStartTown writes a town whose gastown rig owns nux, and the settings
// gt session start reads from that town: deepseek-flash seats on the host,
// deepseek-flash-sandbox on an openshell seat named by sandbox. When slungOn is
// non-empty it records nux as slung on that agent.
func sandboxStartTown(t *testing.T, sandbox config.SandboxProfile, slungOn string) string {
	t.Helper()
	townRoot := setupSessionSeatTown(t, "nux")
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{
		"deepseek-flash":         {Cap: 1},
		"deepseek-flash-sandbox": {Cap: 1, Backend: config.BackendOpenShell, Sandbox: "polecat-default"},
	}}
	ts.Sandboxes = map[string]config.SandboxProfile{"polecat-default": sandbox}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if slungOn != "" {
		if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), slungOn, "test", time.Now()); err != nil {
			t.Fatalf("MarkSlung: %v", err)
		}
	}
	return townRoot
}

// seatInTown resolves gastown/nux in townRoot the way every gt session verb
// resolves its address, so the start under test reads its town from the seat.
func seatInTown(t *testing.T, townRoot string) sessionSeat {
	t.Helper()
	seat, err := resolveSessionSeatWith([]string{"gastown/nux"}, rigsIn(townRoot))
	if err != nil {
		t.Fatalf("resolveSessionSeat(gastown/nux): %v", err)
	}
	return seat
}

// completeSandbox is a sandbox profile config.Seats accepts as openshell-ready.
var completeSandbox = config.SandboxProfile{Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml"}

// TestStartSeatSessionSandboxSeat: gt session start places the seat on the
// backend its recorded agent names, read from the seat's own town, and a
// backend that cannot be built refuses the start instead of falling back to
// the host (gt-zvuyn).
func TestStartSeatSessionSandboxSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sandbox  config.SandboxProfile
		slungOn  string
		wantBack string
		wantErr  string
	}{
		{
			name:     "seat on an openshell agent starts with the openshell backend",
			sandbox:  completeSandbox,
			slungOn:  "deepseek-flash-sandbox",
			wantBack: session.BackendOpenShell,
		},
		{
			name:     "seat on a host agent keeps today's tmux backend",
			sandbox:  completeSandbox,
			slungOn:  "deepseek-flash",
			wantBack: session.BackendTmux,
		},
		{
			name:     "seat with no recorded agent keeps the role default's tmux backend",
			sandbox:  completeSandbox,
			slungOn:  "",
			wantBack: session.BackendTmux,
		},
		{
			name:    "seat whose sandbox profile is incomplete refuses the start",
			sandbox: config.SandboxProfile{Image: "gastown-polecat:dev"},
			slungOn: "deepseek-flash-sandbox",
			wantErr: "polecat-default.policy: required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seat := seatInTown(t, sandboxStartTown(t, tt.sandbox, tt.slungOn))
			s := &recordingStarter{}
			err := startSeatSession(seat, s, polecat.SessionStartOptions{Issue: "gt-zvuyn"})
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
				t.Fatalf("startSeatSession: %v", err)
			}
			if s.name != "nux" {
				t.Errorf("Start(%q), want the resolved seat's polecat nux", s.name)
			}
			if s.opts.Backend == nil || s.opts.Backend.Name() != tt.wantBack {
				t.Errorf("Backend = %v, want %s", s.opts.Backend, tt.wantBack)
			}
			if s.opts.Issue != "gt-zvuyn" {
				t.Errorf("Issue = %q, want the start's issue", s.opts.Issue)
			}
		})
	}
}

// TestStartSeatSessionUnreadableSettings: town settings that cannot be read
// decide nothing, so the start refuses instead of choosing the host for a seat
// whose sling may have put it in a sandbox (gt-zvuyn).
func TestStartSeatSessionUnreadableSettings(t *testing.T) {
	t.Parallel()
	townRoot := setupSessionSeatTown(t, "nux")
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), config.NewTownSettings()); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	writeTestFile(t, config.TownSettingsPath(townRoot), "{not json")
	if err := intent.MarkSlung(townRoot, polecatIntentSeat("gastown", "nux"), "deepseek-flash-sandbox", "test", time.Now()); err != nil {
		t.Fatalf("MarkSlung: %v", err)
	}

	s := &recordingStarter{}
	err := startSeatSession(seatInTown(t, townRoot), s, polecat.SessionStartOptions{})
	if err == nil {
		t.Fatal("startSeatSession = nil error, want the unreadable settings to refuse the start")
	}
	if s.start {
		t.Error("Start was called despite the settings being unreadable")
	}
}
