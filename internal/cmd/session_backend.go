package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// sessionBackendForAgent picks the session backend for a polecat slung on
// agent: the backend of the seat named agent in polecat_pool.seats, with
// its sandbox profile; an agent that is no seat (a role default, a direct
// --agent of a preset that has no seat) gets the tmux backend. host is the
// process's tmux client.
func sessionBackendForAgent(ts *config.TownSettings, townRoot, agent string, host *tmux.Tmux) (session.Backend, error) {
	return sessionBackendForAgentWith(session.Resolve, ts, townRoot, agent, host)
}

// backendResolver is session.Resolve's shape, injected by tests.
type backendResolver func(seat, backend string, p session.Profile, townRoot string, host *tmux.Tmux) (session.Backend, error)

func sessionBackendForAgentWith(resolve backendResolver, ts *config.TownSettings, townRoot, agent string, host *tmux.Tmux) (session.Backend, error) {
	if ts == nil || agent == "" {
		return session.NewTmuxBackend(host), nil
	}
	seats, err := config.Seats(ts, nil)
	if err != nil {
		return nil, fmt.Errorf("reading polecat_pool.seats: %w", err)
	}
	for _, seat := range seats {
		if seat.Agent != agent {
			continue
		}
		var p session.Profile
		if seat.Backend == config.BackendOpenShell {
			cp := ts.Sandboxes[seat.Sandbox].WithDefaults()
			p = session.Profile{
				Name:             seat.Sandbox,
				Gateway:          cp.Gateway,
				Image:            cp.Image,
				Policy:           cp.Policy,
				Providers:        append([]string(nil), cp.Providers...),
				CPU:              cp.CPU,
				Memory:           cp.Memory,
				ControlPlaneHost: cp.ControlPlaneHost,
				ForgejoHost:      cp.ForgejoHost,
				CacheVolume:      cp.CacheVolume,
				Driver:           cp.Driver,
				NodeSelector:     cp.NodeSelector,
				Tolerations:      cp.Tolerations,
				PriorityClass:    cp.PriorityClass,
				RuntimeClass:     cp.RuntimeClass,
				Requests:         cp.Requests,
				Limits:           cp.Limits,
			}
		}
		return resolve(seat.Agent, seat.Backend, p, townRoot, host)
	}
	return session.NewTmuxBackend(host), nil
}
