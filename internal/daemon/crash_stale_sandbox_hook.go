package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// staleSandboxHookTimeout bounds the one gateway question the reap asks.
const staleSandboxHookTimeout = 15 * time.Second

// reapStaleSandboxHook clears a sandbox seat's agent-bead hook that nothing
// will ever release, and reports whether it did. A sandbox seat whose sandbox
// is gone (a rollback, a gateway or Docker restart) and whose work bead was
// handed back leaves hook_bead naming that bead on the agent bead, and the pool
// roster counts the seat as dead-hooked until an operator runs gt polecat nuke.
//
// It acts only when every condition holds, and any one that cannot be answered
// leaves the hook alone:
//   - the seat's agent (the one its agent bead recorded) is an openshell seat in
//     the town's settings, so a host seat, whose patrol restart needs the hook,
//     is never touched;
//   - no registry record names the session and the gateway has no sandbox of
//     that name (a sandbox whose record write failed is not orphaned);
//   - the hooked work bead is open and unassigned, handed back.
//
// The work bead is read first: a seat whose work is still assigned is the common
// crash, and asks nothing else.
//
// The caller has declared the crash (the session is dead, the spawn grace
// passed, the bead is neither closed nor submitted for landing). The agent bead
// is reset only while its hook_bead still names hookBead.
func (h *patrolScanHost) reapStaleSandboxHook(rig, name, sessionName, hookBead string) bool {
	log := func(format string, args ...any) {
		h.d.logger.Printf("Stale sandbox hook of %s/%s (%s): "+format, append([]any{rig, name, hookBead}, args...)...)
	}
	issue, err := h.d.workBeads(h.d.workBeadsEnv(rig), workBeadReadTimeout).Show(hookBead)
	if err != nil {
		log("not reaped: cannot read the work bead: %v", err)
		return false
	}
	if issue.Status != "open" || strings.TrimSpace(issue.Assignee) != "" {
		return false
	}
	seat, ok, err := h.openshellSeat(rig, name)
	if err != nil {
		log("not reaped: %v", err)
		return false
	}
	if !ok {
		return false
	}
	if on, err := session.IsSandboxed(h.town(), sessionName); err != nil || on {
		if err != nil {
			log("not reaped: sandbox registry unreadable: %v", err)
		}
		return false
	}
	exists, err := h.sandboxStillExists(seat.gateway, sessionName)
	if err != nil {
		log("not reaped: cannot ask gateway %q about sandbox %s: %v", seat.gateway, sessionName, err)
		return false
	}
	if exists {
		return false
	}
	id := beads.AgentBeadIDFor(constants.RolePolecat, rig, name, h.town())
	if id == "" {
		return false
	}
	reset, err := beads.ResetAgentBeadWhileHooked(h.recoveryBeads(), id, hookBead, "stale sandbox hook")
	if err != nil {
		log("not reaped: resetting agent bead %s: %v", id, err)
		return false
	}
	if reset {
		log("agent bead %s reset: the sandbox is gone and the work was handed back", id)
	}
	return reset
}

// openshellSeatInfo is the part of an openshell seat the reap reads.
type openshellSeatInfo struct{ gateway string }

// openshellSeat reports whether the polecat's recorded agent is an openshell
// seat in the town's settings, and the gateway its sandboxes live on. A polecat
// that records no agent, or an agent with no seat, is not one.
func (h *patrolScanHost) openshellSeat(rig, name string) (openshellSeatInfo, bool, error) {
	agent, err := h.recordedAgent(rig, name)
	if err != nil {
		return openshellSeatInfo{}, false, fmt.Errorf("reading the agent bead: %w", err)
	}
	if agent == "" {
		return openshellSeatInfo{}, false, nil
	}
	ts, err := agentconfig.LoadOrCreateTownSettings(agentconfig.TownSettingsPath(h.town()))
	if err != nil {
		return openshellSeatInfo{}, false, fmt.Errorf("reading town settings: %w", err)
	}
	seats, err := agentconfig.Seats(ts, nil)
	if err != nil {
		return openshellSeatInfo{}, false, fmt.Errorf("reading polecat_pool.seats: %w", err)
	}
	for _, s := range seats {
		if s.Agent != agent {
			continue
		}
		if s.Backend != agentconfig.BackendOpenShell {
			return openshellSeatInfo{}, false, nil
		}
		return openshellSeatInfo{gateway: ts.Sandboxes[s.Sandbox].WithDefaults().Gateway}, true, nil
	}
	return openshellSeatInfo{}, false, nil
}

// sandboxStillExists asks gateway whether it has a sandbox called name.
func (h *patrolScanHost) sandboxStillExists(gateway, name string) (bool, error) {
	if h.sandboxExists != nil {
		return h.sandboxExists(gateway, name)
	}
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), staleSandboxHookTimeout)
	defer cancel()
	return (&sandbox.CLI{Gateway: gateway}).Exists(ctx, name)
}
