package cmd

import (
	"context"
	"time"

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

// sandboxSeatProbeFor is polecat.NewSandboxSeatProbe (the seat map, read once,
// failing closed on settings it cannot read) with one addition for the
// network-free list path (gt-8q0s): a sandbox with a registry record is running,
// so it is answered from the record without asking the gateway. The gateway
// question, a local CLI call of about 30 ms, is then paid only by a finished
// sandbox seat whose record is gone. exists is the gateway question (nil means
// the sandbox CLI); production passes nil and a test passes a stub.
func sandboxSeatProbeFor(townRoot string, exists sandboxExistsFunc) sandboxSeatProbe { //nolint:unparam // exists is the seam tests use; production asks the CLI
	if exists == nil {
		exists = func(gateway, name string) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), sandboxSeatTimeout)
			defer cancel()
			return (&sandbox.CLI{Gateway: gateway}).Exists(ctx, name)
		}
	}
	return sandboxSeatProbe(polecat.NewSandboxSeatProbe(townRoot, func(gateway, name string) (bool, error) {
		if recorded, err := session.IsSandboxed(townRoot, name); err == nil && recorded {
			return true, nil
		}
		return exists(gateway, name)
	}))
}

// polecatSessionNameIn is the session (and sandbox) name of a rig's polecat.
func polecatSessionNameIn(rigName, polecatName string) string {
	return session.PolecatSessionName(townRegistry().PrefixForRig(rigName), polecatName)
}
