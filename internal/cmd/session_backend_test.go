package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestSessionBackendForAgentFollowsTheSeat(t *testing.T) {
	t.Parallel()
	host := tmux.NewTmuxWithSocket("gt-test")
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{
		"claude-haiku":           {Cap: 1},
		"deepseek-flash-sandbox": {Cap: 1, Backend: config.BackendOpenShell, Sandbox: "polecat-default"},
	}}
	ts.Sandboxes = map[string]config.SandboxProfile{"polecat-default": {Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml", Providers: []string{"deepseek"}}}
	town := t.TempDir()

	b, err := sessionBackendForAgent(ts, town, "claude-haiku", host)
	if err != nil || b.Name() != session.BackendTmux || b.Tmux() != host {
		t.Fatalf("tmux seat: %v %v", b, err)
	}
	b, err = sessionBackendForAgent(ts, town, "", host)
	if err != nil || b.Name() != session.BackendTmux {
		t.Fatalf("no agent: %v %v", b, err)
	}
	b, err = sessionBackendForAgent(ts, town, "not-a-seat", host)
	if err != nil || b.Name() != session.BackendTmux {
		t.Fatalf("agent without a seat: %v %v", b, err)
	}

	if _, err := sessionBackendForAgentWith(func(seat, backend string, p session.Profile, townRoot string, host *tmux.Tmux) (session.Backend, error) {
		return session.ResolveWith(nil, seat, backend, p, townRoot, host)
	}, ts, town, "deepseek-flash-sandbox", host); err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Fatalf("openshell seat without the implementation: err = %v", err)
	}
	var got session.Profile
	resolve := func(seat, backend string, p session.Profile, townRoot string, host *tmux.Tmux) (session.Backend, error) {
		return session.ResolveWith(func(_ string, p session.Profile) (session.Backend, error) {
			got = p
			return session.NewTmuxBackend(host), nil
		}, seat, backend, p, townRoot, host)
	}
	if _, err := sessionBackendForAgentWith(resolve, ts, town, "deepseek-flash-sandbox", host); err != nil {
		t.Fatal(err)
	}
	if got.Name != "polecat-default" || got.Image != "gastown-polecat:dev" || got.Gateway != "openshell" || got.ControlPlaneHost != "host.docker.internal" || strings.Join(got.Providers, ",") != "deepseek" {
		t.Fatalf("profile handed to the backend = %+v", got)
	}
}
