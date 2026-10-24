package cmd

import (
	"context"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// sandboxSeatTimeout bounds the one gateway question a seat's git state asks.
const sandboxSeatTimeout = 15 * time.Second

// sandboxSeatProbe answers the git state of a sandbox seat: agent is the agent
// the polecat's agent bead recorded and sessionName its session, which is also
// its sandbox's name. ok is false for a host seat, whose worktree the caller
// probes as before. A sandbox seat's host worktree is a stale leftover (its
// commits were pushed from inside the sandbox), so it is never measured: the
// gateway's answer decides (polecat.SandboxSeatGitState, gt-a0x2w).
type sandboxSeatProbe func(agent, sessionName string) (state polecat.LiveGitState, ok bool)

// sandboxExistsFunc asks a gateway whether it has a sandbox called name.
type sandboxExistsFunc func(gateway, name string) (bool, error)

// sandboxSeatProbeFor reads the seat map of the town at townRoot and asks the
// gateway through exists (nil means the sandbox CLI). A settings file that
// cannot be read leaves the seat unclassified, the same as a host seat.
//
// The read never writes: config.LoadOrCreateTownSettings returns the defaults
// for a missing file and creates nothing (pinned by a test). Only a seat whose
// recorded agent is an openshell seat pays the gateway question, a local CLI
// call of about 30 ms, and a sandbox with a registry record is answered from
// that record alone, so the network-free list path (gt-8q0s) makes a call only
// for a finished sandbox seat whose record is gone.
func sandboxSeatProbeFor(townRoot string, exists sandboxExistsFunc) sandboxSeatProbe {
	if exists == nil {
		exists = func(gateway, name string) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), sandboxSeatTimeout)
			defer cancel()
			return (&sandbox.CLI{Gateway: gateway}).Exists(ctx, name)
		}
	}
	return func(agent, sessionName string) (polecat.LiveGitState, bool) {
		if agent == "" {
			return polecat.LiveGitState{}, false
		}
		ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
		if err != nil {
			return polecat.LiveGitState{}, false
		}
		seats, err := config.Seats(ts, nil)
		if err != nil {
			return polecat.LiveGitState{}, false
		}
		for _, seat := range seats {
			if seat.Agent != agent {
				continue
			}
			if seat.Backend != config.BackendOpenShell {
				return polecat.LiveGitState{}, false
			}
			// A registry record means the sandbox is, or is about to be reaped as,
			// running: its state lives there, and the gateway need not be asked.
			if recorded, err := session.IsSandboxed(townRoot, sessionName); err == nil && recorded {
				return polecat.SandboxSeatGitState("", true, nil, sessionName), true
			}
			gateway := ts.Sandboxes[seat.Sandbox].WithDefaults().Gateway
			found, existsErr := exists(gateway, sessionName)
			return polecat.SandboxSeatGitState("", found, existsErr, sessionName), true
		}
		return polecat.LiveGitState{}, false
	}
}

// polecatSessionNameIn is the session (and sandbox) name of a rig's polecat.
func polecatSessionNameIn(rigName, polecatName string) string {
	return session.PolecatSessionName(townRegistry().PrefixForRig(rigName), polecatName)
}
