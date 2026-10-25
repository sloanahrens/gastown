package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The town's seat settings name the sandbox agents; settings that cannot be
// read are an error the caller turns into an unknown row, never an empty answer
// that reads every seat as a host seat (gt-43k7c).
func TestDashSandboxSeatAgents(t *testing.T) {
	t.Parallel()

	t.Run("names the sandbox seats only", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		ts := config.NewTownSettings()
		ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{
			"claude-haiku":           {Cap: 1},
			"deepseek-flash-sandbox": {Cap: 1, Backend: config.BackendOpenShell, Sandbox: "polecat-default"},
		}}
		ts.Sandboxes = map[string]config.SandboxProfile{"polecat-default": {Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml", Providers: []string{"deepseek"}, ForgejoHost: "forgejo.internal"}}
		if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
			t.Fatal(err)
		}
		got, err := dashSandboxSeatAgents(town)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got["deepseek-flash-sandbox"] {
			t.Errorf("sandbox agents = %v, want only deepseek-flash-sandbox", got)
		}
	})

	t.Run("unreadable settings are an error", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := config.TownSettingsPath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := dashSandboxSeatAgents(town)
		if err == nil {
			t.Fatalf("sandbox agents = %v with no error, want the read error", got)
		}
	})
}

// A seat is sampled once the patrol has recorded its liveness. No record yet is
// the starting case; a record that exists but cannot be read must not leave the
// seat reading "starting" forever (gt-43k7c).
func TestDashSandboxSampled(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, town, body string) {
		t.Helper()
		path := polecatIntentSeat("gastown", "ember").Path(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		body string // empty: no record on disk
		want bool
	}{
		{"no record yet", "", false},
		{"record without a liveness sample", `{}`, false},
		{"record with a liveness sample", `{"progress":{}}`, true},
		{"malformed record", `{not json`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tt.body != "" {
				write(t, town, tt.body)
			}
			if got := dashSandboxSampled(town, "gastown", "ember"); got != tt.want {
				t.Errorf("sampled = %v, want %v", got, tt.want)
			}
		})
	}
}

// When the seat settings cannot be read the row cannot say a seat the host tmux
// does not see is stalled: it may be a live sandbox seat. It reads unknown, with
// the reason, and a seat with no work is unaffected.
func TestBuildDashPolecatsSandboxSettingsUnreadable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 9, 50, 0, 0, time.UTC)
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	build := func(state polecat.State, withWork bool) dashboard.Polecat {
		s := dashSeat{Rig: "gastown", Name: "ember", Session: "gt-ember", Item: dashItem(state, cap1),
			Sandbox: dashSandboxSeat{Unknown: true}, HookedAt: now.Add(-8 * time.Minute)}
		if withWork {
			s.Issue = &beads.Issue{ID: "gt-1", Title: "one"}
		}
		got := buildDashPolecats(dashPolecatInputs{Now: now, SessionsKnown: true, Seats: []dashSeat{s}})
		if len(got) != 1 {
			t.Fatalf("rows = %d, want 1", len(got))
		}
		return got[0]
	}
	if got := build(polecat.StateStalled, true); got.State != dashboard.StateUnknown || got.Reason == "" {
		t.Errorf("stalled with unreadable settings: state = %q (%q), want %q with a reason", got.State, got.Reason, dashboard.StateUnknown)
	}
	if got := build(polecat.StateIdle, false); got.State != dashboard.StateIdle {
		t.Errorf("idle with unreadable settings: state = %q, want %q", got.State, dashboard.StateIdle)
	}
}

// A seat's sandbox standing: settings that could not be read make it unknown,
// a sandbox agent is a sandbox seat whose sample is read only then, and any
// other agent is a host seat.
func TestDashSandboxStatus(t *testing.T) {
	t.Parallel()
	agents := map[string]bool{"deepseek-flash-sandbox": true}
	asked := 0
	sampled := func() bool { asked++; return true }

	if got := dashSandboxStatus(nil, errors.New("unreadable"), "deepseek-flash-sandbox", sampled); !got.Unknown || got.Seat {
		t.Errorf("unreadable settings: %+v, want Unknown and not a sandbox seat", got)
	}
	if got := dashSandboxStatus(agents, nil, "deepseek-flash-sandbox", sampled); got != (dashSandboxSeat{Seat: true, Sampled: true}) {
		t.Errorf("sandbox agent: %+v, want a sampled sandbox seat", got)
	}
	if asked != 1 {
		t.Errorf("sampled asked %d times for the sandbox agent, want 1", asked)
	}
	if got := dashSandboxStatus(agents, nil, "claude-haiku", sampled); got != (dashSandboxSeat{}) {
		t.Errorf("host agent: %+v, want a host seat", got)
	}
	if asked != 1 {
		t.Errorf("sampled asked for a host seat (%d calls total), want it left unread", asked)
	}
}
